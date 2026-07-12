package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"openpoet/internal/database"
)

const CapabilityServiceWorkRuns CapabilityServiceName = "work_runs"

const (
	maxWorkRunDescriptionLength = 20_000
	maxWorkRunMetadataLength    = 200
	maxExecutionTargetType      = 100
	maxExecutionTargetID        = 1_000
	maxExpectedMinutes          = 525_600
)

type WorkRunStore interface {
	WithWorkRunTx(ctx context.Context, fn func(*database.WorkRunTx) error) error
	GetWorkRun(ctx context.Context, id string) (*database.WorkRun, error)
	ListWorkRuns(ctx context.Context, filter database.WorkRunFilter) ([]database.WorkRun, error)
	ListWorkRunAudit(ctx context.Context, runID string, limit int) ([]database.WorkRunAudit, error)
	ListPlans(ctx context.Context, filter database.PlanFilter) ([]database.Plan, error)
	GetPlan(ctx context.Context, idOrExternalRef string) (*database.Plan, error)
	ListPlanItems(ctx context.Context, planIDOrExternalRef string) ([]database.PlanItem, error)
}

type WorkRunServiceOptions struct {
	Now   func() time.Time
	NewID func() string
}

type WorkRunService struct {
	store WorkRunStore
	now   func() time.Time
	newID func() string
}

func NewWorkRunService(store WorkRunStore, options ...WorkRunServiceOptions) *WorkRunService {
	configured := WorkRunServiceOptions{Now: time.Now, NewID: uuid.NewString}
	if len(options) > 0 {
		if options[0].Now != nil {
			configured.Now = options[0].Now
		}
		if options[0].NewID != nil {
			configured.NewID = options[0].NewID
		}
	}
	return &WorkRunService{store: store, now: configured.Now, newID: configured.NewID}
}

func (s *WorkRunService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceWorkRuns
}

type StartWorkRunCommand struct {
	Title           string
	Description     string
	Source          string
	ExpectedMinutes int
	ProjectTaskID   *int64
	SessionID       *string
	PlanItemRef     *string
	ExecutionTarget *database.WorkRunExecutionTarget
	Actor           Actor
	CorrelationID   string
	IdempotencyKey  string
}

func (s *WorkRunService) Start(ctx context.Context, command StartWorkRunCommand) (*database.WorkRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("work run service is unavailable")
	}
	title := strings.TrimSpace(command.Title)
	if title == "" {
		title = strings.TrimSpace(command.Description)
	}
	if title == "" {
		return nil, validationError("work_run_title_required", "Work run title is required")
	}
	if len(title) > 500 {
		return nil, validationError("work_run_title_invalid", "Work run title is too long")
	}
	if len(command.Description) > maxWorkRunDescriptionLength {
		return nil, validationError("work_run_description_invalid", "Work run description is too long")
	}
	if command.ExpectedMinutes <= 0 || command.ExpectedMinutes > maxExpectedMinutes {
		return nil, validationError("expected_minutes_invalid", "expected_minutes must be between 1 and 525600")
	}
	source := strings.TrimSpace(command.Source)
	if source == "" {
		source = "ad-hoc"
	}
	if len(source) > 100 {
		return nil, validationError("work_run_source_invalid", "Work run source is too long")
	}
	if err := validateWorkRunMetadata(command.Actor, command.CorrelationID, command.IdempotencyKey); err != nil {
		return nil, err
	}
	actor := command.Actor.eventValue()
	correlationID := strings.TrimSpace(command.CorrelationID)
	idempotencyKey := strings.TrimSpace(command.IdempotencyKey)
	now := s.now().UTC()

	var run *database.WorkRun
	err := s.store.WithWorkRunTx(ctx, func(tx *database.WorkRunTx) error {
		existing, err := tx.FindStartedWorkRun(ctx, actor, idempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameStartWorkRun(existing, title, command, source) {
				return conflictError("work_run_idempotency_conflict", "Idempotency key was already used for a different work run")
			}
			run = existing
			return nil
		}
		if err := validateWorkRunLinks(ctx, tx, command.ProjectTaskID, command.SessionID, command.PlanItemRef); err != nil {
			return err
		}
		if command.ExecutionTarget != nil {
			targetType := strings.TrimSpace(command.ExecutionTarget.Type)
			targetID := strings.TrimSpace(command.ExecutionTarget.ID)
			if targetType == "" || targetID == "" {
				return validationError("execution_target_invalid", "execution_target type and id are required")
			}
			if len(targetType) > maxExecutionTargetType || len(targetID) > maxExecutionTargetID {
				return validationError("execution_target_invalid", "execution_target exceeds its maximum length")
			}
		}
		run = &database.WorkRun{
			ID: s.newID(), Title: title, Description: command.Description, Source: source,
			Status: database.WorkRunStatusRunning, ExpectedMinutes: command.ExpectedMinutes,
			StartedAt: now, ActiveStartedAt: timePointer(now), ProjectTaskID: command.ProjectTaskID,
			SessionID: trimmedStringPointer(command.SessionID), PlanItemRef: trimmedStringPointer(command.PlanItemRef),
			Version: 1, CreatedByActor: actor, UpdatedByActor: actor,
			CorrelationID: correlationID, IdempotencyKey: nullableString(idempotencyKey),
			CreatedAt: now, UpdatedAt: now,
		}
		if command.ExecutionTarget != nil {
			run.ExecutionTargetType = strings.TrimSpace(command.ExecutionTarget.Type)
			run.ExecutionTargetID = strings.TrimSpace(command.ExecutionTarget.ID)
		}
		if err := tx.InsertWorkRun(ctx, run); err != nil {
			return err
		}
		if err := insertWorkRunAudit(ctx, tx, run, "start", "", run.Status, command.Actor, correlationID, idempotencyKey, now, map[string]any{
			"expected_minutes": run.ExpectedMinutes, "source": run.Source,
		}); err != nil {
			return err
		}
		return appendWorkRunEvent(ctx, tx, "work_run.started", run, command.Actor, correlationID, map[string]any{
			"status": run.Status, "expected_minutes": run.ExpectedMinutes, "source": run.Source,
		})
	})
	if err != nil {
		return nil, err
	}
	run.Hydrate(s.now().UTC())
	return run, nil
}

