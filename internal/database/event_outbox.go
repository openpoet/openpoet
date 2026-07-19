package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	DefaultEventOutboxPageSize = 100
	eventOutboxSnapshotQuery   = `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'event_outbox'), 0)`
)

var ErrEventOutboxCursorAhead = errors.New("event outbox cursor is ahead of the current snapshot")

type EventOutboxAppend struct {
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   string
	Actor         string
	CorrelationID string
	SchemaVersion int
	PayloadJSON   string
	OccurredAt    time.Time
}

type EventOutboxEvent struct {
	Sequence      int64     `db:"sequence" json:"sequence"`
	EventID       string    `db:"event_id" json:"event_id"`
	EventType     string    `db:"event_type" json:"event_type"`
	AggregateType string    `db:"aggregate_type" json:"aggregate_type"`
	AggregateID   string    `db:"aggregate_id" json:"aggregate_id"`
	Actor         string    `db:"actor" json:"actor"`
	CorrelationID string    `db:"correlation_id" json:"correlation_id"`
	SchemaVersion int       `db:"schema_version" json:"schema_version"`
	PayloadJSON   string    `db:"payload_json" json:"payload"`
	OccurredAt    time.Time `db:"occurred_at" json:"occurred_at"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}

type EventOutboxConsumerCursor struct {
	AutomationClientID string       `db:"automation_client_id" json:"automation_client_id"`
	Consumer           string       `db:"consumer" json:"consumer"`
	CursorSequence     int64        `db:"cursor_sequence" json:"cursor_sequence"`
	AckedAt            sql.NullTime `db:"acked_at" json:"acked_at,omitempty"`
	CreatedAt          time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time    `db:"updated_at" json:"updated_at"`
}

// AppendEventOutbox appends an event using the caller-owned transaction. It
// never opens a nested transaction or re-enters DB, which is required by the
// single-connection SQLite pool.
func AppendEventOutbox(ctx context.Context, tx *sqlx.Tx, input EventOutboxAppend) (*EventOutboxEvent, error) {
	if tx == nil {
		return nil, errors.New("event outbox transaction is required")
	}
	input.EventID = strings.TrimSpace(input.EventID)
	input.EventType = strings.TrimSpace(input.EventType)
	input.AggregateType = strings.TrimSpace(input.AggregateType)
	input.AggregateID = strings.TrimSpace(input.AggregateID)
	input.Actor = strings.TrimSpace(input.Actor)
	input.CorrelationID = strings.TrimSpace(input.CorrelationID)
	if input.EventID == "" || input.EventType == "" || input.AggregateType == "" || input.AggregateID == "" || input.Actor == "" {
		return nil, errors.New("event_id, event_type, aggregate_type, aggregate_id, and actor are required")
	}
	if input.SchemaVersion == 0 {
		input.SchemaVersion = 1
	}
	if input.SchemaVersion < 1 {
		return nil, errors.New("event outbox schema_version must be positive")
	}
	if strings.TrimSpace(input.PayloadJSON) == "" {
		input.PayloadJSON = "{}"
	}
	if !json.Valid([]byte(input.PayloadJSON)) {
		return nil, errors.New("event outbox payload_json must be valid JSON")
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = time.Now().UTC()
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO event_outbox
			(event_id, event_type, aggregate_type, aggregate_id, actor,
			 correlation_id, schema_version, payload_json, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.EventID, input.EventType, input.AggregateType, input.AggregateID,
		input.Actor, input.CorrelationID, input.SchemaVersion, input.PayloadJSON,
		input.OccurredAt.UTC())
	if err != nil {
		return nil, err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	var event EventOutboxEvent
	if err := tx.GetContext(ctx, &event, "SELECT * FROM event_outbox WHERE sequence = ?", sequence); err != nil {
		return nil, err
	}
	return &event, nil
}

func (d *DB) ListEventOutboxAfter(ctx context.Context, afterSequence int64, limit int) ([]EventOutboxEvent, error) {
	if afterSequence < 0 {
		return nil, errors.New("event outbox cursor cannot be negative")
	}
	if limit <= 0 {
		limit = DefaultEventOutboxPageSize
	}
	if limit > 1000 {
		return nil, errors.New("event outbox limit cannot exceed 1000")
	}
	var events []EventOutboxEvent
	if err := d.SelectContext(ctx, &events, `
		SELECT * FROM event_outbox
		WHERE sequence > ?
		ORDER BY sequence ASC
		LIMIT ?`, afterSequence, limit); err != nil {
		return nil, err
	}
	if events == nil {
		events = []EventOutboxEvent{}
	}
	return events, nil
}

func (d *DB) SnapshotEventOutboxCursor(ctx context.Context) (int64, error) {
	var cursor int64
	err := d.GetContext(ctx, &cursor, eventOutboxSnapshotQuery)
	return cursor, err
}

// GetEventOutboxConsumerCursor registers a previously unseen consumer at zero.
// Registration is what makes that consumer participate in the safe-prune floor.
func (d *DB) GetEventOutboxConsumerCursor(ctx context.Context, automationClientID, consumer string) (*EventOutboxConsumerCursor, error) {
	automationClientID = strings.TrimSpace(automationClientID)
	consumer = strings.TrimSpace(consumer)
	if automationClientID == "" || consumer == "" {
		return nil, errors.New("automation_client_id and consumer are required")
	}
	// Register at zero on first sight; on every subsequent read, refresh
	// updated_at so an actively-polling consumer that has nothing new to ack is
	// still counted as live and is not wrongly evicted as stale.
	if _, err := d.ExecContext(ctx, `
		INSERT INTO event_outbox_consumers
			(automation_client_id, consumer, cursor_sequence)
		VALUES (?, ?, 0)
		ON CONFLICT(automation_client_id, consumer) DO UPDATE SET
			updated_at = CURRENT_TIMESTAMP`, automationClientID, consumer); err != nil {
		return nil, err
	}
	var cursor EventOutboxConsumerCursor
	if err := d.GetContext(ctx, &cursor, `
		SELECT * FROM event_outbox_consumers
		WHERE automation_client_id = ? AND consumer = ?`, automationClientID, consumer); err != nil {
		return nil, err
	}
	return &cursor, nil
}

// AckEventOutbox advances a client/consumer cursor monotonically. A consumer
// cannot acknowledge beyond the snapshot visible in the same transaction.
func (d *DB) AckEventOutbox(ctx context.Context, automationClientID, consumer string, sequence int64) (*EventOutboxConsumerCursor, error) {
	automationClientID = strings.TrimSpace(automationClientID)
	consumer = strings.TrimSpace(consumer)
	if automationClientID == "" || consumer == "" {
		return nil, errors.New("automation_client_id and consumer are required")
	}
	if sequence < 0 {
		return nil, errors.New("event outbox cursor cannot be negative")
	}

	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var snapshot int64
	if err := tx.GetContext(ctx, &snapshot, eventOutboxSnapshotQuery); err != nil {
		return nil, err
	}
	if sequence > snapshot {
		return nil, fmt.Errorf("%w: cursor=%d snapshot=%d", ErrEventOutboxCursorAhead, sequence, snapshot)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_outbox_consumers
			(automation_client_id, consumer, cursor_sequence, acked_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(automation_client_id, consumer) DO UPDATE SET
			cursor_sequence = MAX(event_outbox_consumers.cursor_sequence, excluded.cursor_sequence),
			acked_at = CASE
				WHEN excluded.cursor_sequence > event_outbox_consumers.cursor_sequence THEN CURRENT_TIMESTAMP
				ELSE event_outbox_consumers.acked_at
			END,
			updated_at = CURRENT_TIMESTAMP`, automationClientID, consumer, sequence); err != nil {
		return nil, err
	}
	var cursor EventOutboxConsumerCursor
	if err := tx.GetContext(ctx, &cursor, `
		SELECT * FROM event_outbox_consumers
		WHERE automation_client_id = ? AND consumer = ?`, automationClientID, consumer); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &cursor, nil
}

