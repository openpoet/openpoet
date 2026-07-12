package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
)

func provisionEventClient(t *testing.T, db *database.DB, name string, seed byte, scopes ...Scope) *ProvisionedClient {
	t.Helper()
	provisioned, err := ProvisionClient(
		context.Background(), db, name, scopes,
		bytes.NewReader(bytes.Repeat([]byte{seed}, 16+9+32)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return provisioned
}

func appendAutomationEvent(t *testing.T, db *database.DB, id, actor string, occurredAt time.Time) *database.EventOutboxEvent {
	t.Helper()
	tx, err := db.BeginTxx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	event, err := database.AppendEventOutbox(context.Background(), tx, database.EventOutboxAppend{
		EventID: id, EventType: "project_task.updated", AggregateType: "project_task",
		AggregateID: strings.TrimPrefix(id, "event-"), Actor: actor,
		SchemaVersion: 1, PayloadJSON: fmt.Sprintf(`{"summary":%q,"index":%s}`, "Event "+id, strings.TrimPrefix(id, "event-")),
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

func TestEventPageMatchesConnectorAndAckIsLedgerIndependent(t *testing.T) {
	db := automationTestDB(t)
	client := provisionEventClient(t, db, "helena-events", 41, ScopeEventsRead)
	handler := automationContractHandler(t, db)
	now := time.Date(2026, 7, 9, 16, 0, 0, 0, time.UTC)
	first := appendAutomationEvent(t, db, "event-1", "automation_client:"+client.Client.ID, now)
	second := appendAutomationEvent(t, db, "event-2", "system", now.Add(time.Second))
	third := appendAutomationEvent(t, db, "event-3", "worker:reporter", now.Add(2*time.Second))

	pageResponse := automationRequest(t, handler, client.Token, http.MethodGet, "/events?after=0&limit=2&consumer=signal-inbox", "", nil)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", pageResponse.Code, pageResponse.Body.String())
	}
	var page eventPage
	if err := json.Unmarshal(pageResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || !page.HasMore || page.NextCursor == nil || *page.NextCursor != strconv.FormatInt(second.Sequence, 10) {
		t.Fatalf("unexpected first page: %+v", page)
	}
	if page.Events[0].Sequence != first.Sequence || page.Events[0].Aggregate != (eventEntityRef{Type: "project_task", ID: "1"}) {
		t.Fatalf("unexpected mapped event: %+v", page.Events[0])
	}
	if page.Events[0].Actor.Type != "automation_client" || page.Events[0].Actor.ID != client.Client.ID {
		t.Fatalf("unexpected actor mapping: %+v", page.Events[0].Actor)
	}
	payload, ok := page.Events[0].Payload.(map[string]any)
	if !ok || payload["summary"] != "Event event-1" {
		t.Fatalf("payload was not decoded: %#v", page.Events[0].Payload)
	}
	if strings.Contains(pageResponse.Body.String(), "payload_json") || strings.Contains(pageResponse.Body.String(), `"payload":"{`) {
		t.Fatalf("flat persistence payload leaked: %s", pageResponse.Body.String())
	}

	nextResponse := automationRequest(t, handler, client.Token, http.MethodGet,
		"/events?after="+*page.NextCursor+"&limit=2&consumer=signal-inbox", "", nil)
	var next eventPage
	if err := json.Unmarshal(nextResponse.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 1 || next.Events[0].Sequence != third.Sequence || next.HasMore || next.NextCursor == nil {
		t.Fatalf("unexpected second page: %+v", next)
	}

	ackBody := map[string]any{"through": *next.NextCursor, "consumer": "signal-inbox"}
	ack := automationRequest(t, handler, client.Token, http.MethodPost, "/events/ack", "", ackBody)
	if ack.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", ack.Code, ack.Body.String())
	}
	var acknowledged eventAckResponse
	if err := json.Unmarshal(ack.Body.Bytes(), &acknowledged); err != nil {
		t.Fatal(err)
	}
	if acknowledged.Acknowledged != strconv.FormatInt(third.Sequence, 10) {
		t.Fatalf("acknowledged=%q", acknowledged.Acknowledged)
	}
	replay := automationRequest(t, handler, client.Token, http.MethodPost, "/events/ack", "", ackBody)
	if replay.Code != http.StatusOK || replay.Body.String() != ack.Body.String() {
		t.Fatalf("monotonic ack replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	older := automationRequest(t, handler, client.Token, http.MethodPost, "/events/ack", "", map[string]any{
		"through": strconv.FormatInt(first.Sequence, 10), "consumer": "signal-inbox",
	})
	if older.Code != http.StatusOK {
		t.Fatalf("older ack status=%d body=%s", older.Code, older.Body.String())
	}
	if err := json.Unmarshal(older.Body.Bytes(), &acknowledged); err != nil || acknowledged.Acknowledged != strconv.FormatInt(third.Sequence, 10) {
		t.Fatalf("older ack regressed cursor: body=%s err=%v", older.Body.String(), err)
	}
	var ledgerCount int
	if err := db.GetContext(context.Background(), &ledgerCount, "SELECT COUNT(*) FROM automation_commands"); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 0 {
		t.Fatalf("event ACK consumed %d command-ledger rows", ledgerCount)
	}
	resumed := automationRequest(t, handler, client.Token, http.MethodGet, "/events?limit=10&consumer=signal-inbox", "", nil)
	if resumed.Code != http.StatusOK {
		t.Fatalf("resumed page status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	var empty eventPage
	if err := json.Unmarshal(resumed.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Events) != 0 || empty.NextCursor != nil || empty.HasMore {
		t.Fatalf("server consumer cursor was not used: %+v", empty)
	}
}

func TestEventConsumerIsolationAndPruneFloor(t *testing.T) {
	db := automationTestDB(t)
	clientA := provisionEventClient(t, db, "client-a", 51, ScopeEventsRead)
	clientB := provisionEventClient(t, db, "client-b", 52, ScopeEventsRead)
	handler := automationContractHandler(t, db)
	old := time.Now().Add(-72 * time.Hour)
	first := appendAutomationEvent(t, db, "event-1", "system", old)
	appendAutomationEvent(t, db, "event-2", "system", old.Add(time.Minute))
	third := appendAutomationEvent(t, db, "event-3", "system", old.Add(2*time.Minute))

	for _, registration := range []struct {
		client   *ProvisionedClient
		consumer string
	}{
		{clientA, "diary"}, {clientA, "notifications"}, {clientB, "diary"},
	} {
		response := automationRequest(t, handler, registration.client.Token, http.MethodGet,
			"/events?after=0&limit=1&consumer="+registration.consumer, "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("register %s status=%d body=%s", registration.consumer, response.Code, response.Body.String())
		}
	}
	ack := func(client *ProvisionedClient, consumer string, sequence int64) {
		t.Helper()
		response := automationRequest(t, handler, client.Token, http.MethodPost, "/events/ack", "", map[string]any{
			"through": strconv.FormatInt(sequence, 10), "consumer": consumer,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("ack %s status=%d body=%s", consumer, response.Code, response.Body.String())
		}
	}
	ack(clientA, "diary", third.Sequence)
	ack(clientB, "diary", first.Sequence)

	deleted, err := db.PruneEventOutbox(context.Background(), time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("pruned %d events while a registered consumer remained at zero", deleted)
	}
	ack(clientA, "notifications", first.Sequence)
	deleted, err = db.PruneEventOutbox(context.Background(), time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("pruned=%d, want one event at shared floor", deleted)
	}
	cursorA, err := db.GetEventOutboxConsumerCursor(context.Background(), clientA.Client.ID, "diary")
	if err != nil {
		t.Fatal(err)
	}
	cursorB, err := db.GetEventOutboxConsumerCursor(context.Background(), clientB.Client.ID, "diary")
	if err != nil {
		t.Fatal(err)
	}
	if cursorA.CursorSequence != third.Sequence || cursorB.CursorSequence != first.Sequence {
		t.Fatalf("client cursors leaked: a=%d b=%d", cursorA.CursorSequence, cursorB.CursorSequence)
	}
}

func TestEventTransportRejectsInvalidAndAheadCursors(t *testing.T) {
	db := automationTestDB(t)
	client := provisionEventClient(t, db, "event-errors", 61, ScopeEventsRead)
	handler := automationContractHandler(t, db)
	event := appendAutomationEvent(t, db, "event-1", "system", time.Now())

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{name: "invalid after", method: http.MethodGet, path: "/events?after=not-decimal", wantStatus: 400, wantCode: "event_cursor_invalid"},
		{name: "ahead after", method: http.MethodGet, path: "/events?after=" + strconv.FormatInt(event.Sequence+1, 10), wantStatus: 409, wantCode: "event_cursor_ahead"},
		{name: "invalid consumer", method: http.MethodGet, path: "/events?consumer=bad%20consumer", wantStatus: 400, wantCode: "event_consumer_invalid"},
		{name: "invalid ack", method: http.MethodPost, path: "/events/ack", body: map[string]any{"through": "x"}, wantStatus: 400, wantCode: "event_cursor_invalid"},
		{name: "ahead ack", method: http.MethodPost, path: "/events/ack", body: map[string]any{"through": strconv.FormatInt(event.Sequence+1, 10)}, wantStatus: 409, wantCode: "event_cursor_ahead"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := automationRequest(t, handler, client.Token, tt.method, tt.path, "", tt.body)
			if response.Code != tt.wantStatus || decodeAutomationErrorCode(t, response) != tt.wantCode {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	withoutScope := provisionEventClient(t, db, "without-event-scope", 62, ScopeTasksRead)
	denied := automationRequest(t, handler, withoutScope.Token, http.MethodGet, "/events", "", nil)
	if denied.Code != http.StatusForbidden || decodeAutomationErrorCode(t, denied) != "insufficient_scope" {
		t.Fatalf("scope status=%d body=%s", denied.Code, denied.Body.String())
	}
}
