package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Workspace persistence. Every state transition appends its typed outbox event
// in the SAME transaction as the row write, so the durable feed can never
// disagree with the table. Git commands always run OUTSIDE these transactions
// (the caller's job) — the single SQLite connection must never wait on git.

func workspaceEventPayload(ws *Workspace) string {
	payload, err := json.Marshal(map[string]interface{}{
		"workspace_id": ws.ID,
		"project_id":   ws.ProjectID,
		"name":         ws.Name,
		"kind":         ws.Kind,
		"branch":       ws.Branch,
		"path":         ws.Path,
		"status":       ws.Status,
	})
	if err != nil {
		return "{}"
	}
	return string(payload)
}

// CreateWorkspace inserts the provisioning row and its workspace.created event
// in one transaction. A UNIQUE violation on (project_id, name) among
// non-removed rows is returned as ErrWorkspaceNameConflict.
var ErrWorkspaceNameConflict = errors.New("workspace name already in use")

func (d *DB) CreateWorkspace(ctx context.Context, ws *Workspace, actor string) error {
	if ws.ID == "" {
		ws.ID = uuid.NewString()
	}
	if ws.Kind == "" {
		ws.Kind = "worktree"
	}
	if ws.Status == "" {
		ws.Status = "provisioning"
	}
	if ws.Version == 0 {
		ws.Version = 1
	}
	now := time.Now().UTC()
	ws.CreatedAt = now
	ws.UpdatedAt = now
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO workspaces
		(id, project_id, kind, name, branch, base_ref, path, task_id, status, keep_on_exit, version, created_by_actor, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ws.ID, ws.ProjectID, ws.Kind, ws.Name, ws.Branch, ws.BaseRef, ws.Path, ws.TaskID,
		ws.Status, ws.KeepOnExit, ws.Version, ws.CreatedByActor, ws.CreatedAt, ws.UpdatedAt)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrWorkspaceNameConflict
		}
		return err
	}
	if actor == "" {
		actor = "system"
	}
	_, err = AppendEventOutbox(ctx, tx, EventOutboxAppend{
		EventID:       uuid.NewString(),
		EventType:     "workspace.created",
		AggregateType: "workspace",
		AggregateID:   ws.ID,
		Actor:         actor,
		SchemaVersion: 1,
		PayloadJSON:   workspaceEventPayload(ws),
		OccurredAt:    now,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.NotifyOutboxAppended()
	return nil
}

func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// SetWorkspaceStatus transitions a workspace and appends the matching typed
// event (workspace.ready / workspace.failed / workspace.removed) in one tx.
func (d *DB) SetWorkspaceStatus(ctx context.Context, id, status, eventType, actor string) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE workspaces SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if eventType != "" {
		var ws Workspace
		if err := tx.Get(&ws, `SELECT * FROM workspaces WHERE id = ?`, id); err != nil {
			return err
		}
		if actor == "" {
			actor = "system"
		}
		_, err = AppendEventOutbox(ctx, tx, EventOutboxAppend{
			EventID:       uuid.NewString(),
			EventType:     eventType,
			AggregateType: "workspace",
			AggregateID:   id,
			Actor:         actor,
			SchemaVersion: 1,
			PayloadJSON:   workspaceEventPayload(&ws),
			OccurredAt:    time.Now().UTC(),
		})
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if eventType != "" {
		d.NotifyOutboxAppended()
	}
	return nil
}

// ReserveWorkspace atomically claims a ready workspace with a pending token
// BEFORE the session exists, closing the resolve→start→lease double-booking
// window: two concurrent creates race here on one CAS, not on a log line.
func (d *DB) ReserveWorkspace(ctx context.Context, workspaceID, token string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE workspaces SET leased_by_session_id = ?, status = 'leased', updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'ready'`, "pending:"+token, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("workspace %s is not ready to be leased", workspaceID)
	}
	return nil
}

// ReserveIdleWorkspace picks ONE idle ready lane of a project AND reserves it in
// a single statement, returning its id ("" when the project has no idle lane).
//
// Picking and reserving used to be two round-trips, so concurrent session creates
// routinely selected the same lane and all but one failed with a retryable error.
// Under orchestrator fan-out — N workers started at once on one project, which is
// exactly when isolation matters — that was the common case, not the rare one.
// The statement runs on the serialized write handle, so the SELECT and the UPDATE
// cannot interleave with another writer.
func (d *DB) ReserveIdleWorkspace(ctx context.Context, projectID int64, token string) (string, error) {
	var id string
	err := d.GetContext(ctx, &id, `
		UPDATE workspaces SET leased_by_session_id = ?, status = 'leased', updated_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM workspaces
			WHERE project_id = ? AND status = 'ready'
			  AND (leased_by_session_id IS NULL OR leased_by_session_id = '')
			ORDER BY updated_at ASC LIMIT 1
		)
		RETURNING id`, "pending:"+token, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// BindWorkspaceLease converts a pending reservation into the real session lease.
func (d *DB) BindWorkspaceLease(ctx context.Context, workspaceID, token, sessionID string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE workspaces SET leased_by_session_id = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND leased_by_session_id = ?`, sessionID, workspaceID, "pending:"+token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("workspace %s reservation is gone", workspaceID)
	}
	return nil
}

