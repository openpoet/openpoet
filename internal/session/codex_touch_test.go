package session

import "testing"

// TestCodexTouchExtractsPathFromItem: patch/file items yield a Write touch on
// their path (single or first-of-list); an opaque exec yields nothing (no
// guessed path from a shell command).
func TestCodexTouchExtractsPathFromItem(t *testing.T) {
	tool, input, ok := codexTouchFromItem(CodexItem{Type: "patch_apply", Path: "/proj/a.go"})
	if !ok || tool != "Write" || input["file_path"] != "/proj/a.go" {
		t.Fatalf("patch_apply → %q %v ok=%v", tool, input, ok)
	}
	_, input2, ok2 := codexTouchFromItem(CodexItem{Type: "apply_patch", Paths: []string{"/proj/b.go", "/proj/c.go"}})
	if !ok2 || input2["file_path"] != "/proj/b.go" {
		t.Fatalf("paths[] fallback → %v ok=%v", input2, ok2)
	}
	if _, _, ok3 := codexTouchFromItem(CodexItem{Type: "exec_command", Command: "sed -i s/a/b/ x.go"}); ok3 {
		t.Fatal("exec item must NOT synthesize a path (opaque, like Bash)")
	}
	if _, _, ok4 := codexTouchFromItem(CodexItem{Type: "patch_apply"}); ok4 {
		t.Fatal("a write item without a path must not synthesize a touch")
	}
}

// TestCodexAdapterSynthesizesPreToolUse: the adapter emits a Claude-Code-shaped
// PreToolUse hook body so codex writes reach the gate/index through the shared
// path.
func TestCodexAdapterSynthesizesPreToolUse(t *testing.T) {
	ev, ok := codexPreToolUseEvent(CodexItem{Type: "file_change", Path: "/proj/c.go"})
	if !ok {
		t.Fatal("file_change should synthesize a PreToolUse event")
	}
	if ev["hook_event_name"] != "PreToolUse" || ev["tool_name"] != "Write" {
		t.Fatalf("bad synthesized event: %v", ev)
	}
	ti, _ := ev["tool_input"].(map[string]interface{})
	if ti["file_path"] != "/proj/c.go" {
		t.Fatalf("tool_input.file_path = %v", ti["file_path"])
	}
	if _, ok := codexPreToolUseEvent(CodexItem{Type: "exec_command"}); ok {
		t.Fatal("exec item must not synthesize a PreToolUse event")
	}
}
