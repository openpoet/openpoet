package automation

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
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

// SessionStartupReadPort is optionally implemented by the session runtime: it
// reports the initial prompt's delivery progress.
type SessionStartupReadPort interface {
	SessionStartupState(sessionID string) (state, detail string)
}

// SessionScreenReadPort is optionally implemented by the session runtime: it
// renders the terminal as a human currently sees it.
type SessionScreenReadPort interface {
	ScreenText(sessionID string) string
}

// Interaction states reported in SessionAutomationView.InteractionState.
const (
	interactionAwaitingInput = "awaiting_input" // blocked on a question: see awaiting_input
	interactionStarting      = "starting"       // initial prompt not delivered yet
	interactionIdle          = "running"        // running, nothing pending
	interactionEnded         = "ended"          // no live runtime
)

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
	// InteractionState is awaiting_input | starting | running | ended.
	InteractionState string `json:"interaction_state,omitempty"`
	// StartupState tracks the initial prompt: starting | awaiting_input |
	// ready | failed (omitted when the session had no initial prompt or the
	// server restarted since it was created).
	StartupState  string `json:"startup_state,omitempty"`
	StartupDetail string `json:"startup_detail,omitempty"`
	// AwaitingInput is the question the session is blocked on; answer it with
	// sessions.answer_prompt using its question_id.
	AwaitingInput *sessionprompt.Question `json:"awaiting_input,omitempty"`
	// ErrorReason and LastOutput explain a session whose status is error.
	ErrorReason string `json:"error_reason,omitempty"`
	LastOutput  string `json:"last_output,omitempty"`
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
	view.ErrorReason = session.ErrorReason
	if session.Status == "error" && session.LastOutput != "" {
		view.LastOutput, _ = boundedExecutionText(session.LastOutput, maxErrorLastOutputBytes)
	}
	return view
}

const maxErrorLastOutputBytes = 4 << 10

// view is sessionAutomationView plus the live interaction state.
func (e *sessionPlatformExecutor) view(ctx context.Context, session database.Session) SessionAutomationView {
	view := sessionAutomationView(session, e.runtime)
	if !view.RuntimeActive {
		view.InteractionState = interactionEnded
		return view
	}
	if startup, ok := e.runtime.(SessionStartupReadPort); ok {
		view.StartupState, view.StartupDetail = startup.SessionStartupState(session.ID)
	}
	view.InteractionState = interactionIdle
	if view.StartupState == "starting" {
		view.InteractionState = interactionStarting
	}
	if e.questions != nil {
		if question, err := e.questions.Pending(ctx, session.ID); err == nil && question != nil {
			bounded := *question
			bounded.Text, _ = boundedExecutionText(bounded.Text, 8<<10)
			bounded.ToolInput, _ = boundedExecutionText(bounded.ToolInput, 4<<10)
			view.AwaitingInput = &bounded
			view.InteractionState = interactionAwaitingInput
		}
	}
	return view
}

