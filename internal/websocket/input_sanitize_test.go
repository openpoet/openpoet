package websocket

import "testing"

func TestSanitizeTerminalClientInputRemovesEscapedResponses(t *testing.T) {
	input := "\x1b[25;1R\x1b]10;rgb:c0c0/caca/f5f5\x1b\\\x1b]11;rgb:1a1a/1b1b/2626\x1b\\\x1b[?1;2cnão é pra usar last"
	got := sanitizeTerminalClientInput(input)
	if got != "não é pra usar last" {
		t.Fatalf("sanitizeTerminalClientInput() = %q", got)
	}
}

func TestSanitizeTerminalClientInputRemovesOrphanedResponses(t *testing.T) {
	input := "[O[25;1R]10;rgb:c0c0/caca/f5f5\\]11;rgb:1a1a/1b1b/2626\\[?1;2cnão é pra usar last"
	got := sanitizeTerminalClientInput(input)
	if got != "não é pra usar last" {
		t.Fatalf("sanitizeTerminalClientInput() = %q", got)
	}
}

func TestSanitizeTerminalClientInputPreservesUserNavigation(t *testing.T) {
	input := "\x1b[A/status\x1b[B"
	got := sanitizeTerminalClientInput(input)
	if got != input {
		t.Fatalf("sanitizeTerminalClientInput() stripped user navigation: %q", got)
	}
}
