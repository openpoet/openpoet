package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type collaborationAIReadFake struct {
	conversations []database.AIConversation
	messages      []database.AIMessage
	suggestions   []database.AISuggestion
	unread        int
	err           error
}

func (f *collaborationAIReadFake) ListAIConversations(context.Context) ([]database.AIConversation, error) {
	return f.conversations, f.err
}
func (f *collaborationAIReadFake) GetAIConversation(_ context.Context, id int64) (*database.AIConversation, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.conversations {
		if f.conversations[i].ID == id {
			item := f.conversations[i]
			return &item, nil
		}
	}
	return nil, nil
}
func (f *collaborationAIReadFake) ListAIMessages(context.Context, int64) ([]database.AIMessage, error) {
	return f.messages, f.err
}
func (f *collaborationAIReadFake) ListPendingAISuggestions(context.Context) ([]database.AISuggestion, error) {
	return f.suggestions, f.err
}
func (f *collaborationAIReadFake) GetAISuggestion(_ context.Context, id int64) (*database.AISuggestion, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.suggestions {
		if f.suggestions[i].ID == id {
			item := f.suggestions[i]
			return &item, nil
		}
	}
	return nil, nil
}
func (f *collaborationAIReadFake) CountUnreadProactive(context.Context) (int, error) {
	return f.unread, f.err
}

type collaborationTokenReadFake struct {
	summary *database.TokenUsageSummary
	err     error
}

func (f *collaborationTokenReadFake) GetTokenUsageSummary(context.Context, time.Time, *int64) (*database.TokenUsageSummary, error) {
	if f.summary == nil && f.err == nil {
		return &database.TokenUsageSummary{}, nil
	}
	return f.summary, f.err
}

func collaborationTestServices() CollaborationPlatformServices {
	return CollaborationPlatformServices{
		Documents:            application.NewDocumentService(nil, nil, nil),
		Proposals:            application.NewProposalService(nil, nil),
		AI:                   application.NewAIAssistantService(nil, nil, nil, nil),
		Notifications:        application.NewNotificationService(nil),
		NotificationDelivery: application.NewNotificationDeliveryService(nil, nil),
		TokenUsage:           application.NewTokenUsageService(nil, nil),
		AIQueries:            &collaborationAIReadFake{},
		TokenUsageQueries:    &collaborationTokenReadFake{},
	}
}

func collaborationTestRegistry(t *testing.T, services CollaborationPlatformServices) (*application.CapabilityRegistry, *PlatformCapabilityRegistry) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err = RegisterCollaborationPlatformCapabilities(registry, services); err != nil {
		t.Fatal(err)
	}
	return capabilities, registry
}

func collaborationDefinitionsForTest() []PlatformCapabilityDefinition {
	groups := [][]PlatformCapabilityDefinition{
		documentCollaborationDefinitions(), proposalCollaborationDefinitions(), aiCollaborationDefinitions(),
		notificationCollaborationDefinitions(), notificationDeliveryCollaborationDefinitions(), tokenUsageCollaborationDefinitions(),
	}
	var definitions []PlatformCapabilityDefinition
	for _, group := range groups {
		definitions = append(definitions, group...)
	}
	return definitions
}

func collaborationActor(definitions []PlatformCapabilityDefinition) Actor {
	scopes := ScopeSet{}
	for _, definition := range definitions {
		for _, scope := range definition.Scopes {
			scopes[Scope(scope)] = struct{}{}
		}
	}
	return Actor{Type: "automation_client", ID: "helena", ClientID: "helena", Name: "Helena", Scopes: scopes}
}