func sessionPlatformDefinitions() []PlatformCapabilityDefinition {
	session := sessionTargetDescription
	return []PlatformCapabilityDefinition{
		withPayloadSchema(executionReadCapability("sessions.list", "sessions", "sessions:read"), "{}", sessionListPayload{}, "", ""),
		withPayloadSchema(executionReadCapability("sessions.get", "sessions", "sessions:read"), session, nil, "", sessionStateNotes),
		withPayloadSchema(executionReadCapability("sessions.history", "sessions", "sessions:read"), session, sessionHistoryPayload{}, "",
			"Live sessions return the terminal buffer in content and the currently displayed screen in screen (source=runtime). Ended sessions return the last screen recorded at exit plus error_reason (source=persisted) instead of failing."),
		withPayloadSchema(executionPayloadLimit(executionReadCapability("sessions.messages", "sessions", "sessions:read"), 4<<10), session, sessionMessagesPayload{},
			`{"last_n":5,"max_chars":400}`, sessionMessagesNotes),
		withPayloadSchema(executionReadCapability("sessions.active", "sessions", "sessions:read"), "{}", nil, "", sessionStateNotes),
		withPayloadSchema(executionPayloadLimit(executionWriteCapability("sessions.create", "sessions", "sessions:write"), 128<<10), projectTargetDescription, sessionCreatePayload{}, "", sessionCreateNotes),
		withPayloadSchema(executionPayloadLimit(executionWriteCapability("sessions.answer_prompt", "sessions", "sessions:write"), 64<<10), session, sessionAnswerPromptPayload{},
			`{"question_id":"t_3f2a9c0d1e4b5a67","option":2}`, sessionAnswerPromptNotes),
		withPayloadSchema(executionDestructiveCapability("sessions.stop", "sessions", "sessions:write"), session, nil, "", sessionStopNotes),
		withPayloadSchema(executionWriteCapability("sessions.close_completed", "sessions", "sessions:write"), session, nil, "", sessionCloseCompletedNotes),
		// Destructive: isolating restarts a live session, which discards its
		// conversation (a runner cannot change working directory in place).
		withPayloadSchema(executionDestructiveCapability("sessions.isolate", "sessions", "sessions:write"), session, sessionIsolatePayload{}, "", ""),
		withPayloadSchema(executionWriteCapability("sessions.reopen", "sessions", "sessions:write"), session, sessionReopenPayload{}, "", ""),
		withPayloadSchema(executionPayloadLimit(executionWriteCapability("sessions.send_input", "sessions", "sessions:write"), 20<<10), session, sessionInputPayload{}, "", ""),
		withPayloadSchema(executionWriteCapability("sessions.set_model", "sessions", "sessions:write"), session, sessionModelPayload{}, "", ""),
		withPayloadSchema(executionWriteCapability("sessions.set_effort", "sessions", "sessions:write"), session, sessionEffortPayload{}, "", ""),
		withPayloadSchema(executionWriteCapability("sessions.evaluate", "sessions", "sessions:write", "tasks:write"), session, nil, "", ""),
		withPayloadSchema(executionPayloadLimit(platformMutation(executionReadCapability("sessions.image_prompt_hint", "sessions", "sessions:write")), 20<<10), session, sessionImageHintPayload{},
			`{"image_count":1,"user_prompt":"Describe this photo"}`,
			"Requires a running session. Stores the prompt that accompanies images pasted with files.paste_session_image; it does not transfer image data."),
	}
}

type sessionPlatformExecutor struct {
	service   *application.SessionService
	questions *application.SessionQuestionService
	queries   SessionOperationalReadPort
	runtime   SessionRuntimeReadPort
	// messages backs sessions.messages (optional).
	messages *application.SessionMessageService
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
	AutoStartTaskPrompt        *bool             `json:"auto_start_task_prompt,omitempty"`
	PlanningMode               bool              `json:"planning_mode,omitempty"`
	CustomPrompt               string            `json:"custom_prompt,omitempty"`
	WorkspaceID                string            `json:"workspace_id,omitempty"`
	// Backend, when set, must name a known backend; the session runs with it
	// even when it differs from the project's backend (Phase 7.3 heterogeneous
	// fan-out).
	Backend string `json:"backend,omitempty"`
	// Isolation:"auto" leases an idle pooled workspace when the project's main
	// path is busy (Phase 6 pooling), instead of requiring an explicit workspace_id.
	Isolation string `json:"isolation,omitempty"`
}

// knownSessionBackends mirrors internal/session BackendType constants (kept local
// to avoid an import cycle from the automation package into session).
var knownSessionBackends = map[string]struct{}{
	"claude_code": {}, "copilot": {}, "acp": {}, "codex": {}, "opencode": {},
}

type sessionReopenPayload struct {
	DangerouslySkipPermissions bool `json:"dangerously_skip_permissions,omitempty"`
}

// sessionIsolatePayload carries the two things the moved session cannot know on
// its own: why it was moved, and what it was doing (its transcript does not
// survive the restart).
type sessionIsolatePayload struct {
	Reason   string `json:"reason,omitempty"`
	Briefing string `json:"briefing,omitempty"`
}

type sessionInputPayload struct {
	Text string `json:"text"`
	// AwaitAck blocks for the agent's UserPromptSubmit ack (default true for
	// coordinator send-with-ack); Force bypasses the mid-turn busy guard.
	AwaitAck *bool `json:"await_ack,omitempty"`
	Force    bool  `json:"force,omitempty"`
}

type sessionModelPayload struct {
	Model string `json:"model"`
}

type sessionEffortPayload struct {
	Effort string `json:"effort"`
}

type sessionImageHintPayload struct {
	UserPrompt string `json:"user_prompt,omitempty" doc:"prompt the pasted images accompany (at most 4000 characters)"`
	ImageCount int    `json:"image_count" doc:"number of images pasted for this prompt (1-20)"`
}

