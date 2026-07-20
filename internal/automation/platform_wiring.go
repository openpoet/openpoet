package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/application"
)

const (
	defaultPlatformTargetBytes  = 16 << 10
	defaultPlatformPayloadBytes = 1 << 20
	hardPlatformTargetBytes     = 64 << 10
	hardPlatformPayloadBytes    = 64 << 20
	maxPlatformJSONDepth        = 32
	maxPlatformReasonRunes      = 2000
	maxPlatformScopes           = 16
)

var platformIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,199}$`)

type PlatformCapabilityLimits struct {
	MaxTargetBytes  int
	MaxPayloadBytes int
}

type PlatformCapabilityDefinition struct {
	Name     application.CapabilityName
	Scopes   []application.CapabilityScope
	Risk     application.CapabilityRisk
	Approval application.ApprovalPolicy
	Mutation bool
	Handler  application.CapabilityHandler
	Service  application.CapabilityServiceName
	Limits   PlatformCapabilityLimits
}

// PlatformDomainExecutor validates and prepares a typed domain invocation.
// Validate must be side-effect free. The returned command is the only object
// allowed to invoke an Application Service.
type PlatformDomainExecutor interface {
	Validate(context.Context, PlatformExecutionInput) (PlatformValidatedCommand, error)
}

type PlatformValidatedCommand interface {
	DryRunResult() any
	Execute(context.Context, application.ActionAuthorization) (any, error)
}

type PlatformExecutionInput struct {
	Capability    application.CapabilityName
	Handler       application.CapabilityHandler
	Scopes        []application.CapabilityScope
	Target        json.RawMessage
	Payload       json.RawMessage
	Authorization application.ActionAuthorization
	// ProjectScope is the acting client's resolved project_filter. Read-tier
	// executors that list across projects (sessions.list) must intersect their
	// results with it so a scoped client never sees another group's projects.
	ProjectScope *ProjectScopeSet
}

type platformCapabilityService struct {
	name application.CapabilityServiceName
}

func (s *platformCapabilityService) CapabilityServiceName() application.CapabilityServiceName {
	if s == nil {
		return ""
	}
	return s.name
}

type platformCapabilityBinding struct {
	definition PlatformCapabilityDefinition
	executor   PlatformDomainExecutor
	service    *platformCapabilityService
}

type PlatformCapabilityRegistry struct {
	capabilities *application.CapabilityRegistry
	mu           sync.RWMutex
	bindings     map[application.CapabilityName]platformCapabilityBinding
	scopeStore   ProjectScopeStore // resolves client project_filter tag membership (may be nil)
}

// SetProjectScopeStore wires the tag→projects resolver used to enforce a
// client's project_filter centrally. Optional: a nil store means tag_ids in a
// filter resolve to nothing (project_ids still apply).
func (r *PlatformCapabilityRegistry) SetProjectScopeStore(store ProjectScopeStore) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.scopeStore = store
	r.mu.Unlock()
}

func (r *PlatformCapabilityRegistry) projectScopeStore() ProjectScopeStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scopeStore
}

func NewPlatformCapabilityRegistry(capabilities *application.CapabilityRegistry) (*PlatformCapabilityRegistry, error) {
	if capabilities == nil {
		return nil, errors.New("application capability registry is required")
	}
	return &PlatformCapabilityRegistry{
		capabilities: capabilities,
		bindings:     make(map[application.CapabilityName]platformCapabilityBinding),
	}, nil
}

func (r *PlatformCapabilityRegistry) Register(definition PlatformCapabilityDefinition, executor PlatformDomainExecutor) error {
	definition, err := normalizePlatformDefinition(definition)
	if err != nil {
		return err
	}
	if executor == nil {
		return errors.New("platform capability executor is required")
	}
	if r == nil || r.capabilities == nil {
		return errors.New("platform capability registry is unavailable")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.bindings[definition.Name]; exists {
		return errors.New("platform capability is already registered")
	}
	service := &platformCapabilityService{name: definition.Service}
	capability := application.Capability{
		Name: definition.Name, Scope: definition.Scopes[0], Risk: definition.Risk,
		Approval: definition.Approval, Handler: definition.Handler, Service: service,
	}
	if err := r.capabilities.Register(capability); err != nil {
		return err
	}
	r.bindings[definition.Name] = platformCapabilityBinding{
		definition: definition, executor: executor, service: service,
	}
	return nil
}

func (r *PlatformCapabilityRegistry) lookup(name application.CapabilityName) (platformCapabilityBinding, bool) {
	if r == nil {
		return platformCapabilityBinding{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, ok := r.bindings[name]
	return binding, ok
}

type PlatformCapabilityDescriptor struct {
	Name             application.CapabilityName        `json:"name"`
	Scope            application.CapabilityScope       `json:"scope"`
	Scopes           []application.CapabilityScope     `json:"scopes"`
	Risk             application.CapabilityRisk        `json:"risk"`
	Approval         application.ApprovalPolicy        `json:"approval"`
	Mutation         bool                              `json:"mutation"`
	Handler          application.CapabilityHandler     `json:"handler"`
	Service          application.CapabilityServiceName `json:"service"`
	Allowed          bool                              `json:"allowed"`
	ApprovalRequired bool                              `json:"approval_required"`
}

// ListForActor is the multi-scope discovery source. Scope remains the first
// required scope for compatibility with the current discovery envelope.
func (r *PlatformCapabilityRegistry) ListForActor(actor Actor) []PlatformCapabilityDescriptor {
	if r == nil {
		return []PlatformCapabilityDescriptor{}
	}
	r.mu.RLock()
	descriptors := make([]PlatformCapabilityDescriptor, 0, len(r.bindings))
	for _, binding := range r.bindings {
		scopes := append([]application.CapabilityScope(nil), binding.definition.Scopes...)
		descriptors = append(descriptors, PlatformCapabilityDescriptor{
			Name: binding.definition.Name, Scope: scopes[0], Scopes: scopes,
			Risk: binding.definition.Risk, Approval: binding.definition.Approval, Mutation: binding.definition.Mutation,
			Handler: binding.definition.Handler, Service: binding.definition.Service,
			Allowed:          actorHasPlatformScopes(actor, scopes),
			ApprovalRequired: binding.definition.Approval == application.ApprovalExplicit,
		})
	}
	r.mu.RUnlock()
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	return descriptors
}

type PlatformApprovalDecision struct {
	validated  bool
	approved   bool
	approvedBy string
}

func NewValidatedPlatformApproval(approvedBy string) (PlatformApprovalDecision, error) {
	approvedBy = strings.TrimSpace(approvedBy)
	if approvedBy == "" || utf8.RuneCountInString(approvedBy) > 200 || hasPlatformControlRune(approvedBy) {
		return PlatformApprovalDecision{}, errors.New("validated approval requires a bounded approver")
	}
	return PlatformApprovalDecision{validated: true, approved: true, approvedBy: approvedBy}, nil
}

type PlatformDispatchRequest struct {
	Capability application.CapabilityName
	Target     json.RawMessage
	Payload    json.RawMessage
	Actor      Actor
	Reason     string
	Approval   PlatformApprovalDecision
	DryRun     bool
}

type PlatformDispatchResult struct {
	Capability application.CapabilityName `json:"capability"`
	Status     string                     `json:"status"`
	Result     any                        `json:"result"`
}

type PlatformDispatchError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	safe      bool
}

func (e *PlatformDispatchError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// DispatchPlatformCapability is the integration point commands.go can call
// after authentication, scope checks and approval-token validation.
func DispatchPlatformCapability(ctx context.Context, registry *PlatformCapabilityRegistry, request PlatformDispatchRequest) (PlatformDispatchResult, error) {
	binding, ok := registry.lookup(request.Capability)
	if !ok {
		return PlatformDispatchResult{}, platformFailure("platform_capability_not_found", "the platform capability is not registered", false)
	}
	authorization, err := derivePlatformAuthorization(binding.definition, request)
	if err != nil {
		return PlatformDispatchResult{}, err
	}
	if !actorHasPlatformScopes(request.Actor, binding.definition.Scopes) {
		return PlatformDispatchResult{}, platformFailure("platform_insufficient_scope", "the automation actor lacks a required platform scope", false)
	}
	target, err := normalizePlatformJSON(request.Target, binding.definition.Limits.MaxTargetBytes, true, "target")
	if err != nil {
		return PlatformDispatchResult{}, err
	}
	payload, err := normalizePlatformJSON(request.Payload, binding.definition.Limits.MaxPayloadBytes, false, "payload")
	if err != nil {
		return PlatformDispatchResult{}, err
	}
	// Central project-scope enforcement: a client provisioned with a
	// project_filter may only target projects inside its group. A target that
	// names a project outside the filter fails closed with a typed out-of-scope
	// error (list capabilities intersect their results downstream via ProjectScope).
	scope := resolveActorProjectScope(ctx, registry.projectScopeStore(), request.Actor)
	if pid := targetProjectID(target); !scope.Allows(pid) {
		return PlatformDispatchResult{}, platformFailure("platform_project_out_of_scope", "the automation actor is not scoped to this project", false)
	}
	prepared, validateErr := binding.executor.Validate(ctx, PlatformExecutionInput{
		Capability: request.Capability, Handler: binding.definition.Handler,
		Scopes: append([]application.CapabilityScope(nil), binding.definition.Scopes...),
		Target: target, Payload: payload, Authorization: authorization,
		ProjectScope: scope,
	})
	if validateErr != nil {
		return PlatformDispatchResult{}, redactPlatformExecutionError(validateErr)
	}
	if prepared == nil {
		return PlatformDispatchResult{}, platformFailure("platform_executor_invalid", "the platform executor returned no validated command", false)
	}
	if request.DryRun {
		return PlatformDispatchResult{
			Capability: request.Capability, Status: "dry_run", Result: prepared.DryRunResult(),
		}, nil
	}
	result, executeErr := prepared.Execute(ctx, authorization)
	if executeErr != nil {
		return PlatformDispatchResult{}, redactPlatformExecutionError(executeErr)
	}
	return PlatformDispatchResult{Capability: request.Capability, Status: "succeeded", Result: result}, nil
}

// dispatchRegisteredPlatformCommand converts the existing command envelope to
// the typed platform dispatcher. commands.go can call it after its current
// authentication, scope and approval-token validation steps.
func dispatchRegisteredPlatformCommand(
	ctx context.Context,
	registry *PlatformCapabilityRegistry,
	command *commandEnvelope,
	actor Actor,
	approval PlatformApprovalDecision,
) (PlatformDispatchResult, error) {
	if command == nil {
		return PlatformDispatchResult{}, platformFailure("platform_command_invalid", "the platform command is required", false)
	}
	target, err := json.Marshal(command.Target)
	if err != nil {
		return PlatformDispatchResult{}, platformFailure("platform_target_invalid", "the command target could not be encoded", false)
	}
	return DispatchPlatformCapability(ctx, registry, PlatformDispatchRequest{
		Capability: command.Capability,
		Target:     target,
		Payload:    command.Payload,
		Actor:      actor,
		Reason:     command.Reason,
		Approval:   approval,
		DryRun:     command.DryRun,
	})
}

func normalizePlatformDefinition(definition PlatformCapabilityDefinition) (PlatformCapabilityDefinition, error) {
	definition.Name = application.CapabilityName(strings.TrimSpace(string(definition.Name)))
	definition.Handler = application.CapabilityHandler(strings.TrimSpace(string(definition.Handler)))
	definition.Service = application.CapabilityServiceName(strings.TrimSpace(string(definition.Service)))
	if definition.Risk != application.CapabilityRiskRead {
		definition.Mutation = true
	}
	if !platformIdentifierPattern.MatchString(string(definition.Name)) {
		return PlatformCapabilityDefinition{}, errors.New("platform capability name is invalid")
	}
	if len(definition.Scopes) == 0 || len(definition.Scopes) > maxPlatformScopes {
		return PlatformCapabilityDefinition{}, errors.New("platform capability must declare between 1 and 16 scopes")
	}
	normalizedScopes := make([]application.CapabilityScope, 0, len(definition.Scopes))
	seenScopes := make(map[application.CapabilityScope]struct{}, len(definition.Scopes))
	for _, scope := range definition.Scopes {
		scope = application.CapabilityScope(strings.TrimSpace(string(scope)))
		if !platformIdentifierPattern.MatchString(string(scope)) {
			return PlatformCapabilityDefinition{}, errors.New("platform capability scope is invalid")
		}
		if _, exists := seenScopes[scope]; exists {
			continue
		}
		seenScopes[scope] = struct{}{}
		normalizedScopes = append(normalizedScopes, scope)
	}
	definition.Scopes = normalizedScopes
	if !platformIdentifierPattern.MatchString(string(definition.Handler)) {
		return PlatformCapabilityDefinition{}, errors.New("platform capability handler is invalid")
	}
	if !platformIdentifierPattern.MatchString(string(definition.Service)) {
		return PlatformCapabilityDefinition{}, errors.New("platform capability service is invalid")
	}
	if definition.Limits.MaxTargetBytes == 0 {
		definition.Limits.MaxTargetBytes = defaultPlatformTargetBytes
	}
	if definition.Limits.MaxPayloadBytes == 0 {
		definition.Limits.MaxPayloadBytes = defaultPlatformPayloadBytes
	}
	if definition.Limits.MaxTargetBytes < 2 || definition.Limits.MaxTargetBytes > hardPlatformTargetBytes {
		return PlatformCapabilityDefinition{}, errors.New("platform target limit is invalid")
	}
	if definition.Limits.MaxPayloadBytes < 2 || definition.Limits.MaxPayloadBytes > hardPlatformPayloadBytes {
		return PlatformCapabilityDefinition{}, errors.New("platform payload limit is invalid")
	}
	return definition, nil
}

func platformMutation(definition PlatformCapabilityDefinition) PlatformCapabilityDefinition {
	definition.Mutation = true
	return definition
}

func (r *PlatformCapabilityRegistry) IsMutation(name application.CapabilityName) bool {
	binding, ok := r.lookup(name)
	return ok && binding.definition.Mutation
}

func actorHasPlatformScopes(actor Actor, scopes []application.CapabilityScope) bool {
	for _, scope := range scopes {
		if !actor.Scopes.Has(Scope(scope)) {
			return false
		}
	}
	return true
}

func derivePlatformAuthorization(definition PlatformCapabilityDefinition, request PlatformDispatchRequest) (application.ActionAuthorization, error) {
	actorID := strings.TrimSpace(request.Actor.ClientID)
	if actorID == "" {
		actorID = strings.TrimSpace(request.Actor.ID)
	}
	actorType := strings.TrimSpace(request.Actor.Type)
	if actorType == "" || actorID == "" || len(actorType) > 200 || len(actorID) > 200 ||
		hasPlatformControlRune(actorType) || hasPlatformControlRune(actorID) {
		return application.ActionAuthorization{}, platformFailure("platform_actor_invalid", "an authenticated bounded actor is required", false)
	}
	reason := strings.TrimSpace(request.Reason)
	if len([]rune(reason)) > maxPlatformReasonRunes || strings.IndexByte(reason, 0) >= 0 {
		return application.ActionAuthorization{}, platformFailure("platform_reason_invalid", "the command reason exceeds 2000 characters", false)
	}
	if !request.DryRun && (definition.Risk == application.CapabilityRiskDestructive || definition.Risk == application.CapabilityRiskUnsafe) && reason == "" {
		return application.ActionAuthorization{}, platformFailure("platform_reason_required", "a reason is required for destructive or unsafe capabilities", false)
	}
	approvalRequired := definition.Approval == application.ApprovalByPolicy || definition.Approval == application.ApprovalExplicit
	if !request.DryRun && approvalRequired && (!request.Approval.validated || !request.Approval.approved || request.Approval.approvedBy == "") {
		return application.ActionAuthorization{}, platformFailure("platform_approval_required", "a validated approval decision is required", false)
	}
	return application.ActionAuthorization{
		Actor:      application.Actor{Type: actorType, ID: actorID},
		Reason:     reason,
		Approved:   request.Approval.validated && request.Approval.approved,
		ApprovedBy: request.Approval.approvedBy,
	}, nil
}

func hasPlatformControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func normalizePlatformJSON(raw json.RawMessage, maxBytes int, objectOnly bool, field string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte(`{}`)
	}
	if len(trimmed) > maxBytes {
		return nil, platformFailure("platform_"+field+"_too_large", "the command "+field+" exceeds its bounded limit", false)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, platformFailure("platform_"+field+"_invalid", "the command "+field+" must be valid JSON", false)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, platformFailure("platform_"+field+"_invalid", "the command "+field+" must contain one JSON value", false)
	}
	switch value.(type) {
	case map[string]any:
	case []any:
		if objectOnly {
			return nil, platformFailure("platform_"+field+"_invalid", "the command "+field+" must be a JSON object", false)
		}
	default:
		return nil, platformFailure("platform_"+field+"_invalid", "the command "+field+" must be a JSON object or array", false)
	}
	if platformJSONDepth(value) > maxPlatformJSONDepth {
		return nil, platformFailure("platform_"+field+"_invalid", "the command "+field+" exceeds the JSON nesting limit", false)
	}
	normalized, err := json.Marshal(value)
	if err != nil || len(normalized) > maxBytes {
		return nil, platformFailure("platform_"+field+"_invalid", "the command "+field+" could not be normalized", false)
	}
	return json.RawMessage(normalized), nil
}

func platformJSONDepth(value any) int {
	type pendingValue struct {
		value any
		depth int
	}
	maximum := 0
	pending := []pendingValue{{value: value, depth: 0}}
	for len(pending) > 0 {
		last := len(pending) - 1
		item := pending[last]
		pending = pending[:last]
		switch typed := item.value.(type) {
		case map[string]any:
			depth := item.depth + 1
			maximum = max(maximum, depth)
			for _, child := range typed {
				pending = append(pending, pendingValue{value: child, depth: depth})
			}
		case []any:
			depth := item.depth + 1
			maximum = max(maximum, depth)
			for _, child := range typed {
				pending = append(pending, pendingValue{value: child, depth: depth})
			}
		}
	}
	return maximum
}

func redactPlatformExecutionError(err error) error {
	if err == nil {
		return nil
	}
	var platformErr *PlatformDispatchError
	if errors.As(err, &platformErr) && platformErr.safe {
		return &PlatformDispatchError{Code: platformErr.Code, Message: platformErr.Message, Retryable: platformErr.Retryable, safe: true}
	}
	var applicationErr *application.Error
	if errors.As(err, &applicationErr) {
		return &PlatformDispatchError{Code: applicationErr.Code, Message: applicationErr.Message, Retryable: false}
	}
	if errors.Is(err, context.Canceled) {
		return platformFailure("platform_command_canceled", "the platform command was canceled", true)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return platformFailure("platform_command_timeout", "the platform command timed out", true)
	}
	return platformFailure("platform_execution_failed", "the platform command could not be completed", true)
}

func platformFailure(code, message string, retryable bool) *PlatformDispatchError {
	return &PlatformDispatchError{Code: code, Message: message, Retryable: retryable, safe: true}
}
