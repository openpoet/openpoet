package session

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/websocket"

	"golang.org/x/crypto/ssh"
)

// exitStatus runs a real process that exits with code and returns its Wait
// error, the same *exec.ExitError a codex app-server crash produces.
func exitStatus(t *testing.T, code string) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+code).Run()
	if err == nil {
		t.Fatalf("exit %s returned no error", code)
	}
	return err
}

func newExitTestCodexRunner(t *testing.T) *CodexRunner {
	t.Helper()
	r, err := newCodexRunner(t.TempDir(), nil, func([]byte) {}, &SessionConfig{}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCodexRunnerWaitReportsUnexpectedExit(t *testing.T) {
	cases := []struct {
		name    string
		waitErr error
		want    string
	}{
		{"non-zero exit code", exitStatus(t, "3"), "exit status 3"},
		{"ssh channel dropped", &ssh.ExitMissingError{}, "connection lost"},
		{"zero exit nobody asked for", nil, "exit code 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newExitTestCodexRunner(t)
			r.finishProcess("codex app-server", tc.waitErr)
			err := r.Wait()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "exited unexpectedly") {
				t.Fatalf("Wait() = %v, want an unexpected-exit error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestCodexRunnerWaitIsCleanAfterStop(t *testing.T) {
	r := newExitTestCodexRunner(t)
	_ = r.Stop()
	r.finishProcess("codex app-server", exitStatus(t, "130"))
	if err := r.Wait(); err != nil {
		t.Fatalf("Wait() after Stop = %v, want nil", err)
	}
}

func newExitTestManager(t *testing.T) (*Manager, *database.DB) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "codex-exit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewManager(db, websocket.NewHub(), "localhost:0"), db
}

func registerExitTestSession(t *testing.T, m *Manager, db *database.DB, id string, runner Runner, remote bool) *runningSession {
	t.Helper()
	ctx := context.Background()
	project := &database.Project{Name: "p-" + id, Path: "/home/dev/p", Type: "local", Backend: string(BackendCodex)}
	if remote {
		project.Type = "remote"
	}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess := &database.Session{ID: id, ProjectID: project.ID, Status: "running", StartTime: time.Now(), Backend: string(BackendCodex)}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	rs := &runningSession{session: sess, runner: runner, remote: remote, cancel: func() {}}
	m.mu.Lock()
	m.sessions[id] = rs
	m.mu.Unlock()
	return rs
}

// A remote codex whose app-server dies (non-zero exit, or the SSH channel
// closing under it) is an error that reaches the reconnect handler, instead
// of the silent "completed" the session used to end with.
func TestRemoteCodexUnexpectedExitTriggersReconnect(t *testing.T) {
	cases := map[string]error{
		"exit code 1":         exitStatus(t, "1"),
		"ssh channel dropped": &ssh.ExitMissingError{},
		"exit code 0":         nil,
	}
	for name, waitErr := range cases {
		t.Run(name, func(t *testing.T) {
			m, db := newExitTestManager(t)
			handled := make(chan string, 1)
			m.OnRemoteSessionDropped = func(id string) bool { handled <- id; return true }

			runner := &RemoteCodexRunner{inner: newExitTestCodexRunner(t)}
			rs := registerExitTestSession(t, m, db, "remote-codex-"+strings.ReplaceAll(name, " ", "-"), runner, true)
			go runner.inner.finishProcess("remote codex app-server", waitErr)
			m.monitorSession(rs.session.ID, rs)

			select {
			case <-handled:
			default:
				t.Fatal("unexpected exit did not reach the reconnect handler")
			}
			got, err := db.GetSession(context.Background(), rs.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "running" {
				t.Fatalf("status = %q, want running (row preserved for the reconnect)", got.Status)
			}
		})
	}
}

// When nobody reconnects (a local codex, or the retry budget is spent) the
// session ends "error" with the exit reason recorded.
func TestCodexUnexpectedExitRecordsErrorReason(t *testing.T) {
	m, db := newExitTestManager(t)
	runner := newExitTestCodexRunner(t)
	rs := registerExitTestSession(t, m, db, "local-codex-crash", runner, false)
	go runner.finishProcess("codex app-server", exitStatus(t, "2"))
	m.monitorSession(rs.session.ID, rs)

	got, err := db.GetSession(context.Background(), rs.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" {
		t.Fatalf("status = %q, want error", got.Status)
	}
	if !strings.Contains(got.ErrorReason, "exit status 2") {
		t.Fatalf("error_reason = %q, want the exit status", got.ErrorReason)
	}
}

// A stop the user asked for stays a clean end — no error, no reconnect.
func TestRemoteCodexUserStopStaysStopped(t *testing.T) {
	m, db := newExitTestManager(t)
	m.OnRemoteSessionDropped = func(string) bool {
		t.Error("user stop reached the reconnect handler")
		return true
	}
	runner := &RemoteCodexRunner{inner: newExitTestCodexRunner(t)}
	rs := registerExitTestSession(t, m, db, "remote-codex-stop", runner, true)
	rs.userStopped = true
	_ = runner.Stop()
	go runner.inner.finishProcess("remote codex app-server", &ssh.ExitMissingError{})
	m.monitorSession(rs.session.ID, rs)

	got, err := db.GetSession(context.Background(), rs.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("status = %q, want stopped", got.Status)
	}
}
