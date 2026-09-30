package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"openpoet/internal/sessionprompt"
)

var (
	// ErrSessionAwaitingInput means a question is open on the session's
	// terminal; keys typed now would answer it instead of the prompt.
	ErrSessionAwaitingInput = errors.New("session is awaiting input")
	// ErrPermissionModeUnconfirmed means the mode indicator could not be read
	// after a key press, so the resulting mode is unknown.
	ErrPermissionModeUnconfirmed = errors.New("permission mode change could not be confirmed")
)

// shiftTab is the key Claude Code binds to chat:cycleMode.
const shiftTab = "\x1b[Z"

// Claude Code's Shift+Tab cycle has at most five stops (default, acceptEdits,
// plan, bypassPermissions, auto); one more press proves a mode is not in it.
const maxPermissionModePresses = 6

var (
	permissionModeSettleTimeout = 3 * time.Second
	permissionModePollInterval  = 50 * time.Millisecond
)

// PermissionModeChange is the result of SetSessionPermissionMode. To is read
// back from the session's screen after the last key press, never assumed.
type PermissionModeChange struct {
	From    string
	To      string
	Presses int
}

// ValidateSettablePermissionMode canonicalizes a mode that may be set on a
// running session. bypassPermissions and dontAsk are refused: they disable
// prompts altogether and are only granted when a session is created.
func ValidateSettablePermissionMode(mode string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	normalized = strings.NewReplacer("_", "", "-", "", " ", "").Replace(normalized)
	switch normalized {
	case "auto":
		return sessionprompt.PermissionModeAuto, nil
	case "acceptedits":
		return sessionprompt.PermissionModeAcceptEdits, nil
	case "default", "manual":
		return sessionprompt.PermissionModeDefault, nil
	case "plan":
		return sessionprompt.PermissionModePlan, nil
	case "bypasspermissions", "dontask":
		return "", fmt.Errorf("%w: permission mode %q cannot be set on a running session", ErrInvalidSessionSetting, strings.TrimSpace(mode))
	}
	return "", fmt.Errorf("%w: permission mode must be one of auto, acceptEdits, default, or plan", ErrInvalidSessionSetting)
}

// SetSessionPermissionMode moves a running Claude Code session to mode with
// the TUI's own Shift+Tab cycle, reading the mode indicator after every press.
// It stops as soon as the target is on screen, when the cycle comes back to a
// mode already seen (the target is not offered, e.g. auto mode unavailable),
// or when a dialog opens (e.g. the first-use auto mode opt-in).
func (m *Manager) SetSessionPermissionMode(ctx context.Context, sessionID, mode string) (PermissionModeChange, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	target, err := ValidateSettablePermissionMode(mode)
	if err != nil {
		return PermissionModeChange{}, err
	}
	rs, err := m.runningSession(sessionID)
	if err != nil {
		return PermissionModeChange{}, err
	}
	if BackendType(rs.session.Backend) != BackendClaudeCode {
		return PermissionModeChange{}, fmt.Errorf("%w: backend %q cannot change permission mode in an active session", ErrSessionSettingUnsupported, rs.session.Backend)
	}
	screen := m.screen(sessionID)
	if screen == nil {
		return PermissionModeChange{}, fmt.Errorf("%w: the session's terminal is not being tracked", ErrSessionSettingUnsupported)
	}
	if m.TerminalQuestion(sessionID) != nil {
		return PermissionModeChange{}, fmt.Errorf("%w: answer the open question before changing the permission mode", ErrSessionAwaitingInput)
	}

	current, known := m.screenPermissionMode(sessionID, screen)
	change := PermissionModeChange{From: current, To: current}
	if !known {
		change.From = "unknown"
	}
	if known && current == target {
		return change, nil
	}
	seen := map[string]bool{}
	if known {
		seen[current] = true
	}
	for change.Presses < maxPermissionModePresses {
		version := screen.Version()
		if err := m.pressPermissionModeKey(sessionID, rs); err != nil {
			return change, err
		}
		change.Presses++
		next, err := m.awaitPermissionModeRepaint(ctx, sessionID, screen, version, current, known)
		if err != nil {
			change.To = "unknown"
			return change, err
		}
		current, known = next, true
		change.To = current
		if current == target {
			return change, nil
		}
		if seen[current] {
			break
		}
		seen[current] = true
	}
	return change, fmt.Errorf("%w: permission mode %q is not offered by this session's Shift+Tab cycle; the session is now in %q", ErrSessionSettingUnsupported, target, current)
}

