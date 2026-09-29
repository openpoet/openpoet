package sessionprompt

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func screenOf(rows, cols int, chunks ...string) *Screen {
	s := NewScreen(rows, cols)
	for _, chunk := range chunks {
		s.Feed([]byte(chunk))
	}
	return s
}

func detect(s *Screen) *Question {
	lines, cursor := s.Lines()
	return Detect(lines, cursor)
}

// The capture is Claude Code 2.1.285 opening in an untrusted folder, then
// repainting after one Down arrow.
func TestDetectClaudeWorkspaceTrustDialogFromRealCapture(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_trust_dialog.bin")
	if err != nil {
		t.Fatal(err)
	}
	split := bytes.Index(raw, []byte("\x1b[1D\x1b[4B\r"))
	if split < 0 {
		t.Fatal("capture has no Down-arrow repaint")
	}
	screen := NewScreen(24, 80)
	screen.Feed(raw[:split])
	q := detect(screen)
	if q == nil {
		lines, _ := screen.Lines()
		t.Fatalf("trust dialog not detected; screen:\n%s", strings.Join(lines, "\n"))
	}
	if q.Kind != KindWorkspaceTrust || q.Source != SourceTerminal {
		t.Fatalf("kind/source = %s/%s", q.Kind, q.Source)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "No, exit" || q.Options[1].Label != "Yes, I trust this folder" {
		t.Fatalf("options = %+v", q.Options)
	}
	if !q.Options[0].EndsSession || q.Options[1].EndsSession {
		t.Fatalf("ends_session flags wrong: %+v", q.Options)
	}
	if q.SelectedIndex != 1 {
		t.Fatalf("selected = %d, want 1 (No, exit is focused)", q.SelectedIndex)
	}
	if !strings.Contains(q.Text, "Is this a project you created or one you trust?") {
		t.Fatalf("text = %q", q.Text)
	}
	firstID := q.ID

	screen.Feed(raw[split:])
	q = detect(screen)
	if q == nil || q.SelectedIndex != 2 {
		t.Fatalf("after Down: %+v", q)
	}
	if q.ID != firstID {
		t.Fatalf("question id changed on repaint: %s -> %s", firstID, q.ID)
	}
}

func TestDetectNumberedToolPermissionPrompt(t *testing.T) {
	screen := screenOf(24, 80,
		"\x1b[?1049h\x1b[H",
		"────────────────────────────────────────\r\n",
		" Bash command\r\n\r\n",
		"   rm -rf build\r\n",
		"   Remove build output\r\n\r\n",
		" Do you want to proceed?\r\n",
		" \x1b[36m❯\x1b[39m 1. Yes\r\n",
		"   2. Yes, and don't ask again for rm commands in /repo\r\n",
		"   3. No, and tell Claude what to do differently (esc)\r\n",
	)
	q := detect(screen)
	if q == nil {
		t.Fatal("permission prompt not detected")
	}
	if q.Kind != KindToolPermission || len(q.Options) != 3 || q.Options[0].Label != "Yes" {
		t.Fatalf("question = %+v", q)
	}
	if !q.Options[0].GrantsPermission || !q.Options[1].GrantsPermission || q.Options[2].GrantsPermission {
		t.Fatalf("grants flags = %+v", q.Options)
	}
	if !q.AcceptsText || !strings.Contains(q.Text, "rm -rf build") {
		t.Fatalf("accepts_text/text = %v %q", q.AcceptsText, q.Text)
	}
}

func TestDetectMenuWithDescriptionsAndWrappedCursorMoves(t *testing.T) {
	screen := screenOf(24, 80,
		"Which database should we use?\r\n\r\n",
		"\x1b[2G❯\x1b[4GPostgres\r\n",
		"\x1b[6GRelational, battle tested\r\n",
		"\x1b[4GSQLite\r\n",
		"\x1b[6GEmbedded\r\n\r\n",
		"\x1b[2GEnter to select · ↑/↓ to navigate · Esc to cancel",
	)
	q := detect(screen)
	if q == nil || q.Kind != KindSelection || len(q.Options) != 2 {
		t.Fatalf("question = %+v", q)
	}
	if q.Options[1].Label != "SQLite" || q.Options[0].Description != "Relational, battle tested" {
		t.Fatalf("options = %+v", q.Options)
	}
}

func TestDetectTypedConfirmOnCursorLine(t *testing.T) {
	screen := screenOf(24, 80, "Overwrite existing config? (y/n) ")
	q := detect(screen)
	if q == nil || q.Kind != KindConfirm || !q.AcceptsText {
		t.Fatalf("question = %+v", q)
	}
}

func TestDetectIgnoresInputBoxAndPlainLists(t *testing.T) {
	cases := map[string]string{
		"input prompt":         "╭──────────────────────────╮\r\n│ ❯ fix the build          │\r\n╰──────────────────────────╯\r\n",
		"numbered transcript":  "Steps:\r\n  1. build\r\n  2. test\r\n",
		"pointer without menu": "❯ only one line\r\n\r\nsomething else\r\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if q := detect(screenOf(24, 80, content)); q != nil {
				t.Fatalf("false positive: %+v", q)
			}
		})
	}
}

func TestDetectBypassPermissionsDisclaimer(t *testing.T) {
	screen := screenOf(30, 100,
		"─────────────────────────────────────────\r\n",
		" WARNING: Claude Code running in Bypass Permissions mode\r\n\r\n",
		" In Bypass Permissions mode, Claude Code will not ask for your approval before running potentially dangerous commands.\r\n\r\n",
		" ❯ 1. No, exit\r\n",
		"   2. Yes, I accept\r\n\r\n",
		" Enter to confirm · Esc to cancel\r\n",
	)
	q := detect(screen)
	if q == nil || q.Kind != KindBypassPermissions {
		t.Fatalf("question = %+v", q)
	}
	if !q.Options[0].EndsSession || !q.Options[1].GrantsPermission {
		t.Fatalf("flags = %+v", q.Options)
	}
}

func TestScreenAltScreenRestoresMainAndResizeKeepsCursorVisible(t *testing.T) {
	screen := screenOf(5, 20, "main line\r\n", "\x1b[?1049h", "alt content")
	lines, _ := screen.Lines()
	if lines[0] != "alt content" {
		t.Fatalf("alt screen = %q", lines)
	}
	screen.Feed([]byte("\x1b[?1049l"))
	lines, _ = screen.Lines()
	if lines[0] != "main line" {
		t.Fatalf("main screen not restored: %q", lines)
	}
	screen.Feed([]byte("\r\na\r\nb\r\nc"))
	screen.Resize(2, 20)
	lines, cursor := screen.Lines()
	if cursor != 1 || lines[1] != "c" {
		t.Fatalf("after resize lines=%q cursor=%d", lines, cursor)
	}
}

func TestScreenCarriesSplitUTF8AcrossFeeds(t *testing.T) {
	pointer := []byte("❯")
	screen := NewScreen(5, 20)
	screen.Feed(pointer[:1])
	screen.Feed(append(pointer[1:], []byte(" ok")...))
	lines, _ := screen.Lines()
	if lines[0] != "❯ ok" {
		t.Fatalf("line = %q", lines[0])
	}
}

func TestNavigationKeys(t *testing.T) {
	keys := NavigationKeys(1, 3)
	if len(keys) != 3 || string(keys[0]) != "\x1b[B" || string(keys[2]) != "\r" {
		t.Fatalf("keys = %q", keys)
	}
	keys = NavigationKeys(3, 1)
	if len(keys) != 3 || string(keys[0]) != "\x1b[A" {
		t.Fatalf("keys = %q", keys)
	}
}
