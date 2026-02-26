package session

// BackendType identifies the CLI backend for a session.
type BackendType string

const (
	BackendClaudeCode BackendType = "claude_code"
	BackendCopilot    BackendType = "copilot"
)

// SessionConfig holds parameters needed to build CLI args and env vars for a session.
type SessionConfig struct {
	SessionID     string
	ServerAddr    string // OpenPoet server address (e.g., "localhost:8080")
	MCPConfigJSON string // JSON for --mcp-config flag
	IsReopen      bool   // true when resuming a previous session

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

// GetBackend returns the strategy for the given backend type.
// Returns the Claude Code backend as default for unknown types.
func GetBackend(backendType string) BackendStrategy {
	switch BackendType(backendType) {
	case BackendCopilot:
		return &CopilotBackend{}
	default:
		return &ClaudeCodeBackend{}
	}
}
