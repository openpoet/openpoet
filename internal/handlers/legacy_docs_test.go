package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/mcp"
)

// documentTestRouter is reviewTestRouter with the routes documents are created
// and listed through.
func documentTestRouter(t *testing.T) (*API, PlatformServices, http.Handler) {
	t.Helper()
	api, services, routes := reviewTestRouter(t)
	router := chi.NewRouter()
	router.Post("/api/documents", api.CreateTempDocument)
	router.Get("/api/sessions/{id}/documents", api.ListSessionDocuments)
	router.Get("/api/artifacts/legacy-docs/report", api.LegacyDocsReport)
	router.Mount("/", routes)
	return api, services, router
}

func countDocs(t *testing.T, api *API) int {
	t.Helper()
	var count int
	if err := api.db.Get(&count, `SELECT COUNT(*) FROM temp_documents`); err != nil {
		t.Fatal(err)
	}
	return count
}

// What the document route and the document tool create is a markdown artifact,
// named by a document id and a document link as before. No Doc is stored.
func TestCreatingADocumentPublishesAMarkdownArtifact(t *testing.T) {
	api, _, router := documentTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "documents")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Write notes", Status: "todo", Priority: "medium"}
	if err := api.db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-docs", ProjectID: project.ID, Status: "running", Name: "writer", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}

	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/documents",
		map[string]any{"title": "Meeting notes", "content": "# Notes\n\n- one\n- two", "session_id": "sess-docs", "task_id": itoa(task.ID)}, nil)
	id, _ := created["id"].(string)
	if recorder.Code != http.StatusCreated || id == "" || created["link"] != "/app/doc/"+id {
		t.Fatalf("create = %d %v", recorder.Code, created)
	}
	if countDocs(t, api) != 0 {
		t.Fatal("creating a document stored a Doc")
	}
	artifact, err := api.db.GetArtifact(ctx, id)
	if err != nil || artifact.Title != "Meeting notes" || artifact.SessionID != "sess-docs" || artifact.TaskID.Int64 != task.ID || artifact.ProjectID.Int64 != project.ID {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	if version, err := api.db.GetArtifactVersion(ctx, id, 1); err != nil || version.Format != database.ArtifactFormatMarkdown || version.Content != "# Notes\n\n- one\n- two" {
		t.Fatalf("version = %+v err=%v", version, err)
	}
	if review, _ := api.db.GetArtifactReview(ctx, id); review != nil {
		t.Fatalf("a document got a review: %+v", review)
	}

	// The id reads and opens as a document: the API names the artifact, the link redirects.
	if recorder, document := artifactRequest(t, router, http.MethodGet, "/api/documents/"+id, nil, nil); recorder.Code != http.StatusOK ||
		document["artifact_id"] != id || document["content"] != "# Notes\n\n- one\n- two" || document["title"] != "Meeting notes" {
		t.Fatalf("document = %d %v", recorder.Code, document)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app/doc/"+id, nil))
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/app/artifact/"+id {
		t.Fatalf("document link = %d -> %q", response.Code, response.Header().Get("Location"))
	}

	// The session tool does the same, and the session lists the documents once, as artifacts.
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	arguments, _ := json.Marshal(map[string]any{"title": "Tool notes", "content": "Written by the tool."})
	result, err := mcp.ExecuteTool(mcp.NewAPIClient(server.URL), "openpoet_create_document", arguments, "sess-docs", "")
	match := docLinkInToolResult.FindStringSubmatch(result)
	if err != nil || match == nil {
		t.Fatalf("openpoet_create_document = %q err=%v", result, err)
	}
	if version, err := api.db.GetArtifactVersion(ctx, match[1], 1); err != nil || version.Format != database.ArtifactFormatMarkdown || version.Content != "Written by the tool." {
		t.Fatalf("version of the tool's document = %+v err=%v", version, err)
	}
	if countDocs(t, api) != 0 {
		t.Fatal("the document tool stored a Doc")
	}
	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sessions/sess-docs/documents", nil))
	var documents []map[string]any
	decodeJSONBody(t, list, &documents)
	ids := map[string]string{}
	for _, document := range documents {
		ids[document["id"].(string)] = document["type"].(string)
	}
	if len(documents) != 2 || ids["artifact:"+id] != "artifact" || ids["artifact:"+match[1]] != "artifact" {
		t.Fatalf("documents of the session = %v", documents)
	}

	// Empty content is still refused.
	if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/documents", map[string]any{"title": "Empty", "content": " "}, nil); recorder.Code == http.StatusCreated {
		t.Fatal("an empty document was created")
	}
}

