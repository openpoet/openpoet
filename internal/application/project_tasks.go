package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"openpoet/internal/database"
)

const (
	TaskStatusTodo             = "todo"
	TaskStatusInProgress       = "in_progress"
	TaskStatusAwaitingApproval = "awaiting_approval"
	TaskStatusDone             = "done"

	TaskAutoApproveVerificationSetting = "task_auto_approve_verification_enabled"
	TaskAutoApproveInherit             = "inherit"
	TaskAutoApproveEnabled             = "enabled"
	TaskAutoApproveDisabled            = "disabled"
)

type Actor struct {
	Type string
	ID   string
}

func UserActor() Actor {
	return Actor{Type: "user"}
}

func (a Actor) historyValue() string {
	actorType := strings.TrimSpace(a.Type)
	if actorType != "" {
		if actorID := strings.TrimSpace(a.ID); actorID != "" {
			return actorType + ":" + actorID
		}
		return actorType
	}
	return "system"
}

type ProjectTaskStore interface {
	ListAllTasks(ctx context.Context, filter database.TaskFilter) ([]database.ProjectTask, error)
	GetAllTasksSummary(ctx context.Context) (map[string]int, error)
	ListTasksByProject(ctx context.Context, projectID int64) ([]database.ProjectTask, error)
	GetTask(ctx context.Context, taskID int64) (*database.ProjectTask, error)
	ListTaskHistory(ctx context.Context, taskID int64, limit int) ([]database.TaskHistory, error)
	WithProjectTaskTx(ctx context.Context, fn func(*database.ProjectTaskTx) error) error
}

type TaskChange struct {
	Action    string
	ProjectID int64
	Task      *database.ProjectTask
	TaskID    int64
}

type SessionRename struct {
	SessionID string
	Name      string
}

type ProjectTaskEffects interface {
	PublishTaskChange(change TaskChange)
	PublishTaskHistory(history *database.TaskHistory)
	PublishSessionRename(rename SessionRename)
	RequestTaskVerification(task *database.ProjectTask)
}

type ProjectTaskService struct {
	store   ProjectTaskStore
	effects ProjectTaskEffects
	events  ProjectTaskEventPublisher
}

func NewProjectTaskService(store ProjectTaskStore, effects ProjectTaskEffects, publishers ...ProjectTaskEventPublisher) *ProjectTaskService {
	publisher := ProjectTaskEventPublisher(NewTransactionalProjectTaskEventPublisher())
	if len(publishers) > 0 && publishers[0] != nil {
		publisher = publishers[0]
	}
	return &ProjectTaskService{store: store, effects: effects, events: publisher}
}

func (s *ProjectTaskService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceProjectTasks
}

type taskAutoApprovalDecision struct {
	ConfigScope     string
	ProjectOverride string
	GlobalEnabled   bool
}

func (s *ProjectTaskService) resolveTaskStatus(
	ctx context.Context,
	tx *database.ProjectTaskTx,
	projectID int64,
	requestedStatus string,
) (string, *taskAutoApprovalDecision, error) {
	if requestedStatus != TaskStatusAwaitingApproval {
		return requestedStatus, nil, nil
	}
	projectOverride, globalValue, err := tx.TaskAutoApprovalConfig(ctx, projectID, TaskAutoApproveVerificationSetting)
	if err != nil {
		return "", nil, err
	}
	globalEnabled := strings.EqualFold(strings.TrimSpace(globalValue), "true")
	switch projectOverride {
	case TaskAutoApproveEnabled:
		return TaskStatusDone, &taskAutoApprovalDecision{
			ConfigScope: "project", ProjectOverride: TaskAutoApproveEnabled, GlobalEnabled: globalEnabled,
		}, nil
	case TaskAutoApproveDisabled:
		return requestedStatus, nil, nil
	default:
		if globalEnabled {
			return TaskStatusDone, &taskAutoApprovalDecision{
				ConfigScope: "global", ProjectOverride: TaskAutoApproveInherit, GlobalEnabled: true,
			}, nil
		}
		return requestedStatus, nil, nil
	}
}

func (s *ProjectTaskService) recordTaskAutoApproval(
	ctx context.Context,
	tx *database.ProjectTaskTx,
	mutation *mutationEffects,
	task *database.ProjectTask,
	oldStatus string,
	triggeredBy Actor,
	decision *taskAutoApprovalDecision,
) error {
	if decision == nil {
		return nil
	}
	if task.VerificationDocID != "" {
		if err := tx.UpdateDocumentStatus(ctx, task.VerificationDocID, "approved"); err != nil {
			return err
		}
	}
	autoActor := Actor{Type: "system", ID: "verification-auto-approval"}
	history, err := createHistory(ctx, tx, task.ID, task.ProjectID, "verification_auto_approved", map[string]any{
		"old":              oldStatus,
		"requested_status": TaskStatusAwaitingApproval,
		"new":              TaskStatusDone,
		"config_scope":     decision.ConfigScope,
		"project_override": decision.ProjectOverride,
		"global_enabled":   decision.GlobalEnabled,
		"setting_key":      TaskAutoApproveVerificationSetting,
		"triggered_by":     triggeredBy.historyValue(),
	}, autoActor, "")
	if err != nil {
		return err
	}
	mutation.history = append(mutation.history, history)
	return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
		EventType: ProjectTaskEventVerificationApproved, AggregateType: "project_task",
		AggregateID: strconv.FormatInt(task.ID, 10), Actor: autoActor,
		Payload: map[string]any{
			"project_id": task.ProjectID, "task_id": task.ID, "status": TaskStatusDone,
			"automatic": true, "config_scope": decision.ConfigScope,
		},
	})
}

