package automation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"openpoet/internal/database"
)

func automationTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "automation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func provisionTestClient(t *testing.T, db *database.DB, scopes ...Scope) *ProvisionedClient {
	t.Helper()
	seed := bytes.Repeat([]byte{byte(len(scopes) + 1)}, 16+9+32)
	provisioned, err := ProvisionClient(context.Background(), db, "helena", scopes, bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	return provisioned
}

func TestProvisionClientStoresOnlyTokenDigest(t *testing.T) {
	db := automationTestDB(t)
	provisioned := provisionTestClient(t, db, ScopeTasksWrite, ScopeTasksRead, ScopeTasksRead)
	if !strings.HasPrefix(provisioned.Token, tokenScheme+"_") {
		t.Fatalf("unexpected token format: %q", provisioned.Token)
	}
	stored, err := db.GetAutomationClientByTokenPrefix(context.Background(), provisioned.Client.TokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(provisioned.Token))
	if !bytes.Equal(stored.TokenHash, wantHash[:]) {
		t.Fatal("stored token hash differs")
	}
	if bytes.Contains(stored.TokenHash, []byte(provisioned.Token)) || strings.Contains(stored.Scopes, provisioned.Token) {
		t.Fatal("plaintext token was persisted")
	}
	var scopes []string
	if err := json.Unmarshal([]byte(stored.Scopes), &scopes); err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, ",") != "tasks:read,tasks:write" {
		t.Fatalf("stored scopes = %v", scopes)
	}
}

func TestProvisionClientRejectsInvalidInput(t *testing.T) {
	db := automationTestDB(t)
	if _, err := ProvisionClient(context.Background(), db, "", nil, nil); err == nil {
		t.Fatal("expected empty name error")
	}
	if _, err := ProvisionClient(context.Background(), db, "helena", []Scope{"unknown"}, nil); err == nil {
		t.Fatal("expected unknown scope error")
	}
}

func TestAuthenticatedHealthSecurityBoundary(t *testing.T) {
	db := automationTestDB(t)
	client := provisionTestClient(t, db, ScopeTasksRead)
	handler := CapturePeerAddress(NewHandler(db))

	tests := []struct {
		name       string
		token      string
		remoteAddr string
		origin     string
		wantStatus int
	}{
		{name: "valid loopback", token: client.Token, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusOK},
		{name: "valid ipv6 loopback", token: client.Token, remoteAddr: "[::1]:1234", wantStatus: http.StatusOK},
		{name: "missing token on localhost", remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", token: mutateToken(client.Token), remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusUnauthorized},
		{name: "non loopback", token: client.Token, remoteAddr: "192.0.2.5:1234", wantStatus: http.StatusForbidden},
		{name: "tunnel is not local origin", token: client.Token, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusForbidden},
		{name: "browser origin", token: client.Token, remoteAddr: "127.0.0.1:1234", origin: "https://example.test", wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://openpoet/health", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.name == "tunnel is not local origin" {
				req.Header.Set("X-Tunnel-Request", "true")
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if rr.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("automation response exposed CORS: %q", rr.Header().Get("Access-Control-Allow-Origin"))
			}
		})
	}
}

func TestAuthenticationUsesCapturedPeerNotForwardedHeader(t *testing.T) {
	db := automationTestDB(t)
	client := provisionTestClient(t, db)
	handler := CapturePeerAddress(middleware.RealIP(NewHandler(db)))

	req := httptest.NewRequest(http.MethodGet, "http://openpoet/health", nil)
	req.RemoteAddr = "192.0.2.8:4321"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("Authorization", "Bearer "+client.Token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("spoofed forwarded address status = %d, want 403", rr.Code)
	}
}

func TestDisabledClientCannotAuthenticate(t *testing.T) {
	db := automationTestDB(t)
	client := provisionTestClient(t, db)
	if err := db.SetAutomationClientEnabled(context.Background(), client.Client.ID, false); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://openpoet/health", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+client.Token)
	rr := httptest.NewRecorder()
	CapturePeerAddress(NewHandler(db)).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestRequireScopesAndActor(t *testing.T) {
	set, err := NewScopeSet(ScopeTasksRead)
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{Type: "automation_client", ID: "1", ClientID: "1", Scopes: set}
	handler := RequireScopes(ScopeTasksRead)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved, ok := ActorFromContext(r.Context())
		if !ok || resolved.ClientID != actor.ClientID {
			t.Fatal("actor missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(WithActor(context.Background(), actor))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	RequireScopes(ScopeTasksWrite)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler called without scope")
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", rr.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	handler := BodyLimit(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := readAll(r)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "too large", false)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

func TestIdempotencyReplayConflictAndBodyLimit(t *testing.T) {
	db := automationTestDB(t)
	client := provisionTestClient(t, db)
	set, _ := NewScopeSet()
	actor := Actor{Type: "automation_client", ID: client.Client.ID, ClientID: client.Client.ID, Scopes: set}
	var calls atomic.Int32
	business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})
	idempotency := NewIdempotency(db, IdempotencyOptions{
		Now: func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
	})
	handler := BodyLimit(32)(idempotency.Middleware(business))

	request := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/commands?mode=test", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(WithActor(req.Context(), actor))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	first := request("same-key", `{"value":1}`)
	if first.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("first status=%d calls=%d body=%s", first.Code, calls.Load(), first.Body.String())
	}
	replay := request("same-key", `{"value":1}`)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("replay status=%d replay=%q calls=%d", replay.Code, replay.Header().Get("Idempotency-Replayed"), calls.Load())
	}
	conflict := request("same-key", `{"value":2}`)
	if conflict.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("conflict status=%d calls=%d", conflict.Code, calls.Load())
	}
	missing := request("", `{}`)
	if missing.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("missing key status=%d calls=%d", missing.Code, calls.Load())
	}
	tooLarge := request("large", strings.Repeat("x", 33))
	if tooLarge.Code != http.StatusRequestEntityTooLarge || calls.Load() != 1 {
		t.Fatalf("large status=%d calls=%d", tooLarge.Code, calls.Load())
	}
}

func TestIdempotencyDoesNotRepeatProcessingCommand(t *testing.T) {
	db := automationTestDB(t)
	client := provisionTestClient(t, db)
	set, _ := NewScopeSet()
	actor := Actor{Type: "automation_client", ID: client.Client.ID, ClientID: client.Client.ID, Scopes: set}
	req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(`{"value":1}`))
	req.Header.Set("Idempotency-Key", "processing")
	req = req.WithContext(WithActor(req.Context(), actor))
	fingerprint, err := requestFingerprint(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := db.ClaimAutomationCommand(context.Background(), &database.AutomationCommand{
		ID: "processing-command", ClientID: client.Client.ID, IdempotencyKey: "processing",
		RequestFingerprint: fingerprint, Operation: "POST /commands",
	}); err != nil || !created {
		t.Fatalf("preclaim created=%v err=%v", created, err)
	}

	var called bool
	rr := httptest.NewRecorder()
	NewIdempotency(db, IdempotencyOptions{}).Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || called {
		t.Fatalf("status=%d called=%v", rr.Code, called)
	}
}

func mutateToken(token string) string {
	replacement := "A"
	if strings.HasSuffix(token, replacement) {
		replacement = "B"
	}
	return token[:len(token)-1] + replacement
}

func readAll(r *http.Request) ([]byte, error) {
	buffer := new(bytes.Buffer)
	_, err := buffer.ReadFrom(r.Body)
	return buffer.Bytes(), err
}
