package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ClaudeCodeBackend implements BackendStrategy for Claude Code CLI.
type ClaudeCodeBackend struct{}

type claudeCodeConfig struct {
	Provider         string `json:"provider"`
	ProviderConfigID int64  `json:"provider_config_id"`
	Model            string `json:"model"`
	SmallModel       string `json:"small_model"`
}

func parseClaudeCodeConfig(raw string) claudeCodeConfig {
	var cfg claudeCodeConfig
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	cfg.SmallModel = strings.TrimSpace(cfg.SmallModel)
	return cfg
}

func (b *ClaudeCodeBackend) Type() BackendType          { return BackendClaudeCode }
func (b *ClaudeCodeBackend) BinaryName() string         { return "claude" }
func (b *ClaudeCodeBackend) SupportsResume() bool       { return true }
func (b *ClaudeCodeBackend) SupportsOTEL() bool         { return true }
func (b *ClaudeCodeBackend) SupportsPlanCapture() bool  { return true }
func (b *ClaudeCodeBackend) SupportsAskUser() bool      { return true }
func (b *ClaudeCodeBackend) PermissionSkipFlag() string { return "--dangerously-skip-permissions" }
func (b *ClaudeCodeBackend) HookFormat() string         { return "claude" }

func (b *ClaudeCodeBackend) BuildCLIArgs(cfg *SessionConfig) []string {
	var args []string
	cc := parseClaudeCodeConfig(cfg.BackendConfig)

	if cfg.IsReopen {
		resumeID := strings.TrimSpace(cfg.ProviderSessionID)
		if resumeID == "" {
			resumeID = cfg.SessionID
		}
		args = append(args, "--resume", resumeID)
	} else {
		args = append(args, "--session-id", cfg.SessionID)
	}

	if cfg.MCPConfigJSON != "" {
		args = append(args, "--mcp-config", cfg.MCPConfigJSON)
	}

	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
	}

	if cfg.DangerouslySkipPermissions {
		args = append(args, b.PermissionSkipFlag())
	}

	if cc.Model != "" && !strings.EqualFold(cc.Model, "default") {
		args = append(args, "--model", cc.Model)
	}

	return args
}

func (b *ClaudeCodeBackend) BuildEnvVars(cfg *SessionConfig) map[string]string {
	env := map[string]string{
		"OPENPOET_HOOK_URL":            "http://" + cfg.ServerAddr,
		"OPENPOET_SESSION_ID":          cfg.SessionID,
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "http/json",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://" + cfg.ServerAddr,
		"OTEL_RESOURCE_ATTRIBUTES":     "openpoet.session_id=" + cfg.SessionID,
		"OTEL_METRIC_EXPORT_INTERVAL":  "10000",
	}
	applySessionTokenEnv(env, cfg)
	return env
}

func (b *ClaudeCodeBackend) StartupMessage(binaryPath, workDir string) string {
	return fmt.Sprintf("\x1b[90mStarting Claude Code from: %s\r\nWorking directory: %s\x1b[0m\r\n\r\n", binaryPath, workDir)
}

func (b *ClaudeCodeBackend) NotFoundMessage() string {
	return "\r\n\x1b[31mError: Claude Code CLI not found in PATH.\r\nPlease install it with: npm install -g @anthropic-ai/claude-code\x1b[0m\r\n"
}
