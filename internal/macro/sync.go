package macro

import (
	"context"
	"devmanager/internal/database"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SyncProgressReporter is an interface for reporting sync progress.
type SyncProgressReporter interface {
	BroadcastSyncProgress(projectID int64, step, status, detail string)
}

type ConfigSyncer struct {
	db          *database.DB
	decryptFunc func(string, string) (string, error)
	hub         SyncProgressReporter
	serverAddr  string
}

func NewConfigSyncer(db *database.DB, decryptFunc func(string, string) (string, error), serverAddr string) *ConfigSyncer {
	return &ConfigSyncer{
		db:          db,
		decryptFunc: decryptFunc,
		serverAddr:  serverAddr,
	}
}

// SetHub sets the WebSocket hub for broadcasting sync progress.
func (cs *ConfigSyncer) SetHub(hub SyncProgressReporter) {
	cs.hub = hub
}

func (cs *ConfigSyncer) reportProgress(projectID int64, step, status, detail string) {
	if cs.hub != nil {
		cs.hub.BroadcastSyncProgress(projectID, step, status, detail)
	}
}

// SyncToProject syncs global configuration to a project
func (cs *ConfigSyncer) SyncToProject(ctx context.Context, project *database.Project) error {
	if project.Type == "local" {
		return cs.syncToLocal(ctx, project)
	}
	return cs.syncToRemote(ctx, project)
}

func (cs *ConfigSyncer) syncToLocal(ctx context.Context, project *database.Project) error {
	projectPath := project.Path
	claudeDir := filepath.Join(projectPath, ".claude")
	skillsDir := filepath.Join(claudeDir, "skills")
	hooksDir := filepath.Join(claudeDir, "hooks")

	// Ensure directories exist
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude/skills directory: %w", err)
	}
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude/hooks directory: %w", err)
	}

	// Sync bridge.sh hook script
	cs.reportProgress(project.ID, "hooks", "running", "Syncing hook scripts...")
	if err := cs.syncBridgeScriptLocal(hooksDir); err != nil {
		cs.reportProgress(project.ID, "hooks", "error", err.Error())
		return fmt.Errorf("failed to sync bridge.sh: %w", err)
	}
	cs.reportProgress(project.ID, "hooks", "done", "Hook scripts synced")

	// Sync skills with smart tracking
	cs.reportProgress(project.ID, "skills", "running", "Syncing skills...")
	if err := cs.syncSkillsToLocal(ctx, skillsDir, projectPath); err != nil {
		cs.reportProgress(project.ID, "skills", "error", err.Error())
		return fmt.Errorf("failed to sync skills: %w", err)
	}
	cs.reportProgress(project.ID, "skills", "done", "Skills synced")

	// MCP servers are passed via --mcp-config CLI flag at session start.
	// No file writing needed — this preserves any MCPs the user configured manually in the project.
	cs.reportProgress(project.ID, "mcps", "done", "MCP servers configured via CLI flag at session start")

	// Sync hooks into settings.json (merge, preserving user's other settings)
	cs.reportProgress(project.ID, "settings", "running", "Syncing hooks to settings...")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	existing := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	existing["hooks"] = cs.buildHooksConfig()

	settingsJSON, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(settingsPath, settingsJSON, 0644); err != nil {
		cs.reportProgress(project.ID, "settings", "error", err.Error())
		return fmt.Errorf("failed to write settings.json: %w", err)
	}
	cs.reportProgress(project.ID, "settings", "done", "Hooks synced")

	return nil
}

