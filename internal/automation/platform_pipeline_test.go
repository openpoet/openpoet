package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func platformPipelineHandler(
	t *testing.T,
	db *database.DB,
	now *time.Time,
	definition PlatformCapabilityDefinition,
	executor PlatformDomainExecutor,
) http.Handler {
	t.Helper()
	service := application.NewProjectTaskService(db, nil)
	capabilities, err := application.NewProjectTaskCapabilityRegistry(service)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.RegisterWorkRunCapabilities(capabilities, application.NewWorkRunService(db)); err != nil {
		t.Fatal(err)
	}
	platform, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.Register(definition, executor); err != nil {
		t.Fatal(err)
	}
	random := bytes.NewReader(bytes.Repeat([]byte{0x6c}, 4096))
	return CapturePeerAddress(NewHandler(db, Dependencies{
		Capabilities: capabilities, PlatformCapabilities: platform,
		ApprovalRandom: random, Now: func() time.Time { return *now },
	}))
}

func platformPipelineDefinition(name application.CapabilityName, approval application.ApprovalPolicy, scopes ...application.CapabilityScope) PlatformCapabilityDefinition {
	return PlatformCapabilityDefinition{
		Name: name, Scopes: scopes, Risk: application.CapabilityRiskWrite,
		Approval: approval, Handler: application.CapabilityHandler(name), Service: "platform_pipeline_test",
	}
}

func findCapabilityDescriptor(t *testing.T, response capabilitiesResponse, name application.CapabilityName) capabilityDescriptor {
	t.Helper()
	for _, descriptor := range response.Capabilities {
		if descriptor.Name == name {
			return descriptor
		}
	}
	t.Fatalf("capability %s was not discovered", name)
	return capabilityDescriptor{}
}

func TestPlatformPipelineDiscoveryMergesWithoutDuplicatesAndRequiresAllScopes(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	definition := platformPipelineDefinition(
		"projects.platform_probe", application.ApprovalNone,
		application.CapabilityScope("projects:read"), application.CapabilityScope("credentials:use"),
	)
	handler := platformPipelineHandler(t, db, &now, definition, &fakePlatformExecutor{})
	partial := provisionApprovalTestClient(t, db, "platform-discovery-partial", ScopeProjectsRead)
	response := automationRequest(t, handler, partial.Token, http.MethodGet, "/capabilities", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("discovery status=%d body=%s", response.Code, response.Body.String())
	}
	var discovered capabilitiesResponse
	if err := decodePlatformPipelineJSON(response.Body.Bytes(), &discovered); err != nil {
		t.Fatal(err)
	}
	if len(discovered.Capabilities) != 25 {
		t.Fatalf("merged capabilities=%d, want 25", len(discovered.Capabilities))
	}
	descriptor := findCapabilityDescriptor(t, discovered, definition.Name)
	if descriptor.Scope != "projects:read" || len(descriptor.Scopes) != 2 ||
		descriptor.Scopes[0] != "projects:read" || descriptor.Scopes[1] != "credentials:use" || descriptor.Allowed || !descriptor.Mutation {
		t.Fatalf("partial multi-scope descriptor=%+v", descriptor)
	}
	legacy := findCapabilityDescriptor(t, discovered, application.CapabilityTasksList)
	if len(legacy.Scopes) != 1 || legacy.Scopes[0] != legacy.Scope || legacy.Mutation {
		t.Fatalf("legacy compatibility descriptor=%+v", legacy)
	}

	allowed := provisionApprovalTestClient(t, db, "platform-discovery-allowed", ScopeProjectsRead, ScopeCredentialsUse)
	allowedResponse := automationRequest(t, handler, allowed.Token, http.MethodGet, "/capabilities", "", nil)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("allowed discovery status=%d body=%s", allowedResponse.Code, allowedResponse.Body.String())
	}
	if err := decodePlatformPipelineJSON(allowedResponse.Body.Bytes(), &discovered); err != nil {
		t.Fatal(err)
	}
	if descriptor = findCapabilityDescriptor(t, discovered, definition.Name); !descriptor.Allowed {
		t.Fatalf("full-scope descriptor not allowed: %+v", descriptor)
	}
}

