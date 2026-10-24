package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostOriginGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	guard := HostOriginGuard([]string{"devbox.example.com"})(ok)

	cases := []struct {
		name   string
		method string
		path   string
		host   string
		origin string
		tunnel bool
		want   int
	}{
		{"localhost read", "GET", "/api/config/settings", "localhost:8080", "", false, 200},
		{"ip literal", "GET", "/api/projects", "192.0.2.10:8080", "", false, 200},
		{"ipv6 literal", "GET", "/api/projects", "[::1]:8080", "", false, 200},
		{"configured name", "GET", "/", "devbox.example.com:8080", "", false, 200},
		{"rebinding host refused", "GET", "/api/config/settings", "attacker.example:8080", "", false, 403},
		{"rebinding page load refused", "GET", "/", "attacker.example:8080", "", false, 403},
		{"same origin post", "POST", "/api/projects", "localhost:8080", "http://localhost:8080", false, 200},
		{"foreign origin read", "GET", "/api/config/settings", "localhost:8080", "https://evil.example", false, 403},
		{"foreign origin websocket", "GET", "/ws/session/abc", "localhost:8080", "https://evil.example", false, 403},
		{"foreign origin mcp text/plain post", "POST", "/mcp", "localhost:8080", "https://evil.example", false, 403},
		{"foreign origin otlp", "POST", "/v1/traces", "127.0.0.1:8080", "https://evil.example", false, 403},
		{"null origin", "POST", "/api/projects", "localhost:8080", "null", false, 403},
		{"port mismatch", "POST", "/api/projects", "localhost:8080", "http://localhost:3000", false, 403},
		{"tunnel request skipped", "GET", "/api/projects", "localhost:8080", "https://relay.example", true, 200},
		{"automation api has its own policy", "POST", "/api/automation/v1/commands", "attacker.example", "https://evil.example", false, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.tunnel {
				req.Header.Set("X-Tunnel-Request", "true")
			}
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("unexpected CORS header %q", got)
			}
		})
	}
}

func TestHostOriginGuardWildcard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Host = "anything.example:8080"
	rec := httptest.NewRecorder()
	HostOriginGuard([]string{"*"})(ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with OPENPOET_ALLOWED_HOSTS=*", rec.Code)
	}
}

func TestResolveActorReadsRequireCredentialOffLoopback(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mw := ResolveActorMiddleware(nil, []byte("jwt"), "ui-secret")(ok)

	cases := []struct {
		name   string
		method string
		path   string
		remote string
		cookie string
		want   int
	}{
		{"loopback read", "GET", "/api/config/settings", "127.0.0.1:5000", "", 200},
		{"loopback ws", "GET", "/ws/events", "[::1]:5000", "", 200},
		{"lan read without credential", "GET", "/api/config/settings", "192.0.2.7:5000", "", 401},
		{"lan ws without credential", "GET", "/ws/session/abc", "192.0.2.7:5000", "", 401},
		{"lan read with ui cookie", "GET", "/api/config/settings", "192.0.2.7:5000", "ui-secret", 200},
		{"lan page load", "GET", "/", "192.0.2.7:5000", "", 200},
		{"lan exempt version", "GET", "/api/version", "192.0.2.7:5000", "", 200},
		{"loopback mutation without credential", "POST", "/api/projects", "127.0.0.1:5000", "", 401},
		{"loopback mutation with ui cookie", "POST", "/api/projects", "127.0.0.1:5000", "ui-secret", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.RemoteAddr = tc.remote
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: uiCookieName, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