func (cs *ConfigSyncer) syncToRemote(ctx context.Context, project *database.Project) error {
	// Build SSH config
	config, err := cs.buildSSHConfig(project)
	if err != nil {
		return err
	}

	// Connect
	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()

	// Create SFTP client
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("SFTP connection failed: %w", err)
	}
	defer sftpClient.Close()

	claudeDir := filepath.Join(project.Path, ".claude")
	skillsDir := filepath.Join(claudeDir, "skills")
	hooksDir := filepath.Join(claudeDir, "hooks")

	// Ensure directories exist
	sftpClient.MkdirAll(skillsDir)
	sftpClient.MkdirAll(hooksDir)

	// Sync bridge.sh hook script to remote
	cs.reportProgress(project.ID, "hooks", "running", "Syncing hook scripts to remote...")
	if err := cs.syncBridgeScriptRemote(client, sftpClient, hooksDir); err != nil {
		cs.reportProgress(project.ID, "hooks", "error", err.Error())
		return fmt.Errorf("failed to sync bridge.sh to remote: %w", err)
	}
	cs.reportProgress(project.ID, "hooks", "done", "Hook scripts synced")

	// Sync skills with smart tracking
	cs.reportProgress(project.ID, "skills", "running", "Syncing skills to remote...")
	if err := cs.syncSkillsToRemote(ctx, sftpClient, skillsDir, project); err != nil {
		cs.reportProgress(project.ID, "skills", "error", err.Error())
		return fmt.Errorf("failed to sync skills to remote: %w", err)
	}
	cs.reportProgress(project.ID, "skills", "done", "Skills synced")

	// MCP servers are passed via --mcp-config CLI flag at session start.
	// No file writing needed — this preserves any MCPs the user configured manually in the project.
	cs.reportProgress(project.ID, "mcps", "done", "MCP servers configured via CLI flag at session start")

	// Sync hooks into settings.json (merge, preserving user's other settings)
	cs.reportProgress(project.ID, "settings", "running", "Syncing hooks to remote settings...")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	existing := make(map[string]interface{})
	if f, err := sftpClient.Open(settingsPath); err == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		json.Unmarshal(data, &existing)
	}

	existing["hooks"] = cs.buildHooksConfig()

	settingsJSON, _ := json.MarshalIndent(existing, "", "  ")
	f, err := sftpClient.Create(settingsPath)
	if err != nil {
		cs.reportProgress(project.ID, "settings", "error", err.Error())
		return fmt.Errorf("failed to create settings.json: %w", err)
	}
	f.Write(settingsJSON)
	f.Close()
	cs.reportProgress(project.ID, "settings", "done", "Hooks synced")

	return nil
}

// syncSkillsToLocal syncs skills to a local project, tracking which files DevManager manages.
// Skills are written as .claude/skills/<name>/SKILL.md with YAML frontmatter.
func (cs *ConfigSyncer) syncSkillsToLocal(ctx context.Context, skillsDir string, projectPath string) error {
	// Get project ID from path
	var projectID int64
	projects, _ := cs.db.ListProjects(ctx)
	for _, p := range projects {
		if p.Path == projectPath {
			projectID = p.ID
			break
		}
	}

	skills, err := cs.db.ListEnabledSkills(ctx)
	if err != nil {
		return fmt.Errorf("failed to list skills: %w", err)
	}

	// Build set of expected directory names (new format: <name>/SKILL.md)
	expectedDirs := make(map[string]int64) // dirName -> skillID
	for _, skill := range skills {
		expectedDirs[skill.Name] = skill.ID
	}

	// Get previously synced files for this project and clean up stale ones
	if projectID > 0 {
		syncedFiles, _ := cs.db.ListSyncedSkillFiles(ctx, projectID)
		for _, sf := range syncedFiles {
			// Extract dir name from tracked file (could be old "name.md" or new "name/SKILL.md")
			dirName := sf.FileName
			if strings.HasSuffix(dirName, ".md") && !strings.Contains(dirName, "/") {
				// Old flat format: "name.md" -> remove flat file
				dirName = strings.TrimSuffix(dirName, ".md")
				oldPath := filepath.Join(skillsDir, sf.FileName)
				os.Remove(oldPath)
			}
			if strings.HasSuffix(dirName, "/SKILL.md") {
				dirName = strings.TrimSuffix(dirName, "/SKILL.md")
			}

			if _, stillNeeded := expectedDirs[dirName]; !stillNeeded {
				// Skill deleted/disabled/renamed — remove directory
				dirPath := filepath.Join(skillsDir, dirName)
				os.RemoveAll(dirPath)
				cs.db.DeleteSyncedSkillFile(ctx, sf.ID)
			}
		}
	}

	// Write enabled skills as <name>/SKILL.md
	for _, skill := range skills {
		skillDirPath := filepath.Join(skillsDir, skill.Name)
		if err := os.MkdirAll(skillDirPath, 0755); err != nil {
			return fmt.Errorf("failed to create skill dir %s: %w", skill.Name, err)
		}

		content := buildSkillMD(skill.Name, skill.Content)
		skillFilePath := filepath.Join(skillDirPath, "SKILL.md")
		if err := os.WriteFile(skillFilePath, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write skill %s: %w", skill.Name, err)
		}

		// Also clean up legacy flat file if it exists
		legacyPath := filepath.Join(skillsDir, skill.Name+".md")
		os.Remove(legacyPath)

		// Track with new format
		trackedName := skill.Name + "/SKILL.md"
		if projectID > 0 {
			cs.db.UpsertSyncedSkillFile(ctx, projectID, skill.ID, trackedName)
		}
		cs.db.IncrementSkillSyncCount(ctx, skill.ID)
	}

	return nil
}

