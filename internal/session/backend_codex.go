package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CodexBackend implements BackendStrategy for OpenAI Codex.
//
// Local Codex sessions run the interactive Codex CLI TUI in a PTY. This keeps
// slash commands, Tab completion, pickers, and newly-added Codex commands owned
// by Codex itself instead of reimplementing TUI behavior in OpenPoet.
type CodexBackend struct{}

func (b *CodexBackend) Type() BackendType         { return BackendCodex }
func (b *CodexBackend) BinaryName() string        { return "codex" }
func (b *CodexBackend) SupportsResume() bool      { return true }
func (b *CodexBackend) SupportsOTEL() bool        { return false }
func (b *CodexBackend) SupportsPlanCapture() bool { return false }
func (b *CodexBackend) SupportsAskUser() bool     { return false }
func (b *CodexBackend) PermissionSkipFlag() string {
	return "--dangerously-bypass-approvals-and-sandbox"
}
func (b *CodexBackend) HookFormat() string { return "codex" }

// CodexConfig holds Codex-specific settings from project.backend_config JSON.
type CodexConfig struct {
	BinaryPath      string `json:"binary_path"`
	HomePath        string `json:"home_path"`
	Runtime         string `json:"runtime"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	ServiceTier     string `json:"service_tier"`
	ApprovalPolicy  string `json:"approval_policy"`
	SandboxMode     string `json:"sandbox_mode"`
}

func parseCodexConfig(raw string) CodexConfig {
	cfg := CodexConfig{
		ApprovalPolicy: "on-request",
		SandboxMode:    "workspace-write",
		Runtime:        "app-server",
	}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.ApprovalPolicy = normalizeCodexApprovalPolicy(cfg.ApprovalPolicy)
	cfg.SandboxMode = normalizeCodexSandboxMode(cfg.SandboxMode)
	cfg.Runtime = normalizeCodexRuntime(cfg.Runtime)
	return cfg
}

func normalizeCodexRuntime(v string) string {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "app-server", "app_server", "appserver":
		return "app-server"
	default:
		return "tui"
	}
}

func normalizeCodexApprovalPolicy(v string) string {
	switch s := strings.TrimSpace(v); s {
	case "untrusted", "on-request", "never":
		return s
	case "read-only", "approval-required":
		return "untrusted"
	case "full-access", "danger-full-access":
		return "never"
	default:
		return "on-request"
	}
}

func normalizeCodexSandboxMode(v string) string {
	switch s := strings.TrimSpace(v); s {
	case "read-only", "workspace-write", "danger-full-access":
		return s
	case "full-access":
		return "danger-full-access"
	default:
		return "workspace-write"
	}
}

func (b *CodexBackend) BuildCLIArgs(cfg *SessionConfig) []string {
	cc := parseCodexConfig(cfg.BackendConfig)
	args := []string{
		"--no-alt-screen",
		"--ask-for-approval", cc.ApprovalPolicy,
		"--sandbox", cc.SandboxMode,
	}
	if cc.Model != "" {
		args = append(args, "--model", cc.Model)
	}
	if cc.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+codexTomlQuotedValue(cc.ReasoningEffort))
	}
	if cc.ServiceTier != "" {
		args = append(args, "-c", "service_tier="+codexTomlQuotedValue(cc.ServiceTier))
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if cfg.IsReopen {
		resumeArgs := append([]string{"resume"}, args...)
		if cfg.ProviderSessionID != "" {
			resumeArgs = append(resumeArgs, cfg.ProviderSessionID)
		}
		args = resumeArgs
	}
	return args
}

func codexTomlQuotedValue(v string) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func (b *CodexBackend) BuildEnvVars(cfg *SessionConfig) map[string]string {
	env := map[string]string{
		"OPENPOET_HOOK_URL":   "http://" + cfg.ServerAddr,
		"OPENPOET_SESSION_ID": cfg.SessionID,
		"OPENPOET_BIN":        cfg.ExecPath,
	}
	cc := parseCodexConfig(cfg.BackendConfig)
	if cc.HomePath != "" {
		env["CODEX_HOME"] = expandCodexHome(cc.HomePath)
	}
	if cc.BinaryPath != "" {
		env["OPENPOET_BACKEND_BINARY"] = expandCodexHome(cc.BinaryPath)
	}
	return env
}

func (b *CodexBackend) StartupMessage(binaryPath, workDir string) string {
	return fmt.Sprintf("\x1b[90mStarting Codex CLI from: %s\r\nWorking directory: %s\x1b[0m\r\n\r\n", binaryPath, workDir)
}

func (b *CodexBackend) NotFoundMessage() string {
	return "\r\n\x1b[31mError: Codex CLI not found in PATH.\r\nPlease install or configure the OpenAI Codex CLI.\x1b[0m\r\n"
}
