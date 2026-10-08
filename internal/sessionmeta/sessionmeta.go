package sessionmeta

import (
	"encoding/json"
	"errors"
	"fmt"
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

	// Model and Effort stay empty when the project does not set them: a
	// session resolves them explicitly (see Resolve), never as "default".
	meta := Metadata{Harness: backend}

	switch backend {
	case "codex":
		if model := firstString(cfg, "model"); model != "" {
			meta.Model = model
		}
		if effort := firstString(cfg, "reasoning_effort", "effort"); effort != "" {
			meta.Effort = effort
		}
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
		if model := firstString(cfg, "model"); model != "" {
			meta.Model = model
		}
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
			meta.Effort = firstString(cfg, "reasoning_effort", "effort")
			meta.Harness = "claude_code/openai"
			meta.HarnessDetails = joinDetails(
				"provider: OpenAI OAuth",
				detailIfSet("profile", firstNumberString(cfg, "provider_config_id")),
			)
		case "anthropic":
			if model := firstString(cfg, "model"); model != "" {
				meta.Model = model
			}
			meta.Effort = firstString(cfg, "reasoning_effort", "effort")
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

// Sources a resolved model or effort can come from.
const (
	SourceRequest = "request" // the create/reopen call named it
	SourceSession = "session" // a reopened session keeps what it ran with
	SourceProject = "project" // the project's backend_config
	SourceGlobal  = "global"  // the global default setting for the backend
)

// ErrUnresolved means no explicit model or effort could be found for a
// session. Sessions never fall back to the CLI's account default.
var ErrUnresolved = errors.New("session model/effort not configured")

// Defaults are the global default model and effort of one backend.
type Defaults struct {
	Model  string
	Effort string
}

// DefaultModelSetting and DefaultEffortSetting are the settings keys holding
// a backend's global defaults (e.g. default_model_codex).
func DefaultModelSetting(backend string) string { return "default_model_" + strings.TrimSpace(backend) }
func DefaultEffortSetting(backend string) string {
	return "default_effort_" + strings.TrimSpace(backend)
}

// Selectable reports which runtime settings OpenPoet passes explicitly to a
// backend's CLI. Backends without them (copilot, acp) run what they run.
func Selectable(backend string) (model, effort bool) {
	switch strings.TrimSpace(backend) {
	case "claude_code", "codex":
		return true, true
	case "opencode":
		return true, false
	}
	return false, false
}

// RuntimeSettings is the explicit model and effort a session runs with and
// where each one came from.
type RuntimeSettings struct {
	Model        string
	Effort       string
	ModelSource  string
	EffortSource string
}

// Explicit reports whether value names a concrete setting: empty, "default"
// and "reset" all mean "let the CLI pick", which sessions never do.
func Explicit(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.EqualFold(value, "default") && !strings.EqualFold(value, "reset")
}

// Resolve picks each setting from the first explicit source: the request,
// then the project's backend config, then the backend's global default. It
// fails with ErrUnresolved when a setting the backend takes has no explicit
// value anywhere. The global model default never applies to Claude Code on
// the OpenAI provider, whose project must name an OpenAI model itself.
func Resolve(backend, rawConfig, requestModel, requestEffort string, global Defaults) (RuntimeSettings, error) {
	wantModel, wantEffort := Selectable(backend)
	meta := FromProjectConfig(backend, rawConfig)
	var out RuntimeSettings
	var missing []string
	if wantModel {
		globalModel := global.Model
		if meta.Harness == "claude_code/openai" {
			globalModel = ""
		}
		out.Model, out.ModelSource = pick(requestModel, meta.Model, globalModel)
		if out.Model == "" {
			missing = append(missing, "model")
		}
	}
	if wantEffort {
		out.Effort, out.EffortSource = pick(requestEffort, meta.Effort, global.Effort)
		if out.Effort == "" {
			missing = append(missing, "effort")
		}
	}
	if len(missing) > 0 {
		what := strings.Join(missing, " and ")
		return out, fmt.Errorf("%w: no explicit %s for backend %s; pass %s in the request, set it in the project's backend_config, or set the global default (settings %s)",
			ErrUnresolved, what, backend, what, strings.Join(defaultSettingKeys(backend, missing), ", "))
	}
	return out, nil
}

func pick(request, project, global string) (string, string) {
	switch {
	case Explicit(request):
		return strings.TrimSpace(request), SourceRequest
	case Explicit(project):
		return strings.TrimSpace(project), SourceProject
	case Explicit(global):
		return strings.TrimSpace(global), SourceGlobal
	}
	return "", ""
}

func defaultSettingKeys(backend string, missing []string) []string {
	keys := make([]string, 0, len(missing))
	for _, m := range missing {
		if m == "model" {
			keys = append(keys, DefaultModelSetting(backend))
		} else {
			keys = append(keys, DefaultEffortSetting(backend))
		}
	}
	return keys
}

// SessionValues are a session row's model and effort columns, for display.
type SessionValues struct {
	Backend         string
	Model           string // runtime-reported model ("unknown" until reported)
	RequestedModel  string // configured model
	Effort          string // configured effort
	EffectiveEffort string // runtime-reported effort
	ModelSource     string
	EffortSource    string
}

// ModelLine describes what model a session runs, e.g.
// "claude-opus-5-5 (configured opus from project; reported by the runtime)".
func (v SessionValues) ModelLine() string {
	effective := strings.TrimSpace(v.Model)
	if strings.EqualFold(effective, "unknown") {
		effective = ""
	}
	configured := strings.TrimSpace(v.RequestedModel)
	if configured == "" && v.Backend != "claude_code" {
		configured = effective
	}
	return describeSetting(effective, configured, v.ModelSource)
}

// EffortLine describes what effort a session runs, like ModelLine.
func (v SessionValues) EffortLine() string {
	return describeSetting(strings.TrimSpace(v.EffectiveEffort), strings.TrimSpace(v.Effort), v.EffortSource)
}

func describeSetting(effective, configured, source string) string {
	if configured != "" && !Explicit(configured) {
		// Sessions from before explicit settings ran the CLI's account default.
		if effective == "" {
			return "unknown (session predates explicit settings; the runtime has not reported it)"
		}
		return effective + " (reported by the runtime; session predates explicit settings)"
	}
	origin := ""
	if configured != "" {
		origin = "configured " + configured
		if source != "" {
			origin += " from " + source
		}
	}
	switch {
	case effective == "" && configured == "":
		return "not set"
	case effective == "":
		return configured + " (" + origin + "; not yet reported by the runtime)"
	case configured == "" || strings.EqualFold(configured, effective):
		if origin == "" {
			return effective + " (reported by the runtime)"
		}
		return effective + " (" + origin + "; reported by the runtime)"
	}
	return effective + " (reported by the runtime; " + origin + ")"
}

// WithProjectFallback fills a row that recorded neither a configured nor a
// reported value (sessions from before these columns) with the project's.
func (v SessionValues) WithProjectFallback(project Metadata) SessionValues {
	if strings.TrimSpace(v.RequestedModel) == "" && (strings.TrimSpace(v.Model) == "" || strings.EqualFold(v.Model, "unknown")) && project.Model != "" {
		v.RequestedModel, v.ModelSource = project.Model, SourceProject
	}
	if strings.TrimSpace(v.Effort) == "" && strings.TrimSpace(v.EffectiveEffort) == "" && project.Effort != "" {
		v.Effort, v.EffortSource = project.Effort, SourceProject
	}
	return v
}
