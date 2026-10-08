package automation

import (
	"context"
	"encoding/json"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessionmeta"
)

// ProjectSessionDefaultsView says which model and effort a new session of the
// project starts with (request aside) and where each comes from. Error says
// why a session would be refused instead.
type ProjectSessionDefaultsView struct {
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ModelSource  string `json:"model_source,omitempty"`
	EffortSource string `json:"effort_source,omitempty"`
	Error        string `json:"error,omitempty"`
}

// backendConfigSecretMarkers mark backend_config keys whose values are
// masked in views. Backend configs hold no secrets today; this keeps a future
// one from leaking through projects.get.
var backendConfigSecretMarkers = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "credential", "private_key"}

// maskedBackendConfig parses a project's backend_config for display, masking
// any value whose key looks secret.
func maskedBackendConfig(raw string) map[string]any {
	config := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &config)
	}
	for key, value := range config {
		lower := strings.ToLower(key)
		for _, marker := range backendConfigSecretMarkers {
			if strings.Contains(lower, marker) {
				if s, ok := value.(string); !ok || s != "" {
					config[key] = "********"
				}
				break
			}
		}
	}
	return config
}

// projectSessionDefaults resolves what a new session of the project gets.
func projectSessionDefaults(ctx context.Context, validator *application.RuntimeSettingsValidator, project database.Project) *ProjectSessionDefaultsView {
	if takesModel, _ := sessionmeta.Selectable(project.Backend); !takesModel {
		return nil
	}
	settings, err := sessionmeta.Resolve(project.Backend, project.BackendConfig, "", "", validator.Defaults(ctx, project.Backend))
	view := &ProjectSessionDefaultsView{
		Model: settings.Model, Effort: settings.Effort, ModelSource: settings.ModelSource, EffortSource: settings.EffortSource,
	}
	if err != nil {
		view.Error = err.Error()
	}
	return view
}

// projectUpdatePayload is a partial project update: only the fields sent
// change. model and effort are shortcuts into backend_config.
type projectUpdatePayload struct {
	Name                        *string         `json:"name,omitempty"`
	Path                        *string         `json:"path,omitempty"`
	Type                        *string         `json:"type,omitempty" doc:"local or remote"`
	SSHHost                     *string         `json:"ssh_host,omitempty"`
	SSHPort                     *int            `json:"ssh_port,omitempty"`
	SSHUser                     *string         `json:"ssh_user,omitempty"`
	SSHAuthType                 *string         `json:"ssh_auth_type,omitempty"`
	SSHCredential               *string         `json:"ssh_credential,omitempty" doc:"new SSH credential; omit to keep the stored one"`
	ToolPolicy                  *string         `json:"tool_policy,omitempty"`
	SkillPolicy                 *string         `json:"skill_policy,omitempty"`
	DangerouslySkipPermissions  *bool           `json:"dangerously_skip_permissions,omitempty"`
	TaskAutoApproveVerification *string         `json:"task_auto_approve_verification,omitempty" doc:"inherit, enabled or disabled"`
	CoordinatorMode             *string         `json:"coordinator_mode,omitempty"`
	ConflictPolicy              *string         `json:"conflict_policy,omitempty"`
	Backend                     *string         `json:"backend,omitempty" doc:"claude_code, copilot, acp, codex or opencode; switching backend resets backend_config unless backend_config is sent too"`
	BackendConfig               json.RawMessage `json:"backend_config,omitempty" doc:"object (or JSON-encoded string) that REPLACES the backend settings; codex: runtime, model, reasoning_effort, service_tier, approval_policy, sandbox_mode, binary_path, home_path; claude_code: provider (anthropic|openai_oauth), provider_config_id, model, reasoning_effort, small_model"`
	Model                       *string         `json:"model,omitempty" doc:"set the backend's model (claude_code, codex, opencode); \"\" removes it so the global default applies. See models.list for accepted ids"`
	Effort                      *string         `json:"effort,omitempty" doc:"set the reasoning effort (claude_code, codex); \"\" removes it so the global default applies. See models.list for the levels each model accepts"`
}

const projectsUpdateNotes = "Partial update: only the fields sent change (name, path, type, ssh_*, policies, backend, backend_config, model, effort); everything else keeps its stored value. " +
	"model and effort write into backend_config (reasoning_effort) and are validated against the model list the backend's CLI reports (models.list): an unknown model, an effort the model does not offer, or \"default\" fails with runtime_setting_invalid. " +
	"An empty string removes the project's value so the global default (settings default_model_<backend>, default_effort_<backend>) applies. " +
	"Sessions always run an explicit model and effort: request, then project, then global default; with none of them a session is refused (session_settings_unresolved). " +
	"Returns the project view, whose session_defaults shows what a new session would get."

const projectsGetNotes = "Returns the project with backend and backend_config (the backend settings object; secret-looking values masked), " +
	"model and effort (the project's own, empty when it relies on the global default) and session_defaults {model, effort, model_source (project|global), effort_source, error}: " +
	"what a new session starts with when the request names nothing; error says why a session would be refused. Change them with projects.update."

