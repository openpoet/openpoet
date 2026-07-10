package automation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openpoet/internal/application"
)

type fakePlatformExecutor struct {
	validateCalls int
	input         PlatformExecutionInput
	validateErr   error
	prepared      *fakePlatformPreparedCommand
}

func (e *fakePlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	e.validateCalls++
	e.input = input
	if e.validateErr != nil {
		return nil, e.validateErr
	}
	if e.prepared == nil {
		e.prepared = &fakePlatformPreparedCommand{}
	}
	return e.prepared, nil
}

type fakePlatformPreparedCommand struct {
	preview       any
	result        any
	executeErr    error
	executeCalls  int
	authorization application.ActionAuthorization
}

func (c *fakePlatformPreparedCommand) DryRunResult() any { return c.preview }

func (c *fakePlatformPreparedCommand) Execute(_ context.Context, authorization application.ActionAuthorization) (any, error) {
	c.executeCalls++
	c.authorization = authorization
	return c.result, c.executeErr
}

func platformTestDefinition() PlatformCapabilityDefinition {
	return PlatformCapabilityDefinition{
		Name: "projects.validate", Scopes: []application.CapabilityScope{"projects:read"}, Risk: application.CapabilityRiskRead,
		Approval: application.ApprovalNone, Mutation: true, Handler: "projects.validate", Service: "project_operations",
	}
}

func platformTestRegistry(t *testing.T, definition PlatformCapabilityDefinition, executor PlatformDomainExecutor) (*application.CapabilityRegistry, *PlatformCapabilityRegistry) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition, executor); err != nil {
		t.Fatal(err)
	}
	return capabilities, registry
}

func platformTestActor() Actor {
	return Actor{
		Type: "automation_client", ID: "internal-id", ClientID: "client-42", Name: "helena",
		Scopes: ScopeSet{ScopeProjectsRead: {}},
	}
}

func TestPlatformCapabilityRegistrationBindsTypedExecutorMetadata(t *testing.T) {
	definition := platformTestDefinition()
	definition.Scopes = []application.CapabilityScope{"projects:read", "projects:write", "projects:read"}
	executor := &fakePlatformExecutor{}
	capabilities, registry := platformTestRegistry(t, definition, executor)
	registered, ok := capabilities.Lookup("projects.validate")
	if !ok {
		t.Fatal("application capability was not registered")
	}
	if registered.Scope != "projects:read" || registered.Risk != application.CapabilityRiskRead ||
		registered.Approval != application.ApprovalNone || registered.Handler != "projects.validate" {
		t.Fatalf("registered metadata mismatch: %#v", registered)
	}
	if registered.Service == nil || registered.Service.CapabilityServiceName() != "project_operations" {
		t.Fatalf("service binding mismatch: %#v", registered.Service)
	}
	descriptors := registry.ListForActor(platformTestActor())
	if len(descriptors) != 1 || descriptors[0].Scope != "projects:read" || len(descriptors[0].Scopes) != 2 || descriptors[0].Allowed || !descriptors[0].Mutation {
		t.Fatalf("multi-scope discovery mismatch: %#v", descriptors)
	}
	actor := platformTestActor()
	actor.Scopes[ScopeProjectsWrite] = struct{}{}
	if allowed := registry.ListForActor(actor); len(allowed) != 1 || !allowed[0].Allowed {
		t.Fatalf("all-scope discovery did not allow actor: %#v", allowed)
	}
	if err := registry.Register(definition, &fakePlatformExecutor{}); err == nil {
		t.Fatal("duplicate platform capability registration succeeded")
	}
}

func TestPlatformDispatchRequiresEveryRegisteredScope(t *testing.T) {
	definition := platformTestDefinition()
	definition.Scopes = []application.CapabilityScope{"projects:read", "projects:write"}
	executor := &fakePlatformExecutor{}
	_, registry := platformTestRegistry(t, definition, executor)
	request := PlatformDispatchRequest{Capability: definition.Name, Actor: platformTestActor()}
	if _, err := DispatchPlatformCapability(context.Background(), registry, request); err == nil {
		t.Fatal("dispatch accepted actor missing a secondary scope")
	}
	if executor.validateCalls != 0 {
		t.Fatal("insufficient-scope request reached executor")
	}
	request.Actor.Scopes[ScopeProjectsWrite] = struct{}{}
	if _, err := DispatchPlatformCapability(context.Background(), registry, request); err != nil {
		t.Fatalf("dispatch with all scopes failed: %v", err)
	}
	if executor.validateCalls != 1 || len(executor.input.Scopes) != 2 {
		t.Fatalf("executor did not receive multi-scope definition: %#v", executor.input.Scopes)
	}
}

