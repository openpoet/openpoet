package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/database"
	"openpoet/internal/devboxpublish"
	"openpoet/internal/mcp"
	"openpoet/internal/sessiontoken"
)

// devboxFakeHost stands in for docker and devbox-forward.
type devboxFakeHost struct {
	forwards map[string]bool
	routes   map[string]string
}

func (h *devboxFakeHost) ContainerForSSHPort(_ context.Context, port int) (string, error) {
	if port == 2201 {
		return "alpha-devbox", nil
	}
	return "", errors.New("no container")
}
func (h *devboxFakeHost) ContainerExists(context.Context, string) error { return nil }
func (h *devboxFakeHost) Probe(context.Context, string, int, string) error {
	return nil
}
func (h *devboxFakeHost) PortFree(int) bool { return true }
func (h *devboxFakeHost) AddForward(_ context.Context, name, _ string, _, _ int, _ string) error {
	h.forwards[name] = true
	return nil
}
func (h *devboxFakeHost) RemoveForward(_ context.Context, name string) error {
	delete(h.forwards, name)
	return nil
}
func (h *devboxFakeHost) ForwardExists(name string) bool { return h.forwards[name] }
func (h *devboxFakeHost) WriteRoute(file, content string) error {
	h.routes[file] = content
	return nil
}
func (h *devboxFakeHost) RemoveRoute(file string) error     { delete(h.routes, file); return nil }
func (h *devboxFakeHost) RouteExists(file string) bool      { _, ok := h.routes[file]; return ok }
func (h *devboxFakeHost) ReloadProxy(context.Context) error { return nil }