// syncSkillsToRemote syncs skills to a remote project via SFTP, tracking which files DevManager manages.
// Skills are written as .claude/skills/<name>/SKILL.md with YAML frontmatter.
func (cs *ConfigSyncer) syncSkillsToRemote(ctx context.Context, sftpClient *sftp.Client, skillsDir string, project *database.Project) error {
	skills, err := cs.db.ListEnabledSkills(ctx)
	if err != nil {
		return err
	}

	// Build set of expected directory names
	expectedDirs := make(map[string]int64)
	for _, skill := range skills {
		expectedDirs[skill.Name] = skill.ID
	}

	// Get previously synced files for this project and remove stale ones
	syncedFiles, _ := cs.db.ListSyncedSkillFiles(ctx, project.ID)
	for _, sf := range syncedFiles {
		dirName := sf.FileName
		if strings.HasSuffix(dirName, ".md") && !strings.Contains(dirName, "/") {
			// Old flat format
			dirName = strings.TrimSuffix(dirName, ".md")
			sftpClient.Remove(filepath.Join(skillsDir, sf.FileName))
		}
		if strings.HasSuffix(dirName, "/SKILL.md") {
			dirName = strings.TrimSuffix(dirName, "/SKILL.md")
		}

		if _, stillNeeded := expectedDirs[dirName]; !stillNeeded {
			dirPath := filepath.Join(skillsDir, dirName)
			// Remove SKILL.md then directory
			sftpClient.Remove(filepath.Join(dirPath, "SKILL.md"))
			sftpClient.RemoveDirectory(dirPath)
			cs.db.DeleteSyncedSkillFile(ctx, sf.ID)
		}
	}

	// Write enabled skills as <name>/SKILL.md
	for _, skill := range skills {
		skillDirPath := filepath.Join(skillsDir, skill.Name)
		sftpClient.MkdirAll(skillDirPath)

		content := buildSkillMD(skill.Name, skill.Content)
		skillFilePath := filepath.Join(skillDirPath, "SKILL.md")
		f, err := sftpClient.Create(skillFilePath)
		if err != nil {
			return fmt.Errorf("failed to create skill file: %w", err)
		}
		f.Write([]byte(content))
		f.Close()

		// Clean up legacy flat file
		sftpClient.Remove(filepath.Join(skillsDir, skill.Name+".md"))

		trackedName := skill.Name + "/SKILL.md"
		cs.db.UpsertSyncedSkillFile(ctx, project.ID, skill.ID, trackedName)
		cs.db.IncrementSkillSyncCount(ctx, skill.ID)
	}

	return nil
}

// buildSkillMD generates a SKILL.md with YAML frontmatter from DB fields.
// If the content already has frontmatter (starts with ---), it is used as-is.
func buildSkillMD(name string, content string) string {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "---") {
		// Content already has frontmatter — use as-is
		return trimmed + "\n"
	}

	// Extract description from first markdown heading or first line
	description := "DevManager managed skill: " + name
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			description = strings.TrimPrefix(line, "# ")
			break
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			description = line
			if len(description) > 120 {
				description = description[:120]
			}
			break
		}
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + name + "\n")
	sb.WriteString("description: " + description + "\n")
	sb.WriteString("user-invocable: false\n")
	sb.WriteString("---\n\n")
	sb.WriteString(trimmed)
	sb.WriteString("\n")
	return sb.String()
}