func collaborationApproval(t *testing.T) PlatformApprovalDecision {
	t.Helper()
	approval, err := NewValidatedPlatformApproval("presidente")
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func TestCollaborationPlatformRegistersCompleteUniqueSurface(t *testing.T) {
	definitions := collaborationDefinitionsForTest()
	if len(definitions) != 48 {
		t.Fatalf("collaboration surface has %d capabilities, want 48", len(definitions))
	}
	seen := make(map[application.CapabilityName]struct{}, len(definitions))
	for _, definition := range definitions {
		if _, duplicate := seen[definition.Name]; duplicate {
			t.Fatalf("duplicate collaboration capability %q", definition.Name)
		}
		seen[definition.Name] = struct{}{}
	}
	capabilities, registry := collaborationTestRegistry(t, collaborationTestServices())
	if len(capabilities.List()) != len(definitions) || len(registry.ListForActor(collaborationActor(definitions))) != len(definitions) {
		t.Fatal("collaboration capabilities were not registered in both registries")
	}

	broken := collaborationTestServices()
	broken.AIQueries = nil
	appRegistry := application.NewCapabilityRegistry()
	platformRegistry, err := NewPlatformCapabilityRegistry(appRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if err = RegisterCollaborationPlatformCapabilities(platformRegistry, broken); err == nil || len(appRegistry.List()) != 0 {
		t.Fatal("missing collaboration port was accepted or partially registered")
	}
}

type collaborationManifest struct {
	Routes []struct {
		Capability         application.CapabilityName `json:"capability"`
		Risk               string                     `json:"risk"`
		Scopes             []string                   `json:"scopes"`
		ApplicationService string                     `json:"application_service"`
	} `json:"routes"`
}

func TestCollaborationPlatformMutationMetadataMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "automation", "ui-action-manifest.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest collaborationManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	definitions := make(map[application.CapabilityName]PlatformCapabilityDefinition)
	for _, definition := range collaborationDefinitionsForTest() {
		definitions[definition.Name] = definition
	}
	wantedServices := map[string]string{
		"DocumentService": "documents", "ProposalService": "proposals", "AIAssistantService": "ai_assistant",
		"NotificationService": "notifications", "NotificationDeliveryService": "notification_delivery", "TokenUsageService": "token_usage",
	}
	riskMetadata := map[string]struct {
		risk     application.CapabilityRisk
		approval application.ApprovalPolicy
	}{
		"R1": {application.CapabilityRiskRead, application.ApprovalNone},
		"R2": {application.CapabilityRiskWrite, application.ApprovalByPolicy},
		"R3": {application.CapabilityRiskDestructive, application.ApprovalExplicit},
		"R4": {application.CapabilityRiskUnsafe, application.ApprovalExplicit},
	}
	counts := map[string]int{}
	checked := 0
	for _, route := range manifest.Routes {
		expectedService, wanted := wantedServices[route.ApplicationService]
		if !wanted {
			continue
		}
		definition, exists := definitions[route.Capability]
		if !exists {
			t.Errorf("manifest mutation %q has no collaboration adapter", route.Capability)
			continue
		}
		expected := riskMetadata[route.Risk]
		gotScopes := make([]string, len(definition.Scopes))
		for i, scope := range definition.Scopes {
			gotScopes[i] = string(scope)
		}
		if !reflect.DeepEqual(gotScopes, route.Scopes) || definition.Risk != expected.risk || definition.Approval != expected.approval ||
			definition.Handler != application.CapabilityHandler(route.Capability) || string(definition.Service) != expectedService {
			t.Errorf("metadata mismatch for %q: %#v, scopes=%v", route.Capability, definition, gotScopes)
		}
		if !definition.Mutation {
			t.Errorf("manifest mutation %q is not marked mutation", route.Capability)
		}
		counts[route.ApplicationService]++
		checked++
	}
	wantCounts := map[string]int{"DocumentService": 2, "ProposalService": 9, "AIAssistantService": 17, "NotificationService": 2, "NotificationDeliveryService": 4, "TokenUsageService": 1}
	if checked != 35 || !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("checked mutations=%d counts=%v, want 35/%v", checked, counts, wantCounts)
	}
}