func (s *WorkRunService) Get(ctx context.Context, id string) (*database.WorkRun, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, validationError("work_run_id_required", "work_run_id is required")
	}
	if len(id) > maxWorkRunMetadataLength {
		return nil, validationError("work_run_id_invalid", "work_run_id is too long")
	}
	run, err := s.store.GetWorkRun(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundError("work_run_not_found", "Work run not found", err)
		}
		return nil, err
	}
	run.Hydrate(s.now().UTC())
	return run, nil
}

func (s *WorkRunService) List(ctx context.Context, filter database.WorkRunFilter) ([]database.WorkRun, error) {
	if filter.Status != "" && !validWorkRunStatus(filter.Status) {
		return nil, validationError("work_run_status_invalid", "Invalid work run status")
	}
	if len(strings.TrimSpace(filter.Source)) > 100 {
		return nil, validationError("work_run_source_invalid", "source is too long")
	}
	runs, err := s.store.ListWorkRuns(ctx, filter)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	for index := range runs {
		runs[index].Hydrate(now)
	}
	return runs, nil
}

func (s *WorkRunService) ListAudit(ctx context.Context, runID string, limit int) ([]database.WorkRunAudit, error) {
	return s.store.ListWorkRunAudit(ctx, strings.TrimSpace(runID), limit)
}

type TransitionWorkRunCommand struct {
	WorkRunID           string
	ExpectedVersion     *int64
	CompleteProjectTask bool
	Reason              string
	Actor               Actor
	CorrelationID       string
	IdempotencyKey      string
}

func (s *WorkRunService) Pause(ctx context.Context, command TransitionWorkRunCommand) (*database.WorkRun, error) {
	return s.transition(ctx, "pause", database.WorkRunStatusPaused, command)
}

func (s *WorkRunService) Resume(ctx context.Context, command TransitionWorkRunCommand) (*database.WorkRun, error) {
	return s.transition(ctx, "resume", database.WorkRunStatusRunning, command)
}

func (s *WorkRunService) Complete(ctx context.Context, command TransitionWorkRunCommand) (*database.WorkRun, error) {
	return s.transition(ctx, "complete", database.WorkRunStatusCompleted, command)
}