// buildHooksConfig returns the hooks configuration that DevManager manages.
// This is the only section DevManager writes to a project's settings.json.
func (cs *ConfigSyncer) buildHooksConfig() map[string]interface{} {
	hookCommand := map[string]interface{}{
		"type":    "command",
		"command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/bridge.sh",
	}
	hookEntry := []interface{}{
		map[string]interface{}{
			"hooks": []interface{}{hookCommand},
		},
	}

	return map[string]interface{}{
		"PermissionRequest":  hookEntry,
		"PreToolUse":         hookEntry,
		"PostToolUse":        hookEntry,
		"PostToolUseFailure": hookEntry,
		"Notification":       hookEntry,
		"Stop":               hookEntry,
		"UserPromptSubmit":   hookEntry,
	}
}

// syncBridgeScriptLocal copies bridge.sh to the project's .claude/hooks/ directory
func (cs *ConfigSyncer) syncBridgeScriptLocal(hooksDir string) error {
	bridgeScript := `#!/bin/bash
# DevManager Hook Bridge Script
# Receives hook event JSON on stdin, POSTs to DevManager API, returns response on stdout.

HOOK_URL="${DEVMANAGER_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${DEVMANAGER_SESSION_ID}"
INPUT=$(cat)

EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

case "$EVENT" in
    PermissionRequest)
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            echo "$RESPONSE"
        fi
        ;;
    PreToolUse|PostToolUse|PostToolUseFailure|Notification|Stop|UserPromptSubmit)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
`
	bridgePath := filepath.Join(hooksDir, "bridge.sh")
	if err := os.WriteFile(bridgePath, []byte(bridgeScript), 0755); err != nil {
		return err
	}
	return nil
}

// syncBridgeScriptRemote copies bridge.sh to the remote project's .claude/hooks/ directory
func (cs *ConfigSyncer) syncBridgeScriptRemote(sshClient *ssh.Client, sftpClient *sftp.Client, hooksDir string) error {
	bridgeScript := `#!/bin/bash
HOOK_URL="${DEVMANAGER_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${DEVMANAGER_SESSION_ID}"
INPUT=$(cat)

EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

case "$EVENT" in
    PermissionRequest)
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            echo "$RESPONSE"
        fi
        ;;
    PreToolUse|PostToolUse|PostToolUseFailure|Notification|Stop|UserPromptSubmit)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
`
	bridgePath := filepath.Join(hooksDir, "bridge.sh")
	f, err := sftpClient.Create(bridgePath)
	if err != nil {
		return fmt.Errorf("failed to create bridge.sh on remote: %w", err)
	}
	f.Write([]byte(bridgeScript))
	f.Close()

	// Make executable
	session, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session for chmod: %w", err)
	}
	defer session.Close()
	session.Run("chmod +x " + bridgePath)

	return nil
}

func (cs *ConfigSyncer) buildSSHConfig(project *database.Project) (*ssh.ClientConfig, error) {
	authType := project.SSHAuthType.String
	user := project.SSHUser.String

	var authMethods []ssh.AuthMethod

	if authType == "password" {
		password, err := cs.decryptFunc(
			project.SSHCredentialEncrypted.String,
			project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.Password(password))
	} else if authType == "key" || authType == "key_passphrase" {
		keyData, err := cs.decryptFunc(
			project.SSHCredentialEncrypted.String,
			project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}

		signer, err := ssh.ParsePrivateKey([]byte(keyData))
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	} else {
		// Try default keys
		homeDir, _ := os.UserHomeDir()
		keyPaths := []string{
			homeDir + "/.ssh/id_rsa",
			homeDir + "/.ssh/id_ed25519",
			homeDir + "/.ssh/id_ecdsa",
		}
		for _, keyPath := range keyPaths {
			if keyData, err := os.ReadFile(keyPath); err == nil {
				if signer, err := ssh.ParsePrivateKey(keyData); err == nil {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
					break
				}
			}
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}, nil
}

// SyncAllProjects syncs configuration to all projects
func (cs *ConfigSyncer) SyncAllProjects(ctx context.Context) error {
	projects, err := cs.db.ListProjects(ctx)
	if err != nil {
		return err
	}

	var lastErr error
	for _, project := range projects {
		if err := cs.SyncToProject(ctx, &project); err != nil {
			lastErr = err
			// Continue with other projects
		} else {
			cs.db.UpdateProjectConfigSyncedAt(ctx, project.ID)
		}
	}

	return lastErr
}
