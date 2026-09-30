package session

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"openpoet/internal/sessionprompt"
)

var (
	// ErrNoPendingQuestion means nothing interactive is on the session's screen.
	ErrNoPendingQuestion = errors.New("no interactive question is pending")
	// ErrQuestionChanged means the question on screen is not the one answered.
	ErrQuestionChanged = errors.New("the pending question changed")
	// ErrInvalidAnswer means the answer does not fit the question.
	ErrInvalidAnswer = errors.New("invalid answer for the pending question")
)

// Startup states of a session's initial prompt delivery.
const (
	StartupStarting      = "starting"       // process up, initial prompt not delivered yet
	StartupAwaitingInput = "awaiting_input" // initial prompt held back: a question is open
	StartupReady         = "ready"          // initial prompt delivered (or none was needed)
	StartupFailed        = "failed"         // initial prompt could not be delivered
)

// StartupState is the observable progress of a freshly created session.
type StartupState struct {
	State     string
	Detail    string
	UpdatedAt time.Time
}

// interactionRegistry holds the per-session terminal screens and startup
// states. The zero value is ready to use so Managers built as literals work.
type interactionRegistry struct {
	mu        sync.Mutex
	screens   map[string]*sessionprompt.Screen
	startup   map[string]StartupState
	firstSeen map[string]map[string]time.Time // sessionID -> questionID -> first detection
	// permissionModes is each session's last observed permission mode.
	permissionModes map[string]sessionprompt.PermissionModeState
}

const (
	terminalKeyDelay  = 120 * time.Millisecond
	terminalTextDelay = 500 * time.Millisecond
)

func (r *interactionRegistry) lazyInit() {
	if r.screens == nil {
		r.screens = make(map[string]*sessionprompt.Screen)
		r.startup = make(map[string]StartupState)
		r.firstSeen = make(map[string]map[string]time.Time)
		r.permissionModes = make(map[string]sessionprompt.PermissionModeState)
	}
}

// resetScreen gives a (re)started session a fresh emulated terminal of the
// runner's initial PTY size.
func (m *Manager) resetScreen(sessionID string, rows, cols int) {
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	m.interaction.screens[sessionID] = sessionprompt.NewScreen(rows, cols)
	delete(m.interaction.firstSeen, sessionID)
	delete(m.interaction.permissionModes, sessionID)
}

func (m *Manager) screen(sessionID string) *sessionprompt.Screen {
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	return m.interaction.screens[sessionID]
}

func (m *Manager) feedScreen(sessionID string, data []byte) {
	if screen := m.screen(sessionID); screen != nil {
		screen.Feed(data)
	}
}

func (m *Manager) resizeScreen(sessionID string, rows, cols uint16) {
	if screen := m.screen(sessionID); screen != nil {
		screen.Resize(int(rows), int(cols))
	}
}

// forgetInteraction drops a finished session's screen and startup state.
func (m *Manager) forgetInteraction(sessionID string) {
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	delete(m.interaction.screens, sessionID)
	delete(m.interaction.startup, sessionID)
	delete(m.interaction.firstSeen, sessionID)
	delete(m.interaction.permissionModes, sessionID)
}

// SetStartupState records the initial-prompt progress of a session.
func (m *Manager) SetStartupState(sessionID, state, detail string) {
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	m.interaction.startup[sessionID] = StartupState{State: state, Detail: detail, UpdatedAt: time.Now()}
}

// SessionStartupState reports the initial-prompt progress ("" when the
// session was not started by this process or has ended).
func (m *Manager) SessionStartupState(sessionID string) (state, detail string) {
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	current := m.interaction.startup[sessionID]
	return current.State, current.Detail
}

// TerminalQuestion returns the interactive question currently painted on the
// session's terminal, or nil.
func (m *Manager) TerminalQuestion(sessionID string) *sessionprompt.Question {
	screen := m.screen(sessionID)
	if screen == nil {
		return nil
	}
	lines, cursor := screen.Lines()
	q := sessionprompt.Detect(lines, cursor)
	if q == nil {
		return nil
	}
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	seen := m.interaction.firstSeen[sessionID]
	if seen == nil {
		seen = make(map[string]time.Time)
		m.interaction.firstSeen[sessionID] = seen
	}
	first, ok := seen[q.ID]
	if !ok {
		first = time.Now().UTC()
		if len(seen) > 64 {
			clear(seen)
		}
		seen[q.ID] = first
	}
	q.DetectedAt = first
	return q
}

