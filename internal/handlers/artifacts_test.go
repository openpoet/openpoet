package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

const artifactHandlerHTML = `<!doctype html><html><body><h1>Roadmap</h1><script>window.ready=true</script></body></html>`

// artifactTestRouter mounts the artifact routes the way cmd/openpoet does.
func artifactTestRouter(t *testing.T) (*API, http.Handler) {
	t.Helper()
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {
		r.Post("/artifacts", api.CreateArtifact)
		r.Get("/artifacts", api.ListArtifacts)
		r.Get("/artifacts/{id}", api.GetArtifact)
		r.Put("/artifacts/{id}", api.UpdateArtifact)
		r.Patch("/artifacts/{id}", api.PatchArtifact)
		r.Get("/artifacts/{id}/versions/{version}", api.GetArtifactVersion)
	})
	return api, router
}

func artifactRequest(t *testing.T, router http.Handler, method, path string, body any, actor *application.Actor) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	if actor != nil {
		request = request.WithContext(withResolvedActor(request.Context(), resolvedActorInfo{actor: *actor}))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

func TestArtifactRESTLifecycle(t *testing.T) {
	api, router := artifactTestRouter(t)
	ctx := context.Background()
	project := &database.Project{Name: "artifact-rest", Path: "/tmp/artifact-rest", Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := api.db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-rest", ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// A session-authenticated caller is attributed to its own session even if
	// the payload claims another one; ids may arrive as strings (MCP callers).
	session := application.Actor{Type: "session", ID: "sess-rest"}
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{
		"title": "Roadmap", "html": artifactHandlerHTML, "session_id": "someone-else", "task_id": "",
	}, &session)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	id, _ := created["id"].(string)
	artifact, _ := created["artifact"].(map[string]any)
	if id == "" || created["version"] != float64(1) || created["created"] != true || created["link"] != "/app/artifact/"+id ||
		artifact["session_id"] != "sess-rest" || artifact["project_id"] != float64(project.ID) || artifact["created_by"] != "session:sess-rest" {
		t.Fatalf("create response = %v", created)
	}
	if strings.Contains(recorder.Body.String(), "window.ready") {
		t.Fatal("create response echoed the HTML")
	}

	// New version with the right base; then a stale base is a 409.
	v2 := strings.Replace(artifactHandlerHTML, "Roadmap", "Roadmap v2", 1)
	recorder, updated := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id, map[string]any{"html": v2, "base_version": "1", "change_note": "v2"}, &session)
	if recorder.Code != http.StatusOK || updated["version"] != float64(2) || updated["created"] != true {
		t.Fatalf("update status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, conflict := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id, map[string]any{"html": v2 + "<!-- -->", "base_version": 1}, &session)
	if recorder.Code != http.StatusConflict || conflict["code"] != "artifact_version_conflict" {
		t.Fatalf("conflict status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	// Metadata + history, without HTML.
	recorder, detail := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id, nil, nil)
	versions, _ := detail["versions"].([]any)
	if recorder.Code != http.StatusOK || detail["current_version"] != float64(2) || len(versions) != 2 || strings.Contains(recorder.Body.String(), "window.ready") {
		t.Fatalf("detail status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	// HTML is returned as JSON data, never as text/html.
	for path, want := range map[string]string{
		"/api/artifacts/" + id + "/versions/current": v2,
		"/api/artifacts/" + id + "/versions/1":       artifactHandlerHTML,
	} {
		recorder, version := artifactRequest(t, router, http.MethodGet, path, nil, nil)
		if recorder.Code != http.StatusOK || version["content"] != want || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: status=%d type=%q body=%s", path, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
		}
	}
	if recorder, _ := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/versions/zero", nil, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad version status = %d", recorder.Code)
	}
	if recorder, _ := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/versions/9", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("missing version status = %d", recorder.Code)
	}
	if recorder, _ := artifactRequest(t, router, http.MethodGet, "/api/artifacts/missing", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("missing artifact status = %d", recorder.Code)
	}

	// Listing by session and by project; archiving hides it.
	listRequest := func(query string) []any {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/artifacts"+query, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		var items []any
		if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("list %s: status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
		return items
	}
	if items := listRequest("?session_id=sess-rest"); len(items) != 1 {
		t.Fatalf("session listing = %v", items)
	}
	if items := listRequest("?project_id=999"); len(items) != 0 {
		t.Fatalf("other project listing = %v", items)
	}
	recorder, patched := artifactRequest(t, router, http.MethodPatch, "/api/artifacts/"+id, map[string]any{"status": "archived", "title": "Old roadmap"}, nil)
	if recorder.Code != http.StatusOK || patched["status"] != "archived" || patched["title"] != "Old roadmap" {
		t.Fatalf("patch status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if items := listRequest("?session_id=sess-rest"); len(items) != 0 {
		t.Fatalf("archived artifact still listed: %v", items)
	}
	if items := listRequest("?session_id=sess-rest&include_archived=1"); len(items) != 1 {
		t.Fatalf("include_archived listing = %v", items)
	}
}

func TestArtifactRESTRejectsInvalidAndOversizedContent(t *testing.T) {
	_, router := artifactTestRouter(t)

	recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "x", "html": "# just markdown"}, nil)
	if recorder.Code != http.StatusBadRequest || body["code"] != "invalid_artifact_html" {
		t.Fatalf("markdown status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, body = artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"html": ""}, nil)
	if recorder.Code != http.StatusBadRequest || body["code"] != "content_required" {
		t.Fatalf("empty status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	// Just over the per-version limit: rejected by the service with a clear code.
	over := "<p>" + strings.Repeat("x", application.MaxArtifactHTMLBytes) + "</p>"
	recorder, body = artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"html": over}, nil)
	if recorder.Code != http.StatusBadRequest || body["code"] != "content_too_large" {
		t.Fatalf("over-limit status = %d body=%.200s", recorder.Code, recorder.Body.String())
	}
	// Far over the request cap: cut off by the body limit before decoding.
	huge := "<p>" + strings.Repeat("x", maxArtifactRequestBytes) + "</p>"
	recorder, body = artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"html": huge}, nil)
	if recorder.Code != http.StatusRequestEntityTooLarge || body["code"] != "content_too_large" {
		t.Fatalf("huge status = %d body=%.200s", recorder.Code, recorder.Body.String())
	}
	if recorder, _ := artifactRequest(t, router, http.MethodPost, "/api/artifacts", nil, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty body status = %d", recorder.Code)
	}
}