// A document the chat assistant creates is an artifact of its conversation and
// of the project the conversation was opened on, and keeps its card on reload.
func TestChatDocumentIsAnArtifactThatKeepsItsCard(t *testing.T) {
	api, services, router := documentTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "chat-documents")
	conversation := &database.AIConversation{Title: "Docs", ProactiveContext: `{"project_id":` + itoa(project.ID) + `}`}
	if err := api.db.CreateAIConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	// The assistant message being written when the document is created.
	result, err := api.db.Exec(`INSERT INTO ai_messages (conversation_id, role, content, tool_calls, status, created_at) VALUES (?, 'assistant', '', '[]', 'streaming', datetime('now', '-5 seconds'))`, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	messageID, _ := result.LastInsertId()
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "ui"}, Approved: true, ApprovedBy: "ui", Reason: "test"}

	created, err := services.AIHandler.executeTool(ctx, "create_document", map[string]interface{}{"title": "Summary", "content": "A long answer.\n\nIn markdown."}, conversation.ID, messageID, nil, "", auth)
	match := docLinkInToolResult.FindStringSubmatch(created)
	if err != nil || match == nil || !strings.Contains(created, "Document created successfully") {
		t.Fatalf("create_document = %q err=%v", created, err)
	}
	id := match[1]
	artifact, err := api.db.GetArtifact(ctx, id)
	if err != nil || artifact.ConversationID.Int64 != conversation.ID || artifact.ProjectID.Int64 != project.ID || artifact.Title != "Summary" {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	if countDocs(t, api) != 0 {
		t.Fatal("the chat's create_document stored a Doc")
	}
	// The chat builds its card from the result, under the document id.
	if card := services.AIHandler.buildDocCard("create_document", created, map[string]interface{}{"title": "Summary"}); card["doc_id"] != id || card["type"] != "document" {
		t.Fatalf("card = %v", card)
	}
	// On reload the card comes back, under the message that made it.
	cards := api.conversationDocCards(ctx, conversation.ID)
	if len(cards) != 1 || cards[0].ID != id || cards[0].Type != "document" || cards[0].Title != "Summary" || cards[0].MessageID != messageID {
		t.Fatalf("cards on reload = %+v (message %d)", cards, messageID)
	}
	// The assistant reads it back by that id.
	if read, err := services.AIHandler.executeTool(ctx, "read_document", map[string]interface{}{"document_id": id}, conversation.ID, 0, nil, "", auth); err != nil || !strings.Contains(read, "In markdown.") {
		t.Fatalf("read_document = %q err=%v", read, err)
	}
	// It is a document, not a proposal: it stays when the conversation goes.
	if err := api.db.DeleteAIConversation(ctx, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := api.db.GetArtifact(ctx, id); err != nil {
		t.Fatalf("the document was deleted with its conversation: %v", err)
	}
	_ = router
}

// Once Docs are copied, a document shows once wherever documents are listed,
// and the report says how the copy stands.
func TestCopiedDocsAreListedOnceAndReported(t *testing.T) {
	api, _, router := documentTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "copied-docs")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Ship it", Status: "awaiting_approval", Priority: "medium"}
	if err := api.db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-copied", ProjectID: project.ID, Status: "stopped", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	conversation := &database.AIConversation{Title: "Old"}
	if err := api.db.CreateAIConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	taskID := sql.NullInt64{Int64: task.ID, Valid: true}
	for _, doc := range []database.TempDocument{
		{ID: "olddoc01", Title: "Old notes", Content: "old notes", SessionID: "sess-copied", TaskID: taskID},
		{ID: "oldver01", Title: "Verification: Ship it", Content: "# Verification", TaskID: taskID, Status: "approved"},
		{ID: "oldchat1", Title: "Chat answer", Content: "answer", ConversationID: sql.NullInt64{Int64: conversation.ID, Valid: true}, MessageID: 3},
	} {
		doc := doc
		if err := api.db.CreateTempDocument(ctx, &doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.db.SetTaskVerificationDoc(ctx, task.ID, "oldver01"); err != nil {
		t.Fatal(err)
	}

	report := func() map[string]any {
		t.Helper()
		recorder, body := artifactRequest(t, router, http.MethodGet, "/api/artifacts/legacy-docs/report", nil, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("report = %d %s", recorder.Code, recorder.Body.String())
		}
		return body
	}
	if before := report(); before["docs"] != float64(3) || before["mapped"] != float64(0) || before["pending"] != float64(3) {
		t.Fatalf("report before the copy = %v", before)
	}
	listed := func(path string) []map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		var documents []map[string]any
		decodeJSONBody(t, response, &documents)
		return documents
	}
	taskPath := "/api/projects/" + itoa(project.ID) + "/tasks/" + itoa(task.ID) + "/documents"
	if documents := listed(taskPath); len(documents) != 2 {
		t.Fatalf("documents of the task before the copy = %v", documents)
	}

	if _, err := api.db.MigrateLegacyDocsToArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	after := report()
	if after["mapped"] != float64(3) || after["pending"] != float64(0) || after["mismatch_count"] != float64(0) ||
		after["verification_docs"] != float64(1) || after["verification_docs_resolved"] != float64(1) {
		t.Fatalf("report after the copy = %v", after)
	}
	if strings.Contains(strings.ToLower(httpBody(t, router, "/api/artifacts/legacy-docs/report")), "old notes") {
		t.Fatal("the report carries a title")
	}

	// Each document once: the Doc as its artifact, the verification document under its bare id.
	byID := map[string]map[string]any{}
	documents := listed(taskPath)
	for _, document := range documents {
		byID[document["id"].(string)] = document
	}
	if len(documents) != 2 || byID["artifact:olddoc01"]["type"] != "artifact" || byID["oldver01"]["type"] != "document" || byID["oldver01"]["status"] != "approved" {
		t.Fatalf("documents of the task after the copy = %v", documents)
	}
	if documents := listed("/api/sessions/sess-copied/documents"); len(documents) != 1 || documents[0]["id"] != "artifact:olddoc01" {
		t.Fatalf("documents of the session after the copy = %v", documents)
	}
	if cards := api.conversationDocCards(ctx, conversation.ID); len(cards) != 1 || cards[0].ID != "oldchat1" || cards[0].Type != "document" {
		t.Fatalf("cards of the conversation after the copy = %+v", cards)
	}

	// The old ids open the artifacts, and the Docs are still there.
	for _, id := range []string{"olddoc01", "oldver01", "oldchat1"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app/doc/"+id, nil))
		if response.Code != http.StatusFound || response.Header().Get("Location") != "/app/artifact/"+id {
			t.Fatalf("/app/doc/%s = %d -> %q", id, response.Code, response.Header().Get("Location"))
		}
	}
	if countDocs(t, api) != 3 {
		t.Fatalf("temp_documents has %d rows after the copy", countDocs(t, api))
	}
}

func httpBody(t *testing.T, router http.Handler, path string) string {
	t.Helper()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response.Body.String()
}