func TestPlatformCapabilityRegistrationRejectsInvalidDefinition(t *testing.T) {
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	definition := platformTestDefinition()
	definition.Service = ""
	if err := registry.Register(definition, &fakePlatformExecutor{}); err == nil {
		t.Fatal("empty service name was registered")
	}
	definition = platformTestDefinition()
	definition.Limits.MaxPayloadBytes = hardPlatformPayloadBytes + 1
	if err := registry.Register(definition, &fakePlatformExecutor{}); err == nil {
		t.Fatal("unbounded payload limit was registered")
	}
	if len(capabilities.List()) != 0 {
		t.Fatalf("invalid definition mutated application registry: %#v", capabilities.List())
	}
}

func TestPlatformDispatchRejectsInvalidAndOversizedJSONBeforeExecutor(t *testing.T) {
	definition := platformTestDefinition()
	definition.Limits = PlatformCapabilityLimits{MaxTargetBytes: 32, MaxPayloadBytes: 32}
	executor := &fakePlatformExecutor{}
	_, registry := platformTestRegistry(t, definition, executor)
	cases := []PlatformDispatchRequest{
		{Capability: definition.Name, Actor: platformTestActor(), Target: json.RawMessage(`[]`), Payload: json.RawMessage(`{}`)},
		{Capability: definition.Name, Actor: platformTestActor(), Target: json.RawMessage(`{`), Payload: json.RawMessage(`{}`)},
		{Capability: definition.Name, Actor: platformTestActor(), Target: json.RawMessage(`{}`), Payload: json.RawMessage(`"scalar"`)},
		{Capability: definition.Name, Actor: platformTestActor(), Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{} {}`)},
		{Capability: definition.Name, Actor: platformTestActor(), Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{"value":"` + strings.Repeat("x", 40) + `"}`)},
	}
	for i, request := range cases {
		if _, err := DispatchPlatformCapability(context.Background(), registry, request); err == nil {
			t.Fatalf("case %d unexpectedly succeeded", i)
		}
	}
	if executor.validateCalls != 0 {
		t.Fatalf("invalid JSON reached domain validator %d times", executor.validateCalls)
	}
}

func TestPlatformDispatchRejectsExcessiveJSONDepth(t *testing.T) {
	definition := platformTestDefinition()
	definition.Limits.MaxPayloadBytes = 4096
	executor := &fakePlatformExecutor{}
	_, registry := platformTestRegistry(t, definition, executor)
	deepPayload := strings.Repeat("[", maxPlatformJSONDepth+1) + `{}` + strings.Repeat("]", maxPlatformJSONDepth+1)
	if _, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(), Payload: json.RawMessage(deepPayload),
	}); err == nil {
		t.Fatal("excessively nested payload was accepted")
	}
	if executor.validateCalls != 0 {
		t.Fatal("excessively nested payload reached executor")
	}
}

func TestPlatformDryRunValidatesWithoutExecuting(t *testing.T) {
	definition := platformTestDefinition()
	definition.Risk = application.CapabilityRiskDestructive
	definition.Approval = application.ApprovalExplicit
	prepared := &fakePlatformPreparedCommand{preview: map[string]any{"project_id": int64(7)}}
	executor := &fakePlatformExecutor{prepared: prepared}
	_, registry := platformTestRegistry(t, definition, executor)

	result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(), DryRun: true,
		Target: json.RawMessage(` { "project_id" : 7 } `), Payload: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "dry_run" || executor.validateCalls != 1 || prepared.executeCalls != 0 {
		t.Fatalf("dry-run crossed execution boundary: result=%#v validate=%d execute=%d", result, executor.validateCalls, prepared.executeCalls)
	}
	if string(executor.input.Target) != `{"project_id":7}` || string(executor.input.Payload) != `{}` {
		t.Fatalf("dry-run input was not normalized: target=%s payload=%s", executor.input.Target, executor.input.Payload)
	}
	if executor.input.Authorization.Actor.ID != "client-42" || executor.input.Authorization.Approved {
		t.Fatalf("dry-run authorization was derived incorrectly: %#v", executor.input.Authorization)
	}
}