func TestCollaborationOperationalReadsUseDedicatedLeastPrivilegeScopes(t *testing.T) {
	want := map[string]string{
		"documents.get_memory": "documents:read", "documents.get_temp": "documents:read",
		"ai.conversations.list": "ai:read", "ai.conversations.get": "ai:read", "ai.messages.list": "ai:read",
		"ai.suggestions.list": "ai:read", "ai.suggestions.get": "ai:read", "ai.status": "ai:read",
		"notifications.list": "notifications:read", "notifications.active": "notifications:read",
		"notifications.unread_count": "notifications:read", "notifications.preference": "notifications:read",
		"token_usage.summary": "token_usage:read",
	}
	got := map[string]string{}
	for _, definition := range collaborationDefinitionsForTest() {
		if scope, exists := want[string(definition.Name)]; exists {
			if definition.Risk != application.CapabilityRiskRead || definition.Approval != application.ApprovalNone || definition.Mutation || len(definition.Scopes) != 1 {
				t.Errorf("read %s is not R1/none/single-scope: %#v", definition.Name, definition)
			}
			got[string(definition.Name)] = string(definition.Scopes[0])
			if string(definition.Scopes[0]) != scope {
				t.Errorf("read %s scope=%s want=%s", definition.Name, definition.Scopes[0], scope)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read surface mismatch: got=%v want=%v", got, want)
	}
}

type collaborationDryRunCase struct {
	capability string
	target     string
	payload    string
}

func collaborationMutationDryRunCases() []collaborationDryRunCase {
	proposal := `{"id":"proposal-1"}`
	conversation := `{"id":41}`
	suggestion := `{"id":9}`
	return []collaborationDryRunCase{
		{"documents.update_memory", `{"id":1}`, `{"content":"document-secret-content","summary":"summary"}`},
		{"documents.create_temp", `{}`, `{"title":"Doc","content":"temporary-secret-content"}`},
		{"proposals.memory.create", `{"id":1}`, `{"content":"proposal-secret-content"}`},
		{"proposals.memory.approve", proposal, `{}`}, {"proposals.memory.reject", proposal, `{}`},
		{"proposals.task.approve", proposal, `{}`}, {"proposals.task.reject", proposal, `{}`},
		{"proposals.skill.approve", proposal, `{}`}, {"proposals.skill.reject", proposal, `{}`},
		{"proposals.tool.approve", proposal, `{}`}, {"proposals.tool.reject", proposal, `{}`},
		{"notifications.mark_read", `{"id":1}`, `{}`}, {"notifications.mark_all_read", `{}`, `{}`},
		{"notifications.subscribe_push", `{}`, `{"endpoint":"https://push.example/sub?token=endpoint-secret","p256dh":"p256-secret","auth":"auth-secret"}`},
		{"notifications.unsubscribe_push", `{}`, `{"endpoint":"https://push.example/sub?token=endpoint-secret"}`},
		{"notifications.test_push", `{}`, `{}`}, {"notifications.update_preference", `{}`, `{"disabled":true}`},
		{"ai.test_connection", `{}`, `{"provider_type":"anthropic","api_key":"sk-0123456789abcdefghijklmnop"}`},
		{"ai.chat", `{}`, `{"project_id":1,"prompt":"prompt-secret-content"}`},
		{"ai.delete_all_conversations", `{}`, `{}`}, {"ai.delete_conversation", conversation, `{}`},
		{"ai.stop_conversation", conversation, `{}`}, {"ai.initiate_memory_edit", `{"id":1}`, `{}`},
		{"ai.initiate_task_creation", `{"id":1}`, `{"title":"Task","description":"description-secret"}`},
		{"ai.initiate_task_discussion", `{"id":9,"project_id":1}`, `{}`},
		{"ai.initiate_skill_customization", `{"id":7,"project_id":1}`, `{}`},
		{"ai.generate_skill", `{}`, `{"description":"generate-secret-description"}`},
		{"ai.validate_skill", `{}`, `{"content":"validate-secret-content"}`},
		{"ai.execute_tool", `{}`, `{"name":"shell.run","arguments":{"command":"echo tool-secret"},"conversation_id":41}`},
		{"ai.accept_suggestion", suggestion, `{}`}, {"ai.dismiss_suggestion", suggestion, `{}`},
		{"ai.discuss_suggestion", suggestion, `{}`}, {"ai.mark_conversation_read", conversation, `{}`},
		{"ai.test_proactive", `{}`, `{"level":"standard"}`}, {"token_usage.clear", `{}`, `{}`},
	}
}

func TestCollaborationEveryManifestMutationDryRunsWithoutEffectOrSecretEcho(t *testing.T) {
	definitions := collaborationDefinitionsForTest()
	_, registry := collaborationTestRegistry(t, collaborationTestServices())
	actor := collaborationActor(definitions)
	cases := collaborationMutationDryRunCases()
	if len(cases) != 35 {
		t.Fatalf("dry-run cases=%d want=35", len(cases))
	}
	seen := map[string]struct{}{}
	for _, testCase := range cases {
		result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
			Capability: application.CapabilityName(testCase.capability), Target: json.RawMessage(testCase.target),
			Payload: json.RawMessage(testCase.payload), Actor: actor, DryRun: true,
		})
		if err != nil {
			t.Errorf("dry-run %s failed: %v", testCase.capability, err)
			continue
		}
		encoded, _ := json.Marshal(result)
		for _, secret := range []string{"document-secret-content", "temporary-secret-content", "proposal-secret-content", "p256-secret", "auth-secret", "endpoint-secret", "sk-0123456789abcdefghijklmnop", "prompt-secret-content", "description-secret", "generate-secret-description", "validate-secret-content", "echo tool-secret"} {
			if strings.Contains(string(encoded), secret) {
				t.Errorf("dry-run %s echoed secret %q: %s", testCase.capability, secret, encoded)
			}
		}
		if result.Status != "dry_run" {
			t.Errorf("dry-run %s status=%s", testCase.capability, result.Status)
		}
		seen[testCase.capability] = struct{}{}
	}
	if len(seen) != 35 {
		t.Fatalf("dry-run covered %d unique mutations", len(seen))
	}
}

