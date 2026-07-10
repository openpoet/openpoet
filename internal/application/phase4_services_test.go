package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openpoet/internal/database"
)

func phase4Authorization() ActionAuthorization {
	return ActionAuthorization{Actor: Actor{Type: "automation", ID: "helena"}}
}

func phase4ApprovedAuthorization() ActionAuthorization {
	authorization := phase4Authorization()
	authorization.Approved = true
	authorization.ApprovedBy = "presidente"
	authorization.Reason = "phase 4 service test"
	return authorization
}

type recordingMemoryMirror struct {
	calls   int
	content string
}

func (r *recordingMemoryMirror) MirrorMemoryDocument(_ context.Context, _ *database.Project, content string) {
	r.calls++
	r.content = content
}

func TestPhase4DocumentsAreBoundedRedactedAndEffectsArePostCommit(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "phase4-documents")
	effects := &recordingApplicationEffects{}
	mirror := &recordingMemoryMirror{}
	service := NewDocumentService(db, mirror, effects)

	memory, err := service.UpdateMemory(ctx, UpdateMemoryDocumentCommand{
		ProjectID: project.ID, Content: "Architecture\napi_key=memory-secret", Summary: "password=summary-secret",
		Authorization: phase4Authorization(),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(memory)
	if strings.Contains(string(encoded), "memory-secret") || strings.Contains(string(encoded), "summary-secret") || mirror.calls != 1 || strings.Contains(mirror.content, "memory-secret") {
		t.Fatalf("memory document leaked secret or mirror was not post-commit: view=%s mirror=%q calls=%d", encoded, mirror.content, mirror.calls)
	}
	stored, err := db.GetMemoryDoc(ctx, project.ID)
	if err != nil || strings.Contains(stored.Content, "memory-secret") || stored.LastUpdatedBy != "automation:helena" {
		t.Fatalf("unsafe memory state: doc=%+v err=%v", stored, err)
	}

	temp, err := service.CreateTemp(ctx, CreateTempDocumentCommand{
		Title: "Plan", Content: "Safe body with access_token=temporary-secret", Authorization: phase4Authorization(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if temp.Content != "" {
		t.Fatal("create response exposed raw document content")
	}
	persisted, err := db.GetTempDocument(ctx, temp.ID)
	if err != nil || strings.Contains(persisted.Content, "temporary-secret") {
		t.Fatalf("temp document secret was persisted: doc=%+v err=%v", persisted, err)
	}
	view, err := service.GetTemp(ctx, temp.ID)
	if err != nil || strings.Contains(view.Content, "temporary-secret") {
		t.Fatalf("temp document view leaked secret: view=%+v err=%v", view, err)
	}
	if len(effects.changes) != 2 {
		t.Fatalf("document effects=%+v", effects.changes)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_memory_update BEFORE UPDATE ON memory_docs BEGIN SELECT RAISE(ABORT, 'memory unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateMemory(ctx, UpdateMemoryDocumentCommand{ProjectID: project.ID, Content: "will fail", Authorization: phase4Authorization()}); err == nil {
		t.Fatal("expected memory transaction failure")
	}
	if mirror.calls != 1 || len(effects.changes) != 2 {
		t.Fatalf("failed memory commit emitted post-commit effects: mirror=%d effects=%+v", mirror.calls, effects.changes)
	}
	if _, err := service.CreateTemp(ctx, CreateTempDocumentCommand{Content: strings.Repeat("x", maxApplicationContentRunes+1), Authorization: phase4Authorization()}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("expected bounded-content validation, got %v", err)
	}
}

type fakeProposalBackend struct {
	records    map[string]*ProposalRecord
	failAccept bool
	acceptance ProposalAcceptance
}

func newFakeProposalBackend() *fakeProposalBackend {
	return &fakeProposalBackend{records: map[string]*ProposalRecord{}}
}

func (b *fakeProposalBackend) CreateMemoryProposalAtomic(_ context.Context, command MemoryProposal) (*ProposalRecord, error) {
	record := &ProposalRecord{ID: "memory-1", Kind: ProposalMemory, Risk: ProposalRiskR2, Status: "pending", Title: "Memory", Summary: command.Summary, ProjectID: command.ProjectID, ConversationID: command.ConversationID}
	b.records[record.ID] = record
	return record, nil
}

func (b *fakeProposalBackend) GetPendingProposal(_ context.Context, id string, kind ProposalKind) (*ProposalRecord, error) {
	record := b.records[id]
	if record == nil || record.Kind != kind {
		return nil, errors.New("not found")
	}
	copy := *record
	return &copy, nil
}

func (b *fakeProposalBackend) AcceptProposalAtomic(_ context.Context, id string, kind ProposalKind, _ ActionAuthorization) (ProposalAcceptance, error) {
	if b.failAccept {
		return ProposalAcceptance{}, errors.New("transaction failed with api_key=backend-secret")
	}
	b.records[id].Status = "approved"
	return b.acceptance, nil
}

func (b *fakeProposalBackend) RejectProposalAtomic(_ context.Context, id string, kind ProposalKind, _ ActionAuthorization) error {
	b.records[id].Status = "rejected"
	return nil
}

func TestPhase4ProposalApprovalRequiresRiskBoundaryAndPublishesAfterAtomicAccept(t *testing.T) {
	ctx := context.Background()
	backend := newFakeProposalBackend()
	effects := &recordingApplicationEffects{}
	service := NewProposalService(backend, effects)

	created, err := service.ProposeMemory(ctx, MemoryProposal{ProjectID: 7, Content: "api_key=proposal-secret\nbody", Summary: "safe", Authorization: phase4Authorization()})
	if err != nil || created.ID == "" {
		t.Fatalf("proposal create failed: view=%+v err=%v", created, err)
	}
	backend.failAccept = true
	if _, err = service.ApproveMemory(ctx, created.ID, phase4Authorization()); err == nil || strings.Contains(err.Error(), "backend-secret") {
		t.Fatal("expected atomic proposal accept failure")
	}
	if backend.records[created.ID].Status != "pending" || len(effects.changes) != 1 {
		t.Fatalf("failed proposal transaction leaked status/effect: record=%+v effects=%+v", backend.records[created.ID], effects.changes)
	}
	backend.failAccept = false
	backend.acceptance = ProposalAcceptance{Status: "approved", Message: "done password=message-secret"}
	accepted, err := service.ApproveMemory(ctx, created.ID, phase4Authorization())
	if err != nil || strings.Contains(accepted.Message, "message-secret") || backend.records[created.ID].Status != "approved" || len(effects.changes) != 2 {
		t.Fatalf("proposal accept contract failed: accepted=%+v record=%+v effects=%+v err=%v", accepted, backend.records[created.ID], effects.changes, err)
	}

	backend.records["tool-1"] = &ProposalRecord{ID: "tool-1", Kind: ProposalTool, Risk: ProposalRiskR4, Status: "pending"}
	if _, err = service.ApproveTool(ctx, "tool-1", phase4Authorization()); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("tool proposal accepted without explicit R4 approval: %v", err)
	}
	backend.acceptance = ProposalAcceptance{Status: "approved", Output: strings.Repeat("x", maxApplicationOutputRunes+10) + " secret=tool-output-secret"}
	toolResult, err := service.ApproveTool(ctx, "tool-1", phase4ApprovedAuthorization())
	if err != nil || !toolResult.WasTruncated || strings.Contains(toolResult.Output, "tool-output-secret") {
		t.Fatalf("tool proposal output was not bounded/redacted: result=%+v err=%v", toolResult, err)
	}
	backend.records["task-1"] = &ProposalRecord{ID: "task-1", Kind: ProposalTask, Risk: ProposalRiskR2, Status: "pending"}
	if _, err = service.ApproveTask(ctx, "task-1", phase4Authorization()); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("task batch proposal bypassed R3 approval: %v", err)
	}
}

type fakeAIProvider struct {
	connectionRequest AIConnectionTestRequest
	chatRequest       AIProviderChatRequest
	chatResult        AIProviderTextResult
	chatErr           error
	generated         AIProviderTextResult
	validation        AISkillValidationResult
}

func (p *fakeAIProvider) TestConnection(_ context.Context, request AIConnectionTestRequest) (AIConnectionTestResult, error) {
	p.connectionRequest = request
	return AIConnectionTestResult{Provider: request.ProviderType, Model: request.Model, Configured: true, Message: "api_key=connection-result-secret"}, nil
}

func (p *fakeAIProvider) Chat(_ context.Context, request AIProviderChatRequest) (AIProviderTextResult, error) {
	p.chatRequest = request
	return p.chatResult, p.chatErr
}

func (p *fakeAIProvider) GenerateSkill(context.Context, string) (AIProviderTextResult, error) {
	return p.generated, nil
}

func (p *fakeAIProvider) ValidateSkill(context.Context, string) (AISkillValidationResult, error) {
	return p.validation, nil
}

type fakeAIConversationBackend struct {
	persistedPrompt string
	persistedOutput string
	failPersist     bool
	deleteCalls     int
	deleteAllCalls  int
	stopCalls       int
	readCalls       int
	initiation      AIInitiationRequest
	suggestion      *AISuggestionRecord
}

func (b *fakeAIConversationBackend) PrepareChatAtomic(_ context.Context, request AIChatPreparationRequest) (*AIChatPreparation, error) {
	id := request.ConversationID
	if id == 0 {
		id = 41
	}
	b.persistedPrompt = request.Prompt
	return &AIChatPreparation{Conversation: AIConversationReference{ID: id, Title: request.Title, Source: "user", IsRead: true}, Prompt: request.Prompt}, nil
}

func (b *fakeAIConversationBackend) BeginAssistantMessage(context.Context, int64) (int64, error) {
	return 99, nil
}

func (b *fakeAIConversationBackend) UpdateAssistantMessageProgress(context.Context, AIChatProgress) error {
	return nil
}

func (b *fakeAIConversationBackend) CompleteAssistantMessageAtomic(_ context.Context, completion AIChatCompletion) error {
	if b.failPersist {
		return errors.New("persist failed secret=database-secret")
	}
	b.persistedOutput = completion.Content
	return nil
}

func (b *fakeAIConversationBackend) DeleteConversationAtomic(context.Context, int64, ActionAuthorization) error {
	b.deleteCalls++
	return nil
}

func (b *fakeAIConversationBackend) DeleteAllConversationsAtomic(context.Context, ActionAuthorization) (int64, error) {
	b.deleteAllCalls++
	return 3, nil
}

func (b *fakeAIConversationBackend) StopConversation(context.Context, int64) error {
	b.stopCalls++
	return nil
}

func (b *fakeAIConversationBackend) InitiateConversationAtomic(_ context.Context, request AIInitiationRequest) (*AIConversationReference, error) {
	b.initiation = request
	return &AIConversationReference{ID: 55, Title: "api_key=init-secret", Source: "ai", ProactiveType: string(request.Kind)}, nil
}

func (b *fakeAIConversationBackend) MarkConversationRead(context.Context, int64) error {
	b.readCalls++
	return nil
}

func (b *fakeAIConversationBackend) GetSuggestion(context.Context, int64) (*AISuggestionRecord, error) {
	if b.suggestion == nil {
		return nil, errors.New("missing")
	}
	copy := *b.suggestion
	return &copy, nil
}

func (b *fakeAIConversationBackend) AcceptSuggestionAtomic(context.Context, int64, ActionAuthorization) (AISuggestionAcceptance, error) {
	b.suggestion.Status = "accepted"
	return AISuggestionAcceptance{Status: "accepted", Message: "api_key=suggestion-secret accepted"}, nil
}

func (b *fakeAIConversationBackend) DismissSuggestionAtomic(context.Context, int64, ActionAuthorization) error {
	b.suggestion.Status = "dismissed"
	return nil
}

func (b *fakeAIConversationBackend) DiscussSuggestionAtomic(context.Context, int64, ActionAuthorization) (*AIConversationReference, error) {
	return &AIConversationReference{ID: 77, Title: "Discussion", Source: "ai", ProactiveType: "task_suggestion"}, nil
}

type fakeAIToolPort struct {
	calls   int
	request AIToolExecutionRequest
	result  AIToolExecutionResult
	err     error
}

func (p *fakeAIToolPort) ExecuteAITool(_ context.Context, request AIToolExecutionRequest) (AIToolExecutionResult, error) {
	p.calls++
	p.request = request
	return p.result, p.err
}

func TestPhase4AIUsesPortsWithoutLeakingPromptTranscriptOrToolSecrets(t *testing.T) {
	ctx := context.Background()
	issues := make([]string, 55)
	for i := range issues {
		issues[i] = "issue"
	}
	issues[0] = "password=validation-secret"
	provider := &fakeAIProvider{
		chatResult: AIProviderTextResult{Text: strings.Repeat("o", maxApplicationOutputRunes+5) + " api_key=output-secret", Model: "model"},
		generated:  AIProviderTextResult{Text: "# Skill\napi_key=generated-secret", Model: "background-model"},
		validation: AISkillValidationResult{Valid: false, Issues: issues, Suggestions: []string{"access_token=suggestion-output-secret"}, Summary: "checked"},
	}
	conversations := &fakeAIConversationBackend{suggestion: &AISuggestionRecord{ID: 9, Status: "pending", Type: "create_task", Risk: ProposalRiskR2}}
	tool := &fakeAIToolPort{result: AIToolExecutionResult{Output: "password=tool-secret\ncompleted", ExitCode: 0}}
	effects := &recordingApplicationEffects{}
	service := NewAIAssistantService(provider, conversations, tool, effects)

	chat, err := service.Chat(ctx, AIChatCommand{Prompt: "Please help api_key=prompt-secret", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	chatJSON, _ := json.Marshal(chat)
	if strings.Contains(provider.chatRequest.Prompt, "prompt-secret") || strings.Contains(conversations.persistedPrompt, "prompt-secret") || strings.Contains(string(chatJSON), "prompt-secret") || strings.Contains(string(chatJSON), "output-secret") || !chat.WasTruncated {
		t.Fatalf("chat boundary leaked raw prompt/output: provider=%q persisted=%q result=%s", provider.chatRequest.Prompt, conversations.persistedPrompt, chatJSON)
	}
	if strings.Contains(string(chatJSON), `"prompt"`) || strings.Contains(string(chatJSON), `"transcript"`) {
		t.Fatalf("chat response exposed prompt/transcript: %s", chatJSON)
	}

	connection, err := service.TestConnection(ctx, AIConnectionTestRequest{ProviderType: "apikey", APIKey: "connection-input-secret", Model: "model"}, phase4Authorization())
	if err != nil || provider.connectionRequest.APIKey != "connection-input-secret" || strings.Contains(connection.Message, "connection-result-secret") {
		t.Fatalf("connection boundary failed: result=%+v request=%+v err=%v", connection, provider.connectionRequest, err)
	}
	generated, err := service.GenerateSkill(ctx, "Create a skill password=description-secret", phase4Authorization())
	if err != nil || strings.Contains(generated.Content, "generated-secret") {
		t.Fatalf("generated skill was not redacted: result=%+v err=%v", generated, err)
	}
	validation, err := service.ValidateSkill(ctx, "# Skill", phase4Authorization())
	if err != nil || len(validation.Issues) != 50 || !validation.WasTruncated || strings.Contains(strings.Join(validation.Issues, " "), "validation-secret") || strings.Contains(strings.Join(validation.Suggestions, " "), "suggestion-output-secret") {
		t.Fatalf("skill validation was not bounded/redacted: result=%+v err=%v", validation, err)
	}

	if _, err = service.ExecuteTool(ctx, AIToolExecutionRequest{Name: "task.create", Arguments: map[string]any{"api_key": "argument-secret"}, Authorization: phase4Authorization()}); !ErrorIsKind(err, ErrorValidation) || tool.calls != 0 {
		t.Fatalf("tool executed without R4 approval: calls=%d err=%v", tool.calls, err)
	}
	toolView, err := service.ExecuteTool(ctx, AIToolExecutionRequest{Name: "task.create", Arguments: map[string]any{"api_key": "argument-secret", "title": "safe"}, Authorization: phase4ApprovedAuthorization()})
	if err != nil || tool.calls != 1 || tool.request.Arguments["api_key"] != "[REDACTED]" || strings.Contains(toolView.Output, "tool-secret") {
		t.Fatalf("tool boundary failed: view=%+v request=%+v calls=%d err=%v", toolView, tool.request, tool.calls, err)
	}

	initView, err := service.InitiateTaskCreation(ctx, AITaskCreationCommand{ProjectID: 3, Title: "api_key=task-secret", Description: "password=description-secret", Authorization: phase4Authorization()})
	if err != nil || strings.Contains(initView.Title, "init-secret") || strings.Contains(conversations.initiation.Title, "task-secret") || strings.Contains(conversations.initiation.Description, "description-secret") {
		t.Fatalf("initiation leaked context: view=%+v request=%+v err=%v", initView, conversations.initiation, err)
	}

	accepted, err := service.AcceptSuggestion(ctx, 9, phase4Authorization())
	if err != nil || strings.Contains(accepted.Message, "suggestion-secret") || conversations.suggestion.Status != "accepted" {
		t.Fatalf("suggestion acceptance failed: accepted=%+v suggestion=%+v err=%v", accepted, conversations.suggestion, err)
	}
	if err = service.DeleteConversation(ctx, 41, phase4Authorization()); !ErrorIsKind(err, ErrorValidation) || conversations.deleteCalls != 0 {
		t.Fatalf("conversation deleted without R3 approval: calls=%d err=%v", conversations.deleteCalls, err)
	}
	if err = service.DeleteConversation(ctx, 41, phase4ApprovedAuthorization()); err != nil || conversations.deleteCalls != 1 {
		t.Fatalf("approved conversation delete failed: calls=%d err=%v", conversations.deleteCalls, err)
	}
	if deleted, err := service.DeleteAllConversations(ctx, phase4ApprovedAuthorization()); err != nil || deleted != 3 || conversations.deleteAllCalls != 1 {
		t.Fatalf("approved delete-all failed: deleted=%d calls=%d err=%v", deleted, conversations.deleteAllCalls, err)
	}
	if err = service.StopConversation(ctx, 41, phase4Authorization()); err != nil || conversations.stopCalls != 1 {
		t.Fatalf("stop conversation failed: calls=%d err=%v", conversations.stopCalls, err)
	}
	if err = service.MarkConversationRead(ctx, 41, phase4Authorization()); err != nil || conversations.readCalls != 1 {
		t.Fatalf("mark conversation read failed: calls=%d err=%v", conversations.readCalls, err)
	}
	proactive, err := service.TestProactive(ctx, "subtle", phase4Authorization())
	if err != nil || proactive.ID != 55 || conversations.initiation.Kind != AIInitiateProactiveTesting || conversations.initiation.ProactiveLevel != "subtle" {
		t.Fatalf("test proactive failed: view=%+v request=%+v err=%v", proactive, conversations.initiation, err)
	}
	conversations.suggestion = &AISuggestionRecord{ID: 10, Status: "pending", Type: "update_task", Risk: ProposalRiskR2}
	discussion, err := service.DiscussSuggestion(ctx, 10, phase4Authorization())
	if err != nil || discussion.ID != 77 {
		t.Fatalf("suggestion discussion failed: view=%+v err=%v", discussion, err)
	}
	if err = service.DismissSuggestion(ctx, 10, phase4Authorization()); err != nil || conversations.suggestion.Status != "dismissed" {
		t.Fatalf("suggestion dismissal failed: suggestion=%+v err=%v", conversations.suggestion, err)
	}
}

func TestPhase4AIFailureDoesNotPublishEffectOrExposeBackendSecret(t *testing.T) {
	provider := &fakeAIProvider{chatResult: AIProviderTextResult{Text: "answer"}}
	conversations := &fakeAIConversationBackend{failPersist: true}
	effects := &recordingApplicationEffects{}
	service := NewAIAssistantService(provider, conversations, nil, effects)
	_, err := service.Chat(context.Background(), AIChatCommand{Prompt: "safe", Authorization: phase4Authorization()})
	if err == nil || strings.Contains(err.Error(), "database-secret") || len(effects.changes) != 0 {
		t.Fatalf("AI persistence failure leaked secret/effect: err=%v effects=%+v", err, effects.changes)
	}
}

type fakeNotificationDeliveryBackend struct {
	subscription PushSubscriptionInput
	unsubscribed string
	disabled     bool
	testCalls    int
	failTest     bool
}

func (b *fakeNotificationDeliveryBackend) SubscribePush(_ context.Context, input PushSubscriptionInput) error {
	b.subscription = input
	return nil
}

func (b *fakeNotificationDeliveryBackend) UnsubscribePush(_ context.Context, endpoint string) error {
	b.unsubscribed = endpoint
	return nil
}

func (b *fakeNotificationDeliveryBackend) GetPushDisabled(context.Context) (bool, error) {
	return b.disabled, nil
}

func (b *fakeNotificationDeliveryBackend) SetPushDisabled(_ context.Context, disabled bool) error {
	b.disabled = disabled
	return nil
}

func (b *fakeNotificationDeliveryBackend) SendTestPush(context.Context) (PushTestResult, error) {
	b.testCalls++
	if b.failTest {
		return PushTestResult{}, errors.New("push failed api_key=push-backend-secret")
	}
	return PushTestResult{Status: "sent", Message: "access_token=push-secret delivered"}, nil
}

type fakeTokenUsageStore struct{ err error }

func (s fakeTokenUsageStore) ClearTokenUsage(context.Context) (int64, error) {
	return 0, s.err
}

func TestPhase4NotificationDeliveryAndTokenClearUsePortsAndAuthorization(t *testing.T) {
	ctx := context.Background()
	backend := &fakeNotificationDeliveryBackend{}
	effects := &recordingApplicationEffects{}
	notifications := NewNotificationDeliveryService(backend, effects)
	view, err := notifications.Subscribe(ctx, PushSubscriptionInput{Endpoint: "https://push.example/sub", P256dh: "p256-secret", Auth: "auth-secret"}, phase4Authorization())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "p256-secret") || strings.Contains(string(encoded), "auth-secret") || backend.subscription.Auth != "auth-secret" {
		t.Fatalf("subscription response leaked key or port did not receive it: view=%s backend=%+v", encoded, backend.subscription)
	}
	if _, err = notifications.Unsubscribe(ctx, "https://push.example/sub", phase4Authorization()); !ErrorIsKind(err, ErrorValidation) || backend.unsubscribed != "" {
		t.Fatalf("unsubscribe bypassed R3 approval: endpoint=%q err=%v", backend.unsubscribed, err)
	}
	if _, err = notifications.Unsubscribe(ctx, "https://push.example/sub", phase4ApprovedAuthorization()); err != nil {
		t.Fatal(err)
	}
	pushResult, err := notifications.Test(ctx, phase4Authorization())
	if err != nil || strings.Contains(pushResult.Message, "push-secret") || backend.testCalls != 1 {
		t.Fatalf("test push boundary failed: result=%+v calls=%d err=%v", pushResult, backend.testCalls, err)
	}
	if _, err = notifications.SetPreference(ctx, true, phase4Authorization()); err != nil || !backend.disabled {
		t.Fatalf("notification preference failed: disabled=%v err=%v", backend.disabled, err)
	}
	effectCount := len(effects.changes)
	backend.failTest = true
	if _, err = notifications.Test(ctx, phase4Authorization()); err == nil || strings.Contains(err.Error(), "push-backend-secret") || len(effects.changes) != effectCount {
		t.Fatalf("failed push leaked secret/effect: err=%v effects=%+v", err, effects.changes)
	}

	db := applicationTestDB(t)
	if err = db.CreateTokenUsage(ctx, &database.TokenUsage{Source: "ai_assistant", Model: "model", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	tokenEffects := &recordingApplicationEffects{}
	tokens := NewTokenUsageService(db, tokenEffects)
	if _, err = tokens.Clear(ctx, phase4Authorization()); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("token usage cleared without R3 approval: %v", err)
	}
	deleted, err := tokens.Clear(ctx, phase4ApprovedAuthorization())
	if err != nil || deleted != 1 || len(tokenEffects.changes) != 1 {
		t.Fatalf("token clear failed: deleted=%d effects=%+v err=%v", deleted, tokenEffects.changes, err)
	}
	failingEffects := &recordingApplicationEffects{}
	failingTokens := NewTokenUsageService(fakeTokenUsageStore{err: errors.New("clear unavailable")}, failingEffects)
	if _, err = failingTokens.Clear(ctx, phase4ApprovedAuthorization()); err == nil || len(failingEffects.changes) != 0 {
		t.Fatalf("failed token clear emitted effect: err=%v effects=%+v", err, failingEffects.changes)
	}
}
