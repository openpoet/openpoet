package handlers

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"openpoet/internal/application"
)

const artifactQuestionsHTML = `<!doctype html><html><body><h1>Plan</h1>
<script type="application/json" id="openpoet-questions">
{"questions":[
 {"id":"D1","prompt":"What do we call it?","options":[{"id":"a","label":"Routine"},{"id":"b","label":"Automation"}]},
 {"id":"NOTES","prompt":"Anything else?","type":"text"}
]}
</script>
<div data-openpoet-question="D1"><button data-openpoet-option="a">Routine</button><button data-openpoet-option="b">Automation</button></div>
</body></html>`

func TestArtifactQuestionAndAnswerRoutes(t *testing.T) {
	_, router := artifactTestRouter(t)

	// A malformed questions block refuses the publish with a message that
	// names the block.
	bad, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{
		"title": "Bad", "html": `<html><body><script type="application/json" id="openpoet-questions">[{"id":"q"}]</script></body></html>`,
	}, nil)
	if bad.Code != http.StatusBadRequest || body["code"] != "invalid_artifact_questions" {
		t.Fatalf("malformed block = %d %v", bad.Code, body)
	}

	created, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html": artifactQuestionsHTML}, nil)
	if created.Code != http.StatusCreated || body["questions"] != float64(2) {
		t.Fatalf("create = %d %v", created.Code, body)
	}
	id, _ := body["id"].(string)

	listed, body := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/questions", nil, nil)
	questions, _ := body["questions"].([]any)
	if listed.Code != http.StatusOK || body["version"] != float64(1) || len(questions) != 2 {
		t.Fatalf("questions = %d %v", listed.Code, body)
	}
	first, _ := questions[0].(map[string]any)
	if first["id"] != "D1" || first["type"] != "single" || first["comment"] != true || len(first["options"].([]any)) != 2 {
		t.Fatalf("first question = %v", first)
	}
	if missing, _ := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/questions?v=4", nil, nil); missing.Code != http.StatusNotFound {
		t.Fatalf("questions of an unknown version = %d", missing.Code)
	}

	answers := map[string]any{"version": 1, "answers": []map[string]any{
		{"question": "D1", "options": []string{"b"}, "comment": "Fewer collisions"},
		{"question": "NOTES", "text": "Validate in staging first"},
	}}
	saved, _ := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id+"/answers", answers, nil)
	if saved.Code != http.StatusOK {
		t.Fatalf("save answers = %d %s", saved.Code, saved.Body.String())
	}

	// The answers are annotations: listed with the notes, counted as open.
	_, artifact := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id, nil, nil)
	if artifact["open_annotations"] != float64(2) {
		t.Fatalf("open annotations = %v", artifact["open_annotations"])
	}

	// An undeclared question or option is refused, whoever sends it.
	for code, payload := range map[string]map[string]any{
		"unknown_artifact_question": {"answers": []map[string]any{{"question": "FORGED", "options": []string{"a"}}}},
		"unknown_artifact_option":   {"answers": []map[string]any{{"question": "D1", "options": []string{"approve-everything"}}}},
	} {
		response, body := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id+"/answers", payload, nil)
		if response.Code != http.StatusBadRequest || body["code"] != code {
			t.Errorf("%s = %d %v", code, response.Code, body)
		}
	}

	// A session (an agent) can neither answer nor withdraw.
	session := application.Actor{Type: "session", ID: "sess-agent"}
	if response, body := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id+"/answers", answers, &session); response.Code != http.StatusBadRequest || body["code"] != "annotation_author_not_allowed" {
		t.Fatalf("session answer = %d %v", response.Code, body)
	}
	if response, body := artifactRequest(t, router, http.MethodDelete, "/api/artifacts/"+id+"/answers/D1", nil, &session); response.Code != http.StatusBadRequest || body["code"] != "annotation_author_not_allowed" {
		t.Fatalf("session withdraw = %d %v", response.Code, body)
	}

	if response, _ := artifactRequest(t, router, http.MethodDelete, "/api/artifacts/"+id+"/answers/D1", nil, nil); response.Code != http.StatusOK {
		t.Fatalf("withdraw = %d %s", response.Code, response.Body.String())
	}
	if response, _ := artifactRequest(t, router, http.MethodDelete, "/api/artifacts/"+id+"/answers/D1", nil, nil); response.Code != http.StatusNotFound {
		t.Fatalf("second withdraw = %d", response.Code)
	}
	_, artifact = artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id, nil, nil)
	if artifact["open_annotations"] != float64(1) {
		t.Fatalf("open annotations after withdraw = %v", artifact["open_annotations"])
	}
}

