package database

import (
	"context"
	"errors"
	"testing"
)

func claimAutomationEventTestCommand(t *testing.T, db *DB, id, key string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.GetAutomationClientByTokenPrefix(ctx, "event-prefix"); err != nil {
		if err := db.CreateAutomationClient(ctx, &AutomationClient{
			ID: "event-client", Name: "Event Client", TokenPrefix: "event-prefix",
			TokenHash: []byte("event-client-token-hash-32-bytes"), Scopes: `[]`, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	claimed, created, err := db.ClaimAutomationCommand(ctx, &AutomationCommand{
		ID: id, ClientID: "event-client", IdempotencyKey: key,
		RequestFingerprint: "fingerprint-" + key, Operation: "POST /commands",
	})
	if err != nil || !created || claimed.Status != "processing" {
		t.Fatalf("claim failed: created=%v command=%+v err=%v", created, claimed, err)
	}
}

func automationSuccessTestEvent(id string) *EventOutboxAppend {
	return &EventOutboxAppend{
		EventID: "event-" + id, EventType: "automation.command_succeeded",
		AggregateType: "task", AggregateID: "9", Actor: "automation_client:helena",
		CorrelationID: "inbound:42", PayloadJSON: `{"command_id":"cmd-9","capability":"tasks.update","target":{"type":"task","id":"9"},"status":"succeeded"}`,
	}
}

func TestCompleteAutomationCommandCommitsLedgerAndEventAtomically(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	claimAutomationEventTestCommand(t, db, "ledger-event-1", "ledger-event-key-1")
	if err := db.CompleteAutomationCommandWithEvent(ctx, "ledger-event-1", "succeeded", 200, "application/json", []byte(`{"ok":true}`), "", automationSuccessTestEvent("ledger-event-1")); err != nil {
		t.Fatal(err)
	}
	command, err := db.GetAutomationCommand(ctx, "ledger-event-1")
	if err != nil || command.Status != "succeeded" {
		t.Fatalf("ledger not completed: command=%+v err=%v", command, err)
	}
	events, err := db.ListEventOutboxAfter(ctx, 0, 10)
	if err != nil || len(events) != 1 || events[0].EventType != "automation.command_succeeded" {
		t.Fatalf("event not committed exactly once: events=%+v err=%v", events, err)
	}
	if err := db.CompleteAutomationCommandWithEvent(ctx, "ledger-event-1", "succeeded", 200, "application/json", nil, "", automationSuccessTestEvent("duplicate")); !errors.Is(err, ErrAutomationCommandNotProcessing) {
		t.Fatalf("recompletion error=%v", err)
	}
	events, _ = db.ListEventOutboxAfter(ctx, 0, 10)
	if len(events) != 1 {
		t.Fatalf("recompletion appended duplicate event: %+v", events)
	}
}

func TestCompleteAutomationCommandRollsBackBothSidesOnLedgerOrOutboxFailure(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	if err := db.CompleteAutomationCommandWithEvent(ctx, "missing-ledger", "succeeded", 200, "application/json", nil, "", automationSuccessTestEvent("missing")); !errors.Is(err, ErrAutomationCommandNotProcessing) {
		t.Fatalf("missing ledger error=%v", err)
	}
	events, _ := db.ListEventOutboxAfter(ctx, 0, 10)
	if len(events) != 0 {
		t.Fatalf("event committed without ledger: %+v", events)
	}

	claimAutomationEventTestCommand(t, db, "ledger-event-rollback", "ledger-event-rollback-key")
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER reject_automation_success_event BEFORE INSERT ON event_outbox
		WHEN NEW.event_type = 'automation.command_succeeded'
		BEGIN SELECT RAISE(ABORT, 'outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err := db.CompleteAutomationCommandWithEvent(ctx, "ledger-event-rollback", "succeeded", 200, "application/json", nil, "", automationSuccessTestEvent("rollback"))
	if err == nil {
		t.Fatal("outbox failure completed ledger")
	}
	command, getErr := db.GetAutomationCommand(ctx, "ledger-event-rollback")
	if getErr != nil || command.Status != "processing" {
		t.Fatalf("ledger update was not rolled back: command=%+v err=%v", command, getErr)
	}
	events, _ = db.ListEventOutboxAfter(ctx, 0, 10)
	if len(events) != 0 {
		t.Fatalf("outbox failure left event: %+v", events)
	}
}

func TestCompleteAutomationCommandRejectsEventForFailedLedger(t *testing.T) {
	db := setupTestDB(t)
	claimAutomationEventTestCommand(t, db, "ledger-event-failed", "ledger-event-failed-key")
	err := db.CompleteAutomationCommandWithEvent(context.Background(), "ledger-event-failed", "failed", 500, "application/json", nil, "failed", automationSuccessTestEvent("failed"))
	if err == nil {
		t.Fatal("failed ledger accepted a success event")
	}
}