func (m *Manager) pressPermissionModeKey(sessionID string, rs *runningSession) error {
	rs.inputMu.Lock()
	defer rs.inputMu.Unlock()
	return m.writeToRunnerLocked(sessionID, rs, []byte(shiftTab))
}

// awaitPermissionModeRepaint waits for the screen to show a mode other than
// previous. A repaint that keeps showing previous is accepted only at the
// deadline, so a slow second repaint is not mistaken for "no change".
func (m *Manager) awaitPermissionModeRepaint(ctx context.Context, sessionID string, screen *sessionprompt.Screen, since uint64, previous string, previousKnown bool) (string, error) {
	deadline := time.NewTimer(permissionModeSettleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(permissionModePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			if mode, ok := m.screenPermissionMode(sessionID, screen); ok && screen.Version() != since {
				if previousKnown && mode == previous {
					return "", fmt.Errorf("%w: Shift+Tab did not change the mode (still %q)", ErrPermissionModeUnconfirmed, mode)
				}
				return mode, nil
			}
			return "", fmt.Errorf("%w: no mode indicator was painted within %s", ErrPermissionModeUnconfirmed, permissionModeSettleTimeout)
		case <-ticker.C:
			if screen.Version() == since {
				continue
			}
			if m.TerminalQuestion(sessionID) != nil {
				return "", fmt.Errorf("%w: a question opened after Shift+Tab; answer it to finish the mode change", ErrSessionAwaitingInput)
			}
			if mode, ok := m.screenPermissionMode(sessionID, screen); ok && (!previousKnown || mode != previous) {
				return mode, nil
			}
		}
	}
}

// screenPermissionMode reads the mode indicator and records it.
func (m *Manager) screenPermissionMode(sessionID string, screen *sessionprompt.Screen) (string, bool) {
	lines, _ := screen.Lines()
	mode, ok := sessionprompt.DetectPermissionMode(lines)
	if ok {
		m.recordPermissionMode(sessionID, mode, sessionprompt.PermissionModeSourceScreen)
	}
	return mode, ok
}

// RecordHookPermissionMode keeps the permission_mode a Claude Code hook event
// reported, the fallback when the mode indicator is not on screen.
func (m *Manager) RecordHookPermissionMode(sessionID, mode string) {
	mode = strings.TrimSpace(mode)
	if mode == "" || len(mode) > 64 {
		return
	}
	m.recordPermissionMode(sessionID, mode, sessionprompt.PermissionModeSourceHook)
}

func (m *Manager) recordPermissionMode(sessionID, mode, source string) {
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	m.interaction.permissionModes[sessionID] = sessionprompt.PermissionModeState{Mode: mode, Source: source, ObservedAt: time.Now().UTC()}
}

// SessionPermissionMode reports a running session's permission mode: the
// indicator on its screen when one is painted, otherwise the last mode a hook
// event reported. ok is false when neither has been seen.
func (m *Manager) SessionPermissionMode(sessionID string) (sessionprompt.PermissionModeState, bool) {
	rs, err := m.runningSession(sessionID)
	if err != nil || BackendType(rs.session.Backend) != BackendClaudeCode {
		return sessionprompt.PermissionModeState{}, false
	}
	if screen := m.screen(sessionID); screen != nil {
		m.screenPermissionMode(sessionID, screen)
	}
	m.interaction.mu.Lock()
	defer m.interaction.mu.Unlock()
	m.interaction.lazyInit()
	state, ok := m.interaction.permissionModes[sessionID]
	return state, ok
}