// ReleaseWorkspaceReservation frees a pending reservation whose session never
// materialized.
func (d *DB) ReleaseWorkspaceReservation(ctx context.Context, workspaceID, token string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE workspaces SET leased_by_session_id = NULL, status = 'ready', updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND leased_by_session_id = ?`, workspaceID, "pending:"+token)
	return err
}

// LeaseWorkspaceForSession takes (or re-takes) the lease for a session:
// a ready lane, or one this session already holds — the reopen/auto-restore
// path, where the row may still carry the lease from before a crash.
func (d *DB) LeaseWorkspaceForSession(ctx context.Context, workspaceID, sessionID string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE workspaces SET leased_by_session_id = ?, status = 'leased', updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND (status = 'ready' OR leased_by_session_id = ?)`,
		sessionID, workspaceID, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("workspace %s is leased by another session", workspaceID)
	}
	return nil
}

// ReleaseWorkspaceLeaseBySession frees any lease the ended session held.
func (d *DB) ReleaseWorkspaceLeaseBySession(ctx context.Context, sessionID string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE workspaces SET leased_by_session_id = NULL, status = 'ready', updated_at = CURRENT_TIMESTAMP
		 WHERE leased_by_session_id = ? AND status = 'leased'`, sessionID)
	return err
}

// ReleaseOrphanWorkspaceLeases is the boot-time sweep: frees leases held by
// sessions that are no longer alive (crash-time strands, failed restores) and
// pending reservations older than ten minutes. Returns rows freed.
func (d *DB) ReleaseOrphanWorkspaceLeases(ctx context.Context) (int64, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE workspaces SET leased_by_session_id = NULL, status = 'ready', updated_at = CURRENT_TIMESTAMP
		 WHERE status = 'leased' AND (
			(leased_by_session_id LIKE 'pending:%' AND updated_at < datetime('now', '-10 minutes'))
			OR (leased_by_session_id NOT LIKE 'pending:%' AND leased_by_session_id NOT IN
				(SELECT id FROM sessions WHERE status IN ('starting', 'running')))
		 )`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (d *DB) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	var ws Workspace
	err := d.GetContext(ctx, &ws, `SELECT * FROM workspaces WHERE id = ?`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &ws, nil
}

// GetActiveWorkspaceByName finds the workspace occupying this name. Removed
// AND failed rows do not occupy a name — a failed create must be retryable
// without a destructive-tier cleanup.
func (d *DB) GetActiveWorkspaceByName(ctx context.Context, projectID int64, name string) (*Workspace, error) {
	var ws Workspace
	err := d.GetContext(ctx, &ws,
		`SELECT * FROM workspaces WHERE project_id = ? AND name = ? AND status NOT IN ('removed', 'failed')`, projectID, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &ws, nil
}

func (d *DB) ListWorkspaces(ctx context.Context, projectID int64, status string, limit int) ([]Workspace, error) {
	if limit < 1 {
		limit = 100
	}
	query := `SELECT * FROM workspaces WHERE 1=1`
	args := []interface{}{}
	if projectID > 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	var rows []Workspace
	if err := d.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateSessionWorkspace persists the lane a session actually started in —
// the V36 skip_permissions precedent for restart-surviving session state.
func (d *DB) UpdateSessionWorkspace(ctx context.Context, sessionID, workspaceID, workDir string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE sessions SET workspace_id = NULLIF(?, ''), work_dir = ? WHERE id = ?`,
		workspaceID, workDir, sessionID)
	return err
}
