package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// ProjectTaskTx exposes the persistence operations needed by the project-task
// application service. It deliberately does not perform external effects.
type ProjectTaskTx struct {
	tx *sqlx.Tx
}

func (d *DB) WithProjectTaskTx(ctx context.Context, fn func(*ProjectTaskTx) error) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&ProjectTaskTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *ProjectTaskTx) ProjectExists(ctx context.Context, projectID int64) (bool, error) {
	var exists bool
	err := t.tx.GetContext(ctx, &exists, "SELECT EXISTS(SELECT 1 FROM projects WHERE id = ?)", projectID)
	return exists, err
}

func (t *ProjectTaskTx) TaskAutoApprovalConfig(ctx context.Context, projectID int64, globalSettingKey string) (projectOverride, globalValue string, err error) {
	var config struct {
		ProjectOverride string `db:"project_override"`
		GlobalValue     string `db:"global_value"`
	}
	err = t.tx.GetContext(ctx, &config, `
		SELECT task_auto_approve_verification AS project_override,
			COALESCE((SELECT value FROM settings WHERE key = ?), '') AS global_value
		FROM projects WHERE id = ?`, globalSettingKey, projectID)
	return config.ProjectOverride, config.GlobalValue, err
}

func (t *ProjectTaskTx) AppendEventOutbox(ctx context.Context, input EventOutboxAppend) (*EventOutboxEvent, error) {
	return AppendEventOutbox(ctx, t.tx, input)
}

func (t *ProjectTaskTx) GetTask(ctx context.Context, taskID int64) (*ProjectTask, error) {
	var task ProjectTask
	if err := t.tx.GetContext(ctx, &task, "SELECT * FROM project_tasks WHERE id = ?", taskID); err != nil {
		return nil, err
	}
	return &task, nil
}

func (t *ProjectTaskTx) ListTasksByProject(ctx context.Context, projectID int64) ([]ProjectTask, error) {
	var tasks []ProjectTask
	err := t.tx.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE project_id = ? ORDER BY id", projectID)
	return tasks, err
}

func (t *ProjectTaskTx) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	var session Session
	if err := t.tx.GetContext(ctx, &session, "SELECT * FROM sessions WHERE id = ?", sessionID); err != nil {
		return nil, err
	}
	return &session, nil
}

