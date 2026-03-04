package session

import (
	"encoding/json"
	"fmt"
)

// ACPBackend implements BackendStrategy for GitHub Copilot CLI in ACP mode (copilot --acp).
// The acp-agent subcommand of openpoet launches copilot --acp as a subprocess and bridges
// JSON-RPC 2.0 communication with OpenPoet's PTY-based terminal.
type ACPBackend struct{}

func (b *ACPBackend) Type() BackendType { return BackendACP }

// BinaryName returns empty to signal self-exec mode — the runner should
// use OPENPOET_BIN (the currently running binary) instead of LookPath.
func (b *ACPBackend) BinaryName() string         { return "" }
func (b *ACPBackend) SupportsResume() bool       { return true }
func (b *ACPBackend) SupportsOTEL() bool         { return false }
func (b *ACPBackend) SupportsPlanCapture() bool  { return true }
func (b *ACPBackend) SupportsAskUser() bool      { return false }
func (b *ACPBackend) PermissionSkipFlag() string { return "--auto-approve" }
func (b *ACPBackend) HookFormat() string         { return "acp" }

// acpConfig holds ACP-specific settings from project.backend_config JSON.
type acpConfig struct {
	AutoApprove bool   `json:"auto_approve"` // Skip permission prompts
	EnableMCP   bool   `json:"enable_mcp"`   // Opt-in MCP server support
	CopilotPath string `json:"copilot_path"` // Override copilot binary path
	GitHubToken string `json:"github_token"` // GitHub token for Copilot auth
}

func parseACPConfig(raw string) acpConfig {
	var cfg acpConfig
	if raw != "" {
		json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg
}

func (b *ACPBackend) BuildCLIArgs(cfg *SessionConfig) []string {
	// "acp-agent" is a subcommand of the openpoet binary itself
	args := []string{"acp-agent"}
	ac := parseACPConfig(cfg.BackendConfig)

	if ac.CopilotPath != "" {
		args = append(args, "--copilot-path", ac.CopilotPath)
	}

	if cfg.IsReopen {
		args = append(args, "--resume", cfg.SessionID)
	} else {
		args = append(args, "--session-id", cfg.SessionID)
	}

	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.AppendSystemPrompt)
	}

	if cfg.MCPConfigJSON != "" && ac.EnableMCP {
		args = append(args, "--mcp-config", cfg.MCPConfigJSON)
	}

	if ac.AutoApprove || cfg.DangerouslySkipPermissions {
		args = append(args, b.PermissionSkipFlag())
	}

	return args
}

func (b *ACPBackend) BuildEnvVars(cfg *SessionConfig) map[string]string {
	env := map[string]string{
		"OPENPOET_HOOK_URL":   "http://" + cfg.ServerAddr,
		"OPENPOET_SESSION_ID": cfg.SessionID,
	}

	if cfg.ExecPath != "" {
		env["OPENPOET_BIN"] = cfg.ExecPath
	}

	ac := parseACPConfig(cfg.BackendConfig)
	if ac.GitHubToken != "" {
		env["GITHUB_TOKEN"] = ac.GitHubToken
	}

	return env
}

func (b *ACPBackend) StartupMessage(binaryPath, workDir string) string {
	return fmt.Sprintf("\x1b[90mStarting Copilot ACP agent from: %s\r\nWorking directory: %s\x1b[0m\r\n\r\n", binaryPath, workDir)
}

func (b *ACPBackend) NotFoundMessage() string {
	return "\r\n\x1b[31mError: OpenPoet binary not found.\r\nThe ACP agent runs as 'openpoet acp-agent' subcommand.\x1b[0m\r\n"
}
