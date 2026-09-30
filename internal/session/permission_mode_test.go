package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
)

// cyclingRunner stands in for Claude Code: every Shift+Tab repaints the next
// frame of the real capture (auto -> default -> acceptEdits -> plan -> auto).
type cyclingRunner struct {
	fakeRunner
	mu      sync.Mutex
	manager *Manager
	frames  [][]byte
	next    int
	loop    int // frame index Shift+Tab wraps back to after the last frame
}

func (r *cyclingRunner) Write(data []byte) (int, error) {
	n, err := r.fakeRunner.Write(data)
	if err != nil || string(data) != shiftTab {
		return n, err
	}
	r.mu.Lock()
	if r.next >= len(r.frames) {
		r.next = r.loop
	}
	frame := r.frames[r.next]
	r.next++
	r.mu.Unlock()
	go r.manager.feedScreen("sess-1", frame)
	return n, nil
}

func (r *cyclingRunner) presses() int {
	r.fakeRunner.mu.Lock()
	defer r.fakeRunner.mu.Unlock()
	count := 0
	for _, w := range r.writes {
		if string(w) == shiftTab {
			count++
		}
	}
	return count
}

func permissionModeManager(t *testing.T, backend string) (*Manager, *cyclingRunner) {
	t.Helper()
	raw, err := os.ReadFile("../sessionprompt/testdata/claude_permission_mode_cycle.bin")
	if err != nil {
		t.Fatal(err)
	}
	frames := bytes.Split(raw, []byte("\x00FRAME\x00"))
	runner := &cyclingRunner{frames: frames[1:], next: 0, loop: 0}
	m := &Manager{sessions: map[string]*runningSession{
		"sess-1": {session: &database.Session{ID: "sess-1", Backend: backend}, runner: runner},
	}}
	runner.manager = m
	m.resetScreen("sess-1", 30, 100)
	m.feedScreen("sess-1", frames[0]) // auto mode on
	previousTimeout := permissionModeSettleTimeout
	permissionModeSettleTimeout = 500 * time.Millisecond
	t.Cleanup(func() { permissionModeSettleTimeout = previousTimeout })
	return m, runner
}

func TestSetSessionPermissionModeCyclesUntilTheScreenShowsTheTarget(t *testing.T) {
	m, runner := permissionModeManager(t, "claude_code")
	change, err := m.SetSessionPermissionMode(context.Background(), "sess-1", "accept_edits")
	if err != nil {
		t.Fatal(err)
	}
	if change.From != "auto" || change.To != "acceptEdits" || change.Presses != 2 || runner.presses() != 2 {
		t.Fatalf("change = %+v, presses = %d", change, runner.presses())
	}
	state, ok := m.SessionPermissionMode("sess-1")
	if !ok || state.Mode != "acceptEdits" || state.Source != sessionprompt.PermissionModeSourceScreen {
		t.Fatalf("state = %+v ok=%v", state, ok)
	}
}

func TestSetSessionPermissionModeIsANoOpWhenAlreadyInTheMode(t *testing.T) {
	m, runner := permissionModeManager(t, "claude_code")
	change, err := m.SetSessionPermissionMode(context.Background(), "sess-1", "auto")
	if err != nil || change.From != "auto" || change.To != "auto" || change.Presses != 0 || runner.presses() != 0 {
		t.Fatalf("change = %+v err = %v presses = %d", change, err, runner.presses())
	}
}

func TestSetSessionPermissionModeStopsWhenTheCycleDoesNotOfferTheTarget(t *testing.T) {
	m, runner := permissionModeManager(t, "claude_code")
	// Drop the plan and auto frames: the cycle is default <-> acceptEdits.
	runner.frames = runner.frames[:2]
	_, err := m.SetSessionPermissionMode(context.Background(), "sess-1", "plan")
	if !errors.Is(err, ErrSessionSettingUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if runner.presses() > maxPermissionModePresses {
		t.Fatalf("pressed %d times", runner.presses())
	}
}

func TestSetSessionPermissionModeFailsWhenNothingRepaints(t *testing.T) {
	m, runner := permissionModeManager(t, "claude_code")
	runner.frames = [][]byte{nil}
	change, err := m.SetSessionPermissionMode(context.Background(), "sess-1", "plan")
	if !errors.Is(err, ErrPermissionModeUnconfirmed) || change.To != "unknown" || change.Presses != 1 {
		t.Fatalf("change = %+v err = %v", change, err)
	}
}

func TestSetSessionPermissionModeRefusesUnsafeModesAndOtherBackends(t *testing.T) {
	m, runner := permissionModeManager(t, "claude_code")
	for _, mode := range []string{"bypassPermissions", "dontAsk", "yolo", ""} {
		if _, err := m.SetSessionPermissionMode(context.Background(), "sess-1", mode); !errors.Is(err, ErrInvalidSessionSetting) {
			t.Fatalf("mode %q: err = %v", mode, err)
		}
	}
	if runner.presses() != 0 {
		t.Fatalf("pressed %d times", runner.presses())
	}
	codex, _ := permissionModeManager(t, "codex")
	if _, err := codex.SetSessionPermissionMode(context.Background(), "sess-1", "plan"); !errors.Is(err, ErrSessionSettingUnsupported) {
		t.Fatalf("codex err = %v", err)
	}
	if _, err := m.SetSessionPermissionMode(context.Background(), "missing", "plan"); !errors.Is(err, ErrSessionNotRunning) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestSessionPermissionModeFallsBackToTheHookReport(t *testing.T) {
	m := &Manager{sessions: map[string]*runningSession{
		"sess-1": {session: &database.Session{ID: "sess-1", Backend: "claude_code"}, runner: &fakeRunner{}},
	}}
	m.resetScreen("sess-1", 24, 80)
	if _, ok := m.SessionPermissionMode("sess-1"); ok {
		t.Fatal("mode known before any signal")
	}
	m.RecordHookPermissionMode("sess-1", "acceptEdits")
	state, ok := m.SessionPermissionMode("sess-1")
	if !ok || state.Mode != "acceptEdits" || state.Source != sessionprompt.PermissionModeSourceHook {
		t.Fatalf("state = %+v ok=%v", state, ok)
	}
}