const modelsListNotes = "Lists the models a backend's CLI reports (the same source as the UI's model picker, cached for an hour; refresh true probes again) " +
	"with the reasoning efforts each model accepts (efforts) and its default_effort when the CLI reports one, the union of effort levels, and the backend's global defaults " +
	"(global_defaults {model, effort, model_setting, effort_setting}, changed with settings.update). With a project target the project's backend is used; otherwise payload backend is required. " +
	"catalog_error is set when the CLI could not be probed (models is then empty and only syntax is validated)."

type modelsListPayload struct {
	Backend string `json:"backend,omitempty" doc:"claude_code, codex or opencode (default: the target project's backend)"`
	Refresh bool   `json:"refresh,omitempty" doc:"probe the CLI again instead of using the cached list"`
}

// projectInputFromCurrent is the full ProjectInput that leaves a project as
// it is; a partial update overlays the fields it sends.
func projectInputFromCurrent(project *database.Project) database.ProjectInput {
	return database.ProjectInput{
		Name: project.Name, Path: project.Path, Type: project.Type,
		SSHHost: project.SSHHost.String, SSHPort: int(project.SSHPort.Int64), SSHUser: project.SSHUser.String,
		SSHAuthType: project.SSHAuthType.String, ToolPolicy: project.ToolPolicy, SkillPolicy: project.SkillPolicy,
		DangerouslySkipPermissions:  project.DangerouslySkipPermissions,
		TaskAutoApproveVerification: project.TaskAutoApproveVerification,
		CoordinatorMode:             project.CoordinatorMode, ConflictPolicy: project.ConflictPolicy,
		Backend: project.Backend, BackendConfig: project.BackendConfig,
	}
}

// mergeProjectUpdate overlays a partial update on the current project.
func mergeProjectUpdate(current *database.Project, payload projectUpdatePayload) (database.ProjectInput, error) {
	input := projectInputFromCurrent(current)
	setString := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	setString(&input.Name, payload.Name)
	setString(&input.Path, payload.Path)
	setString(&input.Type, payload.Type)
	setString(&input.SSHHost, payload.SSHHost)
	setString(&input.SSHUser, payload.SSHUser)
	setString(&input.SSHAuthType, payload.SSHAuthType)
	setString(&input.SSHCredential, payload.SSHCredential)
	setString(&input.ToolPolicy, payload.ToolPolicy)
	setString(&input.SkillPolicy, payload.SkillPolicy)
	setString(&input.TaskAutoApproveVerification, payload.TaskAutoApproveVerification)
	setString(&input.CoordinatorMode, payload.CoordinatorMode)
	setString(&input.ConflictPolicy, payload.ConflictPolicy)
	if payload.SSHPort != nil {
		input.SSHPort = *payload.SSHPort
	}
	if payload.DangerouslySkipPermissions != nil {
		input.DangerouslySkipPermissions = *payload.DangerouslySkipPermissions
	}
	if payload.Backend != nil {
		input.Backend = strings.TrimSpace(*payload.Backend)
		if input.Backend != current.Backend {
			input.BackendConfig = "{}" // backend settings are not portable
		}
	}
	if len(payload.BackendConfig) > 0 && string(payload.BackendConfig) != "null" {
		config, err := backendConfigObject(payload.BackendConfig)
		if err != nil {
			return input, err
		}
		input.BackendConfig = config
	}
	if payload.Model != nil || payload.Effort != nil {
		config := map[string]any{}
		if strings.TrimSpace(input.BackendConfig) != "" {
			if err := json.Unmarshal([]byte(input.BackendConfig), &config); err != nil {
				config = map[string]any{}
			}
		}
		takesModel, takesEffort := sessionmeta.Selectable(input.Backend)
		if payload.Model != nil {
			if !takesModel {
				return input, platformFailure("platform_payload_invalid", "backend "+input.Backend+" takes no model", false)
			}
			setConfigValue(config, "model", *payload.Model)
		}
		if payload.Effort != nil {
			if !takesEffort {
				return input, platformFailure("platform_payload_invalid", "backend "+input.Backend+" takes no reasoning effort", false)
			}
			delete(config, "effort")
			setConfigValue(config, "reasoning_effort", strings.ToLower(*payload.Effort))
		}
		// Claude Code settings only count with an explicit provider.
		if input.Backend == "claude_code" {
			if provider, _ := config["provider"].(string); strings.TrimSpace(provider) == "" {
				config["provider"] = "anthropic"
			}
		}
		encoded, _ := json.Marshal(config)
		input.BackendConfig = string(encoded)
	}
	return input, nil
}

func setConfigValue(config map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value == "" {
		delete(config, key)
		return
	}
	config[key] = value
}

// backendConfigObject accepts backend_config as an object or as the
// JSON-encoded string the REST API stores, and returns the object encoding.
func backendConfigObject(raw json.RawMessage) (string, error) {
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		raw = json.RawMessage(asString)
		if strings.TrimSpace(asString) == "" {
			return "{}", nil
		}
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil || config == nil {
		return "", platformFailure("platform_payload_invalid", "backend_config must be a JSON object", false)
	}
	encoded, _ := json.Marshal(config)
	return string(encoded), nil
}
