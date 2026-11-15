package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

var docLinkInToolResult = regexp.MustCompile(`/app/doc/([a-zA-Z0-9-]+)`)

// reviewTestRouter mounts the routes a proposal goes through, the way
// cmd/openpoet does.
func reviewTestRouter(t *testing.T) (*API, PlatformServices, http.Handler) {
	t.Helper()
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {
		r.Get("/artifacts", api.ListArtifacts)
		r.Get("/artifacts/{id}", api.GetArtifact)
		r.Get("/artifacts/{id}/versions/{version}", api.GetArtifactVersion)
		r.Get("/documents/{id}", api.GetTempDocument)
		r.Get("/links", api.GetLink)
		r.Post("/memory-doc/propose", api.ProposeMemoryDoc)
		r.Post("/memory-doc/approve/{docId}", api.ApproveMemoryDoc)
		r.Post("/memory-doc/reject/{docId}", api.RejectMemoryDoc)
		r.Post("/task-proposal/approve/{docId}", api.ApproveTaskProposal)
		r.Get("/projects/{id}/tasks/{taskId}/documents", api.ListTaskDocuments)
		r.Get("/ai/conversations/{id}", services.AIHandler.HandleGetConversation)
	})
	app := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("the app")) }
	router.Get("/app/doc/{id}", api.RedirectDocumentLink(app))
	return api, services, router
}

func reviewTestProject(t *testing.T, api *API, name string) *database.Project {
	t.Helper()
	project := &database.Project{Name: name, Path: "/tmp/" + name, Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := api.db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	return project
}

// A memory proposal made by a session (the MCP tool) is an artifact that waits
// for the user, and approving it by the id the card carries applies it once.
func TestMemoryProposalIsAnArtifactTheUserDecides(t *testing.T) {
	api, _, router := reviewTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "memory-proposal")

	recorder, proposed := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/propose",
		map[string]any{"project_id": project.ID, "content": "# Memory\n\nUse **tabs**.", "summary": "Indentation rule"}, nil)
	if recorder.Code != http.StatusOK || proposed["status"] != "pending_approval" {
		t.Fatalf("propose = %d %v", recorder.Code, proposed)
	}
	id, _ := proposed["doc_id"].(string)

	// It is an artifact: markdown, linked to the project, with a pending review.
	stored, err := api.db.GetArtifact(ctx, id)
	if err != nil || stored.ProjectID.Int64 != project.ID || stored.Summary != "Indentation rule" {
		t.Fatalf("stored artifact = %+v err=%v", stored, err)
	}
	if version, err := api.db.GetArtifactVersion(ctx, id, 1); err != nil || version.Format != database.ArtifactFormatMarkdown || version.Content != "# Memory\n\nUse **tabs**." {
		t.Fatalf("stored version = %+v err=%v", version, err)
	}
	if recorder, detail := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id, nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("detail = %d", recorder.Code)
	} else if review, _ := detail["review"].(map[string]any); review["kind"] != "memory" || review["status"] != "pending" {
		t.Fatalf("review in the detail = %v", detail["review"])
	}

	// Read by the id a card carries, it says which artifact it is.
	recorder, document := artifactRequest(t, router, http.MethodGet, "/api/documents/"+id, nil, nil)
	review, _ := document["review"].(map[string]any)
	if recorder.Code != http.StatusOK || document["artifact_id"] != id || document["status"] != "pending" || document["format"] != "markdown" ||
		document["content"] != "# Memory\n\nUse **tabs**." || review["kind"] != "memory" {
		t.Fatalf("document = %d %v", recorder.Code, document)
	}

	// A listing of documents leaves it out; it is listed when asked for.
	list := func(query string) []any {
		request := httptest.NewRequest(http.MethodGet, "/api/artifacts?"+query, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		var items []any
		decodeJSONBody(t, response, &items)
		return items
	}
	if items := list("project_id=" + itoa(project.ID)); len(items) != 0 {
		t.Fatalf("a proposal is listed among the documents of its project: %v", items)
	}
	if items := list("include_reviewed=1&project_id=" + itoa(project.ID)); len(items) != 1 {
		t.Fatalf("listing with reviewed artifacts = %v", items)
	}

	// Approving applies the change and decides the review; it cannot be decided again.
	if recorder, approved := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/approve/"+id, nil, nil); recorder.Code != http.StatusOK || approved["status"] != "approved" {
		t.Fatalf("approve = %d %v", recorder.Code, approved)
	}
	if memory, err := api.db.GetMemoryDoc(ctx, project.ID); err != nil || memory.Content != "# Memory\n\nUse **tabs**." {
		t.Fatalf("memory document after approval = %+v err=%v", memory, err)
	}
	if stored, err := api.db.GetArtifactReview(ctx, id); err != nil || stored.Status != database.ArtifactReviewApproved {
		t.Fatalf("review after approval = %+v err=%v", stored, err)
	}
	for _, route := range []string{"/api/memory-doc/approve/", "/api/memory-doc/reject/"} {
		if recorder, _ := artifactRequest(t, router, http.MethodPost, route+id, nil, nil); recorder.Code == http.StatusOK {
			t.Fatalf("%s decided a proposal that was already decided", route)
		}
	}
	// A proposal is decided on its own route.
	if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/task-proposal/approve/"+id, nil, nil); recorder.Code == http.StatusOK {
		t.Fatal("a memory proposal was approved as a task proposal")
	}
}