// ScreenText returns the session's current screen as plain text (blank edges
// trimmed), for diagnostics such as an errored session's last output.
func (m *Manager) ScreenText(sessionID string) string {
	screen := m.screen(sessionID)
	if screen == nil {
		return ""
	}
	lines, _ := screen.Lines()
	return strings.Trim(strings.Join(lines, "\n"), "\n ")
}

// AnswerTerminalQuestion answers the question painted on the session's
// terminal by pressing the keys a human would. questionID must match the
// question currently on screen so a stale answer can never land on a newer
// dialog.
func (m *Manager) AnswerTerminalQuestion(sessionID, questionID string, answer sessionprompt.Answer) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotRunning
	}
	rs.inputMu.Lock()
	defer rs.inputMu.Unlock()

	q := m.TerminalQuestion(sessionID)
	if q == nil {
		return ErrNoPendingQuestion
	}
	if q.ID != questionID {
		return fmt.Errorf("%w: now %s", ErrQuestionChanged, q.ID)
	}
	write := func(data []byte) error { return m.writeToRunnerLocked(sessionID, rs, data) }

	if q.Kind == sessionprompt.KindConfirm {
		text := strings.TrimSpace(answer.Text)
		if option, ok := q.OptionByIndex(answer.Option); ok && text == "" {
			text = option.Label
		}
		if text == "" {
			return fmt.Errorf("%w: send option 1 (y), 2 (n) or text", ErrInvalidAnswer)
		}
		if err := write([]byte(text)); err != nil {
			return err
		}
		time.Sleep(terminalTextDelay)
		return write([]byte("\r"))
	}

	if _, ok := q.OptionByIndex(answer.Option); !ok {
		return fmt.Errorf("%w: option must be one of 1..%d", ErrInvalidAnswer, len(q.Options))
	}
	text := strings.TrimSpace(answer.Text)
	if text != "" && !q.AcceptsText {
		return fmt.Errorf("%w: this question does not accept text", ErrInvalidAnswer)
	}
	for i, key := range sessionprompt.NavigationKeys(q.SelectedIndex, answer.Option) {
		if i > 0 {
			time.Sleep(terminalKeyDelay)
		}
		if err := write(key); err != nil {
			return err
		}
	}
	if text == "" {
		return nil
	}
	time.Sleep(terminalTextDelay)
	if err := write([]byte(text)); err != nil {
		return err
	}
	time.Sleep(terminalTextDelay)
	return write([]byte("\r"))
}

// terminalErrorReason describes an errored exit, naming the dialog that was
// on screen when the process died (the usual culprit for a startup exit).
func (m *Manager) terminalErrorReason(sessionID string, exitErr error) string {
	reason := "agent process exited with an error"
	if exitErr != nil {
		reason = "agent process exited: " + exitErr.Error()
	}
	if q := m.TerminalQuestion(sessionID); q != nil {
		selected := ""
		if option, ok := q.OptionByIndex(q.SelectedIndex); ok {
			selected = option.Label
		}
		firstLine, _, _ := strings.Cut(q.Text, "\n")
		reason += fmt.Sprintf(" while an interactive %s question was on screen (%q, selected %q)",
			q.Kind, sessionprompt.Truncate(firstLine, 160), selected)
	}
	return reason
}

const maxErrorOutputBytes = 4 << 10

var outputControlPattern = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_]`)

// errorOutputTail is what an errored session last showed: its emulated screen
// when there is one, otherwise the escape-stripped tail of the raw output.
func (m *Manager) errorOutputTail(sessionID string, raw []byte) string {
	text := m.ScreenText(sessionID)
	if strings.TrimSpace(text) == "" {
		if len(raw) > 4*maxErrorOutputBytes {
			raw = raw[len(raw)-4*maxErrorOutputBytes:]
		}
		text = strings.TrimSpace(strings.ReplaceAll(outputControlPattern.ReplaceAllString(string(raw), ""), "\r", ""))
	}
	if len(text) > maxErrorOutputBytes {
		text = strings.ToValidUTF8(text[len(text)-maxErrorOutputBytes:], "")
	}
	return text
}

// failSession ends a session that could not start, recording why, and
// returns the cause so call sites can `return m.failSession(...)`.
func (m *Manager) failSession(ctx context.Context, sessionID string, cause error) error {
	if m.db != nil {
		_ = m.db.EndSession(ctx, sessionID, "error")
		_ = m.db.SetSessionErrorDetails(ctx, sessionID, sessionprompt.Truncate(cause.Error(), 2000), "")
	}
	m.forgetInteraction(sessionID)
	return cause
}
