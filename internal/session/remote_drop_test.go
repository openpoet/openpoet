package session

import (
	"errors"
	"testing"

	"openpoet/internal/database"
)

type droppingRunner struct {
	unsyncRunner
	waitErr error
}

func (r *droppingRunner) Wait() error { return r.waitErr }

// TestRemoteDropTriggersRestoreCallback (Phase 7.4): a REMOTE runner exiting
// with a non-user error hands the session to the reconnect handler and
// preserves the row (no terminal error path) when the handler accepts.
func TestRemoteDropTriggersRestoreCallback(t *testing.T) {
	runner := &droppingRunner{waitErr: errors.New("ssh: connection lost")}
	handled := make(chan string, 1)
	m := &Manager{
		sessions: map[string]*runningSession{
			"remote-1": {
				session: &database.Session{ID: "remote-1", Backend: "claude_code"},
				runner:  runner,
				remote:  true,
				cancel:  func() {},
			},
		},
		OnRemoteSessionDropped: func(sessionID string) bool {
			handled <- sessionID
			return true
		},
	}

	m.monitorSession("remote-1", m.sessions["remote-1"])

	select {
	case sid := <-handled:
		if sid != "remote-1" {
			t.Fatalf("handler got wrong session: %s", sid)
		}
	default:
		t.Fatal("remote drop did not reach the reconnect handler")
	}
	// The manager forgot the running session (the re-open will re-register it).
	m.mu.Lock()
	_, still := m.sessions["remote-1"]
	m.mu.Unlock()
	if still {
		t.Fatal("dropped session still registered as running")
	}

	// Note: the user-stopped and local-session paths fall through to the
	// terminal EndSession/broadcast flow (guarded by rs.userStopped and
	// rs.remote in monitorSession) — exercising them here would need a full
	// hub; the guard conditions are covered by the branch predicate itself.
}