func TestPlatformScopeCatalogAcceptsManifestScopesWithoutWildcard(t *testing.T) {
	manifestScopes := []Scope{
		ScopeAgentsRead, ScopeAgentsWrite, ScopeAIRead, ScopeAIUse, ScopeAIWrite,
		ScopeAIConfigsRead, ScopeAIConfigsWrite, ScopeConfigWrite,
		ScopeCredentialsRead, ScopeCredentialsUse, ScopeCredentialsWrite,
		ScopeDocumentsRead, ScopeDocumentsWrite, ScopeFilesRead, ScopeFilesWrite,
		ScopeGitRead, ScopeGitWrite, ScopeHooksRespond,
		ScopeMCPRead, ScopeMCPWrite, ScopeNotificationsWrite,
		ScopeSettingsRead, ScopeSettingsWrite, ScopeSkillsRead, ScopeSkillsWrite,
		ScopeTagsRead, ScopeTagsWrite, ScopeTokenUsageRead, ScopeTokenUsageWrite,
		ScopeToolsExecute, ScopeToolsPolicy, ScopeToolsRead, ScopeToolsWrite,
		ScopeTunnelRead, ScopeTunnelAdmin, ScopeUpdateRead, ScopeUpdateExecute,
		ScopeVoiceUse, ScopeSessionsWrite,
	}
	set, err := NewScopeSet(manifestScopes...)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != len(manifestScopes) {
		t.Fatalf("manifest scope set=%d want=%d", len(set), len(manifestScopes))
	}
	if _, err := NewScopeSet(Scope("admin:*")); err == nil {
		t.Fatal("global wildcard scope was accepted")
	}
	if _, err := NewScopeSet(Scope("config:read")); err == nil {
		t.Fatal("unused config:read scope was accepted")
	}
}

func TestPlatformPipelineRejectsMissingSecondaryScopeBeforeDispatch(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 20, 5, 0, 0, time.UTC)
	definition := platformPipelineDefinition(
		"projects.platform_multiscope", application.ApprovalNone,
		application.CapabilityScope("projects:read"), application.CapabilityScope("credentials:use"),
	)
	executor := &fakePlatformExecutor{}
	handler := platformPipelineHandler(t, db, &now, definition, executor)
	client := provisionApprovalTestClient(t, db, "platform-secondary-scope", ScopeProjectsRead)
	response := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-secondary", map[string]any{
		"command_id": "platform-secondary", "capability": definition.Name,
		"target": map[string]any{}, "payload": map[string]any{},
	})
	if response.Code != http.StatusForbidden || decodeAutomationErrorCode(t, response) != "insufficient_scope" {
		t.Fatalf("secondary scope status=%d body=%s", response.Code, response.Body.String())
	}
	if executor.validateCalls != 0 {
		t.Fatalf("missing secondary scope reached executor %d times", executor.validateCalls)
	}
	var envelope errorEnvelope
	if err := decodePlatformPipelineJSON(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	required, requiredOK := envelope.Error.Details["required_scopes"].([]any)
	missing, missingOK := envelope.Error.Details["missing_scopes"].([]any)
	if !requiredOK || len(required) != 2 || !missingOK || len(missing) != 1 || missing[0] != "credentials:use" {
		t.Fatalf("multi-scope error details=%+v", envelope.Error.Details)
	}
}

func TestPlatformPipelinePolicyRequiresCanonicalCorrelationAndBuildsAuthorization(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 20, 10, 0, 0, time.UTC)
	definition := platformPipelineDefinition(
		"projects.platform_policy", application.ApprovalByPolicy, application.CapabilityScope("projects:write"),
	)
	prepared := &fakePlatformPreparedCommand{result: map[string]any{"updated": true}}
	executor := &fakePlatformExecutor{prepared: prepared}
	handler := platformPipelineHandler(t, db, &now, definition, executor)
	client := provisionApprovalTestClient(t, db, "platform-policy", ScopeProjectsWrite)
	base := map[string]any{
		"command_id": "platform-policy", "capability": definition.Name,
		"target": map[string]any{}, "payload": map[string]any{"enabled": true},
	}
	missing := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-policy-missing", base)
	if missing.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, missing) != "correlation_id_required" {
		t.Fatalf("missing policy correlation status=%d body=%s", missing.Code, missing.Body.String())
	}
	if executor.validateCalls != 0 {
		t.Fatal("policy command without evidence reached executor")
	}
	base["correlation_id"] = " policy:automation:platform-policy "
	nonCanonical := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-policy-noncanonical", base)
	if nonCanonical.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, nonCanonical) != "correlation_id_required" {
		t.Fatalf("non-canonical policy correlation status=%d body=%s", nonCanonical.Code, nonCanonical.Body.String())
	}
	base["correlation_id"] = "policy:automation:platform-policy"
	succeeded := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-policy-valid", base)
	if succeeded.Code != http.StatusOK {
		t.Fatalf("policy dispatch status=%d body=%s", succeeded.Code, succeeded.Body.String())
	}
	metadata, result := decodeCommandResult[map[string]any](t, succeeded)
	if metadata.Status != "succeeded" || result["updated"] != true || prepared.executeCalls != 1 {
		t.Fatalf("policy result=%+v metadata=%+v calls=%d", result, metadata, prepared.executeCalls)
	}
	authorization := prepared.authorization
	if !authorization.Approved || authorization.ApprovedBy != client.Client.ID ||
		authorization.Actor.ID != client.Client.ID || authorization.Actor.Type != "automation_client" {
		t.Fatalf("policy authorization=%+v", authorization)
	}
}

