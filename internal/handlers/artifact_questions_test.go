package handlers

import (
	"net/http"
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
