package jsonlview

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveJSONLPath computes the JSONL file path for a Claude Code session.
// Claude Code encodes project paths by replacing "/" with "-" and prepending "-".
// Example: "/Users/foo/bar" → "~/.claude/projects/-Users-foo-bar/{sessionID}.jsonl"
func ResolveJSONLPath(projectPath, sessionID string) string {
	encoded := "-" + strings.ReplaceAll(strings.TrimPrefix(projectPath, "/"), "/", "-")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = os.Getenv("HOME")
	}
	return filepath.Join(homeDir, ".claude", "projects", encoded, sessionID+".jsonl")
}
