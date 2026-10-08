package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/modelcatalog"
)

type fakeModelCatalogs map[string]modelcatalog.Catalog

func (f fakeModelCatalogs) Get(_ context.Context, harness string, _ bool) modelcatalog.Catalog {
	if c, ok := f[harness]; ok {
		return c
	}
	return modelcatalog.Catalog{Harness: harness, Models: []modelcatalog.Model{}, Error: "CLI not found"}
}

type fakeRuntimeDefaults map[string]string

func (f fakeRuntimeDefaults) GetSetting(_ context.Context, key string) (string, error) {
	if v, ok := f[key]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func testRuntimeValidator(settings fakeRuntimeDefaults) *RuntimeSettingsValidator {
	return NewRuntimeSettingsValidator(fakeModelCatalogs{
		"codex": {Harness: "codex", Models: []modelcatalog.Model{
			{ID: "gpt-6.1-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, IsDefault: true},
			{ID: "gpt-6-luna", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
		}},
		"claude_code": {Harness: "claude_code", Models: []modelcatalog.Model{
			{ID: "opus", ResolvedID: "claude-opus-5-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
			{ID: "claude-opus-5-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
			{ID: "claude-haiku-4-5-20251001"},
		}},
	}, settings)
}

func requireAppErrorCode(t *testing.T, err error, code, contains string) {
	t.Helper()
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != code || !strings.Contains(appErr.Message, contains) {
		t.Fatalf("err = %v, want %s containing %q", err, code, contains)
	}
}

func TestRuntimeSettingsValidatorChecksCatalog(t *testing.T) {
	v := testRuntimeValidator(nil)
	ctx := context.Background()
	for _, ok := range []RuntimeSettingsCheck{
		{Backend: "codex", Model: "gpt-6.1-sol", Effort: "ultra"},
		{Backend: "claude_code", Harness: "claude_code/anthropic", Model: "opus[1m]", Effort: "max"},
		{Backend: "claude_code", Model: "claude-opus-5-5", Effort: "low"},
		{Backend: "claude_code", Model: "opusplan", Effort: "high"}, // alias the catalog does not list
		{Backend: "codex", Model: "gpt-7-preview", Remote: true},    // remote CLI may offer it
		{Backend: "opencode", Model: "anthropic/claude-sonnet-5"},   // no catalog: syntax only
	} {
		if err := v.Validate(ctx, ok); err != nil {
			t.Errorf("Validate(%+v) = %v", ok, err)
		}
	}
	requireAppErrorCode(t, v.Validate(ctx, RuntimeSettingsCheck{Backend: "codex", Model: "gpt-nope"}), "runtime_setting_invalid", "accepted: gpt-6-luna, gpt-6.1-sol")
	requireAppErrorCode(t, v.Validate(ctx, RuntimeSettingsCheck{Backend: "codex", Model: "gpt-6-luna", Effort: "ultra"}), "runtime_setting_invalid", "model gpt-6-luna")
	requireAppErrorCode(t, v.Validate(ctx, RuntimeSettingsCheck{Backend: "codex", Model: "default"}), "runtime_setting_invalid", "account default")
	requireAppErrorCode(t, v.Validate(ctx, RuntimeSettingsCheck{Backend: "claude_code", Effort: "ultra"}), "runtime_setting_invalid", "claude_code CLI")
	requireAppErrorCode(t, v.Validate(ctx, RuntimeSettingsCheck{Backend: "opencode", Effort: "high"}), "runtime_setting_invalid", "does not take a reasoning effort")
	requireAppErrorCode(t, v.Validate(ctx, RuntimeSettingsCheck{Backend: "copilot", Model: "x"}), "runtime_setting_invalid", "does not take a model")
}

func TestRuntimeSettingsValidatorResolve(t *testing.T) {
	v := testRuntimeValidator(fakeRuntimeDefaults{"default_model_codex": "gpt-6-luna", "default_effort_codex": "high"})
	ctx := context.Background()
	got, err := v.Resolve(ctx, "codex", `{"reasoning_effort":"max"}`, "", "", false)
	if err != nil || got.Model != "gpt-6-luna" || got.ModelSource != "global" || got.Effort != "max" || got.EffortSource != "project" {
		t.Fatalf("resolve = %+v err=%v", got, err)
	}
	_, err = v.Resolve(ctx, "codex", `{}`, "", "ultra", false)
	requireAppErrorCode(t, err, "runtime_setting_invalid", "model from global, effort from request")
	_, err = testRuntimeValidator(nil).Resolve(ctx, "claude_code", `{}`, "", "", false)
	requireAppErrorCode(t, err, "session_settings_unresolved", "default_model_claude_code")
}

func TestRuntimeSettingsValidatorDefaultSettings(t *testing.T) {
	v := testRuntimeValidator(fakeRuntimeDefaults{"default_model_codex": "gpt-6-luna"})
	ctx := context.Background()
	requireAppErrorCode(t, v.ValidateDefaultSettings(ctx, map[string]string{"default_effort_codex": "ultra"}), "runtime_setting_invalid", "gpt-6-luna")
	if err := v.ValidateDefaultSettings(ctx, map[string]string{"default_model_codex": "gpt-6.1-sol", "default_effort_codex": "ultra"}); err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateDefaultSettings(ctx, map[string]string{"default_model_codex": "", "unrelated": "x"}); err != nil {
		t.Fatalf("clearing a default must be allowed: %v", err)
	}
	requireAppErrorCode(t, v.ValidateDefaultSettings(ctx, map[string]string{"default_model_claude_code": "default"}), "runtime_setting_invalid", "account default")
}

func TestRuntimeSettingsValidatorListModels(t *testing.T) {
	v := testRuntimeValidator(fakeRuntimeDefaults{"default_model_codex": "gpt-6.1-sol", "default_effort_codex": "high"})
	list, err := v.ListModels(context.Background(), "codex", "", false)
	if err != nil || len(list.Models) != 2 || list.Defaults.Model != "gpt-6.1-sol" || list.Defaults.EffortKey != "default_effort_codex" || len(list.Efforts) != 6 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	openai, _ := v.ListModels(context.Background(), "claude_code", "claude_code/openai", false)
	if openai.Catalog != "codex" {
		t.Fatalf("claude_code on OpenAI must list Codex models, got %q", openai.Catalog)
	}
	if _, err := v.ListModels(context.Background(), "copilot", "", false); err == nil {
		t.Fatal("copilot has no models to list")
	}
}

func TestProjectServiceValidatesChangedModelAndEffort(t *testing.T) {
	store := newFakeProjectStore()
	service := NewProjectService(store, fakeEncryptor{}, nil)
	service.SetRuntimeSettingsValidator(testRuntimeValidator(fakeRuntimeDefaults{"default_model_codex": "gpt-6-luna"}))
	ctx := context.Background()

	_, err := service.Create(ctx, database.ProjectInput{Name: "x", Path: "/srv/x", Type: "local", Backend: "codex", BackendConfig: `{"model":"gpt-nope"}`})
	requireAppErrorCode(t, err, "runtime_setting_invalid", "gpt-nope")
	// An effort without a model is checked against the global default model.
	_, err = service.Create(ctx, database.ProjectInput{Name: "x", Path: "/srv/x", Type: "local", Backend: "codex", BackendConfig: `{"reasoning_effort":"ultra"}`})
	requireAppErrorCode(t, err, "runtime_setting_invalid", "gpt-6-luna")

	project, err := service.Create(ctx, database.ProjectInput{Name: "x", Path: "/srv/x", Type: "local", Backend: "codex", BackendConfig: `{"model":"gpt-6.1-sol","reasoning_effort":"ultra"}`})
	if err != nil {
		t.Fatal(err)
	}
	// A stale value no longer in the catalog never blocks an unrelated edit.
	store.projects[project.ID].BackendConfig = `{"model":"gpt-retired","reasoning_effort":"ultra"}`
	if _, err := service.Update(ctx, project.ID, database.ProjectInput{Name: "renamed", Path: "/srv/x", Type: "local"}); err != nil {
		t.Fatalf("unrelated edit refused: %v", err)
	}
	_, err = service.Update(ctx, project.ID, database.ProjectInput{Name: "renamed", Path: "/srv/x", Type: "local", BackendConfig: `{"model":"default"}`})
	requireAppErrorCode(t, err, "runtime_setting_invalid", "account default")

	claude, err := service.Create(ctx, database.ProjectInput{Name: "c", Path: "/srv/c", Type: "local", Backend: "claude_code", BackendConfig: `{"provider":"anthropic","model":"opus","effort":"max"}`})
	if err != nil || !strings.Contains(claude.BackendConfig, `"reasoning_effort":"max"`) || strings.Contains(claude.BackendConfig, `"effort"`) {
		t.Fatalf("claude config = %v err=%v", claude, err)
	}
}
