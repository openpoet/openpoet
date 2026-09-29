package session

import (
	"errors"
	"os"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
)

func trustDialogManager(t *testing.T) (*Manager, *unsyncRunner) {
	t.Helper()
	raw, err := os.ReadFile("../sessionprompt/testdata/claude_trust_dialog.bin")
	if err != nil {
		t.Fatal(err)
	}
	runner := &unsyncRunner{}
	m := &Manager{sessions: map[string]*runningSession{
		"sess-1": {session: &database.Session{ID: "sess-1", Backend: "claude_code"}, runner: runner},
	}}
	m.resetScreen("sess-1", 24, 80)
	// Only the initial paint: "No, exit" is focused.
	end := strings.Index(string(raw), "\x1b[1D\x1b[4B\r")
	m.checkForNotificationTriggers("sess-1", raw[:end])
	return m, runner
}

func TestAnswerTerminalQuestionNavigatesToChosenOption(t *testing.T) {
	m, runner := trustDialogManager(t)
	q := m.TerminalQuestion("sess-1")
	if q == nil || q.Kind != sessionprompt.KindWorkspaceTrust {
		t.Fatalf("question = %+v", q)
	}
	if err := m.AnswerTerminalQuestion("sess-1", q.ID, sessionprompt.Answer{Option: 2}); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(runner.writes))
	for _, write := range runner.writes {
		got = append(got, string(write))
	}
	if strings.Join(got, "|") != "\x1b[B|\r" {
		t.Fatalf("keys = %q, want Down then Enter", got)
	}
}

func TestAnswerTerminalQuestionRejectsStaleIDAndBadOption(t *testing.T) {
	m, runner := trustDialogManager(t)
	q := m.TerminalQuestion("sess-1")
	if err := m.AnswerTerminalQuestion("sess-1", "t_stale", sessionprompt.Answer{Option: 2}); !errors.Is(err, ErrQuestionChanged) {
		t.Fatalf("stale id err = %v", err)
	}
	if err := m.AnswerTerminalQuestion("sess-1", q.ID, sessionprompt.Answer{Option: 7}); !errors.Is(err, ErrInvalidAnswer) {
		t.Fatalf("bad option err = %v", err)
	}
	if err := m.AnswerTerminalQuestion("sess-1", q.ID, sessionprompt.Answer{Option: 2, Text: "hi"}); !errors.Is(err, ErrInvalidAnswer) {
		t.Fatalf("text on a text-less question err = %v", err)
	}
	if len(runner.writes) != 0 {
		t.Fatalf("rejected answers wrote keys: %q", runner.writes)
	}
}

func TestTerminalErrorReasonNamesTheDialogOnScreen(t *testing.T) {
	m, _ := trustDialogManager(t)
	reason := m.terminalErrorReason("sess-1", errors.New("exit status 1"))
	for _, want := range []string{"exit status 1", "workspace_trust", `selected "No, exit"`} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	if tail := m.errorOutputTail("sess-1", nil); !strings.Contains(tail, "Yes, I trust this folder") {
		t.Fatalf("last output = %q", tail)
	}
}

func TestStartupStateLifecycle(t *testing.T) {
	m := &Manager{}
	if state, _ := m.SessionStartupState("x"); state != "" {
		t.Fatalf("untracked state = %q", state)
	}
	m.SetStartupState("x", StartupAwaitingInput, "workspace_trust t_1")
	if state, detail := m.SessionStartupState("x"); state != StartupAwaitingInput || detail != "workspace_trust t_1" {
		t.Fatalf("state = %q %q", state, detail)
	}
	m.forgetInteraction("x")
	if state, _ := m.SessionStartupState("x"); state != "" {
		t.Fatalf("state after forget = %q", state)
	}
}
