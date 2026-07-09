package database

import (
	"context"
	"database/sql"
	"errors"
)

const MaxAutomationSnapshotNotifications = 500

type AutomationSnapshot struct {
	Cursor        int64
	Projects      []Project
	Tasks         []ProjectTask
	TaskSummary   map[string]int
	Sessions      []Session
	Notifications []Notification
}

// ReadEventOutboxPage returns the page and outbox high-water mark from one
// read transaction, so cursor validation and pagination share a snapshot.
func (d *DB) ReadEventOutboxPage(ctx context.Context, afterSequence int64, limit int) ([]EventOutboxEvent, int64, error) {
	if afterSequence < 0 {
		return nil, 0, errors.New("event outbox cursor cannot be negative")
	}
	if limit <= 0 {
		limit = DefaultEventOutboxPageSize
	}
	if limit > 1000 {
		return nil, 0, errors.New("event outbox limit cannot exceed 1000")
	}
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	var snapshot int64
	if err := tx.GetContext(ctx, &snapshot, eventOutboxSnapshotQuery); err != nil {
		return nil, 0, err
	}
	if afterSequence > snapshot {
		return nil, snapshot, ErrEventOutboxCursorAhead
	}
	var events []EventOutboxEvent
	if err := tx.SelectContext(ctx, &events, `
		SELECT * FROM event_outbox
		WHERE sequence > ?
		ORDER BY sequence ASC
		LIMIT ?`, afterSequence, limit); err != nil {
		return nil, snapshot, err
	}
	if events == nil {
		events = []EventOutboxEvent{}
	}
	if err := tx.Commit(); err != nil {
		return nil, snapshot, err
	}
	return events, snapshot, nil
}

// ReadAutomationSnapshot materializes the initial reconciliation state and
// its outbox cursor from one SQLite read transaction.
func (d *DB) ReadAutomationSnapshot(ctx context.Context, notificationLimit int) (*AutomationSnapshot, error) {
	if notificationLimit <= 0 {
		notificationLimit = 100
	}
	if notificationLimit > MaxAutomationSnapshotNotifications {
		return nil, errors.New("automation snapshot notification limit cannot exceed 500")
	}
	tx, err := d.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	snapshot := &AutomationSnapshot{TaskSummary: make(map[string]int)}
	if err := tx.GetContext(ctx, &snapshot.Cursor, eventOutboxSnapshotQuery); err != nil {
		return nil, err
	}
	if err := tx.SelectContext(ctx, &snapshot.Projects, `
		SELECT p.* FROM projects p
		LEFT JOIN (
			SELECT project_id, MAX(COALESCE(last_activity_at, start_time)) AS latest_activity
			FROM sessions
			GROUP BY project_id
		) s ON s.project_id = p.id
		ORDER BY s.latest_activity IS NULL, s.latest_activity DESC, p.name`); err != nil {
		return nil, err
	}
	for index := range snapshot.Projects {
		project := &snapshot.Projects[index]
		project.HasCredential = project.SSHCredentialEncrypted.Valid && project.SSHCredentialEncrypted.String != ""
	}
	if err := tx.SelectContext(ctx, &snapshot.Tasks, `
		SELECT * FROM project_tasks
		ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END,
			CASE WHEN status = 'done' THEN NULL ELSE global_sort_order END,
			CASE WHEN status = 'done' THEN updated_at END DESC, created_at`); err != nil {
		return nil, err
	}
	ApplyUmbrellaStatus(snapshot.Tasks)
	type statusCount struct {
		Status string `db:"status"`
		Count  int    `db:"count"`
	}
	var counts []statusCount
	if err := tx.SelectContext(ctx, &counts, `
		SELECT status, COUNT(*) AS count FROM project_tasks
		WHERE id NOT IN (SELECT DISTINCT parent_id FROM project_tasks WHERE parent_id IS NOT NULL)
		GROUP BY status`); err != nil {
		return nil, err
	}
	for _, count := range counts {
		snapshot.TaskSummary[count.Status] = count.Count
	}
	if err := tx.SelectContext(ctx, &snapshot.Sessions, "SELECT * FROM sessions ORDER BY start_time DESC"); err != nil {
		return nil, err
	}
	if err := tx.SelectContext(ctx, &snapshot.Notifications,
		"SELECT * FROM notifications ORDER BY created_at DESC LIMIT ?", notificationLimit); err != nil {
		return nil, err
	}
	if snapshot.Projects == nil {
		snapshot.Projects = []Project{}
	}
	if snapshot.Tasks == nil {
		snapshot.Tasks = []ProjectTask{}
	}
	if snapshot.Sessions == nil {
		snapshot.Sessions = []Session{}
	}
	if snapshot.Notifications == nil {
		snapshot.Notifications = []Notification{}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}
