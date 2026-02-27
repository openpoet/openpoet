package jsonlview

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// nonAlphanumRe matches any character that is not a letter, digit, or hyphen.
var nonAlphanumRe = regexp.MustCompile(`[^a-zA-Z0-9-]`)

// ResolveJSONLPath computes the JSONL file path for a Claude Code session.
// Claude Code encodes project paths by replacing all non-alphanumeric characters
// (slashes, underscores, dots, spaces, etc.) with "-" and prepending "-".
// It resolves symlinks first (e.g. /tmp → /private/tmp on macOS).
// Example: "/Users/foo/my_bar" → "~/.claude/projects/-Users-foo-my-bar/{sessionID}.jsonl"
func ResolveJSONLPath(projectPath, sessionID string) string {
	// Resolve symlinks — Claude Code does this (e.g. /tmp → /private/tmp on macOS)
	if resolved, err := filepath.EvalSymlinks(projectPath); err == nil {
		projectPath = resolved
	}
	trimmed := strings.TrimPrefix(projectPath, "/")
	encoded := "-" + nonAlphanumRe.ReplaceAllString(trimmed, "-")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = os.Getenv("HOME")
	}
	return filepath.Join(homeDir, ".claude", "projects", encoded, sessionID+".jsonl")
}
