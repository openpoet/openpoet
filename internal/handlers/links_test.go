package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
)

func TestGetLinkRouteAndPublishResponsesUseThePublicBaseURL(t *testing.T) {
	t.Setenv(application.PublicBaseURLEnv, "")
	api, router := artifactTestRouter(t)
	mux := chi.NewRouter()
	mux.Get("/api/links", api.GetLink)
	mux.Put("/api/config/settings", api.UpdateSettings)
	mux.Post("/api/documents", api.CreateTempDocument)
	mux.Mount("/", router)

	created, body := artifactRequest(t, mux, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html": artifactHandlerHTML}, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	id, _ := body["id"].(string)
	if body["link"] != "/app/artifact/"+id || body["url"] != "" {
		t.Fatalf("publish response without a public base = %v", body)
	}

	// Not configured: the path and the address the request came in on.
	response, link := artifactRequest(t, mux, http.MethodGet, "/api/links?kind=artifact&id="+id, nil, nil)
	if response.Code != http.StatusOK || link["public"] != false || link["path"] != "/app/artifact/"+id ||
		link["local_url"] != "http://example.com/app/artifact/"+id || link["url"] != nil {
		t.Fatalf("unconfigured link = %d %v", response.Code, link)
	}

	// The owner sets the public base URL; links pick it up at once.
	if saved, _ := artifactRequest(t, mux, http.MethodPut, "/api/config/settings", map[string]string{"public_base_url": "https://openpoet.example.com/"}, nil); saved.Code != http.StatusOK {
		t.Fatalf("save setting = %d %s", saved.Code, saved.Body.String())
	}
	if host := api.PublicHost(); host != "openpoet.example.com" {
		t.Fatalf("public host = %q", host)
	}
	response, link = artifactRequest(t, mux, http.MethodGet, "/api/links?kind=artifact&id="+id+"&version=1", nil, nil)
	if response.Code != http.StatusOK || link["public"] != true || link["url"] != "https://openpoet.example.com/app/artifact/"+id+"?v=1" || link["title"] != "Plan" {
		t.Fatalf("configured link = %d %v", response.Code, link)
	}
	updated, body := artifactRequest(t, mux, http.MethodPut, "/api/artifacts/"+id, map[string]any{"html": artifactHandlerHTML + "<!-- v2 -->", "base_version": 1}, nil)
	if updated.Code != http.StatusOK || body["url"] != "https://openpoet.example.com/app/artifact/"+id {
		t.Fatalf("update response = %d %v", updated.Code, body)
	}
	document, body := artifactRequest(t, mux, http.MethodPost, "/api/documents", map[string]any{"title": "Notes", "content": "# Notes"}, nil)
	docID, _ := body["id"].(string)
	if document.Code != http.StatusCreated || body["link"] != "/app/doc/"+docID || body["url"] != "https://openpoet.example.com/app/doc/"+docID {
		t.Fatalf("document response = %d %v", document.Code, body)
	}

	// A session can ask for its own link without naming itself.
	session := application.Actor{Type: "session", ID: "sess-none"}
	if response, _ = artifactRequest(t, mux, http.MethodGet, "/api/links?kind=session", nil, &session); response.Code != http.StatusNotFound {
		t.Fatalf("link of an unknown session = %d %s", response.Code, response.Body.String())
	}
	for path, want := range map[string]int{
		"/api/links?kind=artifact&id=nope0000":             http.StatusNotFound,
		"/api/links?kind=artifact&id=" + id + "&version=7": http.StatusNotFound,
		"/api/links?kind=artifact&id=" + id + "&version=x": http.StatusBadRequest,
		"/api/links?kind=file&id=x":                        http.StatusBadRequest,
		"/api/links?kind=artifact":                         http.StatusBadRequest,
	} {
		if response, _ = artifactRequest(t, mux, http.MethodGet, path, nil, nil); response.Code != want {
			t.Errorf("%s = %d, want %d (%s)", path, response.Code, want, response.Body.String())
		}
	}

	// An address that is not a URL is refused and the stored one stays.
	if saved, _ := artifactRequest(t, mux, http.MethodPut, "/api/config/settings", map[string]string{"public_base_url": "openpoet.example.com"}, nil); saved.Code != http.StatusBadRequest {
		t.Fatalf("invalid setting = %d %s", saved.Code, saved.Body.String())
	}
	if host := api.PublicHost(); host != "openpoet.example.com" {
		t.Fatalf("public host after a refused change = %q", host)
	}
}

func TestHostOriginGuardAcceptsThePublicHost(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	public := ""
	guard := HostOriginGuard(nil, func() string { return public })(ok)
	status := func(host string) int {
		request := httptest.NewRequest(http.MethodGet, "/app/artifact/ab12cd34", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		guard.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if got := status("openpoet.example.com"); got != http.StatusForbidden {
		t.Fatalf("unconfigured public host = %d, want 403", got)
	}
	public = "openpoet.example.com"
	for _, host := range []string{"openpoet.example.com", "OpenPoet.Example.com:443", "openpoet.example.com."} {
		if got := status(host); got != http.StatusOK {
			t.Fatalf("public host %q = %d, want 200", host, got)
		}
	}
	// Only that exact name: a rebinding domain is still refused.
	for _, host := range []string{"attacker.example", "evil.openpoet.example.com", "openpoet.example.com.attacker.example"} {
		if got := status(host); got != http.StatusForbidden {
			t.Fatalf("host %q = %d, want 403", host, got)
		}
	}
}

func TestGetLinkChatToolSharesTheWording(t *testing.T) {
	t.Setenv(application.PublicBaseURLEnv, "https://openpoet.example.com")
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "ui"}, Approved: true, ApprovedBy: "ui", Reason: "test"}
	call := func(name string, input map[string]interface{}) (string, error) {
		return services.AIHandler.executeTool(ctx, name, input, 0, 0, nil, "", auth)
	}
	published, err := call("create_artifact", map[string]interface{}{"title": "Chat plan", "html": "<html><body><h1>Plan</h1></body></html>"})
	if err != nil {
		t.Fatal(err)
	}
	match := artifactIDInToolResult.FindStringSubmatch(published)
	if match == nil || !strings.Contains(published, "Shareable link, if the user asks for one to pass on: https://openpoet.example.com/app/artifact/"+match[1]) {
		t.Fatalf("chat create result = %s", published)
	}
	link, err := call("get_link", map[string]interface{}{"kind": "artifact", "id": "artifact:" + match[1]})
	if err != nil || !strings.Contains(link, `Link to artifact `+match[1]+` "Chat plan": https://openpoet.example.com/app/artifact/`+match[1]) {
		t.Fatalf("chat get_link = %q err=%v", link, err)
	}
	if _, err := call("get_link", map[string]interface{}{"kind": "artifact", "id": "nope0000"}); err == nil {
		t.Fatal("get_link invented a link to a missing artifact")
	}
	document, err := call("create_document", map[string]interface{}{"title": "Notes", "content": "# Notes"})
	if err != nil || !strings.Contains(document, "Internal link: /app/doc/") || !strings.Contains(document, "Shareable link, if the user asks for one to pass on: https://openpoet.example.com/app/doc/") {
		t.Fatalf("chat create_document = %q err=%v", document, err)
	}
}
