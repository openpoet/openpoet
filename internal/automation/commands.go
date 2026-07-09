package automation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

const maxCommandFieldLength = 200

type commandEnvelope struct {
	CommandID       string                     `json:"command_id"`
	IdempotencyKey  string                     `json:"idempotency_key,omitempty"`
	Actor           json.RawMessage            `json:"actor,omitempty"`
	Capability      application.CapabilityName `json:"capability"`
	Target          commandTarget              `json:"target"`
	Payload         json.RawMessage            `json:"payload"`
	ExpectedVersion *int64                     `json:"expected_version,omitempty"`
	Reason          string                     `json:"reason,omitempty"`
	DryRun          bool                       `json:"dry_run,omitempty"`
	CorrelationID   string                     `json:"correlation_id,omitempty"`
	ApprovalToken   string                     `json:"approval_token,omitempty"`
}

type commandTarget struct {
	Type      string          `json:"type,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	ID        json.RawMessage `json:"id,omitempty"`
	ProjectID int64           `json:"project_id,omitempty"`
	TaskID    int64           `json:"task_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
}

type commandResponse struct {
	APIVersion    string                     `json:"api_version"`
	CommandID     string                     `json:"command_id"`
	CorrelationID string                     `json:"correlation_id,omitempty"`
	Capability    application.CapabilityName `json:"capability"`
	Status        string                     `json:"status"`
	Actor         Actor                      `json:"actor"`
	Result        any                        `json:"result"`
}

type commandFailure struct {
	status    int
	code      string
	message   string
	retryable bool
	details   map[string]any
}

func (e *commandFailure) Error() string {
	return e.message
}

func (a *commandAPI) executeCommand(w http.ResponseWriter, r *http.Request) {
	command, err := decodeCommandEnvelope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "command_envelope_invalid", err.Error(), false)
		return
	}
	command.CommandID = strings.TrimSpace(command.CommandID)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	if command.CommandID == "" {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusBadRequest, code: "command_id_required", message: "command_id is required"})
		return
	}
	if len(command.CommandID) > maxCommandFieldLength {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusBadRequest, code: "command_id_invalid", message: "command_id is too long"})
		return
	}
	if len(command.CorrelationID) > maxCommandFieldLength {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusBadRequest, code: "correlation_id_invalid", message: "correlation_id is too long"})
		return
	}
	if a == nil || a.capabilities == nil {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusServiceUnavailable, code: "capability_registry_unavailable", message: "the capability registry is unavailable", retryable: true})
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusUnauthorized, code: "authentication_required", message: "automation actor is missing"})
		return
	}
	capability, ok := a.capabilities.Lookup(command.Capability)
	if !ok {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusNotFound, code: "capability_not_found", message: "the requested capability is not registered"})
		return
	}
	requiredScope := Scope(capability.Scope)
	if !actor.Scopes.Has(requiredScope) {
		a.writeCommandError(w, command, &commandFailure{
			status: http.StatusForbidden, code: "insufficient_scope", message: "the automation client lacks the capability scope",
			details: map[string]any{"required_scope": requiredScope},
		})
		return
	}
	if command.ExpectedVersion != nil {
		a.writeCommandError(w, command, &commandFailure{
			status: http.StatusUnprocessableEntity, code: "expected_version_unsupported",
			message: "optimistic concurrency is not supported for project tasks in automation API v1",
			details: map[string]any{"expected_version": *command.ExpectedVersion},
		})
		return
	}
	if !command.DryRun && capability.Risk == application.CapabilityRiskDestructive && strings.TrimSpace(command.Reason) == "" {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusBadRequest, code: "reason_required", message: "reason is required for destructive commands"})
		return
	}
	if !command.DryRun && capability.Approval == application.ApprovalExplicit && strings.TrimSpace(command.ApprovalToken) == "" {
		a.writeCommandError(w, command, &commandFailure{status: http.StatusConflict, code: "approval_required", message: "approval_token is required for this capability"})
		return
	}

	result, err := dispatchProjectTaskCommand(r, capability, command, actor)
	if err != nil {
		a.writeCommandError(w, command, err)
		return
	}
	status := "succeeded"
	if command.DryRun {
		status = "dry_run"
	}
	writeJSON(w, http.StatusOK, commandResponse{
		APIVersion: APIVersion, CommandID: command.CommandID, CorrelationID: command.CorrelationID,
		Capability: command.Capability, Status: status, Actor: actor, Result: result,
	})
}

