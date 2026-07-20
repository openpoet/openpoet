package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenCodeBackend implements BackendStrategy for the OpenCode CLI.
type OpenCodeBackend struct{}

func (b *OpenCodeBackend) Type() BackendType         { return BackendOpenCode }
func (b *OpenCodeBackend) BinaryName() string        { return "opencode" }
func (b *OpenCodeBackend) SupportsResume() bool      { return true }
func (b *OpenCodeBackend) SupportsOTEL() bool        { return false }
func (b *OpenCodeBackend) SupportsPlanCapture() bool { return true }
func (b *OpenCodeBackend) SupportsAskUser() bool     { return true }
func (b *OpenCodeBackend) PermissionSkipFlag() string {
	return "--auto"
}
func (b *OpenCodeBackend) HookFormat() string { return "opencode" }

// OpenCodeConfig holds OpenCode-specific settings from project.backend_config JSON.
type OpenCodeConfig struct {
	BinaryPath     string `json:"binary_path"`
	Model          string `json:"model"`
	Agent          string `json:"agent"`
	AutoApprove    bool   `json:"auto_approve"`
	EnableMCP      bool   `json:"enable_mcp"`
	PermissionMode string `json:"permission_mode"`
}

func parseOpenCodeConfig(raw string) OpenCodeConfig {
	cfg := OpenCodeConfig{
		EnableMCP: true,
	}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.PermissionMode = normalizeOpenCodePermissionMode(cfg.PermissionMode)
	cfg.Agent = strings.TrimSpace(cfg.Agent)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.BinaryPath = strings.TrimSpace(cfg.BinaryPath)
	return cfg
}

func normalizeOpenCodePermissionMode(v string) string {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "allow", "ask", "deny":
		return strings.TrimSpace(strings.ToLower(v))
	default:
		return ""
	}
}

func (b *OpenCodeBackend) BuildCLIArgs(cfg *SessionConfig) []string {
	oc := parseOpenCodeConfig(cfg.BackendConfig)
	var args []string
	if cfg.IsReopen {
		if strings.TrimSpace(cfg.ProviderSessionID) != "" {
			args = append(args, "--session", strings.TrimSpace(cfg.ProviderSessionID))
		} else {
			args = append(args, "--continue")
		}
	}
	if oc.Model != "" {
		args = append(args, "--model", oc.Model)
	}
	if oc.Agent != "" {
		args = append(args, "--agent", oc.Agent)
	}
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--prompt", cfg.AppendSystemPrompt)
	}
	if cfg.DangerouslySkipPermissions || oc.AutoApprove {
		args = append(args, b.PermissionSkipFlag())
	}
	return args
}

func (b *OpenCodeBackend) BuildEnvVars(cfg *SessionConfig) map[string]string {
	env := map[string]string{
		"OPENPOET_HOOK_URL":   "http://" + cfg.ServerAddr,
		"OPENPOET_SESSION_ID": cfg.SessionID,
		"OPENPOET_BIN":        cfg.ExecPath,
	}
	oc := parseOpenCodeConfig(cfg.BackendConfig)
	if oc.BinaryPath != "" {
		env["OPENPOET_BACKEND_BINARY"] = expandCodexHome(oc.BinaryPath)
	}
	if oc.EnableMCP && cfg.MCPConfigJSON != "" {
		if content := openCodeConfigContentFromMCPConfig(cfg.MCPConfigJSON); content != "" {
			env["OPENCODE_CONFIG_CONTENT"] = content
		}
	}
	applySessionTokenEnv(env, cfg)
	return env
}

func openCodeConfigContentFromMCPConfig(raw string) string {
	var source struct {
		MCPServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &source); err != nil || len(source.MCPServers) == 0 {
		return ""
	}

	mcpServers := make(map[string]interface{}, len(source.MCPServers))
	for name, server := range source.MCPServers {
		command, _ := server["command"].(string)
		if strings.TrimSpace(command) == "" {
			continue
		}
		commandParts := []string{command}
		if args, ok := server["args"].([]interface{}); ok {
			for _, arg := range args {
				if s, ok := arg.(string); ok {
					commandParts = append(commandParts, s)
				}
			}
		}
		openCodeServer := map[string]interface{}{
			"type":    "local",
			"command": commandParts,
			"enabled": true,
		}
		if env, ok := server["env"].(map[string]interface{}); ok && len(env) > 0 {
			openCodeServer["environment"] = env
		}
		mcpServers[name] = openCodeServer
	}
	if len(mcpServers) == 0 {
		return ""
	}

	content := map[string]interface{}{
		"mcp": mcpServers,
	}
	data, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(data)
}

func (b *OpenCodeBackend) StartupMessage(binaryPath, workDir string) string {
	return fmt.Sprintf("\x1b[90mStarting OpenCode from: %s\r\nWorking directory: %s\x1b[0m\r\n\r\n", binaryPath, workDir)
}

func (b *OpenCodeBackend) NotFoundMessage() string {
	return "\r\n\x1b[31mError: OpenCode CLI not found in PATH.\r\nPlease install or configure the OpenCode CLI.\x1b[0m\r\n"
}