func devboxTestServer(t *testing.T) (*database.DB, *devboxFakeHost, *httptest.Server, map[string]string) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "devbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	tokens := map[string]string{}
	for _, p := range []struct {
		name, host string
		port       int64
	}{{"alpha", "192.0.2.9", 2201}, {"other", "192.0.2.9", 2299}, {"far", "198.51.100.7", 2201}} {
		project := &database.Project{Name: p.name, Path: "/workspace", Type: "remote",
			SSHHost: sql.NullString{String: p.host, Valid: true}, SSHPort: sql.NullInt64{Int64: p.port, Valid: true}}
		if err := db.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
		sid := "sess-" + p.name
		if err := db.CreateSession(ctx, &database.Session{ID: sid, ProjectID: project.ID, Status: "running", StartTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		token, hash, err := sessiontoken.NewMCPToken(sid)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateSessionTokenHashes(ctx, sid, hash, ""); err != nil {
			t.Fatal(err)
		}
		tokens[p.name] = token
	}

	cfg := devboxpublish.Config{
		Enabled: true, HostTemplate: "{app}-dev9.example.com", SSHHosts: []string{"192.0.2.9"},
		Prefixes: map[string]string{"alpha-devbox": "alpha"}, Reserved: map[string]bool{},
		PortMin: 28100, PortMax: 28110, RoutesDir: "/srv/routes",
	}
	host := &devboxFakeHost{forwards: map[string]bool{}, routes: map[string]string{}}
	api := &API{db: db}
	api.SetDevboxPublisher(devboxpublish.NewService(cfg, db, host, nil, nil))

	r := chi.NewRouter()
	r.Use(ResolveActorMiddleware(db, []byte("jwt"), "ui-secret"))
	r.Get("/api/devbox/ports", api.ListDevboxPorts)
	r.Post("/api/devbox/ports", api.PublishDevboxPort)
	r.Delete("/api/devbox/ports/{app}", api.UnpublishDevboxPort)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return db, host, srv, tokens
}

func TestDevboxPortToolsEndToEndThroughTheAPI(t *testing.T) {
	db, host, srv, tokens := devboxTestServer(t)
	client := mcp.NewAPIClientWithToken(srv.URL, tokens["alpha"])
	call := func(name string, args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return mcp.ExecuteTool(client, name, raw, "sess-alpha", "")
	}

	out, err := call("openpoet_devbox_publish_port", map[string]any{"app": "alpha-web", "port": 3000})
	if err != nil || !strings.Contains(out, "Published: https://alpha-web-dev9.example.com/") {
		t.Fatalf("publish: %q %v", out, err)
	}
	if !host.forwards["op-alpha-web"] || host.routes["op-alpha-web.caddy"] == "" {
		t.Fatalf("host state: %+v", host)
	}
	p, _ := db.GetDevboxPublication(context.Background(), "alpha-web")
	if p == nil || p.SessionID != "sess-alpha" || p.Container != "alpha-devbox" || p.HostPort != 28100 {
		t.Fatalf("record: %+v", p)
	}
	out, err = call("openpoet_devbox_publish_port", map[string]any{"app": "alpha-web", "port": "3000"})
	if err != nil || !strings.HasPrefix(out, "Already published") {
		t.Fatalf("again: %q %v", out, err)
	}
	if _, err := call("openpoet_devbox_publish_port", map[string]any{"app": "beta-web", "port": 3000}); err == nil ||
		!strings.Contains(err.Error(), `must be "alpha" or start with "alpha-"`) {
		t.Fatalf("prefix error should be readable: %v", err)
	}
	out, err = call("openpoet_devbox_list_ports", nil)
	if err != nil || !strings.Contains(out, "alpha-web: https://alpha-web-dev9.example.com/ -> port 3000") {
		t.Fatalf("list: %q %v", out, err)
	}
	out, err = call("openpoet_devbox_unpublish_port", map[string]any{"app": "alpha-web"})
	if err != nil || !strings.Contains(out, "Removed") || len(host.forwards) != 0 || len(host.routes) != 0 {
		t.Fatalf("unpublish: %q %v %+v", out, err, host)
	}
	if out, _ := call("openpoet_devbox_list_ports", nil); !strings.Contains(out, "No ports") {
		t.Fatalf("list after: %q", out)
	}
}

func TestDevboxPortsRequireTheCallingSessionsOwnDevbox(t *testing.T) {
	_, host, srv, tokens := devboxTestServer(t)
	body := `{"app":"alpha-web","port":3000}`
	do := func(method, auth string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+"/api/devbox/ports", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		msg, _ := out["error"].(string)
		return resp.StatusCode, msg
	}
	// No credential: mutation refused by the middleware; a loopback read has no session.
	if code, _ := do(http.MethodPost, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token POST = %d", code)
	}
	if code, _ := do(http.MethodGet, ""); code != http.StatusForbidden {
		t.Fatalf("no token GET = %d", code)
	}
	// A forged token for a real session.
	if code, _ := do(http.MethodPost, "opst1_sess-alpha.forged"); code != http.StatusUnauthorized {
		t.Fatalf("forged token = %d", code)
	}
	// A session whose SSH port maps to no container, and one on another machine.
	if code, msg := do(http.MethodPost, tokens["other"]); code != http.StatusForbidden || !strings.Contains(msg, "which container") {
		t.Fatalf("other = %d %q", code, msg)
	}
	if code, msg := do(http.MethodPost, tokens["far"]); code != http.StatusForbidden || !strings.Contains(msg, "not a devbox") {
		t.Fatalf("far = %d %q", code, msg)
	}
	if len(host.forwards) != 0 {
		t.Fatalf("nothing should have been created: %+v", host.forwards)
	}
	if code, _ := do(http.MethodPost, tokens["alpha"]); code != http.StatusCreated {
		t.Fatalf("alpha = %d", code)
	}
}

func TestDevboxPortsDisabled(t *testing.T) {
	api := &API{}
	api.SetDevboxPublisher(devboxpublish.NewService(devboxpublish.Config{}, nil, nil, nil, nil))
	rec := httptest.NewRecorder()
	api.ListDevboxPorts(rec, httptest.NewRequest(http.MethodGet, "/api/devbox/ports", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled = %d", rec.Code)
	}
	api = &API{} // never wired
	rec = httptest.NewRecorder()
	api.ListDevboxPorts(rec, httptest.NewRequest(http.MethodGet, "/api/devbox/ports", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d", rec.Code)
	}
}
