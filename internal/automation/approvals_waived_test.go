package automation

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// waivedStopHandler serves sessions.stop over the real /commands pipeline
// against the running session s1.
func waivedStopHandler(t *testing.T, db *database.DB) (http.Handler, *closeCompletedStore, *closeCompletedManager) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	platform, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	store := &closeCompletedStore{
		session: &database.Session{ID: "s1", ProjectID: 20, Status: "running"},
		task:    &database.ProjectTask{ID: 870, ProjectID: 20, Status: "in_progress"},
	}
	manager := &closeCompletedManager{}
	service := application.NewSessionService(store, manager, nil, nil, nil, nil, nil, nil,
		application.SessionCreationCollaborators{Signals: idleSignals{}})
	sessions := &sessionPlatformExecutor{service: service, runtime: manager}
	for _, definition := range sessionPlatformDefinitions() {
		if definition.Name == "sessions.stop" {
			if err := platform.Register(definition, sessions); err != nil {
				t.Fatal(err)
			}
		}
	}
	return CapturePeerAddress(NewHandler(db, Dependencies{Capabilities: capabilities, PlatformCapabilities: platform})), store, manager
}

func stopEnvelope(key string) map[string]any {
	return map[string]any{
		"command_id": "cmd-" + key, "capability": "sessions.stop",
		"correlation_id": "ain:314", "reason": "presidente pediu para fechar a sessão",
		"target": map[string]any{"type": "session", "id": "s1"}, "payload": map[string]any{},
	}
}

// Task 870: a client holding approvals:waived stops a session without an
// approval_token, but still under an authorization_ref, a reason and the
// command ledger.
func TestApprovalsWaivedClientStopsSessionWithoutGrant(t *testing.T) {
	db := automationTestDB(t)
	handler, store, manager := waivedStopHandler(t, db)
	helena := provisionApprovalTestClient(t, db, "helena-mylifeos", ScopeSessionsWrite, ScopeApprovalsWaived)

	noCorrelation := stopEnvelope("no-correlation")
	delete(noCorrelation, "correlation_id")
	response := automationRequest(t, handler, helena.Token, http.MethodPost, "/commands", "no-correlation", noCorrelation)
	if response.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, response) != "correlation_id_required" {
		t.Fatalf("missing correlation status=%d body=%s", response.Code, response.Body.String())
	}
	noReason := stopEnvelope("no-reason")
	delete(noReason, "reason")
	response = automationRequest(t, handler, helena.Token, http.MethodPost, "/commands", "no-reason", noReason)
	if response.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, response) != "reason_required" {
		t.Fatalf("missing reason status=%d body=%s", response.Code, response.Body.String())
	}
	if manager.stopped != 0 {
		t.Fatal("session stopped by a rejected command")
	}

	response = automationRequest(t, handler, helena.Token, http.MethodPost, "/commands", "ain314-stop", stopEnvelope("ain314-stop"))
	if response.Code != http.StatusOK {
		t.Fatalf("waived stop status=%d body=%s", response.Code, response.Body.String())
	}
	if manager.stopped != 1 || store.session.Status != "stopped" {
		t.Fatalf("session not stopped: stopped=%d status=%s", manager.stopped, store.session.Status)
	}
	var ledger struct {
		Status     string `db:"status"`
		Capability string `db:"capability"`
	}
	if err := db.GetContext(context.Background(), &ledger, `
		SELECT status, capability FROM automation_commands
		WHERE client_id = ? AND idempotency_key = ?`, helena.Client.ID, "ain314-stop"); err != nil {
		t.Fatal(err)
	}
	if ledger.Status != "succeeded" || ledger.Capability != "sessions.stop" {
		t.Fatalf("waived stop not audited: %+v", ledger)
	}
}

func TestApprovalsWaivedIsPerClient(t *testing.T) {
	db := automationTestDB(t)
	handler, _, manager := waivedStopHandler(t, db)
	other := provisionApprovalTestClient(t, db, "other-operator", ScopeSessionsWrite)
	response := automationRequest(t, handler, other.Token, http.MethodPost, "/commands", "other-stop", stopEnvelope("other-stop"))
	if response.Code != http.StatusConflict || decodeAutomationErrorCode(t, response) != "approval_required" {
		t.Fatalf("client without approvals:waived status=%d body=%s", response.Code, response.Body.String())
	}
	if manager.stopped != 0 {
		t.Fatal("session stopped without approval")
	}
}

func TestApprovalsWaivedDiscoveryReportsNoApprovalRequired(t *testing.T) {
	db := automationTestDB(t)
	handler, _, _ := waivedStopHandler(t, db)
	helena := provisionApprovalTestClient(t, db, "helena-mylifeos", ScopeSessionsWrite, ScopeApprovalsWaived)
	other := provisionApprovalTestClient(t, db, "other-operator", ScopeSessionsWrite)
	stopDescriptor := func(token string) capabilityDescriptor {
		t.Helper()
		response := automationRequest(t, handler, token, http.MethodGet, "/capabilities", "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("capabilities status=%d body=%s", response.Code, response.Body.String())
		}
		var decoded capabilitiesResponse
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		for _, descriptor := range decoded.Capabilities {
			if descriptor.Name == "sessions.stop" {
				return descriptor
			}
		}
		t.Fatalf("sessions.stop missing from discovery: %s", response.Body.String())
		return capabilityDescriptor{}
	}
	if got := stopDescriptor(helena.Token); got.ApprovalRequired || got.Approval != application.ApprovalExplicit {
		t.Fatalf("waived client descriptor=%+v", got)
	}
	if got := stopDescriptor(other.Token); !got.ApprovalRequired {
		t.Fatalf("regular client descriptor=%+v", got)
	}
}

func TestCoordinatorScopesNeverWaiveApprovals(t *testing.T) {
	for _, scope := range append(append([]Scope(nil), coordinatorScopes...), coordinatorSessionScopes...) {
		if scope == ScopeApprovalsWaived || scope == ScopeApprovalsGrant || scope == ScopeApprovalsSelf {
			t.Fatalf("coordinator holds approvals scope %s", scope)
		}
	}
}
