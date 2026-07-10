package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"openpoet/internal/database"
)

type fakeRunner struct {
	mu       sync.Mutex
	writeErr error
	writes   [][]byte
	done     chan struct{}
	stopped  bool
}

func (f *fakeRunner) Start(ctx context.Context) error { return nil }
func (f *fakeRunner) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}
func (f *fakeRunner) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(data), nil
}
func (f *fakeRunner) Resize(rows, cols uint16) error { return nil }
func (f *fakeRunner) Wait() error                    { return nil }
func (f *fakeRunner) PID() int                       { return 0 }
func (f *fakeRunner) Done() <-chan struct{} {
	if f.done != nil {
		return f.done
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (f *fakeRunner) stoppedCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

type fakeCodexCommandRunner struct {
	fakeRunner
	err     error
	catalog interface{}
	actions []string
}

func (f *fakeCodexCommandRunner) HandleCodexCommand(ctx context.Context, data json.RawMessage) (interface{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	var request struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(data, &request)
	f.actions = append(f.actions, request.Action)
	if request.Action == "model/list" && f.catalog != nil {
		return f.catalog, nil
	}
	return map[string]string{"ok": "true"}, nil
}

func TestManagerCodexAppServerInputSendNotifiesUserPrompt(t *testing.T) {
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendCodex)},
				runner:  &fakeCodexCommandRunner{},
			},
		},
	}
	var got []string
	m.OnUserPromptSubmitted = func(sessionID string) {
		got = append(got, sessionID)
	}

	_, err := m.HandleCodexCommand(context.Background(), "sess-1", json.RawMessage(`{"action":"input/send","params":{"text":"build it"}}`))
	if err != nil {
		t.Fatalf("HandleCodexCommand returned error: %v", err)
	}
	if len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("OnUserPromptSubmitted calls = %#v, want [sess-1]", got)
	}
}

func TestManagerCodexAppServerDoesNotNotifyOnFailedInputSend(t *testing.T) {
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendCodex)},
				runner:  &fakeCodexCommandRunner{err: errors.New("turn failed")},
			},
		},
	}
	calls := 0
	m.OnUserPromptSubmitted = func(sessionID string) {
		calls++
	}

	_, err := m.HandleCodexCommand(context.Background(), "sess-1", json.RawMessage(`{"action":"input/send","params":{"text":"build it"}}`))
	if err == nil {
		t.Fatal("expected HandleCodexCommand error")
	}
	if calls != 0 {
		t.Fatalf("OnUserPromptSubmitted calls = %d, want 0", calls)
	}
}

func TestManagerCodexTerminalEnterNotifiesUserPrompt(t *testing.T) {
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendCodex)},
				runner:  &fakeRunner{},
			},
		},
	}
	calls := 0
	m.OnUserPromptSubmitted = func(sessionID string) {
		if sessionID == "sess-1" {
			calls++
		}
	}

	if err := m.WriteToSession("sess-1", []byte("\r")); err != nil {
		t.Fatalf("WriteToSession returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnUserPromptSubmitted calls = %d, want 1", calls)
	}
}

func TestManagerNonCodexTerminalEnterDoesNotNotifyUserPrompt(t *testing.T) {
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendClaudeCode)},
				runner:  &fakeRunner{},
			},
		},
	}
	calls := 0
	m.OnUserPromptSubmitted = func(sessionID string) {
		calls++
	}

	if err := m.WriteToSession("sess-1", []byte("\r")); err != nil {
		t.Fatalf("WriteToSession returned error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("OnUserPromptSubmitted calls = %d, want 0", calls)
	}
}

func TestManagerSubmitLineToSessionSeparatesTextAndEnter(t *testing.T) {
	runner := &fakeRunner{}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendClaudeCode)},
				runner:  runner,
			},
		},
	}

	if err := m.SubmitLineToSession("sess-1", "build it", 0); err != nil {
		t.Fatalf("SubmitLineToSession returned error: %v", err)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.writes) != 2 {
		t.Fatalf("writes = %#v, want text and enter writes", runner.writes)
	}
	if string(runner.writes[0]) != "build it" {
		t.Fatalf("first write = %q, want prompt text", string(runner.writes[0]))
	}
	if string(runner.writes[1]) != "\r" {
		t.Fatalf("second write = %q, want carriage return", string(runner.writes[1]))
	}
}

func TestManagerSubmitLineToSessionHonorsTextToEnterDelay(t *testing.T) {
	runner := &fakeRunner{}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendClaudeCode)},
				runner:  runner,
			},
		},
	}

	delay := 25 * time.Millisecond
	start := time.Now()
	if err := m.SubmitLineToSession("sess-1", "build it", delay); err != nil {
		t.Fatalf("SubmitLineToSession returned error: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < delay {
		t.Fatalf("SubmitLineToSession elapsed = %s, want at least %s", elapsed, delay)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.writes) != 2 {
		t.Fatalf("writes = %#v, want text and enter writes", runner.writes)
	}
}

