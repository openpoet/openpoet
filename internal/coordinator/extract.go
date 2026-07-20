// Package coordinator is OpenPoet's observe-only conflict radar (Phase 1). It
// taps the hook firehose that already carries every Edit/Write file path, keeps
// a deterministic in-memory claim index, and evaluates set-intersection rules
// (R1 file_overlap, R3 same_task, R5 shared_claude_dir) to emit conflict.detected
// and session.awaiting_input events. It never sits between a tool call and its
// verdict — no interventions, no deny — the LLM coordinator consumes its events.
package coordinator

import "strings"

// TouchKind classifies how a tool touched a path.
type TouchKind string

const (
	KindRead   TouchKind = "read"
	KindWrite  TouchKind = "write"
	KindOpaque TouchKind = "opaque"
)

// Touch is one normalized file interaction extracted from a tool call. Path is
// the raw path as the tool reported it (absolute for Claude tools); the Indexer
// normalizes it against the session's project. Scope marks a directory-footprint
// touch (Grep/Glob) that must not become a per-file claim. Opaque touches (Bash)
// carry no path — the extractor refuses to guess paths out of shell commands.
type Touch struct {
	Kind  TouchKind
	Path  string
	Scope bool
}

// acpProbeKeys mirrors the ACP agent's own path-extraction probe order
// (internal/acpagent/agent.go), so ACP-synthesized tool_input indexes for free.
var acpProbeKeys = []string{"file_path", "path", "file", "fileName", "filename", "filePath"}

// ExtractTouches deterministically maps a tool call to normalized touches. It is
// pure (no I/O, no DB) so it can run inline on every hook event in microseconds.
func ExtractTouches(tool string, toolInput map[string]interface{}) []Touch {
	switch tool {
	case "Edit", "Write", "MultiEdit":
		if p := stringField(toolInput, "file_path"); p != "" {
			return []Touch{{Kind: KindWrite, Path: p}}
		}
		return nil
	case "NotebookEdit":
		// NotebookEdit uses notebook_path, not file_path.
		if p := stringField(toolInput, "notebook_path"); p != "" {
			return []Touch{{Kind: KindWrite, Path: p}}
		}
		return nil
	case "Read":
		if p := stringField(toolInput, "file_path"); p != "" {
			return []Touch{{Kind: KindRead, Path: p}}
		}
		return nil
	case "Grep", "Glob":
		// The path here is a DIRECTORY search root — a scope touch, never a file
		// read claim (else write-vs-read info conflicts fire against the repo root).
		if p := stringField(toolInput, "path"); p != "" {
			return []Touch{{Kind: KindRead, Path: p, Scope: true}}
		}
		return nil
	case "Bash":
		// Shell commands can edit files opaquely (sed -i, >>, heredocs); refuse to
		// guess a path. An opaque touch records activity without a false claim.
		return []Touch{{Kind: KindOpaque}}
	default:
		// Unknown/other tools (incl. ACP-synthesized events): probe the known
		// path keys as an advisory READ touch. Never a write claim — only the
		// explicit write tools above create write claims.
		for _, key := range acpProbeKeys {
			if p := stringField(toolInput, key); p != "" {
				return []Touch{{Kind: KindRead, Path: p}}
			}
		}
		return []Touch{{Kind: KindOpaque}}
	}
}

func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// IsClaudeDirPath reports whether a normalized (relative) path is under a
// .claude/ directory — the shared configsync-managed state whose write-write
// contention is R5 shared_claude_dir, NOT R1 file_overlap.
func IsClaudeDirPath(relPath string) bool {
	p := strings.TrimPrefix(filepathToSlash(relPath), "./")
	return p == ".claude" || strings.HasPrefix(p, ".claude/") || strings.Contains(p, "/.claude/")
}

func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