func decodeCommandEnvelope(r *http.Request) (*commandEnvelope, error) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var command commandEnvelope
	if err := decoder.Decode(&command); err != nil {
		return nil, fmt.Errorf("invalid command envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid command envelope: multiple JSON values")
	}
	if len(bytes.TrimSpace(command.Payload)) == 0 {
		command.Payload = json.RawMessage(`{}`)
	}
	return &command, nil
}

func (a *commandAPI) writeCommandError(w http.ResponseWriter, command *commandEnvelope, err error) {
	failure := &commandFailure{status: http.StatusInternalServerError, code: "command_failed", message: "the command could not be completed", retryable: true}
	var typedFailure *commandFailure
	var applicationError *application.Error
	switch {
	case errors.As(err, &typedFailure):
		failure = typedFailure
	case errors.As(err, &applicationError):
		failure.code = applicationError.Code
		failure.message = applicationError.Message
		failure.retryable = false
		switch applicationError.Kind {
		case application.ErrorValidation:
			failure.status = http.StatusUnprocessableEntity
		case application.ErrorNotFound:
			failure.status = http.StatusNotFound
		case application.ErrorConflict:
			failure.status = http.StatusConflict
		}
	}
	if failure.code == "" {
		failure.code = "command_failed"
	}
	commandID := ""
	correlationID := ""
	if command != nil {
		commandID = command.CommandID
		correlationID = command.CorrelationID
	}
	writeErrorWithMetadata(w, failure.status, failure.code, failure.message, failure.retryable, commandID, correlationID, failure.details)
}

func projectTaskServiceFor(registry *application.CapabilityRegistry, name application.CapabilityName) (*application.ProjectTaskService, error) {
	if registry == nil {
		return nil, errors.New("capability registry is unavailable")
	}
	capability, ok := registry.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("capability %s is unavailable", name)
	}
	service, ok := capability.Service.(*application.ProjectTaskService)
	if !ok || service == nil {
		return nil, fmt.Errorf("capability %s is not bound to the project task service", name)
	}
	return service, nil
}

