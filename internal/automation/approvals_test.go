package automation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func provisionApprovalTestClient(t *testing.T, db *database.DB, name string, scopes ...Scope) *ProvisionedClient {
	t.Helper()
	seedHash := sha256.Sum256([]byte(name))
	seed := bytes.Repeat(seedHash[:], 2)
	client, err := ProvisionClient(context.Background(), db, name, scopes, bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func approvalTestHandler(t *testing.T, db *database.DB, now *time.Time) http.Handler {
	t.Helper()
	service := application.NewProjectTaskService(db, nil)
	registry, err := application.NewProjectTaskCapabilityRegistry(service)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.RegisterWorkRunCapabilities(registry, application.NewWorkRunService(db)); err != nil {
		t.Fatal(err)
	}
	random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, 4096))
	return CapturePeerAddress(NewHandler(db, Dependencies{
		Capabilities: registry, Snapshot: db, ApprovalRandom: random,
		Now: func() time.Time { return *now },
	}))
}

func issueApprovalGrantForTest(
	t *testing.T,
	handler http.Handler,
	brokerToken string,
	targetClientID string,
	capability application.CapabilityName,
	commandID string,
	authorizationRef string,
	expiresInSeconds int,
) approvalGrantResponse {
	t.Helper()
	body := map[string]any{
		"target_client_id": targetClientID, "capability": capability,
		"command_id": commandID, "authorization_ref": authorizationRef,
	}
	if expiresInSeconds != 0 {
		body["expires_in_seconds"] = expiresInSeconds
	}
	response := automationRequest(t, handler, brokerToken, http.MethodPost, "/approvals", "", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("approval grant status=%d body=%s", response.Code, response.Body.String())
	}
	var grant approvalGrantResponse
	if err := json.Unmarshal(response.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.ApprovalToken == "" || grant.GrantID == "" {
		t.Fatalf("approval grant response=%+v", grant)
	}
	return grant
}

func TestApprovalBrokerRequiresDedicatedScopeAndStoresOnlyDigest(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 18, 30, 0, 0, time.UTC)
	target := provisionApprovalTestClient(t, db, "helena-normal", ScopeTasksWrite)
	broker := provisionApprovalTestClient(t, db, "approval-broker", ScopeApprovalsGrant)
	handler := approvalTestHandler(t, db, &now)
	body := map[string]any{
		"target_client_id": target.Client.ID, "capability": application.CapabilityTasksDelete,
		"command_id": "delete-1", "authorization_ref": "task:approval:1",
	}
	denied := automationRequest(t, handler, target.Token, http.MethodPost, "/approvals", "", body)
	if denied.Code != http.StatusForbidden || decodeAutomationErrorCode(t, denied) != "insufficient_scope" {
		t.Fatalf("normal operator grant status=%d body=%s", denied.Code, denied.Body.String())
	}
	targetScopes, err := ParseScopeSet(target.Client.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	if targetScopes.Has(ScopeApprovalsGrant) {
		t.Fatal("normal Helena/operator client unexpectedly has approvals:grant")
	}
	invalidReferenceBody := map[string]any{
		"target_client_id": target.Client.ID, "capability": application.CapabilityTasksDelete,
		"command_id": "delete-1", "authorization_ref": "opaque:approval:1",
	}
	invalidReference := automationRequest(t, handler, broker.Token, http.MethodPost, "/approvals", "", invalidReferenceBody)
	if invalidReference.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, invalidReference) != "authorization_ref_invalid" {
		t.Fatalf("invalid authorization_ref status=%d body=%s", invalidReference.Code, invalidReference.Body.String())
	}

	response := automationRequest(t, handler, broker.Token, http.MethodPost, "/approvals", "", body)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("broker status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var issued approvalGrantResponse
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetAutomationApprovalGrant(context.Background(), issued.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(issued.ApprovalToken))
	if !bytes.Equal(stored.TokenHash, wantHash[:]) || stored.TargetClientID != target.Client.ID ||
		stored.IssuedByClientID != broker.Client.ID || stored.Capability != string(application.CapabilityTasksDelete) ||
		stored.CommandID != "delete-1" || stored.AuthorizationRef != "task:approval:1" ||
		!stored.ExpiresAt.Equal(now.Add(defaultApprovalGrantTTL)) {
		t.Fatalf("stored grant=%+v", stored)
	}
	storedJSON, _ := json.Marshal(stored)
	if bytes.Contains(storedJSON, []byte(issued.ApprovalToken)) || bytes.Contains(storedJSON, []byte("token_hash")) {
		t.Fatalf("persisted grant exposed plaintext token: %s", storedJSON)
	}
}

func TestExplicitApprovalBindingConsumptionAndCompletedReplay(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)
	project := automationContractProject(t, db, "approval-binding")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Delete with approval", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	target := provisionApprovalTestClient(t, db, "approval-target", ScopeTasksRead, ScopeTasksWrite)
	other := provisionApprovalTestClient(t, db, "approval-other", ScopeTasksRead, ScopeTasksWrite)
	broker := provisionApprovalTestClient(t, db, "approval-broker-binding", ScopeApprovalsGrant)
	handler := approvalTestHandler(t, db, &now)
	grant := issueApprovalGrantForTest(t, handler, broker.Token, target.Client.ID,
		application.CapabilityTasksDelete, "delete-bound", "task:approval:delete-bound", 0)

	commandBody := func(commandID string, capability application.CapabilityName, token string) map[string]any {
		return map[string]any{
			"command_id": commandID, "capability": capability,
			"target":  map[string]any{"project_id": project.ID, "task_id": task.ID},
			"payload": map[string]any{}, "reason": "approved cleanup", "approval_token": token,
			"correlation_id": grant.AuthorizationRef,
		}
	}
	crossClient := automationRequest(t, handler, other.Token, http.MethodPost, "/commands", "cross-client",
		commandBody("delete-bound", application.CapabilityTasksDelete, grant.ApprovalToken))
	if crossClient.Code != http.StatusConflict || decodeAutomationErrorCode(t, crossClient) != "approval_mismatch" {
		t.Fatalf("cross-client status=%d body=%s", crossClient.Code, crossClient.Body.String())
	}
	capabilityMismatch := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "capability-mismatch",
		commandBody("delete-bound", application.CapabilityTasksApproveVerification, grant.ApprovalToken))
	if capabilityMismatch.Code != http.StatusConflict || decodeAutomationErrorCode(t, capabilityMismatch) != "approval_mismatch" {
		t.Fatalf("capability mismatch status=%d body=%s", capabilityMismatch.Code, capabilityMismatch.Body.String())
	}
	commandMismatch := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "command-mismatch",
		commandBody("delete-other", application.CapabilityTasksDelete, grant.ApprovalToken))
	if commandMismatch.Code != http.StatusConflict || decodeAutomationErrorCode(t, commandMismatch) != "approval_mismatch" {
		t.Fatalf("command mismatch status=%d body=%s", commandMismatch.Code, commandMismatch.Body.String())
	}
	authorizationMismatchBody := commandBody("delete-bound", application.CapabilityTasksDelete, grant.ApprovalToken)
	authorizationMismatchBody["correlation_id"] = "signal:approval:other"
	authorizationMismatch := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "authorization-mismatch", authorizationMismatchBody)
	if authorizationMismatch.Code != http.StatusConflict || decodeAutomationErrorCode(t, authorizationMismatch) != "approval_mismatch" {
		t.Fatalf("authorization mismatch status=%d body=%s", authorizationMismatch.Code, authorizationMismatch.Body.String())
	}
	missingCorrelationBody := commandBody("delete-bound", application.CapabilityTasksDelete, grant.ApprovalToken)
	delete(missingCorrelationBody, "correlation_id")
	missingCorrelation := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "missing-correlation", missingCorrelationBody)
	if missingCorrelation.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, missingCorrelation) != "correlation_id_required" {
		t.Fatalf("missing correlation status=%d body=%s", missingCorrelation.Code, missingCorrelation.Body.String())
	}
	invented := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "invented-token",
		commandBody("invented", application.CapabilityTasksDelete, approvalTokenScheme+"_"+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if invented.Code != http.StatusConflict || decodeAutomationErrorCode(t, invented) != "approval_invalid" {
		t.Fatalf("invented token status=%d body=%s", invented.Code, invented.Body.String())
	}
	altered := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "altered-token",
		commandBody("delete-bound", application.CapabilityTasksDelete, grant.ApprovalToken+" "))
	if altered.Code != http.StatusConflict || decodeAutomationErrorCode(t, altered) != "approval_invalid" {
		t.Fatalf("altered token status=%d body=%s", altered.Code, altered.Body.String())
	}

	validBody := commandBody("delete-bound", application.CapabilityTasksDelete, grant.ApprovalToken)
	first := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "valid-delete", validBody)
	if first.Code != http.StatusOK {
		t.Fatalf("approved command status=%d body=%s", first.Code, first.Body.String())
	}
	replay := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "valid-delete", validBody)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != first.Body.String() {
		t.Fatalf("completed replay status=%d replay=%q body=%s", replay.Code, replay.Header().Get("Idempotency-Replayed"), replay.Body.String())
	}
	var persisted struct {
		RequestFingerprint string `db:"request_fingerprint"`
		ResponseBody       []byte `db:"response_body"`
	}
	if err := db.GetContext(context.Background(), &persisted, `
		SELECT request_fingerprint, response_body
		FROM automation_commands
		WHERE client_id = ? AND idempotency_key = ?`, target.Client.ID, "valid-delete"); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(persisted.RequestFingerprint), []byte(grant.ApprovalToken)) ||
		bytes.Contains(persisted.ResponseBody, []byte(grant.ApprovalToken)) {
		t.Fatal("plaintext approval token leaked into the idempotency record")
	}
	used := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "used-delete", validBody)
	if used.Code != http.StatusConflict || decodeAutomationErrorCode(t, used) != "approval_used" {
		t.Fatalf("used grant status=%d body=%s", used.Code, used.Body.String())
	}
	if _, err := db.GetTask(context.Background(), task.ID); err == nil {
		t.Fatal("approved delete did not execute")
	}
}