func TestPlatformPipelineExplicitGrantDryRunDispatchAndReplay(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 20, 20, 0, 0, time.UTC)
	definition := platformPipelineDefinition(
		"projects.platform_delete", application.ApprovalExplicit, application.CapabilityScope("projects:write"),
	)
	definition.Risk = application.CapabilityRiskDestructive
	prepared := &fakePlatformPreparedCommand{
		preview: map[string]any{"would_delete": true}, result: map[string]any{"deleted": true},
	}
	executor := &fakePlatformExecutor{prepared: prepared}
	handler := platformPipelineHandler(t, db, &now, definition, executor)
	client := provisionApprovalTestClient(t, db, "platform-explicit", ScopeProjectsWrite)
	broker := provisionApprovalTestClient(t, db, "platform-explicit-broker", ScopeApprovalsGrant)
	grant := issueApprovalGrantForTest(
		t, handler, broker.Token, client.Client.ID, definition.Name,
		"platform-explicit-command", "policy:automation:platform-explicit", 0,
	)
	base := map[string]any{
		"command_id": "platform-explicit-command", "capability": definition.Name,
		"target": map[string]any{}, "payload": map[string]any{}, "reason": "approved cleanup",
		"correlation_id": grant.AuthorizationRef, "approval_token": grant.ApprovalToken,
	}
	base["dry_run"] = true
	dryRun := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-explicit-dry", base)
	if dryRun.Code != http.StatusOK {
		t.Fatalf("explicit dry-run status=%d body=%s", dryRun.Code, dryRun.Body.String())
	}
	dryMetadata, dryResult := decodeCommandResult[map[string]any](t, dryRun)
	if dryMetadata.Status != "dry_run" || dryResult["would_delete"] != true || prepared.executeCalls != 0 {
		t.Fatalf("dry-run result=%+v metadata=%+v execute=%d", dryResult, dryMetadata, prepared.executeCalls)
	}
	stored, err := db.GetAutomationApprovalGrant(context.Background(), grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UsedAt.Valid {
		t.Fatal("dry-run consumed the explicit approval grant")
	}

	delete(base, "dry_run")
	executed := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-explicit-execute", base)
	if executed.Code != http.StatusOK {
		t.Fatalf("explicit dispatch status=%d body=%s", executed.Code, executed.Body.String())
	}
	executedMetadata, executedResult := decodeCommandResult[map[string]any](t, executed)
	if executedMetadata.Status != "succeeded" || executedResult["deleted"] != true || prepared.executeCalls != 1 {
		t.Fatalf("explicit result=%+v metadata=%+v execute=%d", executedResult, executedMetadata, prepared.executeCalls)
	}
	if !prepared.authorization.Approved || prepared.authorization.ApprovedBy != broker.Client.ID ||
		prepared.authorization.ApprovedBy == grant.ApprovalToken || prepared.authorization.Reason != "approved cleanup" {
		t.Fatalf("explicit authorization did not use issued_by metadata: %+v", prepared.authorization)
	}
	stored, err = db.GetAutomationApprovalGrant(context.Background(), grant.GrantID)
	if err != nil || !stored.UsedAt.Valid {
		t.Fatalf("executed grant=%+v err=%v", stored, err)
	}
	replay := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-explicit-execute", base)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" ||
		replay.Body.String() != executed.Body.String() || prepared.executeCalls != 1 {
		t.Fatalf("explicit replay status=%d replay=%q execute=%d body=%s", replay.Code, replay.Header().Get("Idempotency-Replayed"), prepared.executeCalls, replay.Body.String())
	}
}

func TestPlatformPipelineErrorsUseRedactedVersionedEnvelope(t *testing.T) {
	db := automationTestDB(t)
	now := time.Date(2026, 7, 9, 20, 30, 0, 0, time.UTC)
	definition := platformPipelineDefinition(
		"projects.platform_failure", application.ApprovalNone, application.CapabilityScope("projects:read"),
	)
	secret := "provider-secret-material"
	executor := &fakePlatformExecutor{validateErr: errors.New("provider failed with " + secret)}
	handler := platformPipelineHandler(t, db, &now, definition, executor)
	client := provisionApprovalTestClient(t, db, "platform-redaction", ScopeProjectsRead)
	response := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "platform-redaction", map[string]any{
		"command_id": "platform-redaction", "capability": definition.Name,
		"target": map[string]any{}, "payload": map[string]any{},
	})
	if response.Code != http.StatusServiceUnavailable || decodeAutomationErrorCode(t, response) != "platform_execution_failed" {
		t.Fatalf("platform error status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("platform error leaked secret: %s", response.Body.String())
	}
	var envelope errorEnvelope
	if err := decodePlatformPipelineJSON(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.APIVersion != APIVersion || envelope.CommandID != "platform-redaction" {
		t.Fatalf("unversioned platform error envelope=%+v", envelope)
	}
}

func decodePlatformPipelineJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	return decoder.Decode(target)
}
