package session

// BackendType identifies the CLI backend for a session.
type BackendType string

const (
	BackendClaudeCode BackendType = "claude_code"
	BackendCopilot    BackendType = "copilot"
	BackendACP        BackendType = "acp"
	BackendCodex      BackendType = "codex"
	BackendOpenCode   BackendType = "opencode"
)

// SessionConfig holds parameters needed to build CLI args and env vars for a session.
type SessionConfig struct {
	SessionID         string
	ProviderSessionID string // Native backend thread/session ID, when known
	ServerAddr        string // OpenPoet server address (e.g., "localhost:8080")
	MCPConfigJSON     string // JSON for --mcp-config flag
	ExecPath          string // Resolved path to the openpoet binary
	IsReopen          bool   // true when resuming a previous session

	// Per-session credentials (Phase 0 hardening). MCPToken is the opst1_
	// bearer the injected MCP/CLI caller presents; HookToken is the opht1_
	// value the hook bridge sends as X-Hook-Token. Both are minted at start
	// and only their SHA-256 digests live in the sessions row.
	MCPToken  string
	HookToken string

	// Env var passthrough from API handler
	AppendSystemPrompt         string // task context prompt
	DangerouslySkipPermissions bool   // whether to skip permission prompts
	BackendConfig              string // JSON blob with backend-specific settings
}

// BackendStrategy defines how a specific CLI backend builds its args and env vars.
type BackendStrategy interface {
	// Type returns the backend identifier.
	Type() BackendType

	// BinaryName returns the CLI command to exec (e.g., "claude", "copilot").
	BinaryName() string

	// BuildCLIArgs returns CLI arguments for starting/resuming a session.
	BuildCLIArgs(cfg *SessionConfig) []string

	// BuildEnvVars returns backend-specific env vars to inject.
	BuildEnvVars(cfg *SessionConfig) map[string]string

	// SupportsResume returns true if the backend supports --resume.
	SupportsResume() bool

	// SupportsOTEL returns true if the backend emits OpenTelemetry metrics.
	SupportsOTEL() bool

	// SupportsPlanCapture returns true if the backend exposes plan content via hooks.
	SupportsPlanCapture() bool

	// SupportsAskUser returns true if the backend has an AskUserQuestion tool.
	SupportsAskUser() bool

	// PermissionSkipFlag returns the CLI flag for autonomous mode (e.g., "--dangerously-skip-permissions").
	// Returns empty string if not supported.
	PermissionSkipFlag() string

	// HookFormat returns the hook configuration format ("claude" or "copilot").
	HookFormat() string

	// StartupMessage returns the terminal message shown when starting.
	StartupMessage(binaryPath, workDir string) string

	// NotFoundMessage returns the error message when binary is not in PATH.
	NotFoundMessage() string
}

// applySessionTokenEnv injects the per-session credentials into a backend's
// environment when present: OPENPOET_HOOK_TOKEN authenticates the hook bridge's
// posts, OPENPOET_SESSION_TOKEN authenticates the CLI/MCP caller. Both are
// harmless no-ops when unset (legacy path). Backends call this from BuildEnvVars.
func applySessionTokenEnv(env map[string]string, cfg *SessionConfig) {
	if cfg == nil {
		return
	}
	if cfg.HookToken != "" {
		env["OPENPOET_HOOK_TOKEN"] = cfg.HookToken
	}
	if cfg.MCPToken != "" {
		env["OPENPOET_SESSION_TOKEN"] = cfg.MCPToken
	}
}

// GetBackend returns the strategy for the given backend type.
// Returns the Claude Code backend as default for unknown types.
func GetBackend(backendType string) BackendStrategy {
	switch BackendType(backendType) {
	case BackendCopilot:
		return &CopilotBackend{}
	case BackendACP:
		return &ACPBackend{}
	case BackendCodex:
		return &CodexBackend{}
	case BackendOpenCode:
		return &OpenCodeBackend{}
	default:
		return &ClaudeCodeBackend{}
	}
}
