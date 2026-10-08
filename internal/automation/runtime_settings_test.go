package automation

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"openpoet/internal/database"
)

func TestMergeProjectUpdateIsPartialAndWritesModelEffort(t *testing.T) {
	current := &database.Project{
		ID: 7, Name: "codex-app", Path: "/srv/app", Type: "remote", Backend: "codex",
		SSHHost: sql.NullString{String: "host", Valid: true}, SSHPort: sql.NullInt64{Int64: 22, Valid: true},
		ToolPolicy: `{"mode":"deny"}`, DangerouslySkipPermissions: true, TaskAutoApproveVerification: "enabled",
		BackendConfig: `{"runtime":"app-server","sandbox_mode":"workspace-write"}`,
	}
	model, effort := "gpt-6-astra", "HIGH"
	input, err := mergeProjectUpdate(current, projectUpdatePayload{Model: &model, Effort: &effort})
	if err != nil {
		t.Fatal(err)
	}
	if input.Name != current.Name || input.ToolPolicy != current.ToolPolicy || !input.DangerouslySkipPermissions || input.SSHHost != "host" || input.SSHCredential != "" {
		t.Fatalf("unsent fields changed: %+v", input)
	}
	var config map[string]any
	_ = json.Unmarshal([]byte(input.BackendConfig), &config)
	if config["model"] != "gpt-6-astra" || config["reasoning_effort"] != "high" || config["sandbox_mode"] != "workspace-write" {
		t.Fatalf("config = %v", config)
	}

	empty := ""
	input, _ = mergeProjectUpdate(&database.Project{Backend: "codex", BackendConfig: input.BackendConfig}, projectUpdatePayload{Model: &empty})
	if strings.Contains(input.BackendConfig, "gpt-6-astra") || !strings.Contains(input.BackendConfig, "reasoning_effort") {
		t.Fatalf("empty model must clear only the model: %s", input.BackendConfig)
	}

	// Claude Code settings need a provider; the shortcut adds anthropic.
	opus := "opus"
	input, _ = mergeProjectUpdate(&database.Project{Backend: "claude_code", BackendConfig: `{}`}, projectUpdatePayload{Model: &opus})
	if !strings.Contains(input.BackendConfig, `"provider":"anthropic"`) {
		t.Fatalf("claude config = %s", input.BackendConfig)
	}

	// Switching backend resets the settings; backend_config may be a string.
	codex := "codex"
	input, err = mergeProjectUpdate(&database.Project{Backend: "claude_code", BackendConfig: `{"provider":"anthropic","model":"opus"}`},
		projectUpdatePayload{Backend: &codex, BackendConfig: json.RawMessage(`"{\"runtime\":\"tui\"}"`)})
	if err != nil || input.BackendConfig != `{"runtime":"tui"}` {
		t.Fatalf("backend switch = %s err=%v", input.BackendConfig, err)
	}
	if _, err := mergeProjectUpdate(&database.Project{Backend: "copilot"}, projectUpdatePayload{Effort: &effort}); err == nil {
		t.Fatal("copilot takes no effort")
	}
	if _, err := mergeProjectUpdate(current, projectUpdatePayload{BackendConfig: json.RawMessage(`[1]`)}); err == nil {
		t.Fatal("backend_config must be an object")
	}
}

func TestProjectViewPublishesBackendConfigModelAndEffort(t *testing.T) {
	view := projectAutomationView(database.Project{ID: 1, Backend: "codex", BackendConfig: `{"model":"gpt-6-astra","reasoning_effort":"high","api_key":"sk-x"}`})
	if view.Model != "gpt-6-astra" || view.Effort != "high" || view.BackendConfig["api_key"] != "********" {
		t.Fatalf("view = %+v", view)
	}
	defaults := projectSessionDefaults(t.Context(), nil, database.Project{Backend: "codex", BackendConfig: `{"model":"gpt-6-astra"}`})
	if defaults == nil || defaults.Model != "gpt-6-astra" || defaults.ModelSource != "project" || !strings.Contains(defaults.Error, "effort") {
		t.Fatalf("session defaults = %+v", defaults)
	}
	if projectSessionDefaults(t.Context(), nil, database.Project{Backend: "copilot"}) != nil {
		t.Fatal("copilot has no session defaults")
	}
}

func TestSessionViewShowsEffectiveSettingsAndDivergence(t *testing.T) {
	// A session from before explicit settings: nothing configured, the
	// runtime ran its account default.
	view := sessionAutomationView(database.Session{ID: "s", Backend: "codex", Model: "gpt-6.1-sol", RequestedModel: "default", Effort: "default"}, nil)
	if view.Model != "gpt-6.1-sol" || view.Effort != "unknown" || view.RuntimeSettings == nil || len(view.RuntimeSettings.Warnings) != 2 {
		t.Fatalf("legacy view = %+v %+v", view, view.RuntimeSettings)
	}

	view = sessionAutomationView(database.Session{ID: "s", Backend: "codex", Model: "gpt-6.1-sol", RequestedModel: "gpt-6-astra",
		Effort: "high", EffectiveEffort: "low", ModelSource: "project", EffortSource: "global"}, nil)
	rs := view.RuntimeSettings
	if view.Model != "gpt-6.1-sol" || view.Effort != "low" || !rs.ModelVerified || !rs.EffortVerified || len(rs.Warnings) != 2 || rs.ModelSource != "project" {
		t.Fatalf("divergent view = %+v %+v", view, rs)
	}

	view = sessionAutomationView(database.Session{ID: "s", Backend: "claude_code", Model: "claude-opus-5-5", RequestedModel: "opus", Effort: "high", EffectiveEffort: "high"}, nil)
	if len(view.RuntimeSettings.Warnings) != 0 || view.Model != "claude-opus-5-5" {
		t.Fatalf("alias match must not warn: %+v", view.RuntimeSettings)
	}

	view = sessionAutomationView(database.Session{ID: "s", Backend: "claude_code", Model: "unknown", RequestedModel: "opus", Effort: "max"}, nil)
	if view.Model != "opus" || view.Effort != "max" || view.RuntimeSettings.ModelVerified || view.RuntimeSettings.EffortVerified {
		t.Fatalf("unreported view = %+v %+v", view, view.RuntimeSettings)
	}

	if v := sessionAutomationView(database.Session{ID: "s", Backend: "copilot"}, nil); v.RuntimeSettings != nil || v.Model != "" {
		t.Fatalf("copilot view = %+v", v)
	}
}
