package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

// SessionFileActivity is one row of the claim ledger (V58): one row per
// (session, path, kind), never per event — the batched flusher increments
// touch_count instead of inserting, protecting the single connection.
type SessionFileActivity struct {
	ID           int64     `db:"id" json:"id"`
	SessionID    string    `db:"session_id" json:"session_id"`
	ProjectID    int64     `db:"project_id" json:"project_id"`
	Path         string    `db:"path" json:"path"`
	Kind         string    `db:"kind" json:"kind"`
	FirstTouchAt time.Time `db:"first_touch_at" json:"first_touch_at"`
	LastTouchAt  time.Time `db:"last_touch_at" json:"last_touch_at"`
	TouchCount   int64     `db:"touch_count" json:"touch_count"`
	LastTool     string    `db:"last_tool" json:"last_tool"`
	Source       string    `db:"source" json:"source"`
}

// CoordinatorIncident is a durable conflict object (V59), keyed by
// (rule, scope_key).
type CoordinatorIncident struct {
	ID              string    `db:"id" json:"id"`
	Rule            string    `db:"rule" json:"rule"`
	Severity        string    `db:"severity" json:"severity"`
	ProjectID       int64     `db:"project_id" json:"project_id"`
	ScopeKey        string    `db:"scope_key" json:"scope_key"`
	SessionsJSON    string    `db:"sessions_json" json:"sessions_json"`
	State           string    `db:"state" json:"state"`
	FirstDetectedAt time.Time `db:"first_detected_at" json:"first_detected_at"`
	LastEvidenceAt  time.Time `db:"last_evidence_at" json:"last_evidence_at"`
	EvidenceCount   int64     `db:"evidence_count" json:"evidence_count"`
	DetailsJSON     string    `db:"details_json" json:"details_json"`
}

// SessionFileActivityUpsert is the flusher's batched delta for one ledger row.
type SessionFileActivityUpsert struct {
	SessionID    string
	ProjectID    int64
	Path         string
	Kind         string
	FirstTouchAt time.Time
	LastTouchAt  time.Time
	TouchCount   int64
	LastTool     string
	Source       string
}

// UpsertSessionFileActivityTx applies one ledger delta inside a caller-owned
// transaction. UNIQUE(session_id, path, kind) turns repeats into counter
// bumps: touch_count accumulates, first_touch_at is preserved.
func UpsertSessionFileActivityTx(tx *sqlx.Tx, u SessionFileActivityUpsert) error {
	if u.Source == "" {
		u.Source = "hook"
	}
	if u.TouchCount < 1 {
		u.TouchCount = 1
	}
	_, err := tx.Exec(`INSERT INTO session_file_activity
		(session_id, project_id, path, kind, first_touch_at, last_touch_at, touch_count, last_tool, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, path, kind) DO UPDATE SET
			last_touch_at = excluded.last_touch_at,
			touch_count = touch_count + excluded.touch_count,
			last_tool = excluded.last_tool`,
		u.SessionID, u.ProjectID, u.Path, u.Kind,
		u.FirstTouchAt.UTC(), u.LastTouchAt.UTC(), u.TouchCount, u.LastTool, u.Source)
	return err
}

// UpsertCoordinatorIncidentTx persists one incident inside a caller-owned
// transaction. On re-evidence the original row id and first_detected_at are
// preserved; counters, severity and sessions are refreshed.
func UpsertCoordinatorIncidentTx(tx *sqlx.Tx, inc CoordinatorIncident) error {
	_, err := tx.Exec(`INSERT INTO coordinator_incidents
		(id, rule, severity, project_id, scope_key, sessions_json, state, first_detected_at, last_evidence_at, evidence_count, details_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule, scope_key) DO UPDATE SET
			severity = excluded.severity,
			sessions_json = excluded.sessions_json,
			last_evidence_at = excluded.last_evidence_at,
			evidence_count = excluded.evidence_count,
			details_json = excluded.details_json`,
		inc.ID, inc.Rule, inc.Severity, inc.ProjectID, inc.ScopeKey, inc.SessionsJSON,
		inc.State, inc.FirstDetectedAt.UTC(), inc.LastEvidenceAt.UTC(), inc.EvidenceCount, inc.DetailsJSON)
	return err
}

// ListCoordinatorIncidents returns incidents newest-evidence-first, optionally
// filtered by project and state.
func (d *DB) ListCoordinatorIncidents(ctx context.Context, projectID int64, state string, limit int) ([]CoordinatorIncident, error) {
	if limit < 1 {
		limit = 100
	}
	query := `SELECT * FROM coordinator_incidents WHERE 1=1`
	args := []interface{}{}
	if projectID > 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if state != "" {
		query += ` AND state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY last_evidence_at DESC LIMIT ?`
	args = append(args, limit)
	var incidents []CoordinatorIncident
	if err := d.SelectContext(ctx, &incidents, query, args...); err != nil {
		return nil, err
	}
	return incidents, nil
}

// GetCoordinatorIncident returns one incident by id, or (nil, nil) if absent.
func (d *DB) GetCoordinatorIncident(ctx context.Context, id string) (*CoordinatorIncident, error) {
	var inc CoordinatorIncident
	err := d.GetContext(ctx, &inc, `SELECT * FROM coordinator_incidents WHERE id = ?`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &inc, nil
}

// ListSessionFileActivity returns a session's claim ledger, most recent first.
func (d *DB) ListSessionFileActivity(ctx context.Context, sessionID string, limit int) ([]SessionFileActivity, error) {
	if limit < 1 {
		limit = 200
	}
	var rows []SessionFileActivity
	err := d.SelectContext(ctx, &rows,
		`SELECT * FROM session_file_activity WHERE session_id = ? ORDER BY last_touch_at DESC, id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