// A task proposal of the chat is an artifact too: approving it by its id
// creates the tasks and decides the review, in one step.
func TestTaskProposalArtifactIsApprovedOnItsOwnRoute(t *testing.T) {
	api, _, router := reviewTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "task-proposal")
	conversation := &database.AIConversation{Title: "Planning"}
	if err := api.db.CreateAIConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	id, err := api.createReviewArtifact(ctx, reviewArtifact{
		Kind: database.ArtifactReviewTask, Title: "Task: Write the changelog", Summary: "1 new task — task-proposal",
		Markdown: "# Task Proposal — task-proposal\n\n### 1. Write the changelog\n", ProjectID: project.ID, ConversationID: conversation.ID, MessageID: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := []PlanningTaskAction{{Action: "create", ProjectID: project.ID, Title: "Write the changelog", Priority: "high"}}
	api.storePendingTaskProposal(id, actions, "1 new task")

	// The wrong route does not decide it.
	if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/approve/"+id, nil, nil); recorder.Code == http.StatusOK {
		t.Fatal("a task proposal was approved as a memory proposal")
	}
	recorder, approved := artifactRequest(t, router, http.MethodPost, "/api/task-proposal/approve/"+id, nil, nil)
	if recorder.Code != http.StatusOK || approved["status"] != "approved" || approved["created"] != float64(1) {
		t.Fatalf("approve = %d %v", recorder.Code, approved)
	}
	tasks, err := api.db.ListTasksByProject(ctx, project.ID)
	if err != nil || len(tasks) != 1 || tasks[0].Title != "Write the changelog" || tasks[0].Priority != "high" {
		t.Fatalf("tasks after approval = %+v err=%v", tasks, err)
	}
	if review, err := api.db.GetArtifactReview(ctx, id); err != nil || review.Status != database.ArtifactReviewApproved {
		t.Fatalf("review after approval = %+v err=%v", review, err)
	}
	// Approved once: a second approval creates nothing.
	if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/task-proposal/approve/"+id, nil, nil); recorder.Code == http.StatusOK {
		t.Fatal("a task proposal was approved twice")
	}
	if tasks, _ := api.db.ListTasksByProject(ctx, project.ID); len(tasks) != 1 {
		t.Fatalf("%d tasks after a second approval", len(tasks))
	}
	// The card of the conversation shows the decision.
	cards := api.conversationDocCards(ctx, conversation.ID)
	if len(cards) != 1 || cards[0].ID != id || cards[0].Type != "task_proposal" || cards[0].Status != "approved" || cards[0].MessageID != 4 {
		t.Fatalf("cards = %+v", cards)
	}
}

