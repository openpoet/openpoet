package coordinator

import "testing"

// TestACPSynthesizedItemBecomesTouch: the ACP agent synthesizes PreToolUse
// events from JSON-RPC tool-call items; their tool_input carries the edited path
// under one of the probe keys. This asserts the SAME extractor the claim index
// uses turns those synthesized items into touches — so ACP indexes for free.
func TestACPSynthesizedItemBecomesTouch(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    map[string]interface{}
		wantKind TouchKind
		wantPath string
	}{
		{"write via file_path", "Write", map[string]interface{}{"file_path": "/proj/a.go"}, KindWrite, "/proj/a.go"},
		{"acp probe key path", "fs/write_text_file", map[string]interface{}{"path": "/proj/b.go"}, KindRead, "/proj/b.go"},
		{"acp probe key fileName", "acp_edit", map[string]interface{}{"fileName": "/proj/c.go"}, KindRead, "/proj/c.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			touches := ExtractTouches(tc.tool, tc.input)
			if len(touches) != 1 {
				t.Fatalf("expected 1 touch, got %d (%v)", len(touches), touches)
			}
			if touches[0].Kind != tc.wantKind || touches[0].Path != tc.wantPath {
				t.Fatalf("touch = %+v, want kind=%s path=%s", touches[0], tc.wantKind, tc.wantPath)
			}
		})
	}
	// A synthesized item with NO path key is opaque (no false claim).
	if got := ExtractTouches("acp_unknown", map[string]interface{}{"foo": "bar"}); len(got) != 1 || got[0].Kind != KindOpaque {
		t.Fatalf("pathless ACP item should be opaque, got %v", got)
	}
}
