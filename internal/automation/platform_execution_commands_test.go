package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// ackingManager is a running session whose agent accepts each submitted line
// after ackDelay, like Claude Code's UserPromptSubmit hook.
type ackingManager struct {
	closeCompletedManager
	signals  *ackingSignals
	ackDelay time.Duration
	lines    atomic.Int32
}

func (m *ackingManager) WriteToSession(_ string, data []byte) error {
	if string(data) == "\n" {
		m.lines.Add(1)
		time.AfterFunc(m.ackDelay, m.signals.fire)
	}
	return nil
}

type ackingSignals struct {
	mu      sync.Mutex
	waiters []chan struct{}
}

func (s *ackingSignals) RegisterPromptWaiter(string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()
	return ch, func() {}
}

func (s *ackingSignals) fire() {
	s.mu.Lock()
	waiters := s.waiters
	s.waiters = nil
	s.mu.Unlock()
	for _, ch := range waiters {
		ch <- struct{}{}
	}
}

func (s *ackingSignals) GetSessionMode(string) string { return "idle" }

func sendInputLedgerHandler(t *testing.T, db *database.DB, ackDelay time.Duration) (http.Handler, *ackingManager) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	platform, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	store := &closeCompletedStore{session: &database.Session{ID: "s1", ProjectID: 20, Status: "running"}}
	signals := &ackingSignals{}
	manager := &ackingManager{signals: signals, ackDelay: ackDelay}
	service := application.NewSessionService(store, manager, nil, nil, nil, nil, nil, nil,
		application.SessionCreationCollaborators{Signals: signals})
	sessions := &sessionPlatformExecutor{service: service, runtime: manager}
	for _, definition := range sessionPlatformDefinitions() {
		if definition.Name == "sessions.send_input" {
			if err := platform.Register(definition, sessions); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, definition := range automationCommandPlatformDefinitions() {
		if err := platform.Register(definition, &automationCommandPlatformExecutor{ledger: db}); err != nil {
			t.Fatal(err)
		}
	}
	return CapturePeerAddress(NewHandler(db, Dependencies{Capabilities: capabilities, PlatformCapabilities: platform})), manager
}

func sendInputEnvelope(key string) map[string]any {
	return map[string]any{
		"command_id": "cmd-" + key, "idempotency_key": key, "capability": "sessions.send_input",
		"correlation_id": "ain:238", "target": map[string]any{"type": "session", "id": "s1"},
		"payload": map[string]any{"text": "leia o doc"},
	}
}

// postWithDeadline sends a command the way a client with a short timeout
// does: its request context is cancelled after the deadline.
func postWithDeadline(t *testing.T, handler http.Handler, token string, body any, deadline time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "http://openpoet/commands", bytes.NewReader(encoded)).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:3210"
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func getCommand(t *testing.T, handler http.Handler, token, queryKey string, payload map[string]any) AutomationCommandView {
	t.Helper()
	response := automationRequest(t, handler, token, http.MethodPost, "/commands", queryKey, map[string]any{
		"command_id": queryKey, "capability": "automation.commands.get", "target": map[string]any{}, "payload": payload,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("automation.commands.get status=%d body=%s", response.Code, response.Body.String())
	}
	_, view := decodeCommandResult[AutomationCommandView](t, response)
	return view
}

// The client in the d1f920bb incident gave up after 5 s, while the agent
// confirmed the prompt later: the server cut its own ack wait short and
// recorded acknowledged=false. Now the abort changes nothing on the server,
// a retry of the same envelope returns the real outcome without typing the
// text again, and automation.commands.get reports it.
func TestSendInputAbortedByClientRecordsRealAckAndRetryDoesNotRetype(t *testing.T) {
	db := automationTestDB(t)
	handler, manager := sendInputLedgerHandler(t, db, 400*time.Millisecond)
	client := provisionApprovalTestClient(t, db, "helena-send", ScopeSessionsWrite, ScopeEventsRead)
	envelope := sendInputEnvelope("ain238-doc-d1f920bb")

	// First attempt: the client aborts at 100 ms, before the ack at 400 ms.
	// A retry of the same envelope arrives while it is still running.
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postWithDeadline(t, handler, client.Token, envelope, 100*time.Millisecond) }()
	time.Sleep(150 * time.Millisecond)
	if view := getCommand(t, handler, client.Token, "status-while-running", map[string]any{"idempotency_key": "ain238-doc-d1f920bb"}); view.State != "pending" {
		t.Fatalf("status while running = %+v, want pending", view)
	}
	retry := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "", envelope)
	if retry.Code != http.StatusOK || retry.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry status=%d replayed=%q body=%s", retry.Code, retry.Header().Get("Idempotency-Replayed"), retry.Body.String())
	}
	_, result := decodeCommandResult[map[string]any](t, retry)
	if result["acknowledged"] != true || result["sent"] != true {
		t.Fatalf("retry result = %v, want the recorded acknowledged send", result)
	}
	<-first
	if got := manager.lines.Load(); got != 1 {
		t.Fatalf("text submitted %d times, want exactly once", got)
	}

	view := getCommand(t, handler, client.Token, "status-after", map[string]any{"idempotency_key": "ain238-doc-d1f920bb"})
	if !view.Found || view.State != "applied" || view.Capability != "sessions.send_input" ||
		view.CommandID != "cmd-ain238-doc-d1f920bb" || view.Acknowledged == nil || !*view.Acknowledged {
		t.Fatalf("commands.get = %+v", view)
	}
	byCommandID := getCommand(t, handler, client.Token, "status-by-command", map[string]any{"command_id": "cmd-ain238-doc-d1f920bb"})
	if byCommandID.IdempotencyKey != "ain238-doc-d1f920bb" || byCommandID.State != "applied" {
		t.Fatalf("commands.get by command_id = %+v", byCommandID)
	}
	if missing := getCommand(t, handler, client.Token, "status-missing", map[string]any{"idempotency_key": "never-sent"}); missing.Found {
		t.Fatalf("unknown key = %+v, want found=false", missing)
	}
	other := provisionApprovalTestClient(t, db, "other-client", ScopeEventsRead)
	if foreign := getCommand(t, handler, other.Token, "foreign-status", map[string]any{"idempotency_key": "ain238-doc-d1f920bb"}); foreign.Found {
		t.Fatalf("another client read the command: %+v", foreign)
	}
}

