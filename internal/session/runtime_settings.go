package session

import (
	"context"
	"fmt"
	"log"
	"strings"

	"openpoet/internal/database"
	"openpoet/internal/sessionmeta"
)

// ErrSessionSettingsUnresolved means a session has no explicit model or effort
// to start with. Sessions never fall back to the CLI's account default.
var ErrSessionSettingsUnresolved = sessionmeta.ErrUnresolved

// Env markers the application layer uses to hand a create/reopen request's
// explicit model and effort to the manager. They never reach the child
// process.
const (
	RequestModelEnv  = "OPENPOET_REQUEST_MODEL"
	RequestEffortEnv = "OPENPOET_REQUEST_EFFORT"
)

func takeRuntimeRequest(envVars map[string]string) (model, effort string) {
	if envVars == nil {
		return "", ""
	}
	model, effort = envVars[RequestModelEnv], envVars[RequestEffortEnv]
	delete(envVars, RequestModelEnv)
	delete(envVars, RequestEffortEnv)
	return strings.TrimSpace(model), strings.TrimSpace(effort)
}

// RuntimeDefaults returns a backend's global default model and effort.
func (m *Manager) RuntimeDefaults(ctx context.Context, backend string) sessionmeta.Defaults {
	if m == nil || m.db == nil {
		return sessionmeta.Defaults{}
	}
	model, _ := m.db.GetSetting(ctx, sessionmeta.DefaultModelSetting(backend))
	effort, _ := m.db.GetSetting(ctx, sessionmeta.DefaultEffortSetting(backend))
	return sessionmeta.Defaults{Model: strings.TrimSpace(model), Effort: strings.TrimSpace(effort)}
}

// resolveRuntimeSettings picks the explicit model and effort a session runs
// with (request, then project, then global default) and checks their syntax.
// The model catalog check is the application layer's job: it needs the CLI.
func (m *Manager) resolveRuntimeSettings(ctx context.Context, backend, rawConfig, requestModel, requestEffort string) (sessionmeta.RuntimeSettings, error) {
	settings, err := sessionmeta.Resolve(backend, rawConfig, requestModel, requestEffort, m.RuntimeDefaults(ctx, backend))
	if err != nil {
		return settings, err
	}
	return settings, validateResolvedSettings(backend, sessionmeta.FromProjectConfig(backend, rawConfig).Harness, settings)
}

func validateResolvedSettings(backend, harness string, settings sessionmeta.RuntimeSettings) error {
	if settings.Model != "" {
		var err error
		if BackendType(backend) == BackendClaudeCode {
			_, err = validateClaudeCodeModelIDForHarness(settings.Model, harness)
		} else {
			_, err = validateSessionModelID(settings.Model)
		}
		if err != nil {
			return fmt.Errorf("%s model: %w", settings.ModelSource, err)
		}
	}
	if settings.Effort != "" {
		if _, err := validateSessionEffort(settings.Effort); err != nil {
			return fmt.Errorf("%s effort: %w", settings.EffortSource, err)
		}
	}
	return nil
}

// initialEffectiveModel is what the session row says the runtime runs before
// the runtime reports it: "unknown" where it will report (Claude Code hooks,
// Codex thread/rollout), the configured model where nothing ever will.
func initialEffectiveModel(backend BackendType, configured string) string {
	switch backend {
	case BackendClaudeCode, BackendCodex:
		return "unknown"
	}
	return configured
}

// RecordEffectiveSettings persists the model and/or effort the runtime itself
// reported (Codex thread/start, thread/resume, model/rerouted; rollout
// turn_context). Empty values are left as they are.
func (m *Manager) RecordEffectiveSettings(ctx context.Context, sessionID, model, effort string) {
	model, effort = strings.TrimSpace(model), strings.ToLower(strings.TrimSpace(effort))
	if model != "" {
		if err := m.RecordEffectiveModel(ctx, sessionID, model); err != nil {
			log.Printf("[Session] %s: %v", sessionID, err)
		}
	}
	if effort == "" {
		return
	}
	if m.db != nil {
		if err := m.db.UpdateSessionEffectiveEffort(ctx, sessionID, effort); err != nil {
			log.Printf("[Session] failed to persist effective effort for %s: %v", sessionID, err)
			return
		}
	}
	m.mu.Lock()
	if rs, ok := m.sessions[sessionID]; ok && rs != nil && rs.session != nil {
		rs.session.EffectiveEffort = effort
	}
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.BroadcastStateUpdate("session", map[string]interface{}{
			"action": "runtime_metadata_changed", "session_id": sessionID, "effective_effort": effort,
		})
	}
}

// applyResolvedSettings stamps a session row with its resolved settings.
func applyResolvedSettings(session *database.Session, backend BackendType, settings sessionmeta.RuntimeSettings) {
	session.RequestedModel = settings.Model
	session.Effort = settings.Effort
	session.ModelSource = settings.ModelSource
	session.EffortSource = settings.EffortSource
	session.Model = initialEffectiveModel(backend, settings.Model)
	session.EffectiveEffort = ""
}

// resolveReopenSettings resolves a reopened session's settings: the request,
// then what the session ran with, then project and global default.
func (m *Manager) resolveReopenSettings(ctx context.Context, session *database.Session, rawConfig, requestModel, requestEffort string) (sessionmeta.RuntimeSettings, error) {
	model, effort := requestModel, requestEffort
	modelFromSession, effortFromSession := false, false
	if !sessionmeta.Explicit(model) {
		if v := sessionRunModel(session); sessionmeta.Explicit(v) {
			model, modelFromSession = v, true
		}
	}
	if !sessionmeta.Explicit(effort) && sessionmeta.Explicit(session.Effort) {
		effort, effortFromSession = session.Effort, true
	}
	settings, err := sessionmeta.Resolve(session.Backend, rawConfig, model, effort, m.RuntimeDefaults(ctx, session.Backend))
	if err != nil {
		return settings, err
	}
	if modelFromSession && settings.ModelSource == sessionmeta.SourceRequest {
		settings.ModelSource = sessionmeta.SourceSession
	}
	if effortFromSession && settings.EffortSource == sessionmeta.SourceRequest {
		settings.EffortSource = sessionmeta.SourceSession
	}
	harness := strings.TrimSpace(session.Harness)
	if harness == "" {
		harness = sessionmeta.FromProjectConfig(session.Backend, rawConfig).Harness
	}
	return settings, validateResolvedSettings(session.Backend, harness, settings)
}

// sessionRunModel is the model a session was configured with. Rows written
// before requested_model existed hold it in model (non-Claude backends).
func sessionRunModel(session *database.Session) string {
	if v := strings.TrimSpace(session.RequestedModel); v != "" {
		return v
	}
	if BackendType(session.Backend) != BackendClaudeCode && !strings.EqualFold(strings.TrimSpace(session.Model), "unknown") {
		return strings.TrimSpace(session.Model)
	}
	return ""
}

func (m *Manager) effectiveSettingsRecorder(sessionID string) func(model, effort string) {
	return func(model, effort string) {
		m.RecordEffectiveSettings(context.Background(), sessionID, model, effort)
	}
}