type AllTasksResult struct {
	Tasks   []database.ProjectTask
	Summary map[string]int
}

func (s *ProjectTaskService) ListAll(ctx context.Context, filter database.TaskFilter) (*AllTasksResult, error) {
	statusFilter := filter.Status
	filter.Status = ""
	tasks, err := s.store.ListAllTasks(ctx, filter)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = []database.ProjectTask{}
	}
	database.ApplyUmbrellaStatus(tasks)
	if statusFilter != "" {
		filtered := make([]database.ProjectTask, 0, len(tasks))
		for _, task := range tasks {
			if task.Status == statusFilter {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	summary, _ := s.store.GetAllTasksSummary(ctx)
	return &AllTasksResult{Tasks: tasks, Summary: summary}, nil
}

func (s *ProjectTaskService) ListByProject(ctx context.Context, projectID int64) ([]database.ProjectTask, error) {
	tasks, err := s.store.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = []database.ProjectTask{}
	}
	database.ApplyUmbrellaStatus(tasks)
	return tasks, nil
}

func (s *ProjectTaskService) Get(ctx context.Context, projectID, taskID int64) (*database.ProjectTask, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil || task == nil || task.ProjectID != projectID {
		return nil, notFoundError("task_not_found", "Task not found", err)
	}
	tasks, err := s.store.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	database.ApplyUmbrellaStatus(tasks)
	for _, candidate := range tasks {
		if candidate.ID == taskID {
			task.Status = candidate.Status
			break
		}
	}
	return task, nil
}

type CreateProjectTaskCommand struct {
	ProjectID   int64
	Title       string
	Description string
	Status      string
	Priority    string
	DueDate     string
	ParentID    *int64
	SortOrder   int
	Actor       Actor
}

func (s *ProjectTaskService) Create(ctx context.Context, command CreateProjectTaskCommand) (*database.ProjectTask, error) {
	title := strings.TrimSpace(command.Title)
	if title == "" {
		return nil, validationError("title_required", "Title is required")
	}
	status := command.Status
	if status == "" {
		status = TaskStatusTodo
	}
	if !validTaskStatus(status) {
		return nil, validationError("invalid_status", "Invalid status. Must be: todo, in_progress, awaiting_approval, done")
	}
	priority := command.Priority
	if priority == "" {
		priority = "medium"
	}
	if !validTaskPriority(priority) {
		return nil, validationError("invalid_priority", "Invalid priority. Must be: low, medium, high, urgent")
	}
	if command.SortOrder < 0 {
		return nil, validationError("invalid_sort_order", "Invalid sort_order")
	}
	dueDate, err := parseOptionalTaskTime(command.DueDate)
	if err != nil {
		return nil, validationError("invalid_due_date", "Invalid due_date format")
	}

	var created *database.ProjectTask
	mutation := mutationEffects{}
	err = s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		exists, err := tx.ProjectExists(ctx, command.ProjectID)
		if err != nil {
			return err
		}
		if !exists {
			return notFoundError("project_not_found", "Project not found", sql.ErrNoRows)
		}
		parentID, err := validateParent(ctx, tx, command.ProjectID, 0, command.ParentID)
		if err != nil {
			return err
		}
		resolvedStatus, autoApproval, err := s.resolveTaskStatus(ctx, tx, command.ProjectID, status)
		if err != nil {
			return err
		}
		task := &database.ProjectTask{
			ProjectID: command.ProjectID, Title: title, Description: command.Description,
			Status: resolvedStatus, Priority: priority, DueDate: dueDate, ParentID: parentID,
			SortOrder: command.SortOrder,
		}
		if err := tx.CreateTask(ctx, task); err != nil {
			return err
		}
		createdDetails := map[string]any{
			"title": task.Title, "status": task.Status, "priority": task.Priority,
		}
		if autoApproval != nil {
			createdDetails["requested_status"] = TaskStatusAwaitingApproval
		}
		history, err := createHistory(ctx, tx, task.ID, task.ProjectID, "task_created", createdDetails, command.Actor, "")
		if err != nil {
			return err
		}
		created = task
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "created", ProjectID: task.ProjectID, Task: task})
		mutation.history = append(mutation.history, history)
		if autoApproval == nil && task.Status == TaskStatusAwaitingApproval {
			mutation.verify = task
		}
		if err := s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventCreated, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(task.ID, 10), Actor: command.Actor,
			Payload: map[string]any{
				"project_id": task.ProjectID, "task_id": task.ID, "status": task.Status,
			},
		}); err != nil {
			return err
		}
		return s.recordTaskAutoApproval(ctx, tx, &mutation, task, "created", command.Actor, autoApproval)
	})
	if err != nil {
		return nil, err
	}
	s.publish(mutation)
	return created, nil
}

type UpdateProjectTaskCommand struct {
	ProjectID   int64
	TaskID      int64
	Title       *string
	Description *string
	Status      *string
	Priority    *string
	DueDate     *string
	ParentID    *int64
	SortOrder   *int
	Actor       Actor
}

