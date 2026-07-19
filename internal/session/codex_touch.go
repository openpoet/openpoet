package session

// Codex activity adapter (Phase 5). Codex is an app-server backend that streams
// JSON-RPC items instead of firing Claude Code hooks — so its file edits never
// reached the conflict radar, which taps the PreToolUse hook firehose. This
// adapter maps a Codex item to a SYNTHESIZED Claude-Code-style PreToolUse so the
// SAME deterministic extractor that indexes claude_code and ACP indexes codex
// too. A patch/file item becomes a Write on its path; an opaque exec never
// synthesizes a guessed path (the same stance the extractor takes for Bash).

// CodexItem is the subset of a Codex app-server item the touch adapter reads.
type CodexItem struct {
	Type    string   // e.g. "patch_apply", "file_change", "apply_patch", "write_file", "exec_command"
	Path    string   // single edited path, when the item carries one
	Paths   []string // multiple edited paths (first is used)
	Command string   // opaque shell command (exec items) — never mined for paths
}

// codexWriteItemTypes are the Codex item types that represent a file write.
var codexWriteItemTypes = map[string]struct{}{
	"patch_apply": {}, "apply_patch": {}, "file_change": {}, "write_file": {}, "file_write": {},
}

// codexTouchFromItem maps a Codex item to a synthesized (toolName, toolInput)
// pair matching the Claude Code PreToolUse contract. ok is false for items that
// carry no attributable path (exec/opaque) — guessing a path out of a shell
// command is exactly how false conflicts start.
func codexTouchFromItem(item CodexItem) (toolName string, toolInput map[string]interface{}, ok bool) {
	if _, isWrite := codexWriteItemTypes[item.Type]; !isWrite {
		return "", nil, false
	}
	path := item.Path
	if path == "" && len(item.Paths) > 0 {
		path = item.Paths[0]
	}
	if path == "" {
		return "", nil, false
	}
	return "Write", map[string]interface{}{"file_path": path}, true
}

// codexPreToolUseEvent builds the hook-event body a bridge would POST for a
// synthesized Codex touch, so codex writes flow through /api/hooks/pretooluse
// (and the async index) exactly like claude_code and ACP. Returns ok=false when
// the item is not an attributable write.
func codexPreToolUseEvent(item CodexItem) (map[string]interface{}, bool) {
	tool, input, ok := codexTouchFromItem(item)
	if !ok {
		return nil, false
	}
	return map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_input":      input,
	}, true
}
