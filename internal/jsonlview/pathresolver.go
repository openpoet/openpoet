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
	configDir := ClaudeConfigDir(homeDir, os.Getenv("CLAUDE_CONFIG_DIR"))
	return filepath.Join(configDir, "projects", encoded, sessionID+".jsonl")
}

// ClaudeConfigDir returns the Claude Code config root that holds projects/.
// Claude Code honors CLAUDE_CONFIG_DIR (containers sharing one install point
// it at a per-container volume); without it the root is ~/.claude. Values
// that are not POSIX-absolute or ~-relative are ignored.
func ClaudeConfigDir(homeDir, configDirEnv string) string {
	configDirEnv = strings.TrimSpace(configDirEnv)
	switch {
	case strings.HasPrefix(configDirEnv, "/"):
		return filepath.Clean(configDirEnv)
	case configDirEnv == "~":
		return homeDir
	case strings.HasPrefix(configDirEnv, "~/"):
		return filepath.Join(homeDir, configDirEnv[2:])
	}
	return filepath.Join(homeDir, ".claude")
}

// ResolveRemoteJSONLPath computes the JSONL file path on a remote machine.
// configDir is the remote Claude Code config root (see ClaudeConfigDir).
// projectPath is the absolute project path on the remote machine.
// Unlike the local version, this cannot resolve symlinks since the path is remote.
//
// For Windows hosts the project path comes back from SFTP as "/C:/Users/foo",
// but claude.exe encodes the *native* form ("C:\Users\foo"), producing
// folders like "C--Users-foo" — no leading "-" because the original path
// doesn't start with a separator. Detect that shape and skip the leading-dash
// convention that POSIX paths need.
func ResolveRemoteJSONLPath(projectPath, sessionID, configDir string) string {
	if isSFTPWindowsAbsPath(projectPath) {
		// Strip the leading "/" so we encode "C:/Users/foo" rather than
		// "/C:/Users/foo".
		trimmed := projectPath[1:]
		encoded := nonAlphanumRe.ReplaceAllString(trimmed, "-")
		return filepath.Join(configDir, "projects", encoded, sessionID+".jsonl")
	}
	trimmed := strings.TrimPrefix(projectPath, "/")
	encoded := "-" + nonAlphanumRe.ReplaceAllString(trimmed, "-")
	return filepath.Join(configDir, "projects", encoded, sessionID+".jsonl")
}

func isSFTPWindowsAbsPath(p string) bool {
	if len(p) < 4 || p[0] != '/' || p[2] != ':' {
		return false
	}
	c := p[1]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
		return false
	}
	return p[3] == '/' || p[3] == '\\'
}