func (s *ProjectTaskService) Update(ctx context.Context, command UpdateProjectTaskCommand) (*database.ProjectTask, error) {
	var updated *database.ProjectTask
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		task, err := tx.GetTask(ctx, command.TaskID)
		if err != nil || task == nil || task.ProjectID != command.ProjectID {
			return notFoundError("task_not_found", "Task not found", err)
		}
		old := *task

		if command.Title != nil {
			title := strings.TrimSpace(*command.Title)
			if title == "" {
				return validationError("title_empty", "Title cannot be empty")
			}
			task.Title = title
		}
		if command.Description != nil {
			task.Description = *command.Description
		}
		if command.Priority != nil {
			if !validTaskPriority(*command.Priority) {
				return validationError("invalid_priority", "Invalid priority")
			}
			task.Priority = *command.Priority
		}
		if command.DueDate != nil {
			dueDate, err := parseOptionalTaskTime(*command.DueDate)
			if err != nil {
				return validationError("invalid_due_date", "Invalid due_date format")
			}
			task.DueDate = dueDate
			task.DueNotified = false
		}
		if command.ParentID != nil {
			parentID, err := validateParent(ctx, tx, command.ProjectID, command.TaskID, command.ParentID)
			if err != nil {
				return err
			}
			task.ParentID = parentID
		}
		if command.SortOrder != nil {
			if *command.SortOrder < 0 {
				return validationError("invalid_sort_order", "Invalid sort_order")
			}
			task.SortOrder = *command.SortOrder
		}
		newStatus := old.Status
		var autoApproval *taskAutoApprovalDecision
		if command.Status != nil {
			if !validTaskStatus(*command.Status) {
				return validationError("invalid_status", "Invalid status")
			}
			newStatus = *command.Status
			if old.Status != TaskStatusAwaitingApproval {
				newStatus, autoApproval, err = s.resolveTaskStatus(ctx, tx, command.ProjectID, newStatus)
				if err != nil {
					return err
				}
			}
			task.Status = newStatus
		}

		if err := tx.UpdateTaskFields(ctx, task); err != nil {
			return err
		}
		if newStatus != old.Status {
			if err := tx.ChangeTaskStatus(ctx, task.ID, newStatus); err != nil {
				return err
			}
		}
		fresh, err := tx.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		updated = fresh
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "updated", ProjectID: fresh.ProjectID, Task: fresh})

		statusDetails := map[string]any{"old": old.Status, "new": fresh.Status}
		if autoApproval != nil {
			statusDetails["requested_status"] = TaskStatusAwaitingApproval
			statusDetails["reason"] = "verification_auto_approved"
		}
		changes := []struct {
			changed bool
			event   string
			details map[string]any
		}{
			{fresh.Status != old.Status, "status_change", statusDetails},
			{fresh.Priority != old.Priority, "priority_change", map[string]any{"old": old.Priority, "new": fresh.Priority}},
			{fresh.Title != old.Title, "title_updated", map[string]any{"old": old.Title, "new": fresh.Title}},
			{fresh.Description != old.Description, "description_updated", map[string]any{}},
		}
		for _, change := range changes {
			if !change.changed {
				continue
			}
			history, err := createHistory(ctx, tx, fresh.ID, fresh.ProjectID, change.event, change.details, command.Actor, "")
			if err != nil {
				return err
			}
			mutation.history = append(mutation.history, history)
		}
		if autoApproval == nil && old.Status != TaskStatusAwaitingApproval && fresh.Status == TaskStatusAwaitingApproval {
			mutation.verify = fresh
		}
		if err := s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventUpdated, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(fresh.ID, 10), Actor: command.Actor,
			Payload: map[string]any{
				"project_id": fresh.ProjectID, "task_id": fresh.ID, "status": fresh.Status,
			},
		}); err != nil {
			return err
		}
		return s.recordTaskAutoApproval(ctx, tx, &mutation, fresh, old.Status, command.Actor, autoApproval)
	})
	if err != nil {
		return nil, err
	}
	s.publish(mutation)
	return updated, nil
}

type ChangeTaskStatusCommand struct {
	ProjectID int64
	TaskID    int64
	Status    string
	Actor     Actor
}

func (s *ProjectTaskService) ChangeStatus(ctx context.Context, command ChangeTaskStatusCommand) (*database.ProjectTask, error) {
	if !validTaskStatus(command.Status) {
		return nil, validationError("invalid_status", "Invalid status")
	}
	var updated *database.ProjectTask
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		task, err := tx.GetTask(ctx, command.TaskID)
		if err != nil || task == nil || task.ProjectID != command.ProjectID {
			return notFoundError("task_not_found", "Task not found", err)
		}
		oldStatus := task.Status
		resolvedStatus := command.Status
		var autoApproval *taskAutoApprovalDecision
		if oldStatus != TaskStatusAwaitingApproval {
			resolvedStatus, autoApproval, err = s.resolveTaskStatus(ctx, tx, command.ProjectID, command.Status)
			if err != nil {
				return err
			}
		}
		if err := tx.ChangeTaskStatus(ctx, task.ID, resolvedStatus); err != nil {
			return err
		}
		updated, err = tx.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "updated", ProjectID: task.ProjectID, Task: updated})
		if oldStatus != resolvedStatus {
			details := map[string]any{"old": oldStatus, "new": resolvedStatus}
			if autoApproval != nil {
				details["requested_status"] = TaskStatusAwaitingApproval
				details["reason"] = "verification_auto_approved"
			}
			history, err := createHistory(ctx, tx, task.ID, task.ProjectID, "status_change",
				details, command.Actor, "")
			if err != nil {
				return err
			}
			mutation.history = append(mutation.history, history)
		}
		if autoApproval == nil && oldStatus != TaskStatusAwaitingApproval && resolvedStatus == TaskStatusAwaitingApproval {
			mutation.verify = updated
		}
		if err := s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventStatusChanged, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(task.ID, 10), Actor: command.Actor,
			Payload: map[string]any{
				"project_id": task.ProjectID, "task_id": task.ID,
				"old_status": oldStatus, "new_status": resolvedStatus,
			},
		}); err != nil {
			return err
		}
		return s.recordTaskAutoApproval(ctx, tx, &mutation, updated, oldStatus, command.Actor, autoApproval)
	})
	if err != nil {
		return nil, err
	}
	s.publish(mutation)
	return updated, nil
}

