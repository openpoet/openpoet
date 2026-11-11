package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		r.Get("/artifacts/{id}/render", api.RenderArtifact)
		r.Get("/sessions/{id}/documents", api.ListSessionDocuments)
		r.Get("/projects/{id}/tasks/{taskId}/documents", api.ListTaskDocuments)
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

// TestArtifactRenderIsAlwaysSandboxed pins the response that carries
// agent-generated HTML to a browser: the HTML itself, and never without the
// headers that contain it.
func TestArtifactRenderIsAlwaysSandboxed(t *testing.T) {
	_, router := artifactTestRouter(t)
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Render", "html": artifactHandlerHTML}, nil)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	id, _ := created["id"].(string)
	v2 := strings.Replace(artifactHandlerHTML, "Roadmap", "Roadmap v2", 1)
	if recorder, _ := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id, map[string]any{"html": v2}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d", recorder.Code)
	}

	render := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Accept", "text/html")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}
	assertContained := func(name string, recorder *httptest.ResponseRecorder) {
		t.Helper()
		csp := recorder.Header().Get("Content-Security-Policy")
		for _, directive := range []string{
			"sandbox allow-scripts;", "default-src 'none'", "script-src 'unsafe-inline'", "connect-src 'none'",
			"form-action 'none'", "base-uri 'none'", "frame-src 'none'", "worker-src 'none'", "object-src 'none'", "frame-ancestors 'self'",
		} {
			if !strings.Contains(csp, directive) {
				t.Errorf("%s: CSP lacks %q: %s", name, directive, csp)
			}
		}
		// The sandbox must grant scripts and nothing else: any of these would
		// hand the artifact the OpenPoet origin, popups, forms or navigation.
		sandbox := strings.SplitN(csp, ";", 2)[0]
		for _, forbidden := range []string{"allow-same-origin", "allow-popups", "allow-forms", "allow-top-navigation", "allow-downloads", "allow-modals"} {
			if strings.Contains(sandbox, forbidden) {
				t.Errorf("%s: sandbox grants %s: %s", name, forbidden, sandbox)
			}
		}
		for _, source := range []string{"http:", "https:", "*", "'self'"} {
			for _, directive := range strings.Split(csp, ";") {
				directive = strings.TrimSpace(directive)
				if strings.HasPrefix(directive, "frame-ancestors") {
					continue
				}
				for _, token := range strings.Fields(directive)[1:] {
					if token == source {
						t.Errorf("%s: CSP directive %q allows %s", name, directive, source)
					}
				}
			}
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store",
			"Cross-Origin-Resource-Policy": "same-origin", "X-Frame-Options": "SAMEORIGIN",
		} {
			if got := recorder.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", name, header, got, want)
			}
		}
		if cookies := recorder.Header().Values("Set-Cookie"); len(cookies) != 0 {
			t.Errorf("%s: render response sets cookies: %v", name, cookies)
		}
	}

	current := render("/api/artifacts/" + id + "/render")
	if current.Code != http.StatusOK || !strings.HasPrefix(current.Header().Get("Content-Type"), "text/html") || !strings.Contains(current.Body.String(), "Roadmap v2") {
		t.Fatalf("current render: status=%d type=%q body=%s", current.Code, current.Header().Get("Content-Type"), current.Body.String())
	}
	assertContained("current", current)

	first := render("/api/artifacts/" + id + "/render?v=1")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "<h1>Roadmap</h1>") {
		t.Fatalf("v1 render: status=%d body=%s", first.Code, first.Body.String())
	}
	assertContained("v1", first)

	// Errors are plain text and carry the same headers.
	for path, status := range map[string]int{
		"/api/artifacts/missing/render":         http.StatusNotFound,
		"/api/artifacts/" + id + "/render?v=99": http.StatusNotFound,
		"/api/artifacts/" + id + "/render?v=x":  http.StatusBadRequest,
	} {
		failed := render(path)
		if failed.Code != status || !strings.HasPrefix(failed.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("%s: status=%d type=%q", path, failed.Code, failed.Header().Get("Content-Type"))
		}
		assertContained(path, failed)
	}
}

func TestArtifactsAppearInSessionAndTaskDocumentListings(t *testing.T) {
	api, router := artifactTestRouter(t)
	ctx := context.Background()
	project := &database.Project{Name: "artifact-docs", Path: "/tmp/artifact-docs", Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := api.db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Write plan", Status: "todo", Priority: "medium"}
	if err := api.db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-docs", ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := api.db.LinkSessionToTask(ctx, "sess-docs", task.ID); err != nil {
		t.Fatal(err)
	}
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html": artifactHandlerHTML, "session_id": "sess-docs"}, nil)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	id, _ := created["id"].(string)

	for _, path := range []string{
		"/api/sessions/sess-docs/documents",
		"/api/projects/" + strconv.FormatInt(project.ID, 10) + "/tasks/" + strconv.FormatInt(task.ID, 10) + "/documents",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		var entries []map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if len(entries) != 1 || entries[0]["id"] != "artifact:"+id || entries[0]["type"] != "artifact" || entries[0]["title"] != "Plan" || entries[0]["status"] != "v1" {
			t.Fatalf("%s: entries = %v", path, entries)
		}
	}
}
