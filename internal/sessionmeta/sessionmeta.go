package sessionmeta

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Metadata describes the AI harness configuration visible to session tools.
type Metadata struct {
	Model          string
	Effort         string
	Harness        string
	HarnessDetails string
}

// WithSessionValues overlays runtime values persisted for a specific session.
// Empty values retain the project-derived fallback for pre-migration sessions.
func WithSessionValues(meta Metadata, model, effort, harness string) Metadata {
	if value := strings.TrimSpace(model); value != "" {
		meta.Model = value
	}
	if value := strings.TrimSpace(effort); value != "" {
		meta.Effort = value
	}
	if value := strings.TrimSpace(harness); value != "" {
		meta.Harness = value
	}
	return meta
}

// ApplyRuntimeValues returns a backend config snapshot with the session's
// persisted model and effort. "default" removes the project-level override.
func ApplyRuntimeValues(rawConfig, model, effort string) string {
	var cfg map[string]interface{}
	if strings.TrimSpace(rawConfig) != "" {
		_ = json.Unmarshal([]byte(rawConfig), &cfg)
	}
	if cfg == nil {
		cfg = make(map[string]interface{})
	}
	if value := strings.TrimSpace(model); value != "" {
		if strings.EqualFold(value, "default") {
			delete(cfg, "model")
		} else {
			cfg["model"] = value
		}
	}
	if value := strings.TrimSpace(effort); value != "" {
		delete(cfg, "effort")
		if strings.EqualFold(value, "default") {
			delete(cfg, "reasoning_effort")
		} else {
			cfg["reasoning_effort"] = value
		}
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return rawConfig
	}
	return string(encoded)
}

// FromProjectConfig derives initial/fallback session metadata from a project's
// backend configuration. Runtime changes are overlaid with WithSessionValues.
func FromProjectConfig(backend, rawConfig string) Metadata {
	backend = strings.TrimSpace(backend)
	var cfg map[string]interface{}
	if strings.TrimSpace(rawConfig) != "" {
		_ = json.Unmarshal([]byte(rawConfig), &cfg)
	}

	meta := Metadata{
		Model:   firstString(cfg, "model"),
		Effort:  firstString(cfg, "reasoning_effort", "effort"),
		Harness: backend,
	}
	if meta.Model == "" {
		meta.Model = "default"
	}
	if meta.Effort == "" {
		meta.Effort = "default"
	}

	switch backend {
	case "codex":
		runtime := normalizeCodexRuntime(firstString(cfg, "runtime"))
		approval := normalizeCodexApprovalPolicy(firstString(cfg, "approval_policy"))
		sandbox := normalizeCodexSandboxMode(firstString(cfg, "sandbox_mode"))
		meta.Harness = "codex/" + runtime
		meta.HarnessDetails = joinDetails(
			"runtime: "+runtime,
			"approval: "+approval,
			"sandbox: "+sandbox,
		)
	case "opencode":
		agent := firstString(cfg, "agent")
		permissionMode := firstString(cfg, "permission_mode")
		meta.Harness = "opencode"
		meta.HarnessDetails = joinDetails(
			detailIfSet("agent", agent),
			detailIfSet("permission", permissionMode),
		)
	case "claude_code":
		switch strings.ToLower(firstString(cfg, "provider")) {
		case "openai", "openai_oauth":
			if model := firstString(cfg, "model"); model != "" {
				meta.Model = model
			}
			meta.Harness = "claude_code/openai"
			meta.HarnessDetails = joinDetails(
				"provider: OpenAI OAuth",
				detailIfSet("profile", firstNumberString(cfg, "provider_config_id")),
			)
		case "anthropic":
			if model := firstString(cfg, "model"); model != "" {
				meta.Model = model
			}
			meta.Harness = "claude_code/anthropic"
		default:
			meta.Harness = "claude_code"
		}
	case "copilot":
		meta.Harness = "copilot"
	case "acp":
		meta.Harness = "acp"
	}

	return meta
}

func firstNumberString(cfg map[string]interface{}, key string) string {
	if cfg == nil {
		return ""
	}
	switch value := cfg[key].(type) {
	case float64:
		if value > 0 {
			return strconv.FormatInt(int64(value), 10)
		}
	case json.Number:
		return value.String()
	}
	return ""
}

func firstString(cfg map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if cfg == nil {
			return ""
		}
		if raw, ok := cfg[key]; ok {
			if s, ok := raw.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func normalizeCodexRuntime(v string) string {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "", "app-server", "app_server", "appserver":
		return "app-server"
	default:
		return "tui"
	}
}

func normalizeCodexApprovalPolicy(v string) string {
	switch strings.TrimSpace(v) {
	case "untrusted", "on-request", "never":
		return strings.TrimSpace(v)
	case "read-only", "approval-required":
		return "untrusted"
	case "full-access", "danger-full-access":
		return "never"
	default:
		return "on-request"
	}
}

func normalizeCodexSandboxMode(v string) string {
	switch strings.TrimSpace(v) {
	case "read-only", "workspace-write", "danger-full-access":
		return strings.TrimSpace(v)
	case "full-access":
		return "danger-full-access"
	default:
		return "workspace-write"
	}
}

func detailIfSet(label, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return label + ": " + strings.TrimSpace(value)
}

func joinDetails(parts ...string) string {
	var out []string
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return strings.Join(out, " | ")
}
