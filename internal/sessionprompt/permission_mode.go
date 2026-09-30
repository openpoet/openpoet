package sessionprompt

import (
	"strings"
	"time"
)

// Claude Code permission modes, spelled as the CLI's --permission-mode flag.
const (
	PermissionModeDefault     = "default" // shown as "manual mode on" since 2.1.28x
	PermissionModeAcceptEdits = "acceptEdits"
	PermissionModePlan        = "plan"
	PermissionModeAuto        = "auto"
	PermissionModeBypass      = "bypassPermissions"
	PermissionModeDontAsk     = "dontAsk"
)

// Where a PermissionModeState was read from.
const (
	PermissionModeSourceScreen = "screen" // the mode indicator painted under the prompt box
	PermissionModeSourceHook   = "hook"   // permission_mode of the session's latest hook event
)

// PermissionModeState is a session's permission mode as last observed.
type PermissionModeState struct {
	Mode       string    `json:"mode"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}

// footerModeLabels maps the Claude Code mode indicator ("⏵⏵ accept edits on
// (shift+tab to cycle)") to its mode. Ink repaints only the cells that changed,
// but the screen emulator keeps the untouched ones, so whole labels survive.
var footerModeLabels = []struct{ label, mode string }{
	{"accept edits on", PermissionModeAcceptEdits},
	{"plan mode on", PermissionModePlan},
	{"auto mode on", PermissionModeAuto},
	{"bypass permissions on", PermissionModeBypass},
	{"manual mode on", PermissionModeDefault},
	{"don't ask on", PermissionModeDontAsk},
	{"dont ask mode on", PermissionModeDontAsk},
}

// DetectPermissionMode reads the permission mode from the footer Claude Code
// paints under its prompt box. Only lines after the box's last horizontal rule
// are read, so conversation text quoting a label never matches. Older builds
// print nothing for the default mode, only "? for shortcuts".
func DetectPermissionMode(lines []string) (string, bool) {
	rule := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isHorizontalRule(lines[i]) {
			rule = i
			break
		}
	}
	if rule < 0 {
		return "", false
	}
	shortcutsHint := false
	for _, line := range lines[rule+1:] {
		lower := strings.ToLower(line)
		for _, candidate := range footerModeLabels {
			if strings.Contains(lower, candidate.label) {
				return candidate.mode, true
			}
		}
		if strings.Contains(lower, "? for shortcuts") {
			shortcutsHint = true
		}
	}
	if shortcutsHint {
		return PermissionModeDefault, true
	}
	return "", false
}

func isHorizontalRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len([]rune(trimmed)) < 10 {
		return false
	}
	return strings.Trim(trimmed, "─━") == ""
}
