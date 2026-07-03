package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"openpoet/internal/database"
)

type fakeRunner struct {
	writeErr error
	writes   [][]byte
}

func (f *fakeRunner) Start(ctx context.Context) error { return nil }
func (f *fakeRunner) Stop() error                     { return nil }
func (f *fakeRunner) Write(data []byte) (int, error) {
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
	ch := make(chan struct{})
	close(ch)
	return ch
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
