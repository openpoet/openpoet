package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"openpoet/internal/modelcatalog"
	"openpoet/internal/sessionmeta"
)

// ModelCatalogSource returns the model list a harness CLI reports (the same
// source as the UI's model picker). Implemented by modelcatalog.Service.
type ModelCatalogSource interface {
	Get(ctx context.Context, harness string, refresh bool) modelcatalog.Catalog
}

// RuntimeDefaultsReader reads a backend's global default model and effort.
type RuntimeDefaultsReader interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// RuntimeSettingsValidator resolves and checks the explicit model and effort
// sessions run with. Projects, global defaults and session requests all go
// through it, so a value the CLI does not offer is refused where it is set.
type RuntimeSettingsValidator struct {
	catalogs ModelCatalogSource
	defaults RuntimeDefaultsReader
}

func NewRuntimeSettingsValidator(catalogs ModelCatalogSource, defaults RuntimeDefaultsReader) *RuntimeSettingsValidator {
	return &RuntimeSettingsValidator{catalogs: catalogs, defaults: defaults}
}

// RuntimeSettingsCheck is one model/effort pair to validate. Remote projects
// run their own CLI: a model the local catalog does not list is accepted
// there, since the remote CLI may offer it.
type RuntimeSettingsCheck struct {
	Backend string
	Harness string
	Model   string
	Effort  string
	Remote  bool
}

// claudeCodeAliases are model names Claude Code accepts that its catalog may
// not list.
var claudeCodeAliases = map[string]bool{"best": true, "opusplan": true, "opus": true, "sonnet": true, "haiku": true, "fable": true}

// CatalogHarness is the model catalog serving a backend: Claude Code on the
// OpenAI provider runs Codex's models.
func CatalogHarness(backend, harness string) string {
	switch strings.TrimSpace(backend) {
	case "codex", "opencode":
		return strings.TrimSpace(backend)
	case "claude_code":
		if harness == "claude_code/openai" {
			return "codex"
		}
		return "claude_code"
	}
	return ""
}

// Validate checks a model and/or effort (empty ones are skipped) against the
// backend and the model list its CLI reports.
func (v *RuntimeSettingsValidator) Validate(ctx context.Context, check RuntimeSettingsCheck) error {
	check.Model = strings.TrimSpace(check.Model)
	check.Effort = strings.ToLower(strings.TrimSpace(check.Effort))
	takesModel, takesEffort := sessionmeta.Selectable(check.Backend)
	if check.Model != "" {
		if !sessionmeta.Explicit(check.Model) {
			return runtimeSettingError("model must be an explicit model id; %q hands the choice to the CLI's account default", check.Model)
		}
		if !takesModel {
			return runtimeSettingError("backend %s does not take a model", check.Backend)
		}
	}
	if check.Effort != "" {
		if !sessionmeta.Explicit(check.Effort) {
			return runtimeSettingError("effort must be an explicit level; %q hands the choice to the CLI's account default", check.Effort)
		}
		if !takesEffort {
			return runtimeSettingError("backend %s does not take a reasoning effort", check.Backend)
		}
	}
	if check.Model == "" && check.Effort == "" {
		return nil
	}
	harness := CatalogHarness(check.Backend, check.Harness)
	catalog := modelcatalog.Catalog{Harness: harness}
	if v != nil && v.catalogs != nil && harness != "" {
		catalog = v.catalogs.Get(ctx, harness, false)
	}
	var entry modelcatalog.Model
	found := false
	if check.Model != "" {
		lookup := check.Model
		if harness == "claude_code" || check.Harness == "claude_code/openai" {
			lookup = strings.TrimSuffix(strings.TrimSuffix(lookup, "[1m]"), "[1M]")
		}
		entry, found = catalog.Find(lookup)
		alias := check.Backend == "claude_code" && harness == "claude_code" && claudeCodeAliases[strings.ToLower(lookup)]
		if !found && !alias && len(catalog.Models) > 0 && !check.Remote {
			return runtimeSettingError("model %q is not in the list the %s CLI reports; accepted: %s", check.Model, harness, modelIDList(catalog))
		}
	}
	if check.Effort != "" {
		accepted := modelcatalog.KnownEfforts(harness, catalog)
		scope := "the " + harness + " CLI"
		if found && len(entry.Efforts) > 0 {
			accepted, scope = entry.Efforts, "model "+check.Model
		}
		if !containsString(accepted, check.Effort) {
			return runtimeSettingError("effort %q is not accepted by %s; accepted: %s", check.Effort, scope, strings.Join(accepted, ", "))
		}
	}
	return nil
}

// Defaults returns a backend's global default model and effort.
func (v *RuntimeSettingsValidator) Defaults(ctx context.Context, backend string) sessionmeta.Defaults {
	if v == nil || v.defaults == nil {
		return sessionmeta.Defaults{}
	}
	model, _ := v.defaults.GetSetting(ctx, sessionmeta.DefaultModelSetting(backend))
	effort, _ := v.defaults.GetSetting(ctx, sessionmeta.DefaultEffortSetting(backend))
	return sessionmeta.Defaults{Model: strings.TrimSpace(model), Effort: strings.TrimSpace(effort)}
}

