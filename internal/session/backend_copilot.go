package session

import (
	"encoding/json"
	"fmt"
)

// CopilotBackend implements BackendStrategy for GitHub Copilot CLI.
type CopilotBackend struct{}

func (b *CopilotBackend) Type() BackendType          { return BackendCopilot }
func (b *CopilotBackend) BinaryName() string         { return "copilot" }
func (b *CopilotBackend) SupportsResume() bool       { return false }
func (b *CopilotBackend) SupportsOTEL() bool         { return false }
func (b *CopilotBackend) SupportsPlanCapture() bool  { return false }
func (b *CopilotBackend) SupportsAskUser() bool      { return false }
func (b *CopilotBackend) PermissionSkipFlag() string { return "--allow-all-tools" }
func (b *CopilotBackend) HookFormat() string         { return "copilot" }

// copilotConfig holds Copilot-specific settings from project.backend_config JSON.
type copilotConfig struct {
	AllowAllTools bool   `json:"allow_all_tools"`
	GitHubToken   string `json:"github_token"`
}

func parseCopilotConfig(raw string) copilotConfig {
	var cfg copilotConfig
	if raw != "" {
		json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg
}

func (b *CopilotBackend) BuildCLIArgs(cfg *SessionConfig) []string {
	var args []string
	cc := parseCopilotConfig(cfg.BackendConfig)

	if cc.AllowAllTools || cfg.DangerouslySkipPermissions {
		args = append(args, b.PermissionSkipFlag())
	}

	if cfg.MCPConfigJSON != "" {
		args = append(args, "--mcp-config", cfg.MCPConfigJSON)
	}

	// Copilot uses -p for one-shot prompts; for task context, inject via initial prompt
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "-p", cfg.AppendSystemPrompt)
	}

	return args
}

func (b *CopilotBackend) BuildEnvVars(cfg *SessionConfig) map[string]string {
	env := map[string]string{
		"OPENPOET_HOOK_URL":   "http://" + cfg.ServerAddr,
		"OPENPOET_SESSION_ID": cfg.SessionID,
	}

	cc := parseCopilotConfig(cfg.BackendConfig)
	if cc.GitHubToken != "" {
		env["GITHUB_TOKEN"] = cc.GitHubToken
	}

	return env
}

func (b *CopilotBackend) StartupMessage(binaryPath, workDir string) string {
	return fmt.Sprintf("\x1b[90mStarting GitHub Copilot CLI from: %s\r\nWorking directory: %s\x1b[0m\r\n\r\n", binaryPath, workDir)
}

func (b *CopilotBackend) NotFoundMessage() string {
	return "\r\n\x1b[31mError: GitHub Copilot CLI not found in PATH.\r\nPlease install it: https://docs.github.com/en/copilot/how-tos/copilot-cli/use-copilot-cli\x1b[0m\r\n"
}