func TestRejectedMemoryProposalChangesNothing(t *testing.T) {
	api, _, router := reviewTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "memory-rejected")
	_, proposed := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/propose",
		map[string]any{"project_id": project.ID, "content": "# Memory\n\nNew rule.\n", "summary": "A rule"}, nil)
	id, _ := proposed["doc_id"].(string)
	if recorder, rejected := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/reject/"+id, nil, nil); recorder.Code != http.StatusOK || rejected["status"] != "rejected" {
		t.Fatalf("reject = %d %v", recorder.Code, rejected)
	}
	if review, err := api.db.GetArtifactReview(ctx, id); err != nil || review.Status != database.ArtifactReviewRejected {
		t.Fatalf("review after rejection = %+v err=%v", review, err)
	}
	if memory, err := api.db.GetMemoryDoc(ctx, project.ID); err == nil && memory != nil && strings.Contains(memory.Content, "New rule") {
		t.Fatal("a rejected proposal was applied")
	}
	if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/approve/"+id, nil, nil); recorder.Code == http.StatusOK {
		t.Fatal("a rejected proposal was approved")
	}
}

// The chat assistant's proposals and file cards are the cards of their
// conversation, each under the message that made it, and go with it.
func TestChatProposalsAndFileCardsAreTheCardsOfTheirConversation(t *testing.T) {
	api, services, router := reviewTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "chat-cards")
	conversation := &database.AIConversation{Title: "Cards"}
	if err := api.db.CreateAIConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "ui"}, Approved: true, ApprovedBy: "ui", Reason: "test"}
	tool := func(name string, input map[string]interface{}, messageID int64) string {
		t.Helper()
		result, err := services.AIHandler.executeTool(ctx, name, input, conversation.ID, messageID, nil, "", auth)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}

	// update_memory_doc publishes a proposal and names it by a document link.
	proposed := tool("update_memory_doc", map[string]interface{}{"project_id": float64(project.ID), "content": "# Memory\n\nKeep it short.\n", "summary": "Brevity"}, 11)
	match := docLinkInToolResult.FindStringSubmatch(proposed)
	if match == nil || !strings.Contains(proposed, "AWAITS user approval") {
		t.Fatalf("update_memory_doc result = %s", proposed)
	}
	proposalID := match[1]
	if review, err := api.db.GetArtifactReview(ctx, proposalID); err != nil || review == nil || review.Kind != database.ArtifactReviewMemory || review.MessageID != 11 {
		t.Fatalf("review of the chat proposal = %+v err=%v", review, err)
	}
	// The card the chat builds from that result carries the same id.
	if card := services.AIHandler.buildDocCard("update_memory_doc", proposed, map[string]interface{}{"summary": "Brevity"}); card["doc_id"] != proposalID || card["type"] != "proposal" {
		t.Fatalf("card = %v", card)
	}

	// get_memory_doc copies nothing: its card opens the memory document itself.
	if _, err := api.db.UpsertMemoryDoc(ctx, project.ID, "# Memory\n\nCurrent.\n", "user:ui", ""); err != nil {
		t.Fatal(err)
	}
	artifactsBefore := countArtifacts(t, api)
	viewed := tool("get_memory_doc", map[string]interface{}{"project_id": float64(project.ID)}, 12)
	viewID := application.MemoryDocumentViewID(project.ID)
	if card := services.AIHandler.buildDocCard("get_memory_doc", viewed, nil); card["doc_id"] != viewID || card["type"] != "view" {
		t.Fatalf("view card = %v (result %s)", card, viewed)
	}
	if countArtifacts(t, api) != artifactsBefore {
		t.Fatal("reading the memory document stored a copy of it")
	}
	if recorder, document := artifactRequest(t, router, http.MethodGet, "/api/documents/"+viewID, nil, nil); recorder.Code != http.StatusOK ||
		document["content"] != "# Memory\n\nCurrent." || document["artifact_id"] != nil || !strings.HasPrefix(document["title"].(string), "Memory document: chat-cards") {
		t.Fatalf("memory view = %d %v", recorder.Code, document)
	}

	// open_file records file cards, which are not documents.
	tool("open_file", map[string]interface{}{"project_id": float64(project.ID), "paths": []interface{}{"docs/plan.md"}}, 12)

	// A Doc the conversation still has from before is listed with them.
	if err := api.db.CreateTempDocument(ctx, &database.TempDocument{ID: "olddoc01", Title: "Old notes", Content: "x",
		ConversationID: sql.NullInt64{Int64: conversation.ID, Valid: true}, MessageID: 5, Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	recorder, loaded := artifactRequest(t, router, http.MethodGet, "/api/ai/conversations/"+itoa(conversation.ID), nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("conversation = %d %s", recorder.Code, recorder.Body.String())
	}
	cards := map[string]map[string]any{}
	for _, raw := range loaded["doc_cards"].([]any) {
		card := raw.(map[string]any)
		cards[card["type"].(string)] = card
	}
	if len(cards) != 3 {
		t.Fatalf("cards of the conversation = %v", loaded["doc_cards"])
	}
	if card := cards["proposal"]; card["id"] != proposalID || card["status"] != "pending" || card["message_id"] != float64(11) || card["summary"] != "Brevity" {
		t.Fatalf("proposal card = %v", card)
	}
	if card := cards["file"]; card["title"] != "plan.md" || card["path"] != "docs/plan.md" || card["project_id"] != itoa(project.ID) || card["message_id"] != float64(12) {
		t.Fatalf("file card = %v", card)
	}
	if card := cards["document"]; card["id"] != "olddoc01" || card["message_id"] != float64(5) {
		t.Fatalf("legacy Doc card = %v", card)
	}

	// Deleting the conversation takes its proposals and file cards.
	if err := api.db.DeleteAIConversation(ctx, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := api.db.GetArtifact(ctx, proposalID); err == nil {
		t.Fatal("the proposal outlived its conversation")
	}
	if cards := api.conversationDocCards(ctx, conversation.ID); len(cards) != 0 {
		t.Fatalf("cards left after the conversation was deleted: %+v", cards)
	}
}

// The verification document of a task is an artifact named by the bare id the
// task holds; the task's documents list it the way they listed the Doc.
func TestVerificationDocumentIsAnArtifactNamedByTheTask(t *testing.T) {
	api, services, router := reviewTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "verification")
	task := &database.ProjectTask{ProjectID: project.ID, Title: strings.Repeat("A long task title ", 20), Status: "awaiting_approval", Priority: "medium"}
	if err := api.db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	services.AIHandler.persistVerificationDoc(ctx, task, "# Verification\n\nAll checks passed.\n")

	stored, err := api.db.GetTask(ctx, task.ID)
	if err != nil || stored.VerificationDocID == "" {
		t.Fatalf("task after verification = %+v err=%v", stored, err)
	}
	id := stored.VerificationDocID
	artifact, err := api.db.GetArtifact(ctx, id)
	if err != nil || artifact.TaskID.Int64 != task.ID || artifact.ProjectID.Int64 != project.ID || !strings.HasPrefix(artifact.Title, "Verification: A long task title") || len([]rune(artifact.Title)) > 200 {
		t.Fatalf("verification artifact = %+v err=%v", artifact, err)
	}
	if review, err := api.db.GetArtifactReview(ctx, id); err != nil || review == nil || review.Kind != database.ArtifactReviewVerification || review.Status != database.ArtifactReviewPending {
		t.Fatalf("review = %+v err=%v", review, err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/projects/"+itoa(project.ID)+"/tasks/"+itoa(task.ID)+"/documents", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var documents []map[string]any
	decodeJSONBody(t, response, &documents)
	if len(documents) != 1 || documents[0]["id"] != id || documents[0]["type"] != "document" || documents[0]["status"] != "pending" {
		t.Fatalf("documents of the task = %v", documents)
	}

	// The id opens the artifact, by the API and by the link.
	if recorder, document := artifactRequest(t, router, http.MethodGet, "/api/documents/"+id, nil, nil); recorder.Code != http.StatusOK || document["artifact_id"] != id || document["content"] != "# Verification\n\nAll checks passed.\n" {
		t.Fatalf("document = %d %v", recorder.Code, document)
	}
	if recorder, link := artifactRequest(t, router, http.MethodGet, "/api/links?kind=document&id="+id, nil, nil); recorder.Code != http.StatusOK || link["path"] != "/app/artifact/"+id {
		t.Fatalf("link of the document = %d %v", recorder.Code, link)
	}
	// The chat assistant reads it with read_document, and is told the artifact.
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "ui"}, Approved: true, ApprovedBy: "ui", Reason: "test"}
	read, err := services.AIHandler.executeTool(ctx, "read_document", map[string]interface{}{"document_id": id}, 0, 0, nil, "", auth)
	if err != nil || !strings.Contains(read, "All checks passed.") || !strings.Contains(read, "| Artifact: "+id) || !strings.Contains(read, "Status: pending") {
		t.Fatalf("read_document = %q err=%v", read, err)
	}
}

func TestDocumentLinkRedirectsToTheArtifactItStandsFor(t *testing.T) {
	api, _, router := reviewTestRouter(t)
	ctx := context.Background()
	project := reviewTestProject(t, api, "redirect")
	_, proposed := artifactRequest(t, router, http.MethodPost, "/api/memory-doc/propose",
		map[string]any{"project_id": project.ID, "content": "# Memory\n", "summary": "x"}, nil)
	artifactID, _ := proposed["doc_id"].(string)
	if err := api.db.CreateTempDocument(ctx, &database.TempDocument{ID: "plaindoc", Title: "Only a Doc", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateTempDocument(ctx, &database.TempDocument{ID: "copied01", Title: "Copied", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.db.Exec(`INSERT INTO artifact_legacy_docs (doc_id, artifact_id) VALUES ('copied01', ?)`, artifactID); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	for name, path := range map[string]string{"an artifact named by its id": "/app/doc/" + artifactID, "a Doc copied into an artifact": "/app/doc/copied01"} {
		response := get(path)
		if response.Code != http.StatusFound || response.Header().Get("Location") != "/app/artifact/"+artifactID {
			t.Fatalf("%s: %d -> %q", name, response.Code, response.Header().Get("Location"))
		}
	}
	// A Doc, a session plan, a memory document and an unknown id are the app's to open.
	for _, path := range []string{"/app/doc/plaindoc", "/app/doc/plan:sess-1", "/app/doc/memory-3", "/app/doc/missing0"} {
		if response := get(path); response.Code != http.StatusOK || response.Body.String() != "the app" {
			t.Fatalf("%s: %d %q", path, response.Code, response.Body.String())
		}
	}
	// The document API still reads a Doc as a Doc.
	if recorder, document := artifactRequest(t, router, http.MethodGet, "/api/documents/plaindoc", nil, nil); recorder.Code != http.StatusOK || document["artifact_id"] != nil || document["title"] != "Only a Doc" {
		t.Fatalf("plain Doc = %d %v", recorder.Code, document)
	}
	// And a copied Doc reads as its artifact, under the id that was asked for.
	if recorder, document := artifactRequest(t, router, http.MethodGet, "/api/documents/copied01", nil, nil); recorder.Code != http.StatusOK || document["id"] != "copied01" || document["artifact_id"] != artifactID {
		t.Fatalf("copied Doc = %d %v", recorder.Code, document)
	}
}

func countArtifacts(t *testing.T, api *API) int {
	t.Helper()
	var count int
	if err := api.db.Get(&count, `SELECT COUNT(*) FROM artifacts`); err != nil {
		t.Fatal(err)
	}
	return count
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }

func decodeJSONBody(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("response is not the expected JSON (%d): %s", response.Code, response.Body.String())
	}
}
