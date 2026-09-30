package sessionprompt

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The capture is Claude Code 2.1.285 started with --permission-mode auto in a
// 30x100 terminal, then Shift+Tab pressed four times; frames are separated by
// NUL FRAME NUL.
func TestDetectPermissionModeFromRealShiftTabCycle(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_permission_mode_cycle.bin")
	if err != nil {
		t.Fatal(err)
	}
	frames := bytes.Split(raw, []byte("\x00FRAME\x00"))
	want := []string{PermissionModeAuto, PermissionModeDefault, PermissionModeAcceptEdits, PermissionModePlan, PermissionModeAuto}
	if len(frames) != len(want) {
		t.Fatalf("capture has %d frames, want %d", len(frames), len(want))
	}
	screen := NewScreen(30, 100)
	for i, frame := range frames {
		screen.Feed(frame)
		lines, _ := screen.Lines()
		mode, ok := DetectPermissionMode(lines)
		if !ok || mode != want[i] {
			t.Fatalf("frame %d: mode = %q (ok=%v), want %q; screen:\n%s", i, mode, ok, want[i], strings.Join(lines, "\n"))
		}
	}
}

func TestDetectPermissionModeOnlyReadsBelowThePromptBox(t *testing.T) {
	rule := strings.Repeat("─", 40)
	cases := []struct {
		name  string
		lines []string
		mode  string
		ok    bool
	}{
		{"quoted label above the box", []string{"● I will turn accept edits on for you", rule, "❯ ", rule, "  ? for shortcuts"}, PermissionModeDefault, true},
		{"no footer", []string{"plan mode on", "hello"}, "", false},
		{"bypass", []string{rule, "❯", rule, "⏵⏵ bypass permissions on (shift+tab to cycle)"}, PermissionModeBypass, true},
		{"blank footer", []string{rule, "❯", rule, ""}, "", false},
	}
	for _, tc := range cases {
		mode, ok := DetectPermissionMode(tc.lines)
		if mode != tc.mode || ok != tc.ok {
			t.Errorf("%s: got %q/%v, want %q/%v", tc.name, mode, ok, tc.mode, tc.ok)
		}
	}
}
