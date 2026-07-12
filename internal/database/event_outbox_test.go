package database

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func eventOutboxTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "event-outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func appendTestEvent(t *testing.T, db *DB, id string, occurredAt time.Time) *EventOutboxEvent {
	t.Helper()
	tx, err := db.BeginTxx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	event, err := AppendEventOutbox(context.Background(), tx, EventOutboxAppend{
		EventID: id, EventType: "test.changed", AggregateType: "missing_aggregate",
		AggregateID: "does-not-need-a-foreign-key", Actor: "test", PayloadJSON: `{"ok":true}`,
		OccurredAt: occurredAt,
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return event
}

func createEventOutboxClient(t *testing.T, db *DB, id string) {
	t.Helper()
	err := db.CreateAutomationClient(context.Background(), &AutomationClient{
		ID: id, Name: "Client " + id, TokenPrefix: "prefix-" + id,
		TokenHash: []byte("hash-" + id), Scopes: `[]`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventOutboxAppendUsesCallerTransaction(t *testing.T) {
	db := eventOutboxTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendEventOutbox(ctx, tx, EventOutboxAppend{
		EventID: "rolled-back", EventType: "test.changed", AggregateType: "task",
		AggregateID: "1", Actor: "test", PayloadJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	events, err := db.ListEventOutboxAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("rolled-back event was visible: %+v", events)
	}

	first := appendTestEvent(t, db, "committed-1", time.Now().Add(-2*time.Hour))
	second := appendTestEvent(t, db, "committed-2", time.Now().Add(-time.Hour))
	if second.Sequence <= first.Sequence {
		t.Fatalf("sequence is not monotonic: first=%d second=%d", first.Sequence, second.Sequence)
	}
	events, err = db.ListEventOutboxAfter(ctx, first.Sequence, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != second.EventID {
		t.Fatalf("unexpected cursor page: %+v", events)
	}

	tx, err = db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AppendEventOutbox(ctx, tx, EventOutboxAppend{
		EventID: first.EventID, EventType: "test.duplicate", AggregateType: "task",
		AggregateID: "2", Actor: "test", PayloadJSON: `{}`,
	})
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("duplicate event_id was accepted")
	}
}

func TestEventOutboxAckIsMonotonicAndBoundedBySnapshot(t *testing.T) {
	db := eventOutboxTestDB(t)
	ctx := context.Background()
	createEventOutboxClient(t, db, "helena")
	first := appendTestEvent(t, db, "event-1", time.Now())
	second := appendTestEvent(t, db, "event-2", time.Now())

	snapshot, err := db.SnapshotEventOutboxCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot != second.Sequence {
		t.Fatalf("snapshot=%d, want %d", snapshot, second.Sequence)
	}
	cursor, err := db.AckEventOutbox(ctx, "helena", "notifications", second.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.CursorSequence != second.Sequence {
		t.Fatalf("cursor=%d, want %d", cursor.CursorSequence, second.Sequence)
	}
	cursor, err = db.AckEventOutbox(ctx, "helena", "notifications", first.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.CursorSequence != second.Sequence {
		t.Fatalf("cursor regressed to %d", cursor.CursorSequence)
	}
	_, err = db.AckEventOutbox(ctx, "helena", "notifications", snapshot+1)
	if !errors.Is(err, ErrEventOutboxCursorAhead) {
		t.Fatalf("future ack error=%v", err)
	}
}

func TestEventOutboxPruneRequiresEveryConsumerAck(t *testing.T) {
	db := eventOutboxTestDB(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	events := make([]*EventOutboxEvent, 3)
	for i := range events {
		events[i] = appendTestEvent(t, db, fmt.Sprintf("retention-%d", i+1), old.Add(time.Duration(i)*time.Minute))
	}

	deleted, err := db.PruneEventOutbox(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("pruned %d events without registered consumers", deleted)
	}

	createEventOutboxClient(t, db, "consumer-a")
	createEventOutboxClient(t, db, "consumer-b")
	if _, err := db.GetEventOutboxConsumerCursor(ctx, "consumer-b", "diary"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AckEventOutbox(ctx, "consumer-a", "diary", events[2].Sequence); err != nil {
		t.Fatal(err)
	}
	deleted, err = db.PruneEventOutbox(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("pruned %d events while a registered consumer was at zero", deleted)
	}
	if _, err := db.AckEventOutbox(ctx, "consumer-b", "diary", events[0].Sequence); err != nil {
		t.Fatal(err)
	}
	deleted, err = db.PruneEventOutbox(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("pruned=%d, want 1 at the minimum consumer cursor", deleted)
	}
	remaining, err := db.ListEventOutboxAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].Sequence != events[1].Sequence {
		t.Fatalf("unexpected retained events: %+v", remaining)
	}
	if _, err := db.AckEventOutbox(ctx, "consumer-b", "diary", events[2].Sequence); err != nil {
		t.Fatal(err)
	}
	deleted, err = db.PruneEventOutbox(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("final prune=%d, want 2", deleted)
	}
	snapshot, err := db.SnapshotEventOutboxCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot != events[2].Sequence {
		t.Fatalf("snapshot regressed after full prune: got %d want %d", snapshot, events[2].Sequence)
	}
	if _, err := db.AckEventOutbox(ctx, "consumer-a", "diary", snapshot); err != nil {
		t.Fatalf("idempotent ack failed after full prune: %v", err)
	}
}