func (s *WorkRunService) Cancel(ctx context.Context, command TransitionWorkRunCommand) (*database.WorkRun, error) {
	return s.transition(ctx, "cancel", database.WorkRunStatusCancelled, command)
}

func (s *WorkRunService) transition(ctx context.Context, action, targetStatus string, command TransitionWorkRunCommand) (*database.WorkRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("work run service is unavailable")
	}
	runID := strings.TrimSpace(command.WorkRunID)
	if runID == "" {
		return nil, validationError("work_run_id_required", "work_run_id is required")
	}
	if len(runID) > maxWorkRunMetadataLength {
		return nil, validationError("work_run_id_invalid", "work_run_id is too long")
	}
	if len(strings.TrimSpace(command.Reason)) > maxWorkRunDescriptionLength {
		return nil, validationError("work_run_reason_invalid", "reason is too long")
	}
	if err := validateWorkRunMetadata(command.Actor, command.CorrelationID, command.IdempotencyKey); err != nil {
		return nil, err
	}
	actor := command.Actor.eventValue()
	correlationID := strings.TrimSpace(command.CorrelationID)
	idempotencyKey := strings.TrimSpace(command.IdempotencyKey)
	now := s.now().UTC()
	var run *database.WorkRun
	err := s.store.WithWorkRunTx(ctx, func(tx *database.WorkRunTx) error {
		current, err := tx.GetWorkRun(ctx, runID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFoundError("work_run_not_found", "Work run not found", err)
			}
			return err
		}
		run = current
		audit, err := tx.GetAuditByIdempotency(ctx, runID, idempotencyKey)
		if err != nil {
			return err
		}
		if audit != nil {
			if audit.Action != action {
				return conflictError("work_run_idempotency_conflict", "Idempotency key was already used for a different transition")
			}
			return nil
		}
		if command.ExpectedVersion != nil && current.Version != *command.ExpectedVersion {
			return conflictError("work_run_version_conflict", fmt.Sprintf("Work run version is %d, expected %d", current.Version, *command.ExpectedVersion))
		}
		if current.Status == targetStatus {
			if targetStatus == database.WorkRunStatusCompleted && command.CompleteProjectTask {
				return conflictError("work_run_already_completed", "Work run was already completed without this project task transition")
			}
			return nil
		}
		if !allowedWorkRunTransition(current.Status, targetStatus) {
			return conflictError("work_run_transition_invalid", fmt.Sprintf("Cannot %s work run from %s", action, current.Status))
		}

		previousStatus := current.Status
		previousVersion := current.Version
		if current.Status == database.WorkRunStatusRunning {
			current.AccumulatedActiveSeconds += elapsedSeconds(current.ActiveStartedAt, now)
		}
		current.Status = targetStatus
		current.Version++
		current.UpdatedByActor = actor
		current.CorrelationID = correlationID
		current.UpdatedAt = now
		switch targetStatus {
		case database.WorkRunStatusRunning:
			current.ActiveStartedAt = timePointer(now)
			current.PausedAt = nil
		case database.WorkRunStatusPaused:
			current.ActiveStartedAt = nil
			current.PausedAt = timePointer(now)
		case database.WorkRunStatusCompleted:
			current.ActiveStartedAt = nil
			current.PausedAt = nil
			current.CompletedAt = timePointer(now)
		case database.WorkRunStatusCancelled:
			current.ActiveStartedAt = nil
			current.PausedAt = nil
			current.CancelledAt = timePointer(now)
		}
		if err := tx.UpdateWorkRun(ctx, current, previousVersion); err != nil {
			if errors.Is(err, database.ErrWorkRunVersionConflict) {
				return conflictError("work_run_version_conflict", "Work run changed concurrently")
			}
			return err
		}
		if targetStatus == database.WorkRunStatusCompleted && command.CompleteProjectTask {
			if err := completeLinkedProjectTask(ctx, tx, current, command.Actor, correlationID, now); err != nil {
				return err
			}
		}
		if err := insertWorkRunAudit(ctx, tx, current, action, previousStatus, targetStatus,
			command.Actor, correlationID, idempotencyKey, now, map[string]any{
				"reason": strings.TrimSpace(command.Reason), "complete_project_task": command.CompleteProjectTask,
			}); err != nil {
			return err
		}
		return appendWorkRunEvent(ctx, tx, "work_run."+pastTense(action), current, command.Actor, correlationID, map[string]any{
			"old_status": previousStatus, "new_status": targetStatus,
			"active_seconds":        current.AccumulatedActiveSeconds,
			"complete_project_task": command.CompleteProjectTask,
		})
	})
	if err != nil {
		return nil, err
	}
	run.Hydrate(s.now().UTC())
	return run, nil
}

