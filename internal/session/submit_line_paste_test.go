package session

import (
	"strings"
	"testing"

	"openpoet/internal/database"
)

// pasteLimitedRunner is an unsyncRunner whose program cannot receive
// bracketed paste (a Windows host).
type pasteLimitedRunner struct{ unsyncRunner }

func (r *pasteLimitedRunner) SupportsBracketedPaste() bool { return false }

func longPortugueseText(chars int) string {
	var b strings.Builder
	for b.Len() < chars*2 {
		b.WriteString("Confira a configuração da máquina antes de seguir, ")
	}
	return string([]rune(b.String())[:chars])
}

func runningWith(backend string, runner Runner) *runningSession {
	return &runningSession{session: &database.Session{ID: "s1", Backend: backend}, runner: runner}
}

// TestSubmitLinePayloadBracketsLongAndMultilineText pins which lines go as one
// bracketed paste: a long or multi-line text for Claude Code or Codex, so the
// agent buffers the whole text however the PTY splits it (a macOS PTY hands
// it over in 1024-byte reads, and Claude Code dropped the head of an
// unbracketed ~1.3k-character text). Short single lines stay typed.
func TestSubmitLinePayloadBracketsLongAndMultilineText(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		runner  Runner
		text    string
		bracket bool
	}{
		{"short line", "claude_code", &unsyncRunner{}, "/model opus", false},
		{"~1.5k one paragraph", "claude_code", &unsyncRunner{}, longPortugueseText(1500), true},
		{"~10k one paragraph", "claude_code", &unsyncRunner{}, longPortugueseText(10000), true},
		{"short with line breaks", "claude_code", &unsyncRunner{}, "linha 1\nlinha 2", true},
		{"codex long", "codex", &unsyncRunner{}, longPortugueseText(1500), true},
		{"other backend keeps typing", "opencode", &unsyncRunner{}, longPortugueseText(1500), false},
		{"runner without paste support", "claude_code", &pasteLimitedRunner{}, longPortugueseText(1500), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(submitLinePayload(runningWith(tc.backend, tc.runner), tc.text))
			want := tc.text
			if tc.bracket {
				want = bracketedPasteStart + tc.text + bracketedPasteEnd
			}
			if got != want {
				t.Fatalf("payload (%d bytes) = %.80q..., want bracket=%v", len(got), got, tc.bracket)
			}
		})
	}
}

func TestSubmitLinePayloadStripsPasteMarkersFromText(t *testing.T) {
	text := longPortugueseText(600) + bracketedPasteEnd + "cauda"
	got := string(submitLinePayload(runningWith("claude_code", &unsyncRunner{}), text))
	inner := strings.TrimSuffix(strings.TrimPrefix(got, bracketedPasteStart), bracketedPasteEnd)
	if strings.Contains(inner, bracketedPasteEnd) || !strings.HasSuffix(inner, "cauda") {
		t.Fatalf("paste body still holds a marker or lost text: %q", inner[len(inner)-40:])
	}
}

// TestSubmitLineToSessionPastesLongTextThenEnter pins the write sequence: the
// whole text in one bracketed paste, then Enter in its own write.
func TestSubmitLineToSessionPastesLongTextThenEnter(t *testing.T) {
	runner := &unsyncRunner{}
	m := &Manager{sessions: map[string]*runningSession{"s1": runningWith("claude_code", runner)}}
	text := longPortugueseText(1300) + "\nsegunda linha com acentuação"
	if err := m.SubmitLineToSession("s1", text, 0); err != nil {
		t.Fatal(err)
	}
	if len(runner.writes) != 2 {
		t.Fatalf("writes = %d, want paste then Enter", len(runner.writes))
	}
	if got := string(runner.writes[0]); got != bracketedPasteStart+text+bracketedPasteEnd {
		t.Fatalf("first write is not the whole text as one paste (%d bytes)", len(got))
	}
	if got := string(runner.writes[1]); got != "\r" {
		t.Fatalf("second write = %q, want Enter", got)
	}
}

func TestCodexTerminalSubmitIgnoresPastedLineBreaks(t *testing.T) {
	m := &Manager{}
	rs := runningWith("codex", &unsyncRunner{})
	if m.isCodexTerminalSubmit(rs, []byte(bracketedPasteStart+"linha 1\nlinha 2"+bracketedPasteEnd)) {
		t.Fatal("a pasted line break counted as Enter")
	}
	if !m.isCodexTerminalSubmit(rs, []byte("\r")) {
		t.Fatal("Enter not counted as submit")
	}
	if !m.isCodexTerminalSubmit(rs, []byte(bracketedPasteStart+"x"+bracketedPasteEnd+"\r")) {
		t.Fatal("Enter after a paste not counted as submit")
	}
}

// TestCodexRunnerKeepsBracketedPasteAsOnePrompt feeds a multi-line paste split
// across writes, as a PTY or SSH channel may deliver it: the line breaks stay
// in the prompt and nothing is submitted before Enter.
func TestCodexRunnerKeepsBracketedPasteAsOnePrompt(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{outputHandler: func(b []byte) { out.Write(b) }}
	text := "primeira linha com ação\r\nsegunda linha\núltima"
	paste := []byte(bracketedPasteStart + text + bracketedPasteEnd)
	for _, part := range [][]byte{paste[:9], paste[9:30], paste[30:]} {
		if _, err := r.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.transcript) != 0 {
		t.Fatalf("paste was submitted before Enter: %#v", r.transcript)
	}
	want := "primeira linha com ação\nsegunda linha\núltima"
	if got := string(r.inputBuffer); got != want {
		t.Fatalf("input buffer = %q, want %q", got, want)
	}
	if r.inputPasting {
		t.Fatal("still pasting after the closing marker")
	}
}
