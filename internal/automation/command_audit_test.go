package automation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func auditTestClassifier(t *testing.T) (func(string) bool, []PlatformCapabilityDefinition) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	service := &platformCapabilityService{name: "audit_test"}
	for _, capability := range []application.Capability{
		{Name: "legacy.lookup", Scope: "tasks:write", Risk: application.CapabilityRiskWrite, Approval: application.ApprovalByPolicy, Handler: "legacy.lookup", Service: service},
		{Name: "legacy.delete_sounding_read", Scope: "tasks:read", Risk: application.CapabilityRiskRead, Approval: application.ApprovalNone, Handler: "legacy.delete_sounding_read", Service: service},
	} {
		if err := capabilities.Register(capability); err != nil {
			t.Fatal(err)
		}
	}
	platform, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	definitions := []PlatformCapabilityDefinition{
		{Name: "platform.lookup", Scopes: []application.CapabilityScope{"notifications:write"}, Risk: application.CapabilityRiskRead, Approval: application.ApprovalNone, Mutation: true, Handler: "platform.lookup", Service: "audit_test"},
		{Name: "platform.delete_sounding_read", Scopes: []application.CapabilityScope{"notifications:read"}, Risk: application.CapabilityRiskRead, Approval: application.ApprovalNone, Handler: "platform.delete_sounding_read", Service: "audit_test"},
	}
	for _, definition := range definitions {
		if err := platform.Register(definition, &fakePlatformExecutor{}); err != nil {
			t.Fatal(err)
		}
	}
	return registeredCapabilityMutationClassifier(capabilities, platform), definitions
}

func auditTestActor(t *testing.T, db *database.DB) Actor {
	t.Helper()
	client := provisionTestClient(t, db)
	return Actor{Type: "automation_client", ID: client.Client.ID, ClientID: client.Client.ID, Name: "Helena", Scopes: ScopeSet{}}
}

func auditTestRequest(t *testing.T, handler http.Handler, actor Actor, key, capability string, target map[string]any, dryRun bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"command_id": "cmd-" + key, "idempotency_key": key, "capability": capability,
		"target": target, "payload": map[string]any{"api_key": "input-payload-secret"},
		"reason": "reason-secret", "approval_token": "approval-token-secret",
		"correlation_id": "inbound:42", "dry_run": dryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(string(body)))
	req.Header.Set("Idempotency-Key", key)
	req = req.WithContext(WithActor(req.Context(), actor))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func automationSuccessEvents(t *testing.T, db *database.DB) []database.EventOutboxEvent {
	t.Helper()
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]database.EventOutboxEvent, 0)
	for _, event := range events {
		if event.EventType == automationCommandSucceededEventType {
			result = append(result, event)
		}
	}
	return result
}

