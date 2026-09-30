package database

import (
	"context"
	"database/sql"
	"time"
)

// SessionRestartState is what a live session was doing when the server last
// saw it: inside a turn or not, and whether it was blocked on a question. It
// outlives the process so the next boot can tell an interrupted turn from an
// idle session.
type SessionRestartState struct {
	SessionID     string       `db:"session_id"`
	TurnOpen      bool         `db:"turn_open"`
	TurnSince     sql.NullTime `db:"turn_since"`
	TurnReason    string       `db:"turn_reason"`
	AwaitingInput bool         `db:"awaiting_input"`
	UpdatedAt     time.Time    `db:"updated_at"`
}

// SaveSessionTurnState records a turn change. The awaiting flag is left as
// it was: only the shutdown snapshot knows whether a question was open.
func (d *DB) SaveSessionTurnState(ctx context.Context, sessionID string, open bool, since time.Time, reason string) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO session_restart_state (session_id, turn_open, turn_since, turn_reason, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			turn_open = excluded.turn_open, turn_since = excluded.turn_since,
			turn_reason = excluded.turn_reason, awaiting_input = 0, updated_at = excluded.updated_at`,
		sessionID, open, since.UTC(), reason, time.Now().UTC())
	return err
}

// SaveSessionRestartState writes the full state, including whether a
// question was pending (the shutdown snapshot).
func (d *DB) SaveSessionRestartState(ctx context.Context, state SessionRestartState) error {
	var since any
	if state.TurnSince.Valid {
		since = state.TurnSince.Time.UTC()
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO session_restart_state (session_id, turn_open, turn_since, turn_reason, awaiting_input, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			turn_open = excluded.turn_open, turn_since = excluded.turn_since, turn_reason = excluded.turn_reason,
			awaiting_input = excluded.awaiting_input, updated_at = excluded.updated_at`,
		state.SessionID, state.TurnOpen, since, state.TurnReason, state.AwaitingInput, time.Now().UTC())
	return err
}

// ListSessionRestartStates returns every recorded state keyed by session.
func (d *DB) ListSessionRestartStates(ctx context.Context) (map[string]SessionRestartState, error) {
	var rows []SessionRestartState
	if err := d.SelectContext(ctx, &rows, `SELECT * FROM session_restart_state`); err != nil {
		return nil, err
	}
	states := make(map[string]SessionRestartState, len(rows))
	for _, row := range rows {
		states[row.SessionID] = row
	}
	return states, nil
}

// DeleteSessionRestartState forgets one session's state.
func (d *DB) DeleteSessionRestartState(ctx context.Context, sessionID string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM session_restart_state WHERE session_id = ?`, sessionID)
	return err
}