func TestManagerSetSessionModelAndEffortViaCodexCommandChannel(t *testing.T) {
	runner := &fakeCodexCommandRunner{catalog: map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"model": "gpt-new",
				"supportedReasoningEfforts": []map[string]string{
					{"reasoningEffort": "medium"},
					{"reasoningEffort": "high"},
				},
			},
		},
	}}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{
					ID:      "sess-1",
					Backend: string(BackendCodex),
					Model:   "gpt-old",
					Effort:  "medium",
					Harness: "codex/app-server",
				},
				runner: runner,
			},
		},
	}

	updated, err := m.SetSessionModel(context.Background(), "sess-1", "gpt-new", 0)
	if err != nil {
		t.Fatalf("SetSessionModel returned error: %v", err)
	}
	if updated.Model != "gpt-new" || updated.Effort != "medium" {
		t.Fatalf("updated metadata = model %q effort %q", updated.Model, updated.Effort)
	}
	updated, err = m.SetSessionEffort(context.Background(), "sess-1", "high", 0)
	if err != nil {
		t.Fatalf("SetSessionEffort returned error: %v", err)
	}
	if updated.Model != "gpt-new" || updated.Effort != "high" {
		t.Fatalf("updated metadata = model %q effort %q", updated.Model, updated.Effort)
	}
	wantActions := []string{"model/list", "session/model/set", "model/list", "session/effort/set"}
	if strings.Join(runner.actions, ",") != strings.Join(wantActions, ",") {
		t.Fatalf("Codex actions = %#v, want %#v", runner.actions, wantActions)
	}
}

func TestManagerSetSessionModelRejectsUnknownCodexModel(t *testing.T) {
	runner := &fakeCodexCommandRunner{catalog: map[string]interface{}{
		"data": []map[string]string{{"model": "gpt-known"}},
	}}
	m := &Manager{sessions: map[string]*runningSession{
		"sess-1": {
			session: &database.Session{ID: "sess-1", Backend: string(BackendCodex), Model: "gpt-known"},
			runner:  runner,
		},
	}}

	_, err := m.SetSessionModel(context.Background(), "sess-1", "gpt-missing", 0)
	if !errors.Is(err, ErrInvalidSessionSetting) {
		t.Fatalf("SetSessionModel error = %v, want ErrInvalidSessionSetting", err)
	}
	if len(runner.actions) != 1 || runner.actions[0] != "model/list" {
		t.Fatalf("Codex actions = %#v, model change should not be applied", runner.actions)
	}
}

func TestManagerSetClaudeSessionSettingsUsesSlashCommands(t *testing.T) {
	runner := &fakeRunner{}
	m := &Manager{sessions: map[string]*runningSession{
		"sess-1": {
			session: &database.Session{ID: "sess-1", Backend: string(BackendClaudeCode), Model: "default", Effort: "default", Harness: "claude_code"},
			runner:  runner,
		},
	}}

	if _, err := m.SetSessionModel(context.Background(), "sess-1", "fable", 0); err != nil {
		t.Fatalf("SetSessionModel returned error: %v", err)
	}
	updated, err := m.SetSessionEffort(context.Background(), "sess-1", "max", 0)
	if err != nil {
		t.Fatalf("SetSessionEffort returned error: %v", err)
	}
	if updated.Model != "fable" || updated.Effort != "max" {
		t.Fatalf("updated metadata = model %q effort %q", updated.Model, updated.Effort)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	gotWrites := make([]string, len(runner.writes))
	for i, write := range runner.writes {
		gotWrites[i] = string(write)
	}
	wantWrites := []string{"/model fable", "\r", "/effort max", "\r"}
	if strings.Join(gotWrites, "|") != strings.Join(wantWrites, "|") {
		t.Fatalf("writes = %#v, want %#v", gotWrites, wantWrites)
	}
}

func TestManagerStopSessionWaitsForRunnerDone(t *testing.T) {
	done := make(chan struct{})
	runner := &fakeRunner{done: done}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendCodex)},
				runner:  runner,
				cancel:  func() {},
			},
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- m.StopSession(context.Background(), "sess-1")
	}()

	select {
	case err := <-errCh:
		t.Fatalf("StopSession returned before runner done closed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !runner.stoppedCalled() {
		t.Fatal("runner Stop was not called")
	}

	close(done)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("StopSession returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopSession did not return after runner done closed")
	}
}

func TestManagerStopSessionReturnsErrorWhenRunnerDoesNotExit(t *testing.T) {
	runner := &fakeRunner{done: make(chan struct{})}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {
				session: &database.Session{ID: "sess-1", Backend: string(BackendCodex)},
				runner:  runner,
				cancel:  func() {},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	err := m.StopSession(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected StopSession error")
	}
	if !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("StopSession error = %q, want did not stop", err.Error())
	}
}