func TestAutomationCommandAuditEmitsOnceForLegacyAndPlatformMutationAndNeverOnReplay(t *testing.T) {
	db := automationTestDB(t)
	actor := auditTestActor(t, db)
	classifier, _ := auditTestClassifier(t)
	var calls atomic.Int32
	business := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"succeeded","result":{"token":"result-secret"}}`))
	})
	handler := NewIdempotency(db, IdempotencyOptions{
		IsMutation: classifier,
		Now:        func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
	}).Middleware(business)

	legacy := auditTestRequest(t, handler, actor, "legacy-1", "legacy.lookup", map[string]any{"type": "task", "id": 9}, false)
	platform := auditTestRequest(t, handler, actor, "platform-1", "platform.lookup", map[string]any{"type": "notification", "id": 7}, false)
	if legacy.Code != http.StatusOK || platform.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("mutation responses/calls: legacy=%d platform=%d calls=%d", legacy.Code, platform.Code, calls.Load())
	}
	events := automationSuccessEvents(t, db)
	if len(events) != 2 {
		t.Fatalf("success events=%d want=2: %+v", len(events), events)
	}
	if events[0].AggregateType != "task" || events[0].AggregateID != "9" || events[1].AggregateType != "notification" || events[1].AggregateID != "7" {
		t.Fatalf("typed aggregates were not preserved: %+v", events)
	}
	for _, event := range events {
		if event.Actor != "automation_client:"+actor.ClientID || event.CorrelationID != "inbound:42" {
			t.Errorf("bearer/correlation mismatch: %+v", event)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 4 || payload["status"] != "succeeded" {
			t.Errorf("event payload is not minimal: %v", payload)
		}
		encoded := event.PayloadJSON + event.AggregateID + event.CorrelationID
		for _, forbidden := range []string{"input-payload-secret", "reason-secret", "approval-token-secret", "result-secret"} {
			if strings.Contains(encoded, forbidden) {
				t.Errorf("event leaked %q: %+v", forbidden, event)
			}
		}
	}

	replay := auditTestRequest(t, handler, actor, "legacy-1", "legacy.lookup", map[string]any{"type": "task", "id": 9}, false)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 2 {
		t.Fatalf("replay response=%d header=%q calls=%d", replay.Code, replay.Header().Get("Idempotency-Replayed"), calls.Load())
	}
	if len(automationSuccessEvents(t, db)) != 2 {
		t.Fatal("replay appended another success event")
	}
}

func TestAutomationCommandAuditUsesRegisteredMetadataNotCapabilityName(t *testing.T) {
	classifier, definitions := auditTestClassifier(t)
	if !classifier("legacy.lookup") || classifier("legacy.delete_sounding_read") || !classifier("platform.lookup") || classifier("platform.delete_sounding_read") {
		t.Fatal("mutation classifier inferred semantics from capability spelling instead of registry metadata")
	}
	if len(definitions) != 2 || !definitions[0].Mutation || definitions[1].Mutation {
		t.Fatalf("platform mutation metadata mismatch: %+v", definitions)
	}
}

func TestAutomationCommandAuditSkipsReadDryRunErrorAndOverflow(t *testing.T) {
	db := automationTestDB(t)
	actor := auditTestActor(t, db)
	classifier, _ := auditTestClassifier(t)
	status := atomic.Int32{}
	status.Store(http.StatusOK)
	business := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		code := int(status.Load())
		w.WriteHeader(code)
		if code < 400 {
			_, _ = w.Write([]byte(`{"status":"succeeded"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"external-secret"}`))
		}
	})
	handler := NewIdempotency(db, IdempotencyOptions{IsMutation: classifier}).Middleware(business)
	if got := auditTestRequest(t, handler, actor, "read-1", "platform.delete_sounding_read", map[string]any{"type": "task", "id": 1}, false); got.Code != http.StatusOK {
		t.Fatalf("read status=%d", got.Code)
	}
	if got := auditTestRequest(t, handler, actor, "dry-1", "platform.lookup", map[string]any{"type": "task", "id": 1}, true); got.Code != http.StatusOK {
		t.Fatalf("dry-run status=%d", got.Code)
	}
	status.Store(http.StatusInternalServerError)
	if got := auditTestRequest(t, handler, actor, "error-1", "platform.lookup", map[string]any{"type": "task", "id": 1}, false); got.Code != http.StatusInternalServerError {
		t.Fatalf("error status=%d", got.Code)
	}
	status.Store(http.StatusOK)
	overflow := NewIdempotency(db, IdempotencyOptions{IsMutation: classifier, MaxCachedResponse: 8}).Middleware(business)
	if got := auditTestRequest(t, overflow, actor, "overflow-1", "platform.lookup", map[string]any{"type": "task", "id": 1}, false); got.Code != http.StatusInternalServerError {
		t.Fatalf("overflow status=%d body=%s", got.Code, got.Body.String())
	}
	if events := automationSuccessEvents(t, db); len(events) != 0 {
		t.Fatalf("read/dry-run/error/overflow published success: %+v", events)
	}
}

func TestAutomationCommandAuditRollsBackLedgerWhenOutboxFails(t *testing.T) {
	db := automationTestDB(t)
	actor := auditTestActor(t, db)
	classifier, _ := auditTestClassifier(t)
	if _, err := db.ExecContext(context.Background(), `
		CREATE TRIGGER reject_command_audit BEFORE INSERT ON event_outbox
		WHEN NEW.event_type = 'automation.command_succeeded'
		BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	handler := NewIdempotency(db, IdempotencyOptions{IsMutation: classifier}).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"succeeded"}`))
	}))
	response := auditTestRequest(t, handler, actor, "rollback-1", "platform.lookup", map[string]any{"type": "task", "id": 1}, false)
	if response.Code != http.StatusServiceUnavailable || len(automationSuccessEvents(t, db)) != 0 {
		t.Fatalf("outbox failure response=%d events=%+v", response.Code, automationSuccessEvents(t, db))
	}
	var command database.AutomationCommand
	if err := db.GetContext(context.Background(), &command, `SELECT * FROM automation_commands WHERE client_id=? AND idempotency_key=?`, actor.ClientID, "rollback-1"); err != nil {
		t.Fatal(err)
	}
	if command.Status != "processing" {
		t.Fatalf("ledger completion was not rolled back: %+v", command)
	}
}

func TestAutomationCommandAuditConcurrentSameKeyHasOneWinnerAndOneEvent(t *testing.T) {
	db := automationTestDB(t)
	actor := auditTestActor(t, db)
	classifier, _ := auditTestClassifier(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	handler := NewIdempotency(db, IdempotencyOptions{IsMutation: classifier}).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"succeeded"}`))
	}))
	var first *httptest.ResponseRecorder
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		first = auditTestRequest(t, handler, actor, "concurrent-1", "platform.lookup", map[string]any{"type": "task", "id": 1}, false)
	}()
	<-started
	second := auditTestRequest(t, handler, actor, "concurrent-1", "platform.lookup", map[string]any{"type": "task", "id": 1}, false)
	close(release)
	wait.Wait()
	if first.Code != http.StatusOK || second.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("concurrent responses first=%d second=%d calls=%d", first.Code, second.Code, calls.Load())
	}
	if events := automationSuccessEvents(t, db); len(events) != 1 {
		t.Fatalf("concurrent command events=%+v", events)
	}
}