// PruneEventOutbox removes old events only through the least advanced cursor.
// With no registered consumers (or a consumer at cursor zero), nothing is
// removed, so retention cannot discard unseen events.
func (d *DB) PruneEventOutbox(ctx context.Context, occurredBefore time.Time, limit int) (int64, error) {
	if occurredBefore.IsZero() {
		return 0, errors.New("event outbox retention cutoff is required")
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		return 0, errors.New("event outbox prune limit cannot exceed 10000")
	}
	result, err := d.ExecContext(ctx, `
		DELETE FROM event_outbox
		WHERE sequence IN (
			SELECT event.sequence
			FROM event_outbox event
			WHERE event.occurred_at < ?
			  AND event.sequence <= COALESCE(
				(SELECT MIN(cursor_sequence) FROM event_outbox_consumers), 0
			  )
			ORDER BY event.sequence ASC
			LIMIT ?
		)`, occurredBefore.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DefaultEventOutboxConsumerStaleAfter is how long a consumer cursor may go
// without an ack (updated_at) before eviction removes it from the prune floor.
const DefaultEventOutboxConsumerStaleAfter = 7 * 24 * time.Hour

// EvictStaleEventOutboxConsumers deletes consumer cursors whose updated_at is
// older than the cutoff. A dead consumer must not pin the prune floor forever;
// an evicted consumer that returns simply re-registers at cursor zero.
func (d *DB) EvictStaleEventOutboxConsumers(ctx context.Context, staleBefore time.Time) (int64, error) {
	if staleBefore.IsZero() {
		return 0, errors.New("event outbox consumer staleness cutoff is required")
	}
	result, err := d.ExecContext(ctx, `
		DELETE FROM event_outbox_consumers
		WHERE updated_at < ?`, staleBefore.UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