func (t *ProjectTaskTx) GetTaskForSession(ctx context.Context, sessionID string) (*ProjectTask, error) {
	var task ProjectTask
	err := t.tx.GetContext(ctx, &task, `
		SELECT task.* FROM project_tasks task
		JOIN sessions session ON session.task_id = task.id
		WHERE session.id = ?`, sessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func (t *ProjectTaskTx) CreateTask(ctx context.Context, task *ProjectTask) error {
	if task.SortOrder == 0 && task.Status != "done" {
		var maxOrder sql.NullInt64
		var err error
		if task.ParentID.Valid {
			err = t.tx.GetContext(ctx, &maxOrder,
				"SELECT MAX(sort_order) FROM project_tasks WHERE project_id = ? AND parent_id = ? AND status != 'done'",
				task.ProjectID, task.ParentID.Int64)
		} else {
			err = t.tx.GetContext(ctx, &maxOrder,
				"SELECT MAX(sort_order) FROM project_tasks WHERE project_id = ? AND parent_id IS NULL AND status != 'done'",
				task.ProjectID)
		}
		if err != nil {
			return err
		}
		task.SortOrder = 1
		if maxOrder.Valid {
			task.SortOrder = int(maxOrder.Int64) + 1
		}
	}
	if task.GlobalSortOrder == 0 && task.Status != "done" {
		var maxGlobal sql.NullInt64
		if err := t.tx.GetContext(ctx, &maxGlobal,
			"SELECT MAX(global_sort_order) FROM project_tasks WHERE status != 'done'"); err != nil {
			return err
		}
		task.GlobalSortOrder = 1
		if maxGlobal.Valid {
			task.GlobalSortOrder = int(maxGlobal.Int64) + 1
		}
	}
	if task.Status == "done" {
		task.SortOrder = 0
		task.GlobalSortOrder = 0
	}
	result, err := t.tx.ExecContext(ctx, `
		INSERT INTO project_tasks
			(project_id, parent_id, title, description, status, priority, due_date,
			 sort_order, global_sort_order, due_notified, verification_doc_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ProjectID, task.ParentID, task.Title, task.Description, task.Status,
		task.Priority, task.DueDate, task.SortOrder, task.GlobalSortOrder,
		task.DueNotified, task.VerificationDocID)
	if err != nil {
		return err
	}
	task.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	fresh, err := t.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	*task = *fresh
	return nil
}

// UpdateTaskFields updates every mutable field except status. Status is always
// changed through ChangeTaskStatus so order invariants cannot be bypassed.
func (t *ProjectTaskTx) UpdateTaskFields(ctx context.Context, task *ProjectTask) error {
	if task.Status == "done" {
		task.SortOrder = 0
		task.GlobalSortOrder = 0
	} else {
		if task.SortOrder <= 0 {
			var maxOrder sql.NullInt64
			var err error
			if task.ParentID.Valid {
				err = t.tx.GetContext(ctx, &maxOrder, `
					SELECT MAX(sort_order) FROM project_tasks
					WHERE project_id = ? AND parent_id = ? AND status != 'done' AND id != ?`,
					task.ProjectID, task.ParentID.Int64, task.ID)
			} else {
				err = t.tx.GetContext(ctx, &maxOrder, `
					SELECT MAX(sort_order) FROM project_tasks
					WHERE project_id = ? AND parent_id IS NULL AND status != 'done' AND id != ?`,
					task.ProjectID, task.ID)
			}
			if err != nil {
				return err
			}
			task.SortOrder = 1
			if maxOrder.Valid {
				task.SortOrder = int(maxOrder.Int64) + 1
			}
		}
		if task.GlobalSortOrder <= 0 {
			var maxGlobal sql.NullInt64
			if err := t.tx.GetContext(ctx, &maxGlobal, `
				SELECT MAX(global_sort_order) FROM project_tasks
				WHERE status != 'done' AND id != ?`, task.ID); err != nil {
				return err
			}
			task.GlobalSortOrder = 1
			if maxGlobal.Valid {
				task.GlobalSortOrder = int(maxGlobal.Int64) + 1
			}
		}
	}
	_, err := t.tx.ExecContext(ctx, `
		UPDATE project_tasks
		SET title = ?, description = ?, priority = ?, due_date = ?, sort_order = ?, global_sort_order = ?,
			due_notified = ?, parent_id = ?, verification_doc_id = ?, updated_at = ?
		WHERE id = ?`,
		task.Title, task.Description, task.Priority, task.DueDate, task.SortOrder, task.GlobalSortOrder,
		task.DueNotified, task.ParentID, task.VerificationDocID, time.Now(), task.ID)
	return err
}

func (t *ProjectTaskTx) ChangeTaskStatus(ctx context.Context, taskID int64, status string) error {
	return changeTaskStatusInTx(ctx, t.tx, taskID, status)
}

func changeTaskStatusInTx(ctx context.Context, tx *sqlx.Tx, taskID int64, status string) error {
	var current struct {
		ProjectID int64         `db:"project_id"`
		ParentID  sql.NullInt64 `db:"parent_id"`
		OldStatus string        `db:"status"`
	}
	if err := tx.GetContext(ctx, &current,
		"SELECT project_id, parent_id, status FROM project_tasks WHERE id = ?", taskID); err != nil {
		return err
	}

	now := time.Now()
	var err error
	switch {
	case status == "done":
		_, err = tx.ExecContext(ctx, `
			UPDATE project_tasks
			SET status = ?, sort_order = 0, global_sort_order = 0, updated_at = ?
			WHERE id = ?`, status, now, taskID)
	case current.OldStatus == "done":
		var maxOrder sql.NullInt64
		if current.ParentID.Valid {
			err = tx.GetContext(ctx, &maxOrder, `
				SELECT MAX(sort_order) FROM project_tasks
				WHERE project_id = ? AND parent_id = ? AND status != 'done'`,
				current.ProjectID, current.ParentID.Int64)
		} else {
			err = tx.GetContext(ctx, &maxOrder, `
				SELECT MAX(sort_order) FROM project_tasks
				WHERE project_id = ? AND parent_id IS NULL AND status != 'done'`, current.ProjectID)
		}
		if err != nil {
			return err
		}
		newOrder := 1
		if maxOrder.Valid {
			newOrder = int(maxOrder.Int64) + 1
		}
		var maxGlobal sql.NullInt64
		if err := tx.GetContext(ctx, &maxGlobal,
			"SELECT MAX(global_sort_order) FROM project_tasks WHERE status != 'done'"); err != nil {
			return err
		}
		newGlobal := 1
		if maxGlobal.Valid {
			newGlobal = int(maxGlobal.Int64) + 1
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE project_tasks
			SET status = ?, sort_order = ?, global_sort_order = ?, updated_at = ?
			WHERE id = ?`, status, newOrder, newGlobal, now, taskID)
	default:
		_, err = tx.ExecContext(ctx,
			"UPDATE project_tasks SET status = ?, updated_at = ? WHERE id = ?", status, now, taskID)
	}
	if err != nil {
		return err
	}

	if status == "done" || current.OldStatus == "done" {
		if err := renumberProjectTaskGroups(ctx, tx, current.ProjectID); err != nil {
			return err
		}
	}
	return nil
}

func renumberProjectTaskGroups(ctx context.Context, tx *sqlx.Tx, projectID int64) error {
	var topLevel []struct {
		ID int64 `db:"id"`
	}
	if err := tx.SelectContext(ctx, &topLevel, `
		SELECT id FROM project_tasks
		WHERE project_id = ? AND parent_id IS NULL AND status != 'done'
		ORDER BY sort_order, created_at`, projectID); err != nil {
		return err
	}
	for index, task := range topLevel {
		if _, err := tx.ExecContext(ctx,
			"UPDATE project_tasks SET sort_order = ? WHERE id = ?", index+1, task.ID); err != nil {
			return err
		}
	}

	var parents []struct {
		ID int64 `db:"id"`
	}
	if err := tx.SelectContext(ctx, &parents, `
		SELECT DISTINCT parent_id AS id FROM project_tasks
		WHERE project_id = ? AND parent_id IS NOT NULL`, projectID); err != nil {
		return err
	}
	for _, parent := range parents {
		var children []struct {
			ID int64 `db:"id"`
		}
		if err := tx.SelectContext(ctx, &children, `
			SELECT id FROM project_tasks
			WHERE project_id = ? AND parent_id = ? AND status != 'done'
			ORDER BY sort_order, created_at`, projectID, parent.ID); err != nil {
			return err
		}
		for index, child := range children {
			if _, err := tx.ExecContext(ctx,
				"UPDATE project_tasks SET sort_order = ? WHERE id = ?", index+1, child.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *ProjectTaskTx) DeleteTask(ctx context.Context, taskID int64) error {
	_, err := t.tx.ExecContext(ctx, "DELETE FROM project_tasks WHERE id = ?", taskID)
	return err
}

func (t *ProjectTaskTx) CreateHistory(ctx context.Context, history *TaskHistory) error {
	if history.Actor == "" {
		history.Actor = "system"
	}
	if history.Details == "" {
		history.Details = "{}"
	}
	result, err := t.tx.ExecContext(ctx, `
		INSERT INTO task_history (task_id, project_id, event_type, details, actor, session_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		history.TaskID, history.ProjectID, history.EventType, history.Details,
		history.Actor, history.SessionID)
	if err != nil {
		return err
	}
	history.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	history.CreatedAt = time.Now()
	return nil
}

func (t *ProjectTaskTx) UpdateDocumentStatus(ctx context.Context, documentID, status string) error {
	_, err := t.tx.ExecContext(ctx,
		"UPDATE temp_documents SET status = ? WHERE id = ?", status, documentID)
	return err
}

func (t *ProjectTaskTx) LinkSession(ctx context.Context, sessionID string, taskID int64, name string) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE sessions SET task_id = ?, name = ?, last_activity_at = ? WHERE id = ?`,
		taskID, name, time.Now(), sessionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (t *ProjectTaskTx) UnlinkSession(ctx context.Context, sessionID, name string) error {
	result, err := t.tx.ExecContext(ctx,
		"UPDATE sessions SET task_id = NULL, name = ? WHERE id = ?", name, sessionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (t *ProjectTaskTx) ReorderProject(ctx context.Context, projectID int64, items []ReorderItem) error {
	now := time.Now()
	for _, item := range items {
		task, err := t.GetTask(ctx, item.ID)
		if err != nil {
			return err
		}
		sortOrder := item.SortOrder
		if task.Status == "done" {
			sortOrder = 0
		}
		parentID := nullableParentID(item.ParentID)
		if _, err := t.tx.ExecContext(ctx, `
			UPDATE project_tasks SET sort_order = ?, parent_id = ?, updated_at = ?
			WHERE id = ? AND project_id = ?`,
			sortOrder, parentID, now, item.ID, projectID); err != nil {
			return err
		}
	}

	ids := make([]string, len(items))
	args := make([]any, 0, len(items)+1)
	args = append(args, projectID)
	for index, item := range items {
		ids[index] = "?"
		args = append(args, item.ID)
	}
	var ordered []struct {
		ID              int64 `db:"id"`
		GlobalSortOrder int   `db:"global_sort_order"`
	}
	query := fmt.Sprintf(`
		SELECT id, global_sort_order FROM project_tasks
		WHERE project_id = ? AND id IN (%s) AND status != 'done'
		ORDER BY sort_order, created_at`, strings.Join(ids, ","))
	if err := t.tx.SelectContext(ctx, &ordered, query, args...); err != nil {
		return err
	}
	globalOrders := make([]int, len(ordered))
	for index, task := range ordered {
		globalOrders[index] = task.GlobalSortOrder
	}
	sort.Ints(globalOrders)
	for index, task := range ordered {
		if _, err := t.tx.ExecContext(ctx,
			"UPDATE project_tasks SET global_sort_order = ? WHERE id = ?", globalOrders[index], task.ID); err != nil {
			return err
		}
	}
	return nil
}

func (t *ProjectTaskTx) ReorderGlobal(ctx context.Context, items []GlobalReorderItem) error {
	now := time.Now()
	for _, item := range items {
		task, err := t.GetTask(ctx, item.ID)
		if err != nil {
			return err
		}
		globalSortOrder := item.GlobalSortOrder
		if task.Status == "done" {
			globalSortOrder = 0
		}
		if _, err := t.tx.ExecContext(ctx, `
			UPDATE project_tasks SET global_sort_order = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
			globalSortOrder, nullableParentID(item.ParentID), now, item.ID); err != nil {
			return err
		}
	}
	projectIDs := make(map[int64]struct{})
	for _, item := range items {
		task, err := t.GetTask(ctx, item.ID)
		if err != nil {
			return err
		}
		projectIDs[task.ProjectID] = struct{}{}
	}
	for projectID := range projectIDs {
		if err := renumberProjectTasksByGlobalOrder(ctx, t.tx, projectID); err != nil {
			return err
		}
	}
	return nil
}

func renumberProjectTasksByGlobalOrder(ctx context.Context, tx *sqlx.Tx, projectID int64) error {
	var topLevel []struct {
		ID int64 `db:"id"`
	}
	if err := tx.SelectContext(ctx, &topLevel, `
		SELECT id FROM project_tasks
		WHERE project_id = ? AND parent_id IS NULL AND status != 'done'
		ORDER BY global_sort_order, created_at`, projectID); err != nil {
		return err
	}
	for index, task := range topLevel {
		if _, err := tx.ExecContext(ctx,
			"UPDATE project_tasks SET sort_order = ? WHERE id = ?", index+1, task.ID); err != nil {
			return err
		}
	}

	var parents []struct {
		ID int64 `db:"id"`
	}
	if err := tx.SelectContext(ctx, &parents, `
		SELECT DISTINCT parent_id AS id FROM project_tasks
		WHERE project_id = ? AND parent_id IS NOT NULL`, projectID); err != nil {
		return err
	}
	for _, parent := range parents {
		var children []struct {
			ID int64 `db:"id"`
		}
		if err := tx.SelectContext(ctx, &children, `
			SELECT id FROM project_tasks
			WHERE project_id = ? AND parent_id = ? AND status != 'done'
			ORDER BY global_sort_order, created_at`, projectID, parent.ID); err != nil {
			return err
		}
		for index, child := range children {
			if _, err := tx.ExecContext(ctx,
				"UPDATE project_tasks SET sort_order = ? WHERE id = ?", index+1, child.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func nullableParentID(parentID *int64) sql.NullInt64 {
	if parentID == nil || *parentID <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *parentID, Valid: true}
}