func validateWorkRunLinks(ctx context.Context, tx *database.WorkRunTx, taskID *int64, sessionID, planItemRef *string) error {
	var task *database.ProjectTask
	var session *database.Session
	var err error
	if taskID != nil {
		if *taskID <= 0 {
			return validationError("project_task_invalid", "project_task_id must be positive")
		}
		task, err = tx.GetProjectTask(ctx, *taskID)
		if err != nil {
			return validationError("project_task_invalid", "Project task not found")
		}
	}
	if sessionID != nil && strings.TrimSpace(*sessionID) != "" {
		if len(strings.TrimSpace(*sessionID)) > maxWorkRunMetadataLength {
			return validationError("session_invalid", "session_id is too long")
		}
		session, err = tx.GetSession(ctx, strings.TrimSpace(*sessionID))
		if err != nil {
			return validationError("session_invalid", "Session not found")
		}
	}
	if task != nil && session != nil && task.ProjectID != session.ProjectID {
		return validationError("work_run_link_mismatch", "Project task and session belong to different projects")
	}
	if planItemRef != nil && strings.TrimSpace(*planItemRef) != "" {
		if len(strings.TrimSpace(*planItemRef)) > maxWorkRunMetadataLength {
			return validationError("plan_item_invalid", "plan_item_ref is too long")
		}
		item, err := tx.GetPlanItemByRef(ctx, strings.TrimSpace(*planItemRef))
		if err != nil {
			return validationError("plan_item_invalid", "Plan item not found")
		}
		if taskID != nil && item.ProjectTaskID != nil && *item.ProjectTaskID != *taskID {
			return validationError("work_run_link_mismatch", "Plan item and work run reference different project tasks")
		}
	}
	return nil
}

func completeLinkedProjectTask(ctx context.Context, tx *database.WorkRunTx, run *database.WorkRun, actor Actor, correlationID string, now time.Time) error {
	if run.ProjectTaskID == nil {
		return validationError("project_task_required", "complete_project_task requires a linked project_task_id")
	}
	task, err := tx.GetProjectTask(ctx, *run.ProjectTaskID)
	if err != nil {
		return validationError("project_task_invalid", "Linked project task not found")
	}
	if task.Status == TaskStatusAwaitingApproval {
		return conflictError("project_task_approval_required", "Linked project task is awaiting approval")
	}
	if task.Status == TaskStatusDone {
		return nil
	}
	oldStatus := task.Status
	if err := tx.ChangeProjectTaskStatus(ctx, task.ID, TaskStatusDone); err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]any{
		"old": oldStatus, "new": TaskStatusDone, "reason": "explicit_work_run_completion", "work_run_id": run.ID,
	})
	history := &database.TaskHistory{
		TaskID: task.ID, ProjectID: task.ProjectID, EventType: "status_change",
		Details: string(details), Actor: actor.historyValue(),
	}
	if run.SessionID != nil {
		history.SessionID = sql.NullString{String: *run.SessionID, Valid: true}
	}
	if err := tx.CreateProjectTaskHistory(ctx, history); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"project_id": task.ProjectID, "task_id": task.ID, "old_status": oldStatus,
		"new_status": TaskStatusDone, "work_run_id": run.ID,
	})
	_, err = tx.AppendEvent(ctx, database.EventOutboxAppend{
		EventID: uuid.NewString(), EventType: ProjectTaskEventStatusChanged,
		AggregateType: "project_task", AggregateID: fmt.Sprint(task.ID), Actor: actor.eventValue(),
		CorrelationID: correlationID, SchemaVersion: 1, PayloadJSON: string(payload), OccurredAt: now,
	})
	return err
}

