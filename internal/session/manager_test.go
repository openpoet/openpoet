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
	err error
}

func (f *fakeCodexCommandRunner) HandleCodexCommand(ctx context.Context, data json.RawMessage) (interface{}, error) {
	if f.err != nil {
		return nil, f.err
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