func TestCollaborationMultiScopeAndExplicitRiskBoundaries(t *testing.T) {
	definitions := collaborationDefinitionsForTest()
	_, registry := collaborationTestRegistry(t, collaborationTestServices())
	actor := collaborationActor(definitions)
	delete(actor.Scopes, ScopeCredentialsUse)
	_, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: "ai.test_connection", Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{"provider_type":"anthropic"}`), Actor: actor, DryRun: true,
	})
	if dispatchCode(err) != "platform_insufficient_scope" {
		t.Fatalf("AI multi-scope accepted missing credentials:use: %v", err)
	}

	actor = collaborationActor(definitions)
	for _, testCase := range collaborationMutationDryRunCases() {
		definition := findCollaborationDefinition(testCase.capability)
		if definition.Risk != application.CapabilityRiskDestructive && definition.Risk != application.CapabilityRiskUnsafe {
			continue
		}
		_, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
			Capability: definition.Name, Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
			Actor: actor, Reason: "explicit operation",
		})
		if dispatchCode(err) != "platform_approval_required" {
			t.Errorf("%s did not fail at explicit approval boundary: %v", definition.Name, err)
		}
	}

	for _, capability := range []string{"ai.execute_tool", "proposals.tool.approve"} {
		definition := findCollaborationDefinition(capability)
		if definition.Risk != application.CapabilityRiskUnsafe || definition.Approval != application.ApprovalExplicit {
			t.Errorf("%s is not R4 explicit: %#v", capability, definition)
		}
	}
	for _, capability := range []string{"ai.delete_all_conversations", "ai.delete_conversation", "notifications.unsubscribe_push", "token_usage.clear", "proposals.task.approve"} {
		definition := findCollaborationDefinition(capability)
		if definition.Risk != application.CapabilityRiskDestructive || definition.Approval != application.ApprovalExplicit {
			t.Errorf("%s is not R3 explicit: %#v", capability, definition)
		}
	}
}

type collaborationDocumentStoreFake struct{}

func (collaborationDocumentStoreFake) GetProject(context.Context, int64) (*database.Project, error) {
	return &database.Project{ID: 1, Name: "Project"}, nil
}
func (collaborationDocumentStoreFake) GetMemoryDoc(context.Context, int64) (*database.MemoryDoc, error) {
	return &database.MemoryDoc{ProjectID: 1, Content: "api_key=document-secret " + strings.Repeat("x", 90<<10), Summary: sql.NullString{String: "token=summary-secret", Valid: true}, Version: 3}, nil
}
func (collaborationDocumentStoreFake) UpsertMemoryDoc(context.Context, int64, string, string, string) (*database.MemoryDoc, error) {
	return nil, errors.New("not used")
}
func (collaborationDocumentStoreFake) CreateTempDocument(context.Context, *database.TempDocument) error {
	return errors.New("not used")
}
func (collaborationDocumentStoreFake) GetTempDocument(context.Context, string) (*database.TempDocument, error) {
	return nil, errors.New("not used")
}

type collaborationNotificationBackendFake struct {
	items []database.Notification
	err   error
}

func (f *collaborationNotificationBackendFake) GetRecent(context.Context, int) ([]database.Notification, error) {
	return f.items, f.err
}
func (f *collaborationNotificationBackendFake) GetActive(context.Context) ([]database.Notification, error) {
	return f.items, f.err
}
func (f *collaborationNotificationBackendFake) GetUnreadCount(context.Context) (int, error) {
	return len(f.items), f.err
}
func (f *collaborationNotificationBackendFake) MarkRead(context.Context, int64) error { return f.err }
func (f *collaborationNotificationBackendFake) MarkAllRead(context.Context) error     { return f.err }

func TestCollaborationReadViewsAreBoundedAndRedacted(t *testing.T) {
	aiReads := &collaborationAIReadFake{
		messages:      []database.AIMessage{{ID: 1, ConversationID: 41, Role: "assistant", Content: "password=message-secret " + strings.Repeat("y", 40<<10), ToolCalls: `[{"arguments":{"token":"tool-call-secret"}}]`, Status: "completed"}},
		conversations: []database.AIConversation{{ID: 41, Title: "api_key=title-secret", ProactiveContext: `{"token":"context-secret"}`, SessionID: "session-secret", Source: "ai"}},
		suggestions:   []database.AISuggestion{{ID: 9, ProjectID: 1, Title: "secret=suggestion-secret", ContextJSON: `{"token":"context-secret"}`, Status: "pending"}},
	}
	notifications := &collaborationNotificationBackendFake{items: []database.Notification{{ID: 1, Type: "info", Title: "api_key=notification-secret", Body: "token=body-secret", Link: "https://example.test/path?token=link-secret#fragment"}}}
	services := collaborationTestServices()
	services.Documents = application.NewDocumentService(collaborationDocumentStoreFake{}, nil, nil)
	services.AIQueries = aiReads
	services.Notifications = application.NewNotificationService(notifications)
	_, registry := collaborationTestRegistry(t, services)
	actor := collaborationActor(collaborationDefinitionsForTest())
	cases := []PlatformDispatchRequest{
		{Capability: "documents.get_memory", Target: json.RawMessage(`{"id":1}`), Payload: json.RawMessage(`{}`), Actor: actor},
		{Capability: "ai.conversations.list", Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{}`), Actor: actor},
		{Capability: "ai.messages.list", Target: json.RawMessage(`{"id":41}`), Payload: json.RawMessage(`{"max_bytes":32768}`), Actor: actor},
		{Capability: "ai.suggestions.list", Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{}`), Actor: actor},
		{Capability: "notifications.list", Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{}`), Actor: actor},
	}
	for _, request := range cases {
		result, err := DispatchPlatformCapability(context.Background(), registry, request)
		if err != nil {
			t.Fatalf("read %s failed: %v", request.Capability, err)
		}
		encoded, _ := json.Marshal(result.Result)
		if len(encoded) > 140<<10 {
			t.Errorf("read %s result is not bounded: %d", request.Capability, len(encoded))
		}
		for _, secret := range []string{"document-secret", "summary-secret", "message-secret", "tool-call-secret", "title-secret", "context-secret", "session-secret", "suggestion-secret", "notification-secret", "body-secret", "link-secret"} {
			if strings.Contains(string(encoded), secret) {
				t.Errorf("read %s leaked %q: %s", request.Capability, secret, encoded)
			}
		}
	}
}