func insertWorkRunAudit(
	ctx context.Context,
	tx *database.WorkRunTx,
	run *database.WorkRun,
	action, fromStatus, toStatus string,
	actor Actor,
	correlationID, idempotencyKey string,
	now time.Time,
	details map[string]any,
) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return tx.InsertAudit(ctx, &database.WorkRunAudit{
		WorkRunID: run.ID, Action: action, FromStatus: fromStatus, ToStatus: toStatus,
		Actor: actor.historyValue(), CorrelationID: correlationID,
		IdempotencyKey: nullableString(idempotencyKey), Version: run.Version,
		DetailsJSON: string(detailsJSON), OccurredAt: now,
	})
}

func appendWorkRunEvent(
	ctx context.Context,
	tx *database.WorkRunTx,
	eventType string,
	run *database.WorkRun,
	actor Actor,
	correlationID string,
	payload map[string]any,
) error {
	payload["work_run_id"] = run.ID
	payload["version"] = run.Version
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.AppendEvent(ctx, database.EventOutboxAppend{
		EventID: uuid.NewString(), EventType: eventType, AggregateType: "work_run",
		AggregateID: run.ID, Actor: actor.eventValue(), CorrelationID: correlationID,
		SchemaVersion: 1, PayloadJSON: string(payloadJSON), OccurredAt: run.UpdatedAt,
	})
	return err
}

func allowedWorkRunTransition(from, to string) bool {
	switch to {
	case database.WorkRunStatusPaused:
		return from == database.WorkRunStatusRunning
	case database.WorkRunStatusRunning:
		return from == database.WorkRunStatusPaused || from == database.WorkRunStatusIndeterminate
	case database.WorkRunStatusCompleted, database.WorkRunStatusCancelled:
		return from == database.WorkRunStatusRunning || from == database.WorkRunStatusPaused || from == database.WorkRunStatusIndeterminate
	default:
		return false
	}
}

func validWorkRunStatus(status string) bool {
	switch status {
	case database.WorkRunStatusRunning, database.WorkRunStatusPaused, database.WorkRunStatusCompleted,
		database.WorkRunStatusCancelled, database.WorkRunStatusIndeterminate:
		return true
	default:
		return false
	}
}

func pastTense(action string) string {
	switch action {
	case "pause":
		return "paused"
	case "resume":
		return "resumed"
	case "complete":
		return "completed"
	case "cancel":
		return "cancelled"
	default:
		return action
	}
}

func elapsedSeconds(start *time.Time, end time.Time) int64 {
	if start == nil || !end.After(*start) {
		return 0
	}
	return int64(end.Sub(*start) / time.Second)
}

func sameStartWorkRun(existing *database.WorkRun, title string, command StartWorkRunCommand, source string) bool {
	if existing.Title != title || existing.Description != command.Description || existing.Source != source || existing.ExpectedMinutes != command.ExpectedMinutes {
		return false
	}
	if command.ExecutionTarget == nil {
		if existing.ExecutionTargetType != "" || existing.ExecutionTargetID != "" {
			return false
		}
	} else if existing.ExecutionTargetType != strings.TrimSpace(command.ExecutionTarget.Type) ||
		existing.ExecutionTargetID != strings.TrimSpace(command.ExecutionTarget.ID) {
		return false
	}
	return equalInt64Pointers(existing.ProjectTaskID, command.ProjectTaskID) &&
		equalStringPointers(existing.SessionID, trimmedStringPointer(command.SessionID)) &&
		equalStringPointers(existing.PlanItemRef, trimmedStringPointer(command.PlanItemRef))
}

func equalInt64Pointers(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalStringPointers(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func trimmedStringPointer(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func nullableString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func validateWorkRunMetadata(actor Actor, correlationID, idempotencyKey string) error {
	if len(strings.TrimSpace(actor.Type)) > maxWorkRunMetadataLength || len(strings.TrimSpace(actor.ID)) > maxWorkRunMetadataLength {
		return validationError("work_run_actor_invalid", "actor type and id must not exceed 200 characters")
	}
	if len(strings.TrimSpace(correlationID)) > maxWorkRunMetadataLength {
		return validationError("correlation_id_invalid", "correlation_id must not exceed 200 characters")
	}
	if len(strings.TrimSpace(idempotencyKey)) > maxWorkRunMetadataLength {
		return validationError("idempotency_key_invalid", "idempotency_key must not exceed 200 characters")
	}
	return nil
}