func (s *ProjectTaskService) Delete(ctx context.Context, projectID, taskID int64) error {
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil || task == nil || task.ProjectID != projectID {
			return notFoundError("task_not_found", "Task not found", err)
		}
		if err := tx.DeleteTask(ctx, taskID); err != nil {
			return err
		}
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "deleted", ProjectID: projectID, TaskID: taskID})
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventDeleted, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(taskID, 10), Actor: UserActor(),
			Payload: map[string]any{"project_id": projectID, "task_id": taskID},
		})
	})
	if err != nil {
		return err
	}
	s.publish(mutation)
	return nil
}

func (s *ProjectTaskService) Duplicate(ctx context.Context, projectID, taskID int64, actor Actor) (*database.ProjectTask, error) {
	var clone *database.ProjectTask
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		original, err := tx.GetTask(ctx, taskID)
		if err != nil || original == nil || original.ProjectID != projectID {
			return notFoundError("task_not_found", "Task not found", err)
		}
		clone = &database.ProjectTask{
			ProjectID: original.ProjectID, ParentID: original.ParentID,
			Title: original.Title + " (Copy)", Description: original.Description,
			Status: TaskStatusTodo, Priority: original.Priority, DueDate: original.DueDate,
		}
		if err := tx.CreateTask(ctx, clone); err != nil {
			return err
		}
		createdTasks := []*database.ProjectTask{clone}
		duplicatedFrom := []int64{original.ID}
		tasks, err := tx.ListTasksByProject(ctx, projectID)
		if err != nil {
			return err
		}
		for _, child := range tasks {
			if !child.ParentID.Valid || child.ParentID.Int64 != original.ID {
				continue
			}
			childClone := &database.ProjectTask{
				ProjectID: child.ProjectID, ParentID: sql.NullInt64{Int64: clone.ID, Valid: true},
				Title: child.Title + " (Copy)", Description: child.Description,
				Status: TaskStatusTodo, Priority: child.Priority, DueDate: child.DueDate,
			}
			if err := tx.CreateTask(ctx, childClone); err != nil {
				return err
			}
			createdTasks = append(createdTasks, childClone)
			duplicatedFrom = append(duplicatedFrom, child.ID)
		}
		for index, createdTask := range createdTasks {
			history, err := createHistory(ctx, tx, createdTask.ID, createdTask.ProjectID, "task_created",
				map[string]any{"title": createdTask.Title, "duplicated_from": duplicatedFrom[index]}, actor, "")
			if err != nil {
				return err
			}
			mutation.tasks = append(mutation.tasks, TaskChange{Action: "created", ProjectID: projectID, Task: createdTask})
			mutation.history = append(mutation.history, history)
		}
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventDuplicated, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(clone.ID, 10), Actor: actor,
			Payload: map[string]any{
				"project_id": projectID, "task_id": clone.ID,
				"source_task_id": original.ID, "created_count": len(createdTasks),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	s.publish(mutation)
	return clone, nil
}

func (s *ProjectTaskService) ReorderProject(ctx context.Context, projectID int64, items []database.ReorderItem) error {
	if len(items) == 0 {
		return validationError("empty_reorder", "Empty reorder list")
	}
	if len(items) > 100 {
		return validationError("reorder_too_large", "Too many items (max 100)")
	}
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		tasks, err := tx.ListTasksByProject(ctx, projectID)
		if err != nil {
			return err
		}
		if err := validateProjectReorder(tasks, items); err != nil {
			return err
		}
		if err := tx.ReorderProject(ctx, projectID, items); err != nil {
			return err
		}
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "reordered", ProjectID: projectID})
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventReordered, AggregateType: "project",
			AggregateID: strconv.FormatInt(projectID, 10), Actor: UserActor(),
			Payload: map[string]any{"project_id": projectID, "item_count": len(items)},
		})
	})
	if err != nil {
		return err
	}
	s.publish(mutation)
	return nil
}

func (s *ProjectTaskService) ReorderGlobal(ctx context.Context, items []database.GlobalReorderItem) error {
	if len(items) == 0 {
		return validationError("empty_reorder", "Empty reorder list")
	}
	if len(items) > 200 {
		return validationError("reorder_too_large", "Too many items (max 200)")
	}
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		if err := validateGlobalReorder(ctx, tx, items); err != nil {
			return err
		}
		if err := tx.ReorderGlobal(ctx, items); err != nil {
			return err
		}
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "reordered"})
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTasksEventReorderedGlobal, AggregateType: "project_tasks",
			AggregateID: "global", Actor: UserActor(),
			Payload: map[string]any{"item_count": len(items)},
		})
	})
	if err != nil {
		return err
	}
	s.publish(mutation)
	return nil
}

func (s *ProjectTaskService) ApproveVerification(ctx context.Context, projectID, taskID int64, actor Actor) (*database.ProjectTask, error) {
	updated, _, err := s.verify(ctx, projectID, taskID, actor, true, verificationOptions{})
	return updated, err
}

