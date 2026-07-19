package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Phase 7.3 (Maestro): durable mission registry. See migrateV71.

type Mission struct {
	ID                   int64        `db:"id" json:"id"`
	Goal                 string       `db:"goal" json:"goal"`
	GroupTagID           int64        `db:"group_tag_id" json:"group_tag_id"`
	CoordinatorSessionID string       `db:"coordinator_session_id" json:"coordinator_session_id"`
	Status               string       `db:"status" json:"status"`
	CreatedAt            time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time    `db:"updated_at" json:"updated_at"`
	CompletedAt          sql.NullTime `db:"completed_at" json:"completed_at,omitempty"`
}

type MissionWorker struct {
	ID            int64     `db:"id" json:"id"`
	MissionID     int64     `db:"mission_id" json:"mission_id"`
	ProjectID     int64     `db:"project_id" json:"project_id"`
	Backend       string    `db:"backend" json:"backend"`
	SessionID     string    `db:"session_id" json:"session_id"`
	WorkspaceID   string    `db:"workspace_id" json:"workspace_id,omitempty"`
	Role          string    `db:"role" json:"role"`
	Status        string    `db:"status" json:"status"`
	LastReportRef string    `db:"last_report_ref" json:"last_report_ref,omitempty"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time `db:"updated_at" json:"updated_at"`
}

// ErrMissionActiveExists is returned when a group already has an active
// mission (enforced by the partial unique index).
var ErrMissionActiveExists = errors.New("the group already has an active mission")

// CreateMission inserts an active mission for a group. The partial unique
// index turns a second active mission into ErrMissionActiveExists.
func (d *DB) CreateMission(ctx context.Context, goal string, groupTagID int64, coordinatorSessionID string) (*Mission, error) {
	res, err := d.ExecContext(ctx, `
		INSERT INTO missions (goal, group_tag_id, coordinator_session_id, status)
		VALUES (?, ?, ?, 'active')`, goal, groupTagID, coordinatorSessionID)
	if err != nil {
		if strings.Contains(err.Error(), "idx_missions_one_active_per_group") ||
			strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrMissionActiveExists
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.GetMission(ctx, id)
}

func (d *DB) GetMission(ctx context.Context, id int64) (*Mission, error) {
	var mission Mission
	err := d.GetContext(ctx, &mission, "SELECT * FROM missions WHERE id = ?", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mission, nil
}

// UpdateMissionStatus transitions a mission; completed/failed/archived stamp
// completed_at once.
func (d *DB) UpdateMissionStatus(ctx context.Context, id int64, status string) error {
	_, err := d.ExecContext(ctx, `
		UPDATE missions SET status = ?, updated_at = CURRENT_TIMESTAMP,
			completed_at = CASE WHEN ? IN ('completed','failed','archived')
				THEN COALESCE(completed_at, CURRENT_TIMESTAMP) ELSE completed_at END
		WHERE id = ?`, status, status, id)
	return err
}

// UpsertMissionWorker registers (or refreshes) a worker row for a mission.
func (d *DB) UpsertMissionWorker(ctx context.Context, worker *MissionWorker) error {
	if worker == nil {
		return errors.New("mission worker is required")
	}
	if worker.Role == "" {
		worker.Role = "worker"
	}
	if worker.Status == "" {
		worker.Status = "spawned"
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO mission_workers (mission_id, project_id, backend, session_id, workspace_id, role, status, last_report_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(mission_id, session_id) DO UPDATE SET
			project_id = excluded.project_id,
			backend = excluded.backend,
			workspace_id = excluded.workspace_id,
			role = excluded.role,
			status = excluded.status,
			last_report_ref = CASE WHEN excluded.last_report_ref != ''
				THEN excluded.last_report_ref ELSE mission_workers.last_report_ref END,
			updated_at = CURRENT_TIMESTAMP`,
		worker.MissionID, worker.ProjectID, worker.Backend, worker.SessionID,
		worker.WorkspaceID, worker.Role, worker.Status, worker.LastReportRef)
	return err
}

func (d *DB) ListMissionWorkers(ctx context.Context, missionID int64) ([]MissionWorker, error) {
	var workers []MissionWorker
	err := d.SelectContext(ctx, &workers,
		"SELECT * FROM mission_workers WHERE mission_id = ? ORDER BY id", missionID)
	return workers, err
}

// UpdateMissionWorkerReport refreshes the rolling last_report_ref of every
// mission row this session participates in (the coordinator's rolling summary).
func (d *DB) UpdateMissionWorkerReport(ctx context.Context, sessionID, reportRef string) error {
	if reportRef == "" {
		return nil
	}
	_, err := d.ExecContext(ctx, `
		UPDATE mission_workers SET last_report_ref = ?, updated_at = CURRENT_TIMESTAMP
		WHERE session_id = ?`, reportRef, sessionID)
	return err
}

// ListActiveMissions returns every active mission (boot-time mission resume).
func (d *DB) ListActiveMissions(ctx context.Context) ([]Mission, error) {
	var missions []Mission
	err := d.SelectContext(ctx, &missions,
		"SELECT * FROM missions WHERE status = 'active' ORDER BY id")
	return missions, err
}

// AppendMissionEvent writes a mission.* event to the durable outbox in its own
// transaction and wakes long-pollers — the mission timeline's raw feed.
func (d *DB) AppendMissionEvent(ctx context.Context, eventType string, missionID int64, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := AppendEventOutbox(ctx, tx, EventOutboxAppend{
		EventID: uuid.NewString(), EventType: eventType,
		AggregateType: "mission", AggregateID: strconv.FormatInt(missionID, 10),
		Actor: "coordinator", SchemaVersion: 1, PayloadJSON: string(encoded),
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.NotifyOutboxAppended()
	return nil
}

// CountActiveSessionsInProjects counts live (starting|running) sessions across
// a set of projects — the mission parallelism cap's input.
func (d *DB) CountActiveSessionsInProjects(ctx context.Context, projectIDs []int64) (int, error) {
	if len(projectIDs) == 0 {
		return 0, nil
	}
	query, args, err := sqlx.In(
		"SELECT COUNT(*) FROM sessions WHERE status IN ('starting','running') AND project_id IN (?)", projectIDs)
	if err != nil {
		return 0, err
	}
	var count int
	err = d.Reader().GetContext(ctx, &count, d.Rebind(query), args...)
	return count, err
}

// SumTokensForSessions totals input+output tokens recorded for a set of
// sessions (the mission token budget's input). Missing rows count as zero.
func (d *DB) SumTokensForSessions(ctx context.Context, sessionIDs []string) (int64, error) {
	if len(sessionIDs) == 0 {
		return 0, nil
	}
	query, args, err := sqlx.In(
		"SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM token_usage WHERE session_id IN (?)", sessionIDs)
	if err != nil {
		return 0, err
	}
	var total int64
	err = d.Reader().GetContext(ctx, &total, d.Rebind(query), args...)
	return total, err
}