func dispatchProjectTaskCommand(r *http.Request, capability application.Capability, command *commandEnvelope, actor Actor) (any, error) {
	service, ok := capability.Service.(*application.ProjectTaskService)
	if !ok || service == nil {
		return nil, &commandFailure{status: http.StatusNotImplemented, code: "capability_handler_unsupported", message: "the capability handler is not available"}
	}
	applicationActor := application.Actor{Type: actor.Type, ID: actor.ClientID}
	commandContext := application.WithEventMetadata(r.Context(), application.EventMetadata{
		Actor: applicationActor, CorrelationID: command.CorrelationID,
	})

	switch capability.Handler {
	case application.CapabilityHandlerTasksList:
		var payload taskListPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID := command.Target.resolveProjectID(payload.ProjectID)
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID}), nil
		}
		if projectID > 0 {
			return service.ListByProject(commandContext, projectID)
		}
		return service.ListAll(commandContext, database.TaskFilter{Status: payload.Status, Priority: payload.Priority, Search: payload.Search})

	case application.CapabilityHandlerTasksGet:
		var payload taskReferencePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		return service.Get(commandContext, projectID, taskID)

	case application.CapabilityHandlerTasksCreate:
		var payload createTaskPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID := command.Target.resolveProjectID(payload.ProjectID)
		if projectID <= 0 {
			return nil, invalidTarget("project_id is required")
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID}), nil
		}
		return service.Create(commandContext, application.CreateProjectTaskCommand{
			ProjectID: projectID, Title: payload.Title, Description: payload.Description,
			Status: payload.Status, Priority: payload.Priority, DueDate: payload.DueDate,
			ParentID: payload.ParentID, SortOrder: payload.SortOrder, Actor: applicationActor,
		})

	case application.CapabilityHandlerTasksUpdate:
		var payload updateTaskPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		return service.Update(commandContext, application.UpdateProjectTaskCommand{
			ProjectID: projectID, TaskID: taskID, Title: payload.Title, Description: payload.Description,
			Status: payload.Status, Priority: payload.Priority, DueDate: payload.DueDate,
			ParentID: payload.ParentID, SortOrder: payload.SortOrder, Actor: applicationActor,
		})

	case application.CapabilityHandlerTasksChangeStatus:
		var payload changeStatusPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		return service.ChangeStatus(commandContext, application.ChangeTaskStatusCommand{
			ProjectID: projectID, TaskID: taskID, Status: payload.Status, Actor: applicationActor,
		})

	case application.CapabilityHandlerTasksDelete:
		var payload taskReferencePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		if err := service.Delete(commandContext, projectID, taskID); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": true, "project_id": projectID, "task_id": taskID}, nil

	case application.CapabilityHandlerTasksDuplicate:
		var payload taskReferencePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		return service.Duplicate(commandContext, projectID, taskID, applicationActor)

	case application.CapabilityHandlerTasksReorderProject:
		projectID := command.Target.resolveProjectID(0)
		if projectID <= 0 {
			return nil, invalidTarget("project_id is required")
		}
		items, err := decodeReorderPayload[database.ReorderItem](command.Payload)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "item_count": len(items)}), nil
		}
		if err := service.ReorderProject(commandContext, projectID, items); err != nil {
			return nil, err
		}
		return map[string]any{"reordered": len(items), "project_id": projectID}, nil

	case application.CapabilityHandlerTasksReorderGlobal:
		items, err := decodeReorderPayload[database.GlobalReorderItem](command.Payload)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"item_count": len(items)}), nil
		}
		if err := service.ReorderGlobal(commandContext, items); err != nil {
			return nil, err
		}
		return map[string]any{"reordered": len(items)}, nil

	case application.CapabilityHandlerTasksApproveVerification, application.CapabilityHandlerTasksRejectVerification:
		var payload taskReferencePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		if capability.Handler == application.CapabilityHandlerTasksApproveVerification {
			return service.ApproveVerification(commandContext, projectID, taskID, applicationActor)
		}
		return service.RejectVerification(commandContext, projectID, taskID, applicationActor)

	case application.CapabilityHandlerTasksLinkSession:
		var payload linkSessionPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		sessionID := command.Target.resolveSessionID(payload.SessionID)
		if sessionID == "" {
			return nil, invalidTarget("session_id is required")
		}
		title, description, priority := payload.Title, payload.Description, payload.Priority
		if payload.TaskData != nil {
			title, description, priority = payload.TaskData.Title, payload.TaskData.Description, payload.TaskData.Priority
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"session_id": sessionID}), nil
		}
		return service.LinkSession(commandContext, application.LinkSessionTaskCommand{
			SessionID: sessionID, TaskID: payload.TaskID, Title: title,
			Description: description, Priority: priority, Actor: applicationActor,
		})

	case application.CapabilityHandlerTasksUnlinkSession:
		var payload sessionReferencePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		sessionID := command.Target.resolveSessionID(payload.SessionID)
		if sessionID == "" {
			return nil, invalidTarget("session_id is required")
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"session_id": sessionID}), nil
		}
		return service.UnlinkSession(commandContext, sessionID, applicationActor)

	case application.CapabilityHandlerTasksAddComment:
		var payload addCommentPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, taskID, err := command.Target.resolveTask(payload.ProjectID, payload.TaskID)
		if err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"project_id": projectID, "task_id": taskID}), nil
		}
		if err := service.AddComment(commandContext, projectID, taskID, payload.Comment, applicationActor); err != nil {
			return nil, err
		}
		return map[string]any{"commented": true, "project_id": projectID, "task_id": taskID}, nil
	default:
		return nil, &commandFailure{status: http.StatusNotImplemented, code: "capability_handler_unsupported", message: "the capability handler is not implemented"}
	}
}

type taskListPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Status    string `json:"status,omitempty"`
	Priority  string `json:"priority,omitempty"`
	Search    string `json:"search,omitempty"`
}