func (s *ProjectTaskService) RejectVerification(ctx context.Context, projectID, taskID int64, actor Actor) (*database.ProjectTask, error) {
	updated, _, err := s.verify(ctx, projectID, taskID, actor, false, verificationOptions{})
	return updated, err
}

type BulkApproveVerificationCommand struct {
	TaskIDs    []int64
	AllPending bool
	ProjectID  *int64
	Actor      Actor
	BulkID     string
}

type BulkApprovalItemResult struct {
	TaskID    int64  `json:"task_id"`
	ProjectID int64  `json:"project_id,omitempty"`
	Outcome   string `json:"outcome"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

type BulkApproveVerificationResult struct {
	Requested   int                      `json:"requested"`
	Approved    int                      `json:"approved"`
	AlreadyDone int                      `json:"already_done"`
	Failed      int                      `json:"failed"`
	Results     []BulkApprovalItemResult `json:"results"`
}

type verificationOptions struct {
	allowAlreadyDone bool
	bulk             bool
	bulkID           string
}

func (s *ProjectTaskService) ApproveVerificationBulk(ctx context.Context, command BulkApproveVerificationCommand) (*BulkApproveVerificationResult, error) {
	if command.AllPending && len(command.TaskIDs) > 0 {
		return nil, validationError("bulk_selection_ambiguous", "Use either task_ids or all_pending, not both")
	}
	if !command.AllPending && len(command.TaskIDs) == 0 {
		return nil, validationError("bulk_selection_required", "Provide task_ids or set all_pending")
	}
	if len(command.TaskIDs) > 500 {
		return nil, validationError("bulk_too_large", "Too many task_ids (max 500)")
	}
	if command.ProjectID != nil && *command.ProjectID <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}

	taskIDs := append([]int64(nil), command.TaskIDs...)
	if command.AllPending {
		tasks, err := s.store.ListAllTasks(ctx, database.TaskFilter{
			Status: TaskStatusAwaitingApproval, ProjectID: command.ProjectID,
		})
		if err != nil {
			return nil, err
		}
		taskIDs = make([]int64, 0, len(tasks))
		for _, task := range tasks {
			taskIDs = append(taskIDs, task.ID)
		}
	}

	result := &BulkApproveVerificationResult{
		Requested: len(taskIDs), Results: make([]BulkApprovalItemResult, 0, len(taskIDs)),
	}
	options := verificationOptions{allowAlreadyDone: true, bulk: true, bulkID: strings.TrimSpace(command.BulkID)}
	for _, taskID := range taskIDs {
		item := BulkApprovalItemResult{TaskID: taskID}
		if taskID <= 0 {
			item.Outcome, item.ErrorCode, item.Error = "failed", "invalid_task_id", "Task ID must be positive"
			result.Failed++
			result.Results = append(result.Results, item)
			continue
		}

		task, err := s.store.GetTask(ctx, taskID)
		if err != nil {
			item.Outcome = "failed"
			if errors.Is(err, sql.ErrNoRows) {
				item.ErrorCode, item.Error = "task_not_found", "Task not found"
			} else {
				item.ErrorCode, item.Error = "task_lookup_failed", err.Error()
			}
			result.Failed++
			result.Results = append(result.Results, item)
			continue
		}
		if task == nil {
			item.Outcome, item.ErrorCode, item.Error = "failed", "task_not_found", "Task not found"
			result.Failed++
			result.Results = append(result.Results, item)
			continue
		}
		item.ProjectID = task.ProjectID
		if command.ProjectID != nil && task.ProjectID != *command.ProjectID {
			item.Outcome, item.ErrorCode, item.Error = "failed", "project_scope_mismatch", "Task is outside the requested project scope"
			result.Failed++
			result.Results = append(result.Results, item)
			continue
		}

		updated, changed, err := s.verify(ctx, task.ProjectID, task.ID, command.Actor, true, options)
		if err != nil {
			item.Outcome = "failed"
			item.ErrorCode, item.Error = bulkApprovalError(err)
			result.Failed++
		} else if changed {
			item.Outcome = "approved"
			item.ProjectID = updated.ProjectID
			result.Approved++
		} else {
			item.Outcome = "already_done"
			item.ProjectID = updated.ProjectID
			result.AlreadyDone++
		}
		result.Results = append(result.Results, item)
	}
	return result, nil
}

func bulkApprovalError(err error) (string, string) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code, appErr.Message
	}
	return "approval_failed", err.Error()
}

func (s *ProjectTaskService) verify(ctx context.Context, projectID, taskID int64, actor Actor, approve bool, options verificationOptions) (*database.ProjectTask, bool, error) {
	var updated *database.ProjectTask
	changed := false
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil || task == nil || task.ProjectID != projectID {
			return notFoundError("task_not_found", "Task not found", err)
		}
		if approve && options.allowAlreadyDone && task.Status == TaskStatusDone {
			updated = task
			return nil
		}
		if task.Status != TaskStatusAwaitingApproval {
			return validationError("not_awaiting_approval", "Task is not awaiting approval")
		}
		status := TaskStatusInProgress
		documentStatus := "rejected"
		event := "verification_rejected"
		if approve {
			status = TaskStatusDone
			documentStatus = "approved"
			event = "verification_approved"
		}
		if task.VerificationDocID != "" {
			if err := tx.UpdateDocumentStatus(ctx, task.VerificationDocID, documentStatus); err != nil {
				return err
			}
		}
		if !approve {
			task.VerificationDocID = ""
			if err := tx.UpdateTaskFields(ctx, task); err != nil {
				return err
			}
		}
		if err := tx.ChangeTaskStatus(ctx, task.ID, status); err != nil {
			return err
		}
		updated, err = tx.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		details := map[string]any{"old": TaskStatusAwaitingApproval, "new": status}
		if approve && options.bulk {
			details["bulk"] = true
			details["approval_mode"] = "bulk"
			if options.bulkID != "" {
				details["bulk_id"] = options.bulkID
			}
		}
		history, err := createHistory(ctx, tx, task.ID, task.ProjectID, event, details, actor, "")
		if err != nil {
			return err
		}
		changed = true
		mutation.tasks = append(mutation.tasks, TaskChange{Action: "updated", ProjectID: projectID, Task: updated})
		mutation.history = append(mutation.history, history)
		eventType := ProjectTaskEventVerificationRejected
		if approve {
			eventType = ProjectTaskEventVerificationApproved
		}
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: eventType, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(task.ID, 10), Actor: actor,
			Payload: map[string]any{
				"project_id": projectID, "task_id": task.ID, "status": status,
				"bulk": approve && options.bulk,
			},
		})
	})
	if err != nil {
		return nil, false, err
	}
	s.publish(mutation)
	return updated, changed, nil
}

type LinkSessionTaskCommand struct {
	SessionID   string
	TaskID      *int64
	Title       string
	Description string
	Priority    string
	Actor       Actor
}

type LinkSessionTaskResult struct {
	Task        *database.ProjectTask
	SessionName string
}

func (s *ProjectTaskService) LinkSession(ctx context.Context, command LinkSessionTaskCommand) (*LinkSessionTaskResult, error) {
	if command.TaskID == nil && strings.TrimSpace(command.Title) == "" {
		return nil, validationError("task_target_required", "Either task_id or task_data with title is required")
	}
	if command.TaskID != nil && *command.TaskID <= 0 {
		return nil, validationError("task_target_required", "Either task_id or task_data with title is required")
	}
	priority := "medium"
	if command.TaskID == nil {
		priority = command.Priority
		if priority == "" {
			priority = "medium"
		}
		if !validTaskPriority(priority) {
			return nil, validationError("invalid_priority", "Invalid priority")
		}
	}

	var result *LinkSessionTaskResult
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		session, err := tx.GetSession(ctx, command.SessionID)
		if err != nil {
			return notFoundError("session_not_found", "Session not found", err)
		}
		existing, err := tx.GetTaskForSession(ctx, command.SessionID)
		if err != nil {
			return err
		}

		var linked *database.ProjectTask
		created := false
		if command.TaskID != nil {
			linked, err = tx.GetTask(ctx, *command.TaskID)
			if err != nil {
				return notFoundError("task_not_found", "Task not found", err)
			}
			if linked.ProjectID != session.ProjectID {
				return validationError("task_project_mismatch", "Task belongs to a different project")
			}
		} else {
			linked = &database.ProjectTask{
				ProjectID: session.ProjectID, Title: strings.TrimSpace(command.Title),
				Description: command.Description, Status: TaskStatusInProgress, Priority: priority,
			}
			if err := tx.CreateTask(ctx, linked); err != nil {
				return err
			}
			created = true
			history, err := createHistory(ctx, tx, linked.ID, linked.ProjectID, "task_created",
				map[string]any{"title": linked.Title, "status": linked.Status, "priority": linked.Priority}, command.Actor, command.SessionID)
			if err != nil {
				return err
			}
			mutation.history = append(mutation.history, history)
		}

		newName := "Task: " + linked.Title
		if err := tx.LinkSession(ctx, command.SessionID, linked.ID, newName); err != nil {
			return err
		}
		if existing != nil {
			history, err := createHistory(ctx, tx, existing.ID, existing.ProjectID, "session_unlinked",
				map[string]any{"session_id": command.SessionID, "session_name": session.Name}, command.Actor, command.SessionID)
			if err != nil {
				return err
			}
			mutation.history = append(mutation.history, history)
		}
		for _, entry := range []struct {
			event string
			actor Actor
		}{
			{"session_linked", command.Actor},
			{"task_assigned", Actor{Type: "system"}},
		} {
			history, err := createHistory(ctx, tx, linked.ID, linked.ProjectID, entry.event,
				map[string]any{"session_id": command.SessionID, "session_name": newName}, entry.actor, command.SessionID)
			if err != nil {
				return err
			}
			mutation.history = append(mutation.history, history)
		}
		if linked.Status == TaskStatusTodo {
			if err := tx.ChangeTaskStatus(ctx, linked.ID, TaskStatusInProgress); err != nil {
				return err
			}
			history, err := createHistory(ctx, tx, linked.ID, linked.ProjectID, "status_change",
				map[string]any{"old": TaskStatusTodo, "new": TaskStatusInProgress, "reason": "auto_on_link"},
				Actor{Type: "system"}, command.SessionID)
			if err != nil {
				return err
			}
			mutation.history = append(mutation.history, history)
			linked, err = tx.GetTask(ctx, linked.ID)
			if err != nil {
				return err
			}
			mutation.tasks = append(mutation.tasks, TaskChange{Action: "updated", ProjectID: linked.ProjectID, Task: linked})
		}
		if created {
			mutation.tasks = append([]TaskChange{{Action: "created", ProjectID: linked.ProjectID, Task: linked}}, mutation.tasks...)
		}
		mutation.sessions = append(mutation.sessions, SessionRename{SessionID: command.SessionID, Name: newName})
		result = &LinkSessionTaskResult{Task: linked, SessionName: newName}
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventSessionLinked, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(linked.ID, 10), Actor: command.Actor,
			CorrelationID: command.SessionID,
			Payload: map[string]any{
				"project_id": linked.ProjectID, "task_id": linked.ID,
				"session_id": command.SessionID, "task_created": created,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	s.publish(mutation)
	return result, nil
}

type UnlinkSessionTaskResult struct {
	TaskID      int64
	SessionName string
}

func (s *ProjectTaskService) UnlinkSession(ctx context.Context, sessionID string, actor Actor) (*UnlinkSessionTaskResult, error) {
	var result *UnlinkSessionTaskResult
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		session, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return notFoundError("session_not_found", "Session not found", err)
		}
		task, err := tx.GetTaskForSession(ctx, sessionID)
		if err != nil {
			return err
		}
		if task == nil {
			return conflictError("session_has_no_task", "Session has no linked task")
		}
		name := session.Name
		if strings.HasPrefix(name, "Task: ") {
			prefix := sessionID
			if len(prefix) > 8 {
				prefix = prefix[:8]
			}
			name = "Session " + prefix
		}
		if err := tx.UnlinkSession(ctx, sessionID, name); err != nil {
			return err
		}
		history, err := createHistory(ctx, tx, task.ID, task.ProjectID, "session_unlinked",
			map[string]any{"session_id": sessionID, "session_name": session.Name}, actor, sessionID)
		if err != nil {
			return err
		}
		mutation.history = append(mutation.history, history)
		mutation.sessions = append(mutation.sessions, SessionRename{SessionID: sessionID, Name: name})
		result = &UnlinkSessionTaskResult{TaskID: task.ID, SessionName: name}
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventSessionUnlinked, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(task.ID, 10), Actor: actor,
			CorrelationID: sessionID,
			Payload: map[string]any{
				"project_id": task.ProjectID, "task_id": task.ID, "session_id": sessionID,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	s.publish(mutation)
	return result, nil
}

func (s *ProjectTaskService) GetSessionTask(ctx context.Context, sessionID string) (*database.ProjectTask, error) {
	var task *database.ProjectTask
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		var err error
		task, err = tx.GetTaskForSession(ctx, sessionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, notFoundError("session_task_not_found", "No task linked to this session", sql.ErrNoRows)
	}
	return task, nil
}

func (s *ProjectTaskService) ListHistory(ctx context.Context, taskID int64, limit int) ([]database.TaskHistory, error) {
	history, err := s.store.ListTaskHistory(ctx, taskID, limit)
	if history == nil {
		history = []database.TaskHistory{}
	}
	return history, err
}

func (s *ProjectTaskService) AddComment(ctx context.Context, projectID, taskID int64, comment string, actor Actor) error {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return validationError("comment_required", "Comment is required")
	}
	mutation := mutationEffects{}
	err := s.store.WithProjectTaskTx(ctx, func(tx *database.ProjectTaskTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil || task.ProjectID != projectID {
			return notFoundError("task_not_found", "Task not found", err)
		}
		history, err := createHistory(ctx, tx, taskID, projectID, "comment_added",
			map[string]any{"comment": comment}, actor, "")
		if err != nil {
			return err
		}
		mutation.history = append(mutation.history, history)
		return s.publishProjectTaskEvent(ctx, tx, ProjectTaskEvent{
			EventType: ProjectTaskEventCommentAdded, AggregateType: "project_task",
			AggregateID: strconv.FormatInt(taskID, 10), Actor: actor,
			Payload: map[string]any{
				"project_id": projectID, "task_id": taskID, "history_id": history.ID,
			},
		})
	})
	if err != nil {
		return err
	}
	s.publish(mutation)
	return nil
}

type mutationEffects struct {
	tasks    []TaskChange
	history  []*database.TaskHistory
	sessions []SessionRename
	verify   *database.ProjectTask
}

func (s *ProjectTaskService) publishProjectTaskEvent(
	ctx context.Context,
	tx *database.ProjectTaskTx,
	event ProjectTaskEvent,
) error {
	metadata := eventMetadataFromContext(ctx)
	if strings.TrimSpace(metadata.Actor.Type) != "" || strings.TrimSpace(metadata.Actor.ID) != "" {
		event.Actor = metadata.Actor
	}
	if correlationID := strings.TrimSpace(metadata.CorrelationID); correlationID != "" {
		event.CorrelationID = correlationID
	}
	return s.events.PublishProjectTaskEvent(ctx, tx, event)
}

func (s *ProjectTaskService) publish(mutation mutationEffects) {
	if s.effects == nil {
		return
	}
	for _, task := range mutation.tasks {
		s.effects.PublishTaskChange(task)
	}
	for _, rename := range mutation.sessions {
		s.effects.PublishSessionRename(rename)
	}
	for _, history := range mutation.history {
		s.effects.PublishTaskHistory(history)
	}
	if mutation.verify != nil {
		s.effects.RequestTaskVerification(mutation.verify)
	}
}

func createHistory(
	ctx context.Context,
	tx *database.ProjectTaskTx,
	taskID, projectID int64,
	event string,
	details map[string]any,
	actor Actor,
	sessionID string,
) (*database.TaskHistory, error) {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	history := &database.TaskHistory{
		TaskID: taskID, ProjectID: projectID, EventType: event,
		Details: string(detailsJSON), Actor: actor.historyValue(),
	}
	if sessionID != "" {
		history.SessionID = sql.NullString{String: sessionID, Valid: true}
	}
	if err := tx.CreateHistory(ctx, history); err != nil {
		return nil, err
	}
	return history, nil
}

func validateParent(ctx context.Context, tx *database.ProjectTaskTx, projectID, taskID int64, raw *int64) (sql.NullInt64, error) {
	if raw == nil || *raw <= 0 {
		return sql.NullInt64{}, nil
	}
	parentID := *raw
	if taskID > 0 && parentID == taskID {
		return sql.NullInt64{}, validationError("invalid_parent", "Task cannot be its own parent")
	}
	seen := map[int64]bool{taskID: taskID > 0}
	currentID := parentID
	for currentID > 0 {
		if seen[currentID] {
			return sql.NullInt64{}, validationError("parent_cycle", "Task parent would create a cycle")
		}
		seen[currentID] = true
		current, err := tx.GetTask(ctx, currentID)
		if err != nil {
			return sql.NullInt64{}, validationError("invalid_parent", "Parent task not found")
		}
		if current.ProjectID != projectID {
			return sql.NullInt64{}, validationError("parent_project_mismatch", "Parent task belongs to a different project")
		}
		if !current.ParentID.Valid {
			break
		}
		currentID = current.ParentID.Int64
	}
	return sql.NullInt64{Int64: parentID, Valid: true}, nil
}

func validateProjectReorder(tasks []database.ProjectTask, items []database.ReorderItem) error {
	byID := make(map[int64]database.ProjectTask, len(tasks))
	parents := make(map[int64]int64, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
		if task.ParentID.Valid {
			parents[task.ID] = task.ParentID.Int64
		}
	}
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		if seen[item.ID] {
			return validationError("duplicate_task", "Duplicate task in reorder list")
		}
		seen[item.ID] = true
		task, ok := byID[item.ID]
		if !ok {
			return notFoundError("task_not_found", "Task not found", sql.ErrNoRows)
		}
		if item.SortOrder < 0 || (task.Status != TaskStatusDone && item.SortOrder == 0) {
			return validationError("invalid_sort_order", "Invalid sort_order")
		}
		if item.ParentID == nil || *item.ParentID <= 0 {
			delete(parents, item.ID)
			continue
		}
		if _, ok := byID[*item.ParentID]; !ok {
			return validationError("parent_project_mismatch", "Parent task belongs to a different project")
		}
		parents[item.ID] = *item.ParentID
	}
	return validateNoParentCycles(parents)
}

func validateGlobalReorder(ctx context.Context, tx *database.ProjectTaskTx, items []database.GlobalReorderItem) error {
	seen := make(map[int64]bool, len(items))
	projectIDs := make(map[int64]bool)
	for _, item := range items {
		if seen[item.ID] {
			return validationError("duplicate_task", "Duplicate task in reorder list")
		}
		seen[item.ID] = true
		if item.GlobalSortOrder < 0 {
			return validationError("invalid_sort_order", "Invalid global_sort_order")
		}
		task, err := tx.GetTask(ctx, item.ID)
		if err != nil {
			return notFoundError("task_not_found", "Task not found", err)
		}
		if task.Status != TaskStatusDone && item.GlobalSortOrder == 0 {
			return validationError("invalid_sort_order", "Invalid global_sort_order")
		}
		projectIDs[task.ProjectID] = true
		if item.ParentID != nil && *item.ParentID > 0 {
			parent, err := tx.GetTask(ctx, *item.ParentID)
			if err != nil || parent == nil || parent.ProjectID != task.ProjectID {
				return validationError("parent_project_mismatch", "Parent task belongs to a different project")
			}
		}
	}
	for projectID := range projectIDs {
		tasks, err := tx.ListTasksByProject(ctx, projectID)
		if err != nil {
			return err
		}
		parents := make(map[int64]int64, len(tasks))
		for _, task := range tasks {
			if task.ParentID.Valid {
				parents[task.ID] = task.ParentID.Int64
			}
		}
		for _, item := range items {
			task, err := tx.GetTask(ctx, item.ID)
			if err != nil || task == nil || task.ProjectID != projectID {
				continue
			}
			if item.ParentID == nil || *item.ParentID <= 0 {
				delete(parents, item.ID)
			} else {
				parents[item.ID] = *item.ParentID
			}
		}
		if err := validateNoParentCycles(parents); err != nil {
			return err
		}
	}
	return nil
}

func validateNoParentCycles(parents map[int64]int64) error {
	for taskID := range parents {
		seen := make(map[int64]bool)
		current := taskID
		for current > 0 {
			if seen[current] {
				return validationError("parent_cycle", "Task parent would create a cycle")
			}
			seen[current] = true
			current = parents[current]
		}
	}
	return nil
}

func validTaskStatus(status string) bool {
	switch status {
	case TaskStatusTodo, TaskStatusInProgress, TaskStatusAwaitingApproval, TaskStatusDone:
		return true
	default:
		return false
	}
}

func validTaskPriority(priority string) bool {
	switch priority {
	case "low", "medium", "high", "urgent":
		return true
	default:
		return false
	}
}

func parseOptionalTaskTime(value string) (sql.NullTime, error) {
	if value == "" {
		return sql.NullTime{}, nil
	}
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, format := range formats {
		if parsed, err := time.Parse(format, value); err == nil {
			return sql.NullTime{Time: parsed, Valid: true}, nil
		}
	}
	return sql.NullTime{}, fmt.Errorf("unrecognized time format: %s", value)
}