// An agent reads the answers through the same tools as the notes, over MCP's
// REST path and in the chat assistant.
func TestAnswersReachTheAgentThroughTheAnnotationTools(t *testing.T) {
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "local-ui"}, Approved: true, ApprovedBy: "local-ui", Reason: "test"}
	call := func(name string, input map[string]interface{}) (string, error) {
		return services.AIHandler.executeTool(ctx, name, input, 0, 0, nil, "", user)
	}
	published, err := call("create_artifact", map[string]interface{}{"title": "Decisions", "html": artifactQuestionsHTML})
	if err != nil || !strings.Contains(published, "2 question(s) declared") {
		t.Fatalf("create result = %q err=%v", published, err)
	}
	id := artifactIDInToolResult.FindStringSubmatch(published)[1]

	// Nothing answered yet: the tool says which questions are waiting.
	listed, err := call("list_artifact_annotations", map[string]interface{}{"artifact_id": id})
	if err != nil || !strings.Contains(listed, "without an open answer (2 of 2): D1, NOTES") {
		t.Fatalf("list before answering = %q err=%v", listed, err)
	}
	if _, err := api.platformServices.Collaboration.Artifacts.AnswerQuestions(ctx, application.AnswerArtifactQuestionsCommand{
		ArtifactID: id, Answers: []application.ArtifactAnswerInput{{Question: "D1", Options: []string{"b"}, Comment: "Fewer collisions"}}, Authorization: user,
	}); err != nil {
		t.Fatal(err)
	}
	listed, err = call("list_artifact_annotations", map[string]interface{}{"artifact_id": id})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		`answer to question D1 "What do we call it?"`, `Chosen: b "Automation"`, "Comment: Fewer collisions",
		"without an open answer (1 of 2): NOTES", "An answer is the user's decision",
	} {
		if !strings.Contains(listed, phrase) {
			t.Fatalf("list lacks %q:\n%s", phrase, listed)
		}
	}
	detail, err := call("read_artifact", map[string]interface{}{"artifact_id": id})
	if err != nil || !strings.Contains(detail, `Chosen: b "Automation"`) || !strings.Contains(detail, "without an open answer (1 of 2): NOTES") {
		t.Fatalf("read_artifact = %q err=%v", detail, err)
	}

	// The agent resolves the answer like a note, and it leaves the open list.
	match := regexp.MustCompile(`- #(\d+) \[open, answered`).FindStringSubmatch(listed)
	if match == nil {
		t.Fatalf("no answer id in:\n%s", listed)
	}
	if _, err := call("resolve_artifact_annotation", map[string]interface{}{"artifact_id": id, "annotation_id": match[1], "resolution_note": "Renamed to Automation in v2"}); err != nil {
		t.Fatal(err)
	}
	listed, _ = call("list_artifact_annotations", map[string]interface{}{"artifact_id": id})
	if !strings.Contains(listed, "No open annotations") || !strings.Contains(listed, "without an open answer (2 of 2)") {
		t.Fatalf("list after resolving = %q", listed)
	}
	resolved, _ := call("list_artifact_annotations", map[string]interface{}{"artifact_id": id, "status": "resolved"})
	if !strings.Contains(resolved, "Renamed to Automation in v2") || strings.Contains(resolved, "without an open answer") {
		t.Fatalf("resolved list = %q", resolved)
	}
}
