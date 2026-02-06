package macro

import (
	"context"
	"devmanager/internal/database"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type ConfigSyncer struct {
	db          *database.DB
	decryptFunc func(string, string) (string, error)
}

func NewConfigSyncer(db *database.DB, decryptFunc func(string, string) (string, error)) *ConfigSyncer {
	return &ConfigSyncer{
		db:          db,
		decryptFunc: decryptFunc,
	}
}

// SyncToProject syncs global configuration to a project
func (cs *ConfigSyncer) SyncToProject(ctx context.Context, project *database.Project) error {
	if project.Type == "local" {
		return cs.syncToLocal(ctx, project.Path)
	}
	return cs.syncToRemote(ctx, project)
}

func (cs *ConfigSyncer) syncToLocal(ctx context.Context, projectPath string) error {
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
	if err := cs.syncBridgeScriptLocal(hooksDir); err != nil {
		return fmt.Errorf("failed to sync bridge.sh: %w", err)
	}

	// Sync skills
	skills, err := cs.db.ListEnabledSkills(ctx)
	if err != nil {
		return fmt.Errorf("failed to list skills: %w", err)
	}

	for _, skill := range skills {
		skillPath := filepath.Join(skillsDir, skill.Name+".md")
		if err := os.WriteFile(skillPath, []byte(skill.Content), 0644); err != nil {
			return fmt.Errorf("failed to write skill %s: %w", skill.Name, err)
		}
	}

	// Sync MCP servers
	mcpServers, err := cs.db.ListEnabledMCPServers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list MCP servers: %w", err)
	}

	mcpConfig := cs.buildMCPConfig(mcpServers)
	mcpPath := filepath.Join(claudeDir, "mcp.json")
	mcpJSON, _ := json.MarshalIndent(mcpConfig, "", "  ")
	if err := os.WriteFile(mcpPath, mcpJSON, 0644); err != nil {
		return fmt.Errorf("failed to write mcp.json: %w", err)
	}

	// Sync settings
	settings, err := cs.db.GetAllSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to get settings: %w", err)
	}

	settingsConfig := cs.buildSettingsConfig(settings)
	settingsPath := filepath.Join(claudeDir, "settings.json")
	settingsJSON, _ := json.MarshalIndent(settingsConfig, "", "  ")
	if err := os.WriteFile(settingsPath, settingsJSON, 0644); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

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
	if err := cs.syncBridgeScriptRemote(client, sftpClient, hooksDir); err != nil {
		return fmt.Errorf("failed to sync bridge.sh to remote: %w", err)
	}

	// Sync skills
	skills, err := cs.db.ListEnabledSkills(ctx)
	if err != nil {
		return err
	}

	for _, skill := range skills {
		skillPath := filepath.Join(skillsDir, skill.Name+".md")
		f, err := sftpClient.Create(skillPath)
		if err != nil {
			return fmt.Errorf("failed to create skill file: %w", err)
		}
		f.Write([]byte(skill.Content))
		f.Close()
	}

	// Sync MCP config
	mcpServers, err := cs.db.ListEnabledMCPServers(ctx)
	if err != nil {
		return err
	}

	mcpConfig := cs.buildMCPConfig(mcpServers)
	mcpJSON, _ := json.MarshalIndent(mcpConfig, "", "  ")
	mcpPath := filepath.Join(claudeDir, "mcp.json")
	f, err := sftpClient.Create(mcpPath)
	if err != nil {
		return fmt.Errorf("failed to create mcp.json: %w", err)
	}
	f.Write(mcpJSON)
	f.Close()

	// Sync settings
	settings, err := cs.db.GetAllSettings(ctx)
	if err != nil {
		return err
	}

	settingsConfig := cs.buildSettingsConfig(settings)
	settingsJSON, _ := json.MarshalIndent(settingsConfig, "", "  ")
	settingsPath := filepath.Join(claudeDir, "settings.json")
	f, err = sftpClient.Create(settingsPath)
	if err != nil {
		return fmt.Errorf("failed to create settings.json: %w", err)
	}
	f.Write(settingsJSON)
	f.Close()

	return nil
}

func (cs *ConfigSyncer) buildMCPConfig(servers []database.MCPServer) map[string]interface{} {
	mcpServers := make(map[string]interface{})

	for _, server := range servers {
		var args []string
		var env map[string]string

		json.Unmarshal([]byte(server.Args), &args)
		json.Unmarshal([]byte(server.Env), &env)

		serverConfig := map[string]interface{}{
			"command": server.Command,
		}
		if len(args) > 0 {
			serverConfig["args"] = args
		}
		if len(env) > 0 {
			serverConfig["env"] = env
		}

		mcpServers[server.Name] = serverConfig
	}

	return map[string]interface{}{
		"mcpServers": mcpServers,
	}
}

func (cs *ConfigSyncer) buildSettingsConfig(settings map[string]string) map[string]interface{} {
	result := make(map[string]interface{})

	for key, value := range settings {
		// Skip internal settings
		if key == "openai_api_key" || key == "vapid_private_key" || key == "vapid_public_key" {
			continue
		}
		// Skip groq_api_key and whisper_provider (internal)
		if key == "groq_api_key" || key == "whisper_provider" || key == "voice_auto_submit" {
			continue
		}

		// Try to parse as JSON
		var jsonVal interface{}
		if err := json.Unmarshal([]byte(value), &jsonVal); err == nil {
			result[key] = jsonVal
		} else {
			result[key] = value
		}
	}

	// Add hooks configuration pointing to bridge.sh
	hookCommand := map[string]interface{}{
		"type":    "command",
		"command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/bridge.sh",
	}
	hookEntry := []interface{}{
		map[string]interface{}{
			"hooks": []interface{}{hookCommand},
		},
	}

	result["hooks"] = map[string]interface{}{
		"PermissionRequest":  hookEntry,
		"PreToolUse":         hookEntry,
		"PostToolUse":        hookEntry,
		"PostToolUseFailure": hookEntry,
		"Notification":       hookEntry,
		"Stop":               hookEntry,
	}

	return result
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
    PreToolUse|PostToolUse|PostToolUseFailure|Notification|Stop)
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
    PreToolUse|PostToolUse|PostToolUseFailure|Notification|Stop)
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