type taskReferencePayload struct {
	ProjectID int64 `json:"project_id,omitempty"`
	TaskID    int64 `json:"task_id,omitempty"`
}

type createTaskPayload struct {
	ProjectID   int64  `json:"project_id,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
	Priority    string `json:"priority,omitempty"`
	DueDate     string `json:"due_date,omitempty"`
	ParentID    *int64 `json:"parent_id,omitempty"`
	SortOrder   int    `json:"sort_order,omitempty"`
}

type updateTaskPayload struct {
	ProjectID   int64   `json:"project_id,omitempty"`
	TaskID      int64   `json:"task_id,omitempty"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Status      *string `json:"status,omitempty"`
	Priority    *string `json:"priority,omitempty"`
	DueDate     *string `json:"due_date,omitempty"`
	ParentID    *int64  `json:"parent_id,omitempty"`
	SortOrder   *int    `json:"sort_order,omitempty"`
}

type changeStatusPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	TaskID    int64  `json:"task_id,omitempty"`
	Status    string `json:"status"`
}

type linkSessionPayload struct {
	SessionID   string `json:"session_id,omitempty"`
	TaskID      *int64 `json:"task_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty"`
	TaskData    *struct {
		Title       string `json:"title"`
		Description string `json:"description,omitempty"`
		Priority    string `json:"priority,omitempty"`
	} `json:"task_data,omitempty"`
}

type sessionReferencePayload struct {
	SessionID string `json:"session_id,omitempty"`
}

type addCommentPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	TaskID    int64  `json:"task_id,omitempty"`
	Comment   string `json:"comment"`
}

func decodePayload(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &commandFailure{status: http.StatusBadRequest, code: "payload_invalid", message: fmt.Sprintf("invalid capability payload: %v", err)}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &commandFailure{status: http.StatusBadRequest, code: "payload_invalid", message: "invalid capability payload: multiple JSON values"}
	}
	return nil
}

func decodeReorderPayload[T any](raw json.RawMessage) ([]T, error) {
	var items []T
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, &commandFailure{status: http.StatusBadRequest, code: "payload_invalid", message: fmt.Sprintf("invalid reorder payload: %v", err)}
		}
		return items, nil
	}
	var wrapper struct {
		Items []T `json:"items"`
	}
	if err := decodePayload(raw, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Items, nil
}

func invalidTarget(message string) error {
	return &commandFailure{status: http.StatusBadRequest, code: "target_invalid", message: message}
}

func dryRunResult(capability application.Capability, target map[string]any) map[string]any {
	return map[string]any{
		"would_execute": true,
		"handler":       capability.Handler,
		"service":       capability.Service.CapabilityServiceName(),
		"target":        target,
	}
}

func (t commandTarget) targetKind() string {
	kind := t.Kind
	if kind == "" {
		kind = t.Type
	}
	return strings.ToLower(strings.TrimSpace(kind))
}

func (t commandTarget) resolveProjectID(fallback int64) int64 {
	if t.ProjectID > 0 {
		return t.ProjectID
	}
	if t.targetKind() == "project" {
		if id, ok := rawInt64(t.ID); ok {
			return id
		}
	}
	return fallback
}

func (t commandTarget) resolveTask(projectFallback, taskFallback int64) (int64, int64, error) {
	projectID := t.resolveProjectID(projectFallback)
	taskID := t.TaskID
	if taskID <= 0 && (t.targetKind() == "task" || t.targetKind() == "project_task") {
		taskID, _ = rawInt64(t.ID)
	}
	if taskID <= 0 {
		taskID = taskFallback
	}
	if projectID <= 0 || taskID <= 0 {
		return 0, 0, invalidTarget("project_id and task_id are required")
	}
	return projectID, taskID, nil
}

func (t commandTarget) resolveSessionID(fallback string) string {
	if strings.TrimSpace(t.SessionID) != "" {
		return strings.TrimSpace(t.SessionID)
	}
	if t.targetKind() == "session" {
		var value string
		if err := json.Unmarshal(t.ID, &value); err == nil {
			return strings.TrimSpace(value)
		}
	}
	return strings.TrimSpace(fallback)
}

func rawInt64(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil && number > 0 {
		return number, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	return parsed, err == nil && parsed > 0
}