type collaborationToolFake struct {
	calls int
}

func (f *collaborationToolFake) ExecuteAITool(_ context.Context, request application.AIToolExecutionRequest) (application.AIToolExecutionResult, error) {
	f.calls++
	return application.AIToolExecutionResult{Output: "api_key=tool-output-secret " + strings.Repeat("z", 90<<10), ExitCode: 0}, nil
}

func TestCollaborationApprovedToolExecutionUsesApplicationServiceAndRedactsOutput(t *testing.T) {
	tool := &collaborationToolFake{}
	services := collaborationTestServices()
	services.AI = application.NewAIAssistantService(nil, nil, tool, nil)
	_, registry := collaborationTestRegistry(t, services)
	result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: "ai.execute_tool", Target: json.RawMessage(`{}`),
		Payload: json.RawMessage(`{"name":"safe.fake","arguments":{"value":"input"},"conversation_id":41}`),
		Actor:   collaborationActor(collaborationDefinitionsForTest()), Reason: "president approved fake tool",
		Approval: collaborationApproval(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.Result)
	if tool.calls != 1 || strings.Contains(string(encoded), "tool-output-secret") || len(encoded) > 70<<10 {
		t.Fatalf("tool execution was not direct/redacted/bounded: calls=%d result=%s", tool.calls, encoded)
	}
}

