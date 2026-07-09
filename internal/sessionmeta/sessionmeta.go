package sessionmeta

import (
	"encoding/json"
	"strings"
)

// Metadata describes the AI harness configuration visible to session tools.
type Metadata struct {
	Model          string
	Effort         string
	Harness        string
	HarnessDetails string
}

// FromProjectConfig derives reportable session metadata from a project's backend
// configuration. Sessions store the backend used at creation time; the model and
// harness options currently live in the project backend_config JSON.
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
		meta.Harness = "claude_code"
	case "copilot":
		meta.Harness = "copilot"
	case "acp":
		meta.Harness = "acp"
	}

	return meta
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