// Resolve picks a new session's model and effort (request, project, global
// default) and validates the pair. It is the early, catalog-aware twin of the
// session manager's own resolution, which stays as the last line for every
// start path.
func (v *RuntimeSettingsValidator) Resolve(ctx context.Context, backend, rawConfig, requestModel, requestEffort string, remote bool) (sessionmeta.RuntimeSettings, error) {
	settings, err := sessionmeta.Resolve(backend, rawConfig, requestModel, requestEffort, v.Defaults(ctx, backend))
	if err != nil {
		return settings, unresolvedSettingsError(err)
	}
	harness := sessionmeta.FromProjectConfig(backend, rawConfig).Harness
	if err := v.Validate(ctx, RuntimeSettingsCheck{Backend: backend, Harness: harness, Model: settings.Model, Effort: settings.Effort, Remote: remote}); err != nil {
		var appErr *Error
		if errors.As(err, &appErr) {
			appErr.Message = fmt.Sprintf("%s (model from %s, effort from %s)", appErr.Message, sourceLabel(settings.ModelSource), sourceLabel(settings.EffortSource))
		}
		return settings, err
	}
	return settings, nil
}

// RuntimeModelList is the model list of one backend with the efforts each
// model accepts and the backend's global defaults.
type RuntimeModelList struct {
	Backend      string               `json:"backend"`
	Catalog      string               `json:"catalog"`
	Models       []modelcatalog.Model `json:"models"`
	Efforts      []string             `json:"efforts"`
	Defaults     RuntimeDefaultsView  `json:"global_defaults"`
	FetchedAt    *time.Time           `json:"fetched_at,omitempty"`
	CatalogError string               `json:"catalog_error,omitempty"`
}

// RuntimeDefaultsView is a backend's global defaults and their settings keys.
type RuntimeDefaultsView struct {
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	ModelKey    string `json:"model_setting"`
	EffortKey   string `json:"effort_setting,omitempty"`
	TakesEffort bool   `json:"takes_effort"`
}

// ListModels returns the catalog a backend's sessions are validated against.
func (v *RuntimeSettingsValidator) ListModels(ctx context.Context, backend, harness string, refresh bool) (RuntimeModelList, error) {
	takesModel, takesEffort := sessionmeta.Selectable(backend)
	if !takesModel {
		return RuntimeModelList{}, runtimeSettingError("backend %s has no selectable model; models are listed for claude_code, codex and opencode", backend)
	}
	catalogHarness := CatalogHarness(backend, harness)
	catalog := modelcatalog.Catalog{Harness: catalogHarness, Models: []modelcatalog.Model{}}
	if v != nil && v.catalogs != nil {
		catalog = v.catalogs.Get(ctx, catalogHarness, refresh)
	}
	defaults := v.Defaults(ctx, backend)
	out := RuntimeModelList{
		Backend: backend, Catalog: catalogHarness, Models: catalog.Models, CatalogError: catalog.Error,
		Defaults: RuntimeDefaultsView{Model: defaults.Model, Effort: defaults.Effort, ModelKey: sessionmeta.DefaultModelSetting(backend), TakesEffort: takesEffort},
	}
	if out.Models == nil {
		out.Models = []modelcatalog.Model{}
	}
	if takesEffort {
		out.Efforts = modelcatalog.KnownEfforts(catalogHarness, catalog)
		out.Defaults.EffortKey = sessionmeta.DefaultEffortSetting(backend)
	} else {
		out.Efforts = []string{}
	}
	if !catalog.FetchedAt.IsZero() {
		at := catalog.FetchedAt
		out.FetchedAt = &at
	}
	return out, nil
}

// ValidateDefaultSettings checks global default keys being written
// (default_model_<backend>, default_effort_<backend>) against the catalog,
// pairing each with the other value (new or stored).
func (v *RuntimeSettingsValidator) ValidateDefaultSettings(ctx context.Context, values map[string]string) error {
	for _, backend := range []string{"claude_code", "codex", "opencode"} {
		modelKey, effortKey := sessionmeta.DefaultModelSetting(backend), sessionmeta.DefaultEffortSetting(backend)
		newModel, modelSet := values[modelKey]
		newEffort, effortSet := values[effortKey]
		if !modelSet && !effortSet {
			continue
		}
		// The pair that will be stored: what is written plus what stays.
		current := v.Defaults(ctx, backend)
		check := RuntimeSettingsCheck{Backend: backend, Model: current.Model, Effort: current.Effort}
		if modelSet {
			check.Model = strings.TrimSpace(newModel)
		}
		if effortSet {
			check.Effort = strings.TrimSpace(newEffort)
		}
		if err := v.Validate(ctx, check); err != nil {
			return err
		}
	}
	for key := range values {
		if strings.HasPrefix(key, "default_effort_") {
			backend := strings.TrimPrefix(key, "default_effort_")
			if _, takesEffort := sessionmeta.Selectable(backend); !takesEffort && strings.TrimSpace(values[key]) != "" {
				return runtimeSettingError("backend %s does not take a reasoning effort", backend)
			}
		}
	}
	return nil
}

func runtimeSettingError(format string, args ...any) error {
	return validationError("runtime_setting_invalid", fmt.Sprintf(format, args...))
}

func unresolvedSettingsError(err error) error {
	return &Error{Kind: ErrorValidation, Code: "session_settings_unresolved", Message: err.Error(), Cause: err}
}

func sourceLabel(source string) string {
	if source == "" {
		return "nowhere"
	}
	return source
}

func modelIDList(catalog modelcatalog.Catalog) string {
	ids := make([]string, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	if len(ids) > 30 {
		ids = append(ids[:30], "…")
	}
	return strings.Join(ids, ", ")
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
