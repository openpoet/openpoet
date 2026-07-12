package automation

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type SessionOperationalReadPort interface {
	ListSessions(context.Context) ([]database.Session, error)
	GetSession(context.Context, string) (*database.Session, error)
}

type SessionRuntimeReadPort interface {
	IsSessionRunning(string) bool
	GetSessionOutput(string) ([]byte, error)
}

type SessionEventStatusView struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

type SessionEventStatusReadPort interface {
	SessionEventStatus(context.Context, string) (SessionEventStatusView, error)
}

type SessionAutomationView struct {
	ID              string     `json:"id"`
	ProjectID       int64      `json:"project_id"`
	Status          string     `json:"status"`
	Name            string     `json:"name"`
	TaskID          *int64     `json:"task_id,omitempty"`
	StartTime       time.Time  `json:"start_time"`
	EndTime         *time.Time `json:"end_time,omitempty"`
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty"`
	Backend         string     `json:"backend,omitempty"`
	Model           string     `json:"model,omitempty"`
	Effort          string     `json:"effort,omitempty"`
	Harness         string     `json:"harness,omitempty"`
	RuntimeActive   bool       `json:"runtime_active"`
	SkipPermissions bool       `json:"skip_permissions"`
}

func sessionAutomationView(session database.Session, runtime SessionRuntimeReadPort) SessionAutomationView {
	view := SessionAutomationView{
		ID: session.ID, ProjectID: session.ProjectID, Status: session.Status, Name: session.Name,
		StartTime: session.StartTime, Backend: session.Backend, Model: session.Model,
		Effort: session.Effort, Harness: session.Harness, SkipPermissions: session.SkipPermissions,
	}
	if session.TaskID.Valid {
		id := session.TaskID.Int64
		view.TaskID = &id
	}
	if session.EndTime.Valid {
		value := session.EndTime.Time
		view.EndTime = &value
	}
	if session.LastActivityAt.Valid {
		value := session.LastActivityAt.Time
		view.LastActivityAt = &value
	}
	if runtime != nil {
		view.RuntimeActive = runtime.IsSessionRunning(session.ID)
	}
	return view
}

func sessionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("sessions.list", "sessions", "sessions:read"),
		executionReadCapability("sessions.get", "sessions", "sessions:read"),
		executionReadCapability("sessions.history", "sessions", "sessions:read"),
		executionReadCapability("sessions.active", "sessions", "sessions:read"),
		executionPayloadLimit(executionWriteCapability("sessions.create", "sessions", "sessions:write"), 128<<10),
		executionDestructiveCapability("sessions.stop", "sessions", "sessions:write"),
		executionWriteCapability("sessions.reopen", "sessions", "sessions:write"),
		executionPayloadLimit(executionWriteCapability("sessions.send_input", "sessions", "sessions:write"), 20<<10),
		executionWriteCapability("sessions.set_model", "sessions", "sessions:write"),
		executionWriteCapability("sessions.set_effort", "sessions", "sessions:write"),
		executionWriteCapability("sessions.evaluate", "sessions", "sessions:write", "tasks:write"),
		executionPayloadLimit(platformMutation(executionReadCapability("sessions.image_prompt_hint", "sessions", "sessions:write")), 20<<10),
	}
}

type sessionPlatformExecutor struct {
	service *application.SessionService
	queries SessionOperationalReadPort
	runtime SessionRuntimeReadPort
}

type sessionListPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Status    string `json:"status,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type sessionCreatePayload struct {
	ProjectID                  int64             `json:"project_id,omitempty"`
	TaskID                     *int64            `json:"task_id,omitempty"`
	Environment                map[string]string `json:"environment,omitempty"`
	DangerouslySkipPermissions bool              `json:"dangerously_skip_permissions,omitempty"`
	AutoStartTaskPrompt        bool              `json:"auto_start_task_prompt,omitempty"`
}

type sessionReopenPayload struct {
	DangerouslySkipPermissions bool `json:"dangerously_skip_permissions,omitempty"`
}

type sessionInputPayload struct {
	Text string `json:"text"`
}

type sessionModelPayload struct {
	Model string `json:"model"`
}

type sessionEffortPayload struct {
	Effort string `json:"effort"`
}

type sessionImageHintPayload struct {
	UserPrompt string `json:"user_prompt,omitempty"`
	ImageCount int    `json:"image_count"`
}

type sessionHistoryPayload struct {
	MaxBytes int `json:"max_bytes,omitempty"`
}

type sessionHistoryAutomationView struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Redacted  bool   `json:"redacted"`
}

func (e *sessionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "sessions.list":
		var payload sessionListPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.ProjectID < 0 || payload.Limit < 0 || payload.Limit > 500 || utf8.RuneCountInString(payload.Status) > 50 {
			return nil, platformFailure("platform_payload_invalid", "session list filters are invalid", false)
		}
		if payload.Limit == 0 {
			payload.Limit = 100
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.queries.ListSessions(ctx)
			if err != nil {
				return nil, err
			}
			views := make([]SessionAutomationView, 0, min(payload.Limit, len(items)))
			for _, item := range items {
				if payload.ProjectID > 0 && item.ProjectID != payload.ProjectID || payload.Status != "" && item.Status != payload.Status {
					continue
				}
				views = append(views, sessionAutomationView(item, e.runtime))
				if len(views) == payload.Limit {
					break
				}
			}
			return views, nil
		}}, nil
	case "sessions.get":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			item, err := e.queries.GetSession(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			if item == nil {
				return nil, platformFailure("session_not_found", "session not found", false)
			}
			return sessionAutomationView(*item, e.runtime), nil
		}}, nil
	case "sessions.history":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionHistoryPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.MaxBytes < 0 || payload.MaxBytes > maxExecutionHistoryBytes {
			return nil, platformFailure("platform_payload_invalid", "history max_bytes exceeds 64 KiB", false)
		}
		if payload.MaxBytes == 0 {
			payload.MaxBytes = 16 << 10
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "max_bytes": payload.MaxBytes}), execute: func(_ context.Context, _ application.ActionAuthorization) (any, error) {
			raw, err := e.runtime.GetSessionOutput(sessionID)
			if err != nil {
				return nil, err
			}
			// Redact the complete bounded runtime buffer before sampling so a
			// head/tail boundary cannot split a credential pattern.
			redacted, _ := boundedExecutionText(string(raw), len(raw))
			raw = []byte(redacted)
			wasSampled := false
			if len(raw) > payload.MaxBytes {
				raw = append([]byte(nil), raw[len(raw)-payload.MaxBytes:]...)
				wasSampled = true
			}
			content, sanitizedTruncated := boundedExecutionText(string(raw), payload.MaxBytes)
			return sessionHistoryAutomationView{
				SessionID: sessionID, Content: content, Bytes: len([]byte(content)),
				Truncated: wasSampled || sanitizedTruncated, Redacted: true,
			}, nil
		}}, nil
	case "sessions.active":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.queries.ListSessions(ctx)
			if err != nil {
				return nil, err
			}
			views := make([]SessionAutomationView, 0)
			for _, item := range items {
				if !e.runtime.IsSessionRunning(item.ID) {
					continue
				}
				views = append(views, sessionAutomationView(item, e.runtime))
				if len(views) == 500 {
					break
				}
			}
			return views, nil
		}}, nil
	case "sessions.create":
		var payload sessionCreatePayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		if payload.TaskID != nil && *payload.TaskID <= 0 || len(payload.Environment) > 64 {
			return nil, platformFailure("platform_payload_invalid", "session task or environment is invalid", false)
		}
		environmentBytes := 0
		for key, value := range payload.Environment {
			environmentBytes += len(key) + len(value)
		}
		if environmentBytes > 64<<10 {
			return nil, platformFailure("platform_payload_invalid", "session environment exceeds 64 KiB", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{
			"project_id": projectID, "has_task": payload.TaskID != nil, "environment_count": len(payload.Environment),
			"unsafe_permissions": payload.DangerouslySkipPermissions,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			authorization.AllowEnvironment = len(payload.Environment) > 0
			authorization.AllowUnsafePermissions = payload.DangerouslySkipPermissions
			item, err := e.service.Create(ctx, application.CreateSessionCommand{
				ProjectID: projectID, TaskID: payload.TaskID, Environment: payload.Environment,
				DangerouslySkipPermissions: payload.DangerouslySkipPermissions, AutoStartTaskPrompt: payload.AutoStartTaskPrompt,
				Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return sessionAutomationView(*item, e.runtime), nil
		}}, nil
	case "sessions.stop":
		return e.sessionWithoutPayload(input, target, func(ctx context.Context, sessionID string, authorization application.ActionAuthorization) (any, error) {
			item, err := e.service.Stop(ctx, application.StopSessionCommand{SessionID: sessionID, Authorization: authorization})
			if err != nil {
				return nil, err
			}
			return sessionAutomationView(*item, e.runtime), nil
		})
	case "sessions.reopen":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionReopenPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "unsafe_permissions": payload.DangerouslySkipPermissions}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			authorization.AllowUnsafePermissions = payload.DangerouslySkipPermissions
			item, err := e.service.Reopen(ctx, application.ReopenSessionCommand{SessionID: sessionID, DangerouslySkipPermissions: payload.DangerouslySkipPermissions, Authorization: authorization})
			if err != nil {
				return nil, err
			}
			return sessionAutomationView(*item, e.runtime), nil
		}}, nil
	case "sessions.send_input":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionInputPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		text := strings.TrimSpace(payload.Text)
		if text == "" || len([]byte(text)) > 16<<10 {
			return nil, platformFailure("platform_payload_invalid", "session input must contain between 1 byte and 16 KiB", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "input_bytes": len([]byte(text))}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if err := e.service.SendInput(ctx, application.SendSessionInputCommand{SessionID: sessionID, Text: text, Authorization: authorization}); err != nil {
				return nil, err
			}
			return map[string]any{"sent": true, "session_id": sessionID}, nil
		}}, nil
	case "sessions.set_model":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionModelPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Model = strings.TrimSpace(payload.Model)
		if payload.Model == "" || utf8.RuneCountInString(payload.Model) > 200 {
			return nil, platformFailure("platform_payload_invalid", "session model is required and must not exceed 200 characters", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "model": payload.Model}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			item, err := e.service.SetModel(ctx, application.SetSessionModelCommand{SessionID: sessionID, Model: payload.Model, Authorization: authorization})
			if err != nil {
				return nil, err
			}
			return sessionAutomationView(*item, e.runtime), nil
		}}, nil
	case "sessions.set_effort":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionEffortPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Effort = strings.TrimSpace(payload.Effort)
		if payload.Effort == "" || utf8.RuneCountInString(payload.Effort) > 50 {
			return nil, platformFailure("platform_payload_invalid", "session effort is required and must not exceed 50 characters", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "effort": payload.Effort}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			item, err := e.service.SetEffort(ctx, application.SetSessionEffortCommand{SessionID: sessionID, Effort: payload.Effort, Authorization: authorization})
			if err != nil {
				return nil, err
			}
			return sessionAutomationView(*item, e.runtime), nil
		}}, nil
	case "sessions.evaluate":
		return e.sessionWithoutPayload(input, target, func(ctx context.Context, sessionID string, authorization application.ActionAuthorization) (any, error) {
			generated, err := e.service.Evaluate(ctx, application.EvaluateSessionCommand{SessionID: sessionID, Authorization: authorization})
			return map[string]any{"evaluated": err == nil, "task_generated": generated, "session_id": sessionID}, err
		})
	case "sessions.image_prompt_hint":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionImageHintPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if utf8.RuneCountInString(strings.TrimSpace(payload.UserPrompt)) > 4000 || payload.ImageCount <= 0 || payload.ImageCount > 20 {
			return nil, platformFailure("platform_payload_invalid", "image prompt hint is invalid or too large", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "image_count": payload.ImageCount, "prompt_bytes": len([]byte(payload.UserPrompt))}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if err := e.service.StoreImagePromptHint(ctx, application.StoreImagePromptHintCommand{SessionID: sessionID, UserPrompt: payload.UserPrompt, ImageCount: payload.ImageCount, Authorization: authorization}); err != nil {
				return nil, err
			}
			return map[string]any{"stored": true, "session_id": sessionID, "image_count": payload.ImageCount}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the session capability handler is unsupported", false)
	}
}

func (e *sessionPlatformExecutor) sessionWithoutPayload(
	input PlatformExecutionInput,
	target executionCommandTarget,
	execute func(context.Context, string, application.ActionAuthorization) (any, error),
) (PlatformValidatedCommand, error) {
	if err := requireEmptyExecutionPayload(input.Payload); err != nil {
		return nil, err
	}
	sessionID, err := executionStringID(target, "session id")
	if err != nil {
		return nil, err
	}
	return &executionValidatedCommand{
		preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID}),
		execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return execute(ctx, sessionID, authorization)
		},
	}, nil
}

func sessionWatcherPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("sessions.events_status", "session_event_watcher", "sessions:read"),
		platformMutation(executionReadCapability("sessions.events_watch_start", "session_event_watcher", "sessions:read")),
		executionDestructiveCapability("sessions.events_watch_stop", "session_event_watcher", "sessions:read"),
	}
}

type sessionWatcherPlatformExecutor struct {
	service  *application.SessionEventWatcherService
	statuses SessionEventStatusReadPort
}

func (e *sessionWatcherPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	if err := requireEmptyExecutionPayload(input.Payload); err != nil {
		return nil, err
	}
	sessionID, err := executionStringID(target, "session id")
	if err != nil {
		return nil, err
	}
	preview := executionPreview(input.Handler, map[string]any{"session_id": sessionID})
	switch input.Handler {
	case "sessions.events_status":
		return &executionValidatedCommand{preview: preview, execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.statuses.SessionEventStatus(ctx, sessionID)
		}}, nil
	case "sessions.events_watch_start":
		return &executionValidatedCommand{preview: preview, execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.Start(ctx, application.StartSessionWatcherCommand{SessionID: sessionID, Authorization: authorization})
		}}, nil
	case "sessions.events_watch_stop":
		return &executionValidatedCommand{preview: preview, execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.Stop(ctx, application.StopSessionWatcherCommand{SessionID: sessionID, Authorization: authorization})
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the session watcher capability handler is unsupported", false)
	}
}

func sessionSuggestionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		platformMutation(executionReadCapability("sessions.suggest_task_data", "session_task_suggestions", "sessions:read", "ai:use")),
	}
}

type sessionSuggestionPlatformExecutor struct {
	service *application.SessionTaskSuggestionService
}

func (e *sessionSuggestionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	if input.Handler != "sessions.suggest_task_data" {
		return nil, platformFailure("platform_handler_unsupported", "the session suggestion capability handler is unsupported", false)
	}
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	if err := requireEmptyExecutionPayload(input.Payload); err != nil {
		return nil, err
	}
	sessionID, err := executionStringID(target, "session id")
	if err != nil {
		return nil, err
	}
	return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
		return e.service.SuggestTaskData(ctx, application.SuggestSessionTaskDataCommand{SessionID: sessionID, Authorization: authorization})
	}}, nil
}