// A command still processing when the server went down must not answer
// "still processing" forever: the restart closes it as indeterminate.
func TestInterruptedCommandIsIndeterminateAfterRestart(t *testing.T) {
	db := automationTestDB(t)
	handler, manager := sendInputLedgerHandler(t, db, time.Millisecond)
	client := provisionApprovalTestClient(t, db, "helena-restart", ScopeSessionsWrite, ScopeEventsRead)
	envelope := sendInputEnvelope("ain300-restart")
	encoded, _ := json.Marshal(envelope)
	req := httptest.NewRequest(http.MethodPost, "http://openpoet/commands", bytes.NewReader(encoded))
	fingerprint, err := requestFingerprint(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := db.ClaimAutomationCommand(context.Background(), &database.AutomationCommand{
		ID: "interrupted", ClientID: client.Client.ID, IdempotencyKey: "ain300-restart", RequestFingerprint: fingerprint,
		Operation: "POST /commands", CommandID: "cmd-ain300-restart", Capability: "sessions.send_input",
	}); err != nil || !created {
		t.Fatalf("claim created=%v err=%v", created, err)
	}
	if n, err := db.MarkInterruptedAutomationCommands(context.Background()); err != nil || n != 1 {
		t.Fatalf("mark interrupted n=%d err=%v", n, err)
	}
	retry := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "", envelope)
	if retry.Code != http.StatusConflict || decodeAutomationErrorCode(t, retry) != "idempotency_indeterminate" || manager.lines.Load() != 0 {
		t.Fatalf("retry status=%d lines=%d body=%s", retry.Code, manager.lines.Load(), retry.Body.String())
	}
	view := getCommand(t, handler, client.Token, "status-restart", map[string]any{"idempotency_key": "ain300-restart"})
	if view.State != "indeterminate" || view.Error == nil || view.Error.Code != "server_restarted" {
		t.Fatalf("commands.get = %+v", view)
	}
}
