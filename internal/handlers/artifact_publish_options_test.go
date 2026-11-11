package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func TestArtifactRESTDryRunRetryMetaAndPlacement(t *testing.T) {
	api, router := artifactTestRouter(t)
	ctx := context.Background()
	project := &database.Project{Name: "wave2", Path: t.TempDir(), Type: "local"}
	if err := api.db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-w2", ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session := &application.Actor{Type: "session", ID: "sess-w2"}
	user := &application.Actor{Type: "user", ID: "ui"}
	html := "<!doctype html>\n<html><body>\n<p id=\"budget\">Budget is 40k.</p>\n<p>api_key = abc123456</p>\n</body></html>\n"

	// dry run: 200, no id, the masking report, nothing stored.
	recorder, dry := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html": html, "dry_run": true}, session)
	redactions, _ := dry["redactions"].([]any)
	if recorder.Code != http.StatusOK || dry["dry_run"] != true || dry["id"] != "" || dry["redaction_count"] != float64(1) || len(redactions) != 1 ||
		redactions[0].(map[string]any)["line"] != float64(4) || redactions[0].(map[string]any)["kind"] != "api_key" {
		t.Fatalf("dry run = %d %v", recorder.Code, dry)
	}
	if recorder, list := artifactRequest(t, router, http.MethodGet, "/api/artifacts?session_id=sess-w2", nil, session); recorder.Code != http.StatusOK || recorder.Body.String() != "[]\n" {
		t.Fatalf("a dry run stored an artifact: %d %s %v", recorder.Code, recorder.Body.String(), list)
	}

	// create, then the same create again: 201, then 200 with the same artifact.
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html": html}, session)
	if recorder.Code != http.StatusCreated || created["created"] != true || created["redaction_count"] != float64(1) {
		t.Fatalf("create = %d %v", recorder.Code, created)
	}
	id := created["id"].(string)
	recorder, again := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html": html}, session)
	if recorder.Code != http.StatusOK || again["duplicate"] != true || again["id"] != id || again["created"] != false {
		t.Fatalf("repeated create = %d %v", recorder.Code, again)
	}

	// The user annotates v1; v2 rewrites the sentence and renames the artifact.
	recorder, note := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/annotations", map[string]any{
		"anchor": map[string]any{"type": "text", "selector": "#budget", "exact": "Budget is 40k.", "near": "#budget"}, "body": "Raise it",
	}, user)
	if recorder.Code != http.StatusCreated || note["anchor"].(map[string]any)["near"] != "#budget" {
		t.Fatalf("annotation = %d %v", recorder.Code, note)
	}
	recorder, updated := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id, map[string]any{
		"html": "<!doctype html>\n<html><body>\n<p id=\"budget\">Budget is 55k.</p>\n</body></html>\n", "base_version": 1, "title": "Plan, revised", "summary": "Budget raised",
	}, session)
	artifact, _ := updated["artifact"].(map[string]any)
	if recorder.Code != http.StatusOK || updated["version"] != float64(2) || artifact["title"] != "Plan, revised" || artifact["summary"] != "Budget raised" {
		t.Fatalf("update with title = %d %v", recorder.Code, updated)
	}

	// The plain listing has no placement; with placement=1 the note says its
	// quote is gone from v2.
	recorder, _ = artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/annotations?status=open", nil, session)
	if recorder.Code != http.StatusOK || containsJSONKey(recorder.Body.String(), "placement") {
		t.Fatalf("plain listing = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder, _ = artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/annotations?status=open&placement=1", nil, session)
	if body := recorder.Body.String(); recorder.Code != http.StatusOK || !containsJSONKey(body, "placement") ||
		!containsText(body, `"placement":{"version":2,"checked":true,"found":false}`) {
		t.Fatalf("listing with placement = %d %s", recorder.Code, recorder.Body.String())
	}
}

func containsJSONKey(body, key string) bool { return containsText(body, `"`+key+`":`) }

func containsText(body, text string) bool {
	for i := 0; i+len(text) <= len(body); i++ {
		if body[i:i+len(text)] == text {
			return true
		}
	}
	return false
}