func TestPlatformCommandEnvelopeDispatchHelper(t *testing.T) {
	definition := platformTestDefinition()
	prepared := &fakePlatformPreparedCommand{result: map[string]any{"valid": true}}
	executor := &fakePlatformExecutor{prepared: prepared}
	_, registry := platformTestRegistry(t, definition, executor)
	result, err := dispatchRegisteredPlatformCommand(context.Background(), registry, &commandEnvelope{
		Capability: definition.Name,
		Target:     commandTarget{ProjectID: 9},
		Payload:    json.RawMessage(`{"probe":true}`),
	}, platformTestActor(), PlatformApprovalDecision{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" || prepared.executeCalls != 1 {
		t.Fatalf("command helper did not dispatch: result=%#v calls=%d", result, prepared.executeCalls)
	}
	if string(executor.input.Target) != `{"project_id":9}` || string(executor.input.Payload) != `{"probe":true}` {
		t.Fatalf("command helper normalized wrong input: target=%s payload=%s", executor.input.Target, executor.input.Payload)
	}
}

func TestPlatformDispatchDerivesApprovedApplicationAuthorization(t *testing.T) {
	definition := platformTestDefinition()
	definition.Risk = application.CapabilityRiskUnsafe
	definition.Approval = application.ApprovalExplicit
	prepared := &fakePlatformPreparedCommand{result: map[string]any{"ok": true}}
	executor := &fakePlatformExecutor{prepared: prepared}
	_, registry := platformTestRegistry(t, definition, executor)

	if _, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(), Reason: "operate remote system",
	}); err == nil {
		t.Fatal("explicit capability executed without validated approval")
	}
	if executor.validateCalls != 0 {
		t.Fatal("unauthorized command reached domain validator")
	}
	approval, err := NewValidatedPlatformApproval("presidente")
	if err != nil {
		t.Fatal(err)
	}
	result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(), Reason: " operate remote system ", Approval: approval,
		Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{"path":"/srv"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" || prepared.executeCalls != 1 {
		t.Fatalf("approved command did not execute once: result=%#v calls=%d", result, prepared.executeCalls)
	}
	authorization := prepared.authorization
	if authorization.Actor.Type != "automation_client" || authorization.Actor.ID != "client-42" ||
		authorization.Reason != "operate remote system" || !authorization.Approved || authorization.ApprovedBy != "presidente" {
		t.Fatalf("application authorization mismatch: %#v", authorization)
	}
	if authorization.AllowEnvironment || authorization.AllowUnsafePermissions {
		t.Fatalf("generic wiring escalated environment/unsafe permission flags: %#v", authorization)
	}
}

func TestValidatedPlatformApprovalRejectsControlCharacters(t *testing.T) {
	if _, err := NewValidatedPlatformApproval("presidente\nspoofed"); err == nil {
		t.Fatal("approval identity with control characters was accepted")
	}
}

func TestPlatformDispatchRejectsMissingActorAndReason(t *testing.T) {
	definition := platformTestDefinition()
	definition.Risk = application.CapabilityRiskDestructive
	executor := &fakePlatformExecutor{}
	_, registry := platformTestRegistry(t, definition, executor)
	if _, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{Capability: definition.Name}); err == nil {
		t.Fatal("missing actor was accepted")
	}
	if _, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(),
	}); err == nil {
		t.Fatal("destructive command without reason was accepted")
	}
	if executor.validateCalls != 0 {
		t.Fatalf("invalid authorization reached validator %d times", executor.validateCalls)
	}
}

func TestPlatformExecutionErrorsAreRedacted(t *testing.T) {
	secret := "ssh-private-key-material"
	definition := platformTestDefinition()
	executor := &fakePlatformExecutor{validateErr: errors.New("dial failed with " + secret)}
	_, registry := platformTestRegistry(t, definition, executor)
	_, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(),
	})
	assertRedactedPlatformError(t, err, secret, "platform_execution_failed")

	executor.validateErr = nil
	executor.prepared = &fakePlatformPreparedCommand{executeErr: errors.New("provider response contained " + secret)}
	_, err = DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(),
	})
	assertRedactedPlatformError(t, err, secret, "platform_execution_failed")

	executor.prepared = &fakePlatformPreparedCommand{executeErr: &application.Error{
		Kind: application.ErrorValidation, Code: "safe_validation", Message: "request is invalid", Cause: errors.New(secret),
	}}
	_, err = DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(),
	})
	assertRedactedPlatformError(t, err, secret, "safe_validation")
	if err.Error() != "request is invalid" {
		t.Fatalf("safe application message was not preserved: %v", err)
	}

	executor.prepared = &fakePlatformPreparedCommand{executeErr: &PlatformDispatchError{
		Code: "unsafe_error", Message: "unsafe " + secret,
	}}
	_, err = DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: definition.Name, Actor: platformTestActor(),
	})
	assertRedactedPlatformError(t, err, secret, "platform_execution_failed")
}

func assertRedactedPlatformError(t *testing.T, err error, secret, code string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected platform dispatch error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	var dispatchErr *PlatformDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != code {
		t.Fatalf("unexpected redacted error: %#v", err)
	}
}