const sessionStateNotes = "Session views carry interaction_state: awaiting_input (blocked on a question), starting (initial prompt not delivered yet), running, or ended. " +
	"When awaiting_input, the awaiting_input object describes the question: question_id, source (hook|terminal), kind (workspace_trust, bypass_permissions, tool_permission, ask_user_question, plan_approval, confirm, selection; treat unknown kinds as selection), " +
	"text, options[{index,label,description,grants_permission,ends_session}], selected_index, accepts_text, questions (ask_user_question), tool_name, tool_input, detected_at. Answer it with sessions.answer_prompt. " +
	"startup_state (starting|awaiting_input|ready|failed) tracks the initial prompt; error_reason and last_output explain a session with status error. " +
	"Events platform.session.awaiting_input and platform.session.input_resolved (aggregate id = session id) signal changes; re-read sessions.get for details."

const sessionCreateNotes = "Returns as soon as the agent process is running (status running, interaction_state starting); it never waits for the agent to be ready. " +
	"The initial prompt (task, planning or custom_prompt) is delivered in the background and is held back while a question is open, so it can never answer a dialog by accident. " +
	"A client timeout or disconnect never stops the session. Poll sessions.get: startup_state becomes ready once the prompt is delivered, or awaiting_input when a question (e.g. workspace_trust) needs an answer via sessions.answer_prompt."

const sessionStopNotes = "Stops any starting or running session; needs an explicit approval_token. " +
	"For an idle session whose linked task is already done, use sessions.close_completed instead (no per-session approval)."

const sessionCloseCompletedNotes = "Closes a session whose work is finished, with policy approval (no approval_token). " +
	"Requires a command reason and correlation_id. Refuses unless ALL hold: the session has a linked task whose status is done (session_task_missing, session_task_not_done); " +
	"it is not waiting on a question (session_awaiting_input); and it is not mid-turn — idle after its last turn, or, when the turn state is unknown, " +
	"silent for 10 minutes (session_busy). A refused session can still be stopped with sessions.stop. An already stopped session is returned unchanged. " +
	"Who closed it and the reason are recorded in the task history (session_closed_completed) and the event outbox (platform.session.closure_recorded)."

const sessionAnswerPromptNotes = "Answers the question in the session's awaiting_input. question_id must be the current one (a stale id fails with session_question_changed). " +
	"Send option (1-based index from awaiting_input.options); text where accepts_text is true (deny reason, plan feedback, free-text answer, y/n text; text alone on a tool_permission denies with that reason); " +
	"options for a single multi-select ask_user_question; answers {question text: label or free text} for ask_user_question with several questions. " +
	"Risk: options with grants_permission (allow a tool, accept bypass-permissions mode) or ends_session require a command reason; accepting bypass-permissions mode also requires a session created with dangerously_skip_permissions."

