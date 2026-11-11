package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/mcp"
)

var artifactIDInToolResult = regexp.MustCompile(`artifact_id ([a-f0-9]{8})`)

// TestArtifactReviewLoopThroughSessionTools is the whole feature from the
// agent's side, over the real routes: a session publishes an artifact with its
// MCP tool, the user annotates it, the session reads the notes, publishes a new
// version and resolves them.
func TestArtifactReviewLoopThroughSessionTools(t *testing.T) {
	api, router := artifactTestRouter(t)
	ctx := context.Background()
	project := &database.Project{Name: "artifact-loop", Path: "/tmp/artifact-loop", Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := api.db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Write the rollout plan", Status: "todo", Priority: "medium"}
	if err := api.db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-loop", ProjectID: project.ID, Status: "running", Name: "writer", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := api.db.LinkSessionToTask(ctx, "sess-loop", task.ID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client := mcp.NewAPIClient(server.URL)
	tool := func(name string, args map[string]any) string {
		t.Helper()
		encoded, _ := json.Marshal(args)
		result, err := mcp.ExecuteTool(client, name, encoded, "sess-loop", "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}

	// 1. The session publishes the plan.
	v1 := `<!doctype html><html><body><h1>Rollout plan</h1><p id="p">Each stage has a rollback point.</p><button id="go">Launch</button></body></html>`
	published := tool("openpoet_create_artifact", map[string]any{"title": "Rollout plan", "html": v1, "summary": "Stages and owners"})
	match := artifactIDInToolResult.FindStringSubmatch(published)
	if match == nil {
		t.Fatalf("create result names no artifact id: %s", published)
	}
	id := match[1]
	stored, err := api.db.GetArtifact(ctx, id)
	if err != nil || stored.SessionID != "sess-loop" || stored.ProjectID.Int64 != project.ID || stored.TaskID.Int64 != task.ID || stored.CurrentVersion != 1 {
		t.Fatalf("stored artifact = %+v err=%v", stored, err)
	}
	// Nothing is written into the project directory: the HTML lives in the database.
	if version, err := api.db.GetArtifactVersion(ctx, id, 1); err != nil || version.Content != v1 {
		t.Fatalf("stored version = %+v err=%v", version, err)
	}

	// 2. The user annotates a sentence and the button (what the viewer posts).
	for _, annotation := range []map[string]any{
		{"version": 1, "body": "Say who approves the rollback.", "anchor": map[string]any{"type": "text", "selector": "#p", "exact": "rollback point"}},
		{"version": 1, "body": "Call it Start rollout.", "anchor": map[string]any{"type": "element", "selector": "#go", "tag": "button", "label": "Launch"}},
	} {
		if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/annotations", annotation, nil); recorder.Code != http.StatusCreated {
			t.Fatalf("annotation create status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	}

	// 3. The session finds the artifact and reads the notes.
	if listed := tool("openpoet_list_artifacts", map[string]any{}); !strings.Contains(listed, id) || !strings.Contains(listed, "2 open annotation(s)") {
		t.Fatalf("list result = %s", listed)
	}
	notes := tool("openpoet_list_artifact_annotations", map[string]any{"artifact_id": id})
	for _, phrase := range []string{`on text "rollback point"`, "Note: Say who approves the rollback.", `on <button> element "Launch"`, "Note: Call it Start rollout."} {
		if !strings.Contains(notes, phrase) {
			t.Fatalf("annotation list lacks %q:\n%s", phrase, notes)
		}
	}
	annotations, err := api.db.ListArtifactAnnotations(ctx, id, "open")
	if err != nil || len(annotations) != 2 {
		t.Fatalf("open annotations = %+v err=%v", annotations, err)
	}
	// The task's document list shows the artifact to an agent reading the task.
	if docs := tool("openpoet_read_document", map[string]any{"document_id": "artifact:" + id}); !strings.Contains(docs, "### Open annotations (2)") {
		t.Fatalf("read_document on the artifact = %s", docs)
	}

	// 4. It reads the HTML, publishes the corrected version, and resolves the notes.
	if html := tool("openpoet_read_artifact", map[string]any{"artifact_id": id, "include_html": true}); !strings.Contains(html, v1) {
		t.Fatalf("read with html lacks the stored document:\n%s", html)
	}
	v2 := strings.NewReplacer("rollback point", "rollback point approved by the on-call lead", "Launch", "Start rollout").Replace(v1)
	if updated := tool("openpoet_update_artifact", map[string]any{"artifact_id": id, "html": v2, "base_version": "1", "change_note": "Named the approver; renamed the button"}); !strings.Contains(updated, "now at version 2") || !strings.Contains(updated, "2 annotation(s) are still open") {
		t.Fatalf("update result = %s", updated)
	}
	for _, annotation := range annotations {
		tool("openpoet_resolve_artifact_annotation", map[string]any{"artifact_id": id, "annotation_id": annotation.ID, "resolution_note": "Fixed in version 2."})
	}

	// 5. The outcome the user sees.
	if open, err := api.db.CountOpenArtifactAnnotations(ctx, id); err != nil || open != 0 {
		t.Fatalf("open annotations after the loop = %d err=%v", open, err)
	}
	resolved, err := api.db.ListArtifactAnnotations(ctx, id, "resolved")
	if err != nil || len(resolved) != 2 || resolved[0].ResolutionNote != "Fixed in version 2." || resolved[0].Version != 1 {
		t.Fatalf("resolved annotations = %+v err=%v", resolved, err)
	}
	after, err := api.db.GetArtifact(ctx, id)
	if err != nil || after.CurrentVersion != 2 {
		t.Fatalf("artifact after the loop = %+v err=%v", after, err)
	}
	if first, err := api.db.GetArtifactVersion(ctx, id, 1); err != nil || first.Content != v1 {
		t.Fatalf("version 1 was not kept: %+v err=%v", first, err)
	}
	if none := tool("openpoet_list_artifact_annotations", map[string]any{"artifact_id": id}); none != "No open annotations on artifact "+id+"." {
		t.Fatalf("annotation list after resolving = %s", none)
	}

	// A second writer working from version 1 is told to re-read, not overwritten.
	encoded, _ := json.Marshal(map[string]any{"artifact_id": id, "html": v1 + "<!-- stale -->", "base_version": "1"})
	if _, err := mcp.ExecuteTool(client, "openpoet_update_artifact", encoded, "sess-other", ""); err == nil || !strings.Contains(err.Error(), "is at version 2") {
		t.Fatalf("stale update err = %v", err)
	}
	// Markdown is refused with a message that says where it belongs.
	encoded, _ = json.Marshal(map[string]any{"title": "Notes", "html": "# heading\n\n- bullet"})
	if _, err := mcp.ExecuteTool(client, "openpoet_create_artifact", encoded, "sess-loop", ""); err == nil || !strings.Contains(err.Error(), "must be an HTML document") {
		t.Fatalf("markdown publish err = %v", err)
	}
}

// The chat assistant's artifact tools run in-process against the same service
// and return the same text as the session tools.
func TestArtifactChatToolsShareTheServiceAndTheWording(t *testing.T) {
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	handler := services.AIHandler
	ctx := context.Background()
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "ui"}, Approved: true, ApprovedBy: "ui", Reason: "test"}
	call := func(name string, input map[string]interface{}) (string, error) {
		return handler.executeTool(ctx, name, input, 0, 0, nil, "", auth)
	}

	published, err := call("create_artifact", map[string]interface{}{"title": "Chat plan", "html": "<html><body><h1>Plan</h1></body></html>"})
	if err != nil {
		t.Fatal(err)
	}
	match := artifactIDInToolResult.FindStringSubmatch(published)
	if match == nil || !strings.Contains(published, "call update_artifact with artifact_id") || strings.Contains(published, "openpoet_") {
		t.Fatalf("chat create result = %s", published)
	}
	id := match[1]

	annotation, err := api.platformServices.Collaboration.Artifacts.CreateAnnotation(ctx, application.CreateArtifactAnnotationCommand{
		ArtifactID: id, Anchor: application.ArtifactAnchor{Type: "text", Selector: "h1", Exact: "Plan"}, Body: "More specific title", Authorization: auth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if notes, err := call("list_artifact_annotations", map[string]interface{}{"artifact_id": "artifact:" + id}); err != nil || !strings.Contains(notes, `on text "Plan"`) || !strings.Contains(notes, "resolve_artifact_annotation") || strings.Contains(notes, "openpoet_") {
		t.Fatalf("chat annotation list = %q err=%v", notes, err)
	}
	if detail, err := call("read_artifact", map[string]interface{}{"artifact_id": id, "include_html": true}); err != nil || !strings.Contains(detail, "### Open annotations (1)") || !strings.Contains(detail, "<h1>Plan</h1>") {
		t.Fatalf("chat read = %q err=%v", detail, err)
	}
	if updated, err := call("update_artifact", map[string]interface{}{"artifact_id": id, "html": "<html><body><h1>Q3 rollout plan</h1></body></html>", "base_version": float64(1)}); err != nil || !strings.Contains(updated, "now at version 2") {
		t.Fatalf("chat update = %q err=%v", updated, err)
	}
	if _, err := call("update_artifact", map[string]interface{}{"artifact_id": id, "html": "<html><body>late</body></html>", "base_version": "1"}); !application.ErrorIsKind(err, application.ErrorConflict) {
		t.Fatalf("chat stale update err = %v", err)
	}
	if resolved, err := call("resolve_artifact_annotation", map[string]interface{}{"artifact_id": id, "annotation_id": float64(annotation.ID), "resolution_note": "Retitled"}); err != nil || !strings.Contains(resolved, "0 annotation(s) remain open") {
		t.Fatalf("chat resolve = %q err=%v", resolved, err)
	}
	if _, err := call("resolve_artifact_annotation", map[string]interface{}{"artifact_id": id, "annotation_id": float64(annotation.ID)}); err == nil {
		t.Fatal("chat resolve without a note was accepted")
	}
	if listed, err := call("list_artifacts", map[string]interface{}{}); err != nil || !strings.Contains(listed, id) {
		t.Fatalf("chat list = %q err=%v", listed, err)
	}
	if _, err := call("create_artifact", map[string]interface{}{"title": "x", "html": "plain text"}); !application.ErrorIsKind(err, application.ErrorValidation) {
		t.Fatalf("chat non-HTML err = %v", err)
	}
}