func TestBulkVerificationApprovalUsesOneExplicitGrantForWholeBatch(t *testing.T) {
	ctx := context.Background()
	db := automationTestDB(t)
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	project := automationContractProject(t, db, "bulk-approval-gate")
	first := &database.ProjectTask{ProjectID: project.ID, Title: "First pending", Status: "awaiting_approval", Priority: "medium"}
	second := &database.ProjectTask{ProjectID: project.ID, Title: "Second pending", Status: "awaiting_approval", Priority: "medium"}
	for _, task := range []*database.ProjectTask{first, second} {
		if err := db.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	target := provisionApprovalTestClient(t, db, "bulk-approval-target", ScopeTasksWrite)
	broker := provisionApprovalTestClient(t, db, "bulk-approval-broker", ScopeApprovalsGrant)
	handler := approvalTestHandler(t, db, &now)

	commandID := "approve-bulk-1"
	authorizationRef := "task:approval:bulk-1"
	baseBody := map[string]any{
		"command_id": commandID, "capability": application.CapabilityTasksApproveVerificationBulk,
		"target":         map[string]any{"type": "project", "id": project.ID},
		"payload":        map[string]any{"task_ids": []int64{first.ID, second.ID}},
		"correlation_id": authorizationRef,
	}
	missingApproval := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "bulk-missing-approval", baseBody)
	if missingApproval.Code != http.StatusConflict || decodeAutomationErrorCode(t, missingApproval) != "approval_required" {
		t.Fatalf("bulk command bypassed explicit gate: status=%d body=%s", missingApproval.Code, missingApproval.Body.String())
	}
	for _, taskID := range []int64{first.ID, second.ID} {
		stored, _ := db.GetTask(ctx, taskID)
		if stored.Status != "awaiting_approval" {
			t.Fatalf("task %d mutated without approval: %+v", taskID, stored)
		}
	}

	grant := issueApprovalGrantForTest(t, handler, broker.Token, target.Client.ID,
		application.CapabilityTasksApproveVerificationBulk, commandID, authorizationRef, 0)
	approvedBody := map[string]any{
		"command_id": commandID, "capability": application.CapabilityTasksApproveVerificationBulk,
		"target":         map[string]any{"type": "project", "id": project.ID},
		"payload":        map[string]any{"task_ids": []int64{first.ID, second.ID}},
		"correlation_id": authorizationRef, "approval_token": grant.ApprovalToken,
	}
	response := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "bulk-approved", approvedBody)
	if response.Code != http.StatusOK {
		t.Fatalf("approved bulk command status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		Result struct {
			Approved int `json:"approved"`
			Failed   int `json:"failed"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Result.Approved != 2 || decoded.Result.Failed != 0 {
		t.Fatalf("unexpected command result: %+v body=%s", decoded.Result, response.Body.String())
	}
	for _, taskID := range []int64{first.ID, second.ID} {
		stored, _ := db.GetTask(ctx, taskID)
		if stored.Status != "done" {
			t.Fatalf("task %d not approved: %+v", taskID, stored)
		}
		history, err := db.ListTaskHistory(ctx, taskID, 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range history {
			if entry.EventType == "verification_approved" {
				found = true
				if entry.Actor != "automation_client:"+target.Client.ID || !strings.Contains(entry.Details, `"bulk":true`) {
					t.Fatalf("task %d audit=%+v", taskID, entry)
				}
			}
		}
		if !found {
			t.Fatalf("task %d missing per-item bulk audit: %+v", taskID, history)
		}
	}
}

func TestExplicitApprovalExpiry(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 19, 30, 0, 0, time.UTC)
	project := automationContractProject(t, db, "approval-expiry")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Must remain", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	target := provisionApprovalTestClient(t, db, "expiry-target", ScopeTasksWrite)
	broker := provisionApprovalTestClient(t, db, "expiry-broker", ScopeApprovalsGrant)
	handler := approvalTestHandler(t, db, &now)
	grant := issueApprovalGrantForTest(t, handler, broker.Token, target.Client.ID,
		application.CapabilityTasksDelete, "expiry-delete", "task:approval:expiry", 1)
	now = now.Add(time.Second)
	response := automationRequest(t, handler, target.Token, http.MethodPost, "/commands", "expiry-delete-key", map[string]any{
		"command_id": "expiry-delete", "capability": application.CapabilityTasksDelete,
		"target": map[string]any{"project_id": project.ID, "task_id": task.ID}, "payload": map[string]any{},
		"reason": "cleanup", "approval_token": grant.ApprovalToken, "correlation_id": grant.AuthorizationRef,
	})
	if response.Code != http.StatusConflict || decodeAutomationErrorCode(t, response) != "approval_expired" {
		t.Fatalf("expired approval status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := db.GetTask(context.Background(), task.ID); err != nil {
		t.Fatalf("expired grant mutated task: %v", err)
	}
}

func TestAuthorizationRefFormat(t *testing.T) {
	for _, value := range []string{
		"ain:authorization:123", "inbound:whatsapp:456", "signal:approval:789",
		"task:42", "policy:destructive-actions/v1",
	} {
		if !validAuthorizationRef(value) {
			t.Errorf("valid authorization_ref %q was rejected", value)
		}
	}
	for _, value := range []string{
		"", "opaque", "approval:123", "task:", "task:contains space", "task:line\nbreak",
		"task:zero\u200bwidth", " task:123", "task:123 ",
	} {
		if validAuthorizationRef(value) {
			t.Errorf("invalid authorization_ref %q was accepted", value)
		}
	}
}