func TestPlatformMutationMetadataTotalsAreExplicit(t *testing.T) {
	definitions := make([]PlatformCapabilityDefinition, 0)
	for _, group := range [][]PlatformCapabilityDefinition{
		configurationPlatformDefinitionsForTest(), executionPlatformDefinitionsForTest(), collaborationDefinitionsForTest(),
	} {
		definitions = append(definitions, group...)
	}
	mutations, reads := 0, 0
	for _, definition := range definitions {
		if definition.Mutation {
			mutations++
		} else {
			reads++
		}
	}
	if mutations != 104 || reads != 49 || len(definitions) != 153 {
		t.Fatalf("platform metadata totals mutations=%d reads=%d total=%d, want 104/49/153", mutations, reads, len(definitions))
	}
}

func TestAutomationAuditInvalidTypedTargetFallsBackWithoutSecret(t *testing.T) {
	body := `{"command_id":"cmd-safe","capability":"platform.lookup","target":{"type":"task","id":"api_key=target-secret"},"payload":{}}`
	req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(body))
	event := requestAutomationCommandSuccessEvent(req, Actor{Type: "automation_client", ClientID: "helena"}, "ledger-1", time.Now(), func(string) bool { return true })
	if event == nil || event.AggregateType != "automation_command" || event.AggregateID != "cmd-safe" || strings.Contains(event.PayloadJSON, "target-secret") {
		t.Fatalf("unsafe target was not safely replaced: %+v", event)
	}
}

func TestCompleteEventRejectsFailedStatusThroughInterface(t *testing.T) {
	db := automationTestDB(t)
	var store IdempotencyStore = db
	err := store.CompleteAutomationCommandWithEvent(context.Background(), "missing", "failed", 500, "application/json", nil, "failed", &database.EventOutboxAppend{})
	if err == nil || !strings.Contains(err.Error(), "requires succeeded") {
		t.Fatalf("failed completion accepted success event: %v", err)
	}
}