func TestCollaborationExternalErrorsAreRedacted(t *testing.T) {
	services := collaborationTestServices()
	services.Notifications = application.NewNotificationService(&collaborationNotificationBackendFake{err: errors.New("api_key=external-backend-secret")})
	_, registry := collaborationTestRegistry(t, services)
	_, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: "notifications.list", Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{}`), Actor: collaborationActor(collaborationDefinitionsForTest()),
	})
	if dispatchCode(err) != "platform_execution_failed" || strings.Contains(err.Error(), "external-backend-secret") {
		t.Fatalf("external error was not safely redacted: %v", err)
	}
}

func findCollaborationDefinition(name string) PlatformCapabilityDefinition {
	for _, definition := range collaborationDefinitionsForTest() {
		if string(definition.Name) == name {
			return definition
		}
	}
	return PlatformCapabilityDefinition{}
}

func dispatchCode(err error) string {
	var dispatchErr *PlatformDispatchError
	if errors.As(err, &dispatchErr) {
		return dispatchErr.Code
	}
	return ""
}

func TestCollaborationReadSurfaceNamesAreStable(t *testing.T) {
	var names []string
	for _, definition := range collaborationDefinitionsForTest() {
		if definition.Risk == application.CapabilityRiskRead && strings.Contains(string(definition.Name), ".") {
			if strings.HasPrefix(string(definition.Name), "documents.get_") || strings.HasPrefix(string(definition.Name), "ai.conversations.") ||
				strings.HasPrefix(string(definition.Name), "ai.messages.") || strings.HasPrefix(string(definition.Name), "ai.suggestions.") ||
				definition.Name == "ai.status" || strings.HasPrefix(string(definition.Name), "notifications.") && string(definition.Scopes[0]) == "notifications:read" ||
				definition.Name == "token_usage.summary" {
				names = append(names, string(definition.Name))
			}
		}
	}
	sort.Strings(names)
	if len(names) != 13 {
		t.Fatalf("operational read names=%v, want 13", names)
	}
}