type sessionAnswerPromptPayload struct {
	QuestionID string            `json:"question_id" doc:"awaiting_input.question_id from sessions.get or sessions.active"`
	Option     int               `json:"option,omitempty" doc:"1-based index from awaiting_input.options"`
	Options    []int             `json:"options,omitempty" doc:"option indexes for a single multi-select ask_user_question"`
	Text       string            `json:"text,omitempty" doc:"free text where accepts_text is true (at most 4000 characters)"`
	Answers    map[string]string `json:"answers,omitempty" doc:"ask_user_question with several questions: {question text: option label or free text}"`
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
	Source    string `json:"source"` // runtime | persisted
	// Screen is the terminal as currently displayed (live sessions), which is
	// far easier to read than the raw repaint stream in content.
	Screen      string `json:"screen,omitempty"`
	Status      string `json:"status,omitempty"`
	ErrorReason string `json:"error_reason,omitempty"`
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
		scope := input.ProjectScope // client project_filter: never leak other groups' sessions
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
				if !scope.Allows(item.ProjectID) {
					continue // outside the client's project_filter
				}
				views = append(views, e.view(ctx, item))
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
			return e.view(ctx, *item), nil
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
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "max_bytes": payload.MaxBytes}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			raw, err := e.runtime.GetSessionOutput(sessionID)
			if err != nil {
				return e.persistedHistory(ctx, sessionID, payload.MaxBytes, err)
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
			view := sessionHistoryAutomationView{
				SessionID: sessionID, Content: content, Bytes: len([]byte(content)),
				Truncated: wasSampled || sanitizedTruncated, Redacted: true, Source: "runtime",
			}
			if screen, ok := e.runtime.(SessionScreenReadPort); ok {
				view.Screen, _ = boundedExecutionText(screen.ScreenText(sessionID), maxExecutionHistoryBytes)
			}
			return view, nil
		}}, nil
	case "sessions.messages":
		return e.validateMessages(input, target)
	case "sessions.active":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		scope := input.ProjectScope
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
				if !scope.Allows(item.ProjectID) {
					continue // outside the client's project_filter
				}
				views = append(views, e.view(ctx, item))
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
		payload.CustomPrompt = strings.TrimSpace(payload.CustomPrompt)
		if payload.PlanningMode && payload.CustomPrompt != "" {
			return nil, platformFailure("platform_payload_invalid", "planning_mode and custom_prompt cannot be used together", false)
		}
		if len([]byte(payload.CustomPrompt)) > 16<<10 || strings.IndexByte(payload.CustomPrompt, 0) >= 0 {
			return nil, platformFailure("platform_payload_invalid", "custom_prompt must not exceed 16 KiB or contain NUL", false)
		}
		environmentBytes := 0
		for key, value := range payload.Environment {
			environmentBytes += len(key) + len(value)
		}
		if environmentBytes > 64<<10 {
			return nil, platformFailure("platform_payload_invalid", "session environment exceeds 64 KiB", false)
		}
		payload.WorkspaceID = strings.TrimSpace(payload.WorkspaceID)
		if len(payload.WorkspaceID) > 128 {
			return nil, platformFailure("platform_payload_invalid", "workspace_id is too large", false)
		}
		payload.Backend = strings.TrimSpace(payload.Backend)
		if payload.Backend != "" {
			if _, ok := knownSessionBackends[payload.Backend]; !ok {
				return nil, platformFailure("platform_payload_invalid", "backend must be one of claude_code, copilot, acp, codex, opencode", false)
			}
		}
		startMode := "default"
		if payload.PlanningMode {
			startMode = "planning"
		} else if payload.CustomPrompt != "" {
			startMode = "custom"
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{
			"project_id": projectID, "has_task": payload.TaskID != nil, "environment_count": len(payload.Environment),
			"unsafe_permissions": payload.DangerouslySkipPermissions, "start_mode": startMode,
			"workspace_id": payload.WorkspaceID, "backend": payload.Backend,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			authorization.AllowEnvironment = len(payload.Environment) > 0
			authorization.AllowUnsafePermissions = payload.DangerouslySkipPermissions
			item, err := e.service.Create(ctx, application.CreateSessionCommand{
				ProjectID: projectID, TaskID: payload.TaskID, Environment: payload.Environment,
				DangerouslySkipPermissions: payload.DangerouslySkipPermissions,
				// Programmatic creates always start immediately. Keep accepting the
				// legacy field for compatibility, but never turn an Automation call
				// into a UI notification that waits for manual confirmation.
				AutoStartTaskPrompt: true,
				PlanningMode:        payload.PlanningMode,
				CustomPrompt:        payload.CustomPrompt,
				WorkspaceID:         payload.WorkspaceID,
				Isolation:           payload.Isolation,
				Backend:             payload.Backend,
				Authorization:       authorization,
			})
			if err != nil {
				return nil, err
			}
			return e.view(ctx, *item), nil
		}}, nil
	case "sessions.answer_prompt":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionAnswerPromptPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.QuestionID = strings.TrimSpace(payload.QuestionID)
		if payload.QuestionID == "" {
			return nil, missingPayloadField("question_id", "the awaiting_input.question_id read from sessions.get")
		}
		if payload.Option == 0 && len(payload.Options) == 0 && strings.TrimSpace(payload.Text) == "" && len(payload.Answers) == 0 {
			return nil, platformFailure("platform_payload_invalid", "send option, options, text or answers", false)
		}
		scope := input.ProjectScope
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{
			"session_id": sessionID, "question_id": payload.QuestionID, "option": payload.Option,
			"option_count": len(payload.Options), "has_text": payload.Text != "", "answer_count": len(payload.Answers),
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if e.questions == nil {
				return nil, platformFailure("platform_service_unavailable", "session question answering is unavailable", true)
			}
			item, err := e.queries.GetSession(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			if item == nil || !scope.Allows(item.ProjectID) {
				return nil, platformFailure("session_not_found", "session not found", false)
			}
			result, err := e.questions.Answer(ctx, application.AnswerSessionQuestionCommand{
				SessionID: item.ID, QuestionID: payload.QuestionID, Authorization: authorization,
				Answer: sessionprompt.Answer{Option: payload.Option, Options: payload.Options, Text: payload.Text, Answers: payload.Answers},
			})
			if err != nil {
				return nil, err
			}
			return result, nil
		}}, nil
	case "sessions.stop":
		return e.sessionWithoutPayload(input, target, func(ctx context.Context, sessionID string, authorization application.ActionAuthorization) (any, error) {
			item, err := e.service.Stop(ctx, application.StopSessionCommand{SessionID: sessionID, Authorization: authorization})
			if err != nil {
				return nil, err
			}
			return e.view(ctx, *item), nil
		})
	case "sessions.close_completed":
		return e.sessionWithoutPayload(input, target, func(ctx context.Context, sessionID string, authorization application.ActionAuthorization) (any, error) {
			item, err := e.queries.GetSession(ctx, sessionID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err != nil || item == nil || !input.ProjectScope.Allows(item.ProjectID) {
				return nil, platformFailure("session_not_found", "session not found", false)
			}
			awaiting := false
			if e.questions != nil && e.runtime != nil && e.runtime.IsSessionRunning(item.ID) {
				question, err := e.questions.Pending(ctx, item.ID)
				awaiting = err == nil && question != nil
			}
			stopped, err := e.service.CloseCompleted(ctx, application.CloseCompletedSessionCommand{
				SessionID: item.ID, AwaitingInput: awaiting, Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return e.view(ctx, *stopped), nil
		})
	case "sessions.isolate":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionIsolatePayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{
			preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "reason": payload.Reason}),
			execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
				result, err := e.service.Isolate(ctx, application.IsolateSessionCommand{
					SessionID: sessionID, Reason: payload.Reason, Briefing: payload.Briefing,
					Authorization: authorization,
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"session":      e.view(ctx, *result.Session),
					"workspace_id": result.Workspace.ID,
					"work_dir":     result.Workspace.Path,
					"branch":       result.Workspace.Branch,
				}, nil
			}}, nil
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
			return e.view(ctx, *item), nil
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
		awaitAck := true
		if payload.AwaitAck != nil {
			awaitAck = *payload.AwaitAck
		}
		rejectIfBusy := !payload.Force
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "input_bytes": len([]byte(text))}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			result, err := e.service.SendInputWithAck(ctx, application.SendSessionInputCommand{
				SessionID: sessionID, Text: text, Authorization: authorization,
				RejectIfBusy: rejectIfBusy, AwaitAck: awaitAck,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"sent": result.Submitted, "acknowledged": result.Acknowledged, "session_id": sessionID}, nil
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
			return e.view(ctx, *item), nil
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
			return e.view(ctx, *item), nil
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
		if payload.ImageCount <= 0 || payload.ImageCount > 20 {
			return nil, missingPayloadField("image_count", "an integer from 1 to 20 (the number of images pasted)")
		}
		if utf8.RuneCountInString(strings.TrimSpace(payload.UserPrompt)) > 4000 {
			return nil, platformFailure("platform_payload_invalid", `payload field "user_prompt" exceeds 4000 characters`, false)
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

// persistedHistory answers sessions.history for a session whose runtime is
// gone: the last screen recorded at exit and why it ended.
func (e *sessionPlatformExecutor) persistedHistory(ctx context.Context, sessionID string, maxBytes int, runtimeErr error) (any, error) {
	if e.queries == nil {
		return nil, runtimeErr
	}
	item, err := e.queries.GetSession(ctx, sessionID)
	if err != nil || item == nil {
		return nil, runtimeErr
	}
	content, truncated := boundedExecutionText(item.LastOutput, maxBytes)
	return sessionHistoryAutomationView{
		SessionID: item.ID, Content: content, Bytes: len([]byte(content)), Truncated: truncated,
		Redacted: true, Source: "persisted", Status: item.Status, ErrorReason: item.ErrorReason,
	}, nil
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
		withPayloadSchema(executionReadCapability("sessions.events_status", "session_event_watcher", "sessions:read"), sessionTargetDescription, nil, "", ""),
		withPayloadSchema(platformMutation(executionReadCapability("sessions.events_watch_start", "session_event_watcher", "sessions:read")), sessionTargetDescription, nil, "", ""),
		withPayloadSchema(executionDestructiveCapability("sessions.events_watch_stop", "session_event_watcher", "sessions:read"), sessionTargetDescription, nil, "", ""),
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
		withPayloadSchema(platformMutation(executionReadCapability("sessions.suggest_task_data", "session_task_suggestions", "sessions:read", "ai:use")), sessionTargetDescription, nil, "", ""),
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
