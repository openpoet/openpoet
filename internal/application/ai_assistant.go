package application

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

type AIConnectionTestRequest struct {
	ProviderType string
	APIKey       string
	Model        string
	BaseURL      string
	ConfigID     *int64
}

type AIConnectionTestResult struct {
	Provider   string
	Model      string
	Configured bool
	Message    string
}

type AIProviderChatRequest struct {
	ConversationID int64
	ProjectID      int64
	AgentID        *int64
	Prompt         string
	Feedback       string
}

type AIProviderTextResult struct {
	Text  string
	Model string
}

type AISkillValidationResult struct {
	Valid       bool
	Issues      []string
	Suggestions []string
	Summary     string
}

// AIProviderPort is the only boundary allowed to contact an LLM/provider.
// Application service tests use a deterministic fake and never perform I/O.
type AIProviderPort interface {
	TestConnection(context.Context, AIConnectionTestRequest) (AIConnectionTestResult, error)
	Chat(context.Context, AIProviderChatRequest) (AIProviderTextResult, error)
	GenerateSkill(context.Context, string) (AIProviderTextResult, error)
	ValidateSkill(context.Context, string) (AISkillValidationResult, error)
}

// AIProviderStreamingPort is an optional provider extension used by the human
// SSE transport. The Application Service still owns validation, output bounds,
// effects, and the final result; the callback only carries presentation deltas.
type AIProviderStreamingPort interface {
	GenerateSkillStream(context.Context, string, func(string) error) (AIProviderTextResult, error)
}

type AIConversationReference struct {
	ID            int64
	Title         string
	Source        string
	ProactiveType string
	IsRead        bool
}

type AIInitiationKind string

const (
	AIInitiateMemoryDoc        AIInitiationKind = "memory_doc_edit"
	AIInitiateTaskCreation     AIInitiationKind = "task_creation"
	AIInitiateTaskDiscussion   AIInitiationKind = "task_discussion"
	AIInitiateSkillCustomize   AIInitiationKind = "skill_customization"
	AIInitiateProactiveTesting AIInitiationKind = "proactive_test"
)

type AIInitiationRequest struct {
	Kind           AIInitiationKind
	ProjectID      int64
	TaskID         int64
	SkillID        int64
	ParentID       int64
	Title          string
	Description    string
	Status         string
	Priority       string
	DueDate        string
	ProactiveLevel string
	Authorization  ActionAuthorization
}

type AISuggestionRecord struct {
	ID     int64
	Status string
	Type   string
	Risk   ProposalRisk
}

type AISuggestionAcceptance struct {
	Status  string
	Message string
}

type AIChatPreparationRequest struct {
	ConversationID int64
	ProjectID      int64
	AgentID        *int64
	Prompt         string
	Title          string
}

type AIChatPreparation struct {
	Conversation AIConversationReference
	Prompt       string
	Feedback     string
}

type AIChatCompletion struct {
	ConversationID int64
	MessageID      int64
	Content        string
	ToolCallsJSON  string
	Status         string
	Error          string
}

type AIChatProgress struct {
	ConversationID int64
	MessageID      int64
	Content        string
	ToolCallsJSON  string
}

// AIConversationBackend owns persistence and runtime coordination. Methods
// named Atomic must commit the conversation/suggestion status and all related
// database mutations as one unit before returning.
type AIConversationBackend interface {
	PrepareChatAtomic(context.Context, AIChatPreparationRequest) (*AIChatPreparation, error)
	BeginAssistantMessage(context.Context, int64) (int64, error)
	UpdateAssistantMessageProgress(context.Context, AIChatProgress) error
	CompleteAssistantMessageAtomic(context.Context, AIChatCompletion) error
	DeleteConversationAtomic(context.Context, int64, ActionAuthorization) error
	DeleteAllConversationsAtomic(context.Context, ActionAuthorization) (int64, error)
	StopConversation(context.Context, int64) error
	InitiateConversationAtomic(context.Context, AIInitiationRequest) (*AIConversationReference, error)
	MarkConversationRead(context.Context, int64) error
	GetSuggestion(context.Context, int64) (*AISuggestionRecord, error)
	AcceptSuggestionAtomic(context.Context, int64, ActionAuthorization) (AISuggestionAcceptance, error)
	DismissSuggestionAtomic(context.Context, int64, ActionAuthorization) error
	DiscussSuggestionAtomic(context.Context, int64, ActionAuthorization) (*AIConversationReference, error)
}

type AIToolExecutionRequest struct {
	Name           string
	Arguments      map[string]any
	ConversationID int64
	Authorization  ActionAuthorization
}

type AIToolExecutionResult struct {
	Output   string
	ExitCode int
}

type AIToolExecutionPort interface {
	ExecuteAITool(context.Context, AIToolExecutionRequest) (AIToolExecutionResult, error)
}

type AIConversationView struct {
	ID            int64  `json:"id"`
	Title         string `json:"title,omitempty"`
	Source        string `json:"source,omitempty"`
	ProactiveType string `json:"proactive_type,omitempty"`
	IsRead        bool   `json:"is_read"`
}

type AIChatResult struct {
	ConversationID int64  `json:"conversation_id"`
	Output         string `json:"output"`
	Model          string `json:"model,omitempty"`
	WasTruncated   bool   `json:"was_truncated,omitempty"`
}

type AIGeneratedSkill struct {
	Content      string `json:"content"`
	Model        string `json:"model,omitempty"`
	WasTruncated bool   `json:"was_truncated,omitempty"`
}

type AISkillValidationView struct {
	Valid        bool     `json:"valid"`
	Issues       []string `json:"issues"`
	Suggestions  []string `json:"suggestions"`
	Summary      string   `json:"summary,omitempty"`
	WasTruncated bool     `json:"was_truncated,omitempty"`
}

type AIToolExecutionView struct {
	Status       string `json:"status"`
	Output       string `json:"output,omitempty"`
	ExitCode     int    `json:"exit_code"`
	WasTruncated bool   `json:"was_truncated,omitempty"`
}

type AIAssistantService struct {
	provider      AIProviderPort
	conversations AIConversationBackend
	tools         AIToolExecutionPort
	effects       ApplicationEffects
}

func NewAIAssistantService(provider AIProviderPort, conversations AIConversationBackend, tools AIToolExecutionPort, effects ApplicationEffects) *AIAssistantService {
	return &AIAssistantService{provider: provider, conversations: conversations, tools: tools, effects: effects}
}

func (s *AIAssistantService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("ai_assistant")
}

func (s *AIAssistantService) TestConnection(ctx context.Context, request AIConnectionTestRequest, authorization ActionAuthorization) (AIConnectionTestResult, error) {
	if err := requireActionActor(authorization); err != nil {
		return AIConnectionTestResult{}, err
	}
	request.ProviderType = strings.TrimSpace(request.ProviderType)
	request.Model = strings.TrimSpace(request.Model)
	request.BaseURL = strings.TrimSpace(request.BaseURL)
	if request.ProviderType == "" || len(request.ProviderType) > 100 {
		return AIConnectionTestResult{}, validationError("invalid_ai_provider", "AI provider type is required and bounded")
	}
	if len(request.Model) > 200 || len(request.BaseURL) > 2048 || utf8.RuneCountInString(request.APIKey) > maxApplicationPromptRunes {
		return AIConnectionTestResult{}, validationError("invalid_ai_connection_input", "AI connection input exceeds its bounded limit")
	}
	if request.ConfigID != nil && *request.ConfigID <= 0 {
		return AIConnectionTestResult{}, validationError("invalid_ai_config_id", "AI config ID must be positive")
	}
	if request.BaseURL != "" {
		parsed, err := url.Parse(request.BaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return AIConnectionTestResult{}, validationError("invalid_ai_base_url", "AI base URL must be HTTP or HTTPS")
		}
		for key := range parsed.Query() {
			if isSensitivePayloadKey(key) {
				return AIConnectionTestResult{}, validationError("invalid_ai_base_url", "AI base URL cannot embed credentials")
			}
		}
	}
	if s.provider == nil {
		return AIConnectionTestResult{}, validationError("ai_provider_unavailable", "AI provider is unavailable")
	}
	result, err := s.provider.TestConnection(ctx, request)
	if err != nil {
		return AIConnectionTestResult{}, safeBackendError("AI connection test failed", err)
	}
	result.Provider, _ = boundedRedactedOutput(defaultStatus(result.Provider, request.ProviderType), 100)
	result.Model, _ = boundedRedactedOutput(result.Model, 200)
	result.Message, _ = boundedRedactedOutput(result.Message, maxApplicationSummaryRunes)
	return result, nil
}

type AIChatCommand struct {
	ConversationID int64
	ProjectID      int64
	AgentID        *int64
	Prompt         string
	Authorization  ActionAuthorization
}

func (s *AIAssistantService) Chat(ctx context.Context, command AIChatCommand) (AIChatResult, error) {
	preparation, err := s.PrepareChat(ctx, command)
	if err != nil {
		return AIChatResult{}, err
	}
	messageID, err := s.BeginChatResponse(ctx, preparation.Conversation.ID, command.Authorization)
	if err != nil {
		return AIChatResult{}, err
	}
	providerResult, err := s.provider.Chat(ctx, AIProviderChatRequest{
		ConversationID: preparation.Conversation.ID, ProjectID: command.ProjectID,
		AgentID: command.AgentID, Prompt: preparation.Prompt, Feedback: preparation.Feedback,
	})
	if err != nil {
		_ = s.CompleteChatResponse(ctx, AIChatCompletion{
			ConversationID: preparation.Conversation.ID, MessageID: messageID,
			Status: "error", Error: "AI chat failed",
		}, command.Authorization)
		return AIChatResult{}, safeBackendError("AI chat failed", err)
	}
	output, truncated := boundedRedactedOutput(providerResult.Text, maxApplicationOutputRunes)
	model, _ := boundedRedactedOutput(providerResult.Model, 200)
	if err = s.CompleteChatResponse(ctx, AIChatCompletion{
		ConversationID: preparation.Conversation.ID, MessageID: messageID,
		Content: output, ToolCallsJSON: "[]", Status: "completed",
	}, command.Authorization); err != nil {
		return AIChatResult{}, err
	}
	return AIChatResult{ConversationID: preparation.Conversation.ID, Output: output, Model: model, WasTruncated: truncated}, nil
}

func (s *AIAssistantService) PrepareChat(ctx context.Context, command AIChatCommand) (AIChatPreparation, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return AIChatPreparation{}, err
	}
	if command.ConversationID < 0 || command.ProjectID < 0 || (command.AgentID != nil && *command.AgentID <= 0) {
		return AIChatPreparation{}, validationError("invalid_ai_chat_reference", "Conversation, project and agent references are invalid")
	}
	prompt, err := boundedRedactedInput(command.Prompt, maxApplicationPromptRunes, "AI prompt", true)
	if err != nil {
		return AIChatPreparation{}, err
	}
	if s.provider == nil || s.conversations == nil {
		return AIChatPreparation{}, validationError("ai_backend_unavailable", "AI backend is unavailable")
	}
	titleRunes := []rune(prompt)
	if len(titleRunes) > 100 {
		titleRunes = titleRunes[:100]
		titleRunes = append(titleRunes, []rune("...")...)
	}
	preparation, err := s.conversations.PrepareChatAtomic(ctx, AIChatPreparationRequest{
		ConversationID: command.ConversationID, ProjectID: command.ProjectID,
		AgentID: command.AgentID, Prompt: prompt, Title: string(titleRunes),
	})
	if err != nil || preparation == nil || preparation.Conversation.ID <= 0 {
		if ErrorIsKind(err, ErrorValidation) || ErrorIsKind(err, ErrorNotFound) || ErrorIsKind(err, ErrorConflict) {
			return AIChatPreparation{}, err
		}
		return AIChatPreparation{}, safeBackendError("AI conversation could not be prepared", err)
	}
	preparation.Prompt = prompt
	preparation.Feedback, _ = boundedRedactedOutput(preparation.Feedback, maxApplicationSummaryRunes)
	return *preparation, nil
}

func (s *AIAssistantService) BeginChatResponse(ctx context.Context, conversationID int64, authorization ActionAuthorization) (int64, error) {
	if err := requireActionActor(authorization); err != nil {
		return 0, err
	}
	if err := validateBoundedID(conversationID, "Conversation"); err != nil {
		return 0, err
	}
	if s.conversations == nil {
		return 0, validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	messageID, err := s.conversations.BeginAssistantMessage(ctx, conversationID)
	if err != nil || messageID <= 0 {
		return 0, safeBackendError("AI response could not be initialized", err)
	}
	return messageID, nil
}

func (s *AIAssistantService) UpdateChatResponse(ctx context.Context, progress AIChatProgress, authorization ActionAuthorization) error {
	if err := requireActionActor(authorization); err != nil {
		return err
	}
	if progress.ConversationID <= 0 || progress.MessageID <= 0 {
		return validationError("invalid_ai_chat_reference", "Conversation and message references must be positive")
	}
	progress.Content, _ = boundedRedactedOutput(progress.Content, maxApplicationOutputRunes)
	if progress.ToolCallsJSON == "" {
		progress.ToolCallsJSON = "[]"
	}
	if utf8.RuneCountInString(progress.ToolCallsJSON) > maxApplicationContentRunes || validateJSONArray(progress.ToolCallsJSON, "invalid_ai_tool_calls", "AI tool calls must be a bounded JSON array") != nil {
		return validationError("invalid_ai_tool_calls", "AI tool calls must be a bounded JSON array")
	}
	if s.conversations == nil {
		return validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	if err := s.conversations.UpdateAssistantMessageProgress(ctx, progress); err != nil {
		return safeBackendError("AI response progress could not be persisted", err)
	}
	return nil
}

func (s *AIAssistantService) CompleteChatResponse(ctx context.Context, completion AIChatCompletion, authorization ActionAuthorization) error {
	if err := requireActionActor(authorization); err != nil {
		return err
	}
	if completion.ConversationID <= 0 || completion.MessageID <= 0 {
		return validationError("invalid_ai_chat_reference", "Conversation and message references must be positive")
	}
	if completion.Status != "completed" && completion.Status != "error" {
		return validationError("invalid_ai_message_status", "AI message status must be completed or error")
	}
	completion.Content, _ = boundedRedactedOutput(completion.Content, maxApplicationOutputRunes)
	completion.Error, _ = boundedRedactedOutput(completion.Error, maxApplicationSummaryRunes)
	if completion.ToolCallsJSON == "" {
		completion.ToolCallsJSON = "[]"
	}
	if utf8.RuneCountInString(completion.ToolCallsJSON) > maxApplicationContentRunes || validateJSONArray(completion.ToolCallsJSON, "invalid_ai_tool_calls", "AI tool calls must be a bounded JSON array") != nil {
		return validationError("invalid_ai_tool_calls", "AI tool calls must be a bounded JSON array")
	}
	if s.conversations == nil {
		return validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	if err := s.conversations.CompleteAssistantMessageAtomic(ctx, completion); err != nil {
		return safeBackendError("AI response could not be persisted", err)
	}
	if completion.Status == "completed" {
		publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "chat_completed", ID: completion.ConversationID})
	}
	return nil
}

func (s *AIAssistantService) DeleteConversation(ctx context.Context, id int64, authorization ActionAuthorization) error {
	if err := requireExplicitActionApproval(authorization); err != nil {
		return err
	}
	if err := validateBoundedID(id, "Conversation"); err != nil {
		return err
	}
	if s.conversations == nil {
		return validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	if err := s.conversations.DeleteConversationAtomic(ctx, id, authorization); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "conversation_deleted", ID: id})
	return nil
}

func (s *AIAssistantService) DeleteAllConversations(ctx context.Context, authorization ActionAuthorization) (int64, error) {
	if err := requireExplicitActionApproval(authorization); err != nil {
		return 0, err
	}
	if s.conversations == nil {
		return 0, validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	deleted, err := s.conversations.DeleteAllConversationsAtomic(ctx, authorization)
	if err != nil {
		return 0, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "conversations_deleted", Meta: map[string]any{"deleted": deleted}})
	return deleted, nil
}

func (s *AIAssistantService) StopConversation(ctx context.Context, id int64, authorization ActionAuthorization) error {
	if err := requireActionActor(authorization); err != nil {
		return err
	}
	if err := validateBoundedID(id, "Conversation"); err != nil {
		return err
	}
	if s.conversations == nil {
		return validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	if err := s.conversations.StopConversation(ctx, id); err != nil {
		return safeBackendError("AI conversation could not be stopped", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "conversation_stopped", ID: id})
	return nil
}

func (s *AIAssistantService) InitiateMemoryDocEdit(ctx context.Context, projectID int64, authorization ActionAuthorization) (AIConversationView, error) {
	return s.initiate(ctx, AIInitiationRequest{Kind: AIInitiateMemoryDoc, ProjectID: projectID, Authorization: authorization})
}

type AITaskCreationCommand struct {
	ProjectID     int64
	ParentID      int64
	Title         string
	Description   string
	Status        string
	Priority      string
	DueDate       string
	Authorization ActionAuthorization
}

func (s *AIAssistantService) InitiateTaskCreation(ctx context.Context, command AITaskCreationCommand) (AIConversationView, error) {
	return s.initiate(ctx, AIInitiationRequest{Kind: AIInitiateTaskCreation, ProjectID: command.ProjectID, ParentID: command.ParentID, Title: command.Title, Description: command.Description, Status: command.Status, Priority: command.Priority, DueDate: command.DueDate, Authorization: command.Authorization})
}

func (s *AIAssistantService) InitiateTaskDiscussion(ctx context.Context, projectID, taskID int64, authorization ActionAuthorization) (AIConversationView, error) {
	return s.initiate(ctx, AIInitiationRequest{Kind: AIInitiateTaskDiscussion, ProjectID: projectID, TaskID: taskID, Authorization: authorization})
}

func (s *AIAssistantService) InitiateSkillCustomization(ctx context.Context, projectID, skillID int64, authorization ActionAuthorization) (AIConversationView, error) {
	return s.initiate(ctx, AIInitiationRequest{Kind: AIInitiateSkillCustomize, ProjectID: projectID, SkillID: skillID, Authorization: authorization})
}

func (s *AIAssistantService) TestProactive(ctx context.Context, level string, authorization ActionAuthorization) (AIConversationView, error) {
	level = strings.TrimSpace(level)
	if level == "" {
		level = "standard"
	}
	if level != "critical" && level != "standard" && level != "subtle" {
		return AIConversationView{}, validationError("invalid_proactive_level", "Proactive level must be critical, standard or subtle")
	}
	return s.initiate(ctx, AIInitiationRequest{Kind: AIInitiateProactiveTesting, ProactiveLevel: level, Authorization: authorization})
}

func (s *AIAssistantService) initiate(ctx context.Context, request AIInitiationRequest) (AIConversationView, error) {
	if err := requireActionActor(request.Authorization); err != nil {
		return AIConversationView{}, err
	}
	if request.Kind != AIInitiateProactiveTesting && request.ProjectID <= 0 {
		return AIConversationView{}, validationError("invalid_project_id", "Project ID must be positive")
	}
	if request.Kind == AIInitiateTaskDiscussion && request.TaskID <= 0 {
		return AIConversationView{}, validationError("invalid_task_id", "Task ID must be positive")
	}
	if request.Kind == AIInitiateSkillCustomize && request.SkillID <= 0 {
		return AIConversationView{}, validationError("invalid_skill_id", "Skill ID must be positive")
	}
	if request.ParentID < 0 {
		return AIConversationView{}, validationError("invalid_parent_id", "Parent task ID cannot be negative")
	}
	var err error
	if request.Title, err = boundedRedactedInput(request.Title, maxApplicationTitleRunes, "Task title", false); err != nil {
		return AIConversationView{}, err
	}
	if request.Description, err = boundedRedactedInput(request.Description, maxApplicationSummaryRunes, "Task description", false); err != nil {
		return AIConversationView{}, err
	}
	if request.Status, err = boundedRedactedInput(request.Status, 100, "Task status", false); err != nil {
		return AIConversationView{}, err
	}
	if request.Priority, err = boundedRedactedInput(request.Priority, 100, "Task priority", false); err != nil {
		return AIConversationView{}, err
	}
	if request.DueDate, err = boundedRedactedInput(request.DueDate, 100, "Task due date", false); err != nil {
		return AIConversationView{}, err
	}
	if s.conversations == nil {
		return AIConversationView{}, validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	reference, err := s.conversations.InitiateConversationAtomic(ctx, request)
	if err != nil || reference == nil {
		if ErrorIsKind(err, ErrorValidation) || ErrorIsKind(err, ErrorNotFound) || ErrorIsKind(err, ErrorConflict) {
			return AIConversationView{}, err
		}
		return AIConversationView{}, safeBackendError("AI conversation could not be initiated", err)
	}
	view := aiConversationView(reference)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "conversation_initiated", ID: reference.ID, Meta: map[string]any{"type": request.Kind}})
	return view, nil
}

func (s *AIAssistantService) GenerateSkill(ctx context.Context, description string, authorization ActionAuthorization) (AIGeneratedSkill, error) {
	return s.generateSkill(ctx, description, authorization, nil)
}

func (s *AIAssistantService) GenerateSkillStream(ctx context.Context, description string, authorization ActionAuthorization, onDelta func(string) error) (AIGeneratedSkill, error) {
	return s.generateSkill(ctx, description, authorization, onDelta)
}

func (s *AIAssistantService) generateSkill(ctx context.Context, description string, authorization ActionAuthorization, onDelta func(string) error) (AIGeneratedSkill, error) {
	if err := requireActionActor(authorization); err != nil {
		return AIGeneratedSkill{}, err
	}
	description, err := boundedRedactedInput(description, maxApplicationPromptRunes, "Skill description", true)
	if err != nil {
		return AIGeneratedSkill{}, err
	}
	if s.provider == nil {
		return AIGeneratedSkill{}, validationError("ai_provider_unavailable", "AI provider is unavailable")
	}
	var result AIProviderTextResult
	if streamingProvider, ok := s.provider.(AIProviderStreamingPort); ok && onDelta != nil {
		emittedRunes := 0
		result, err = streamingProvider.GenerateSkillStream(ctx, description, func(delta string) error {
			remaining := maxApplicationContentRunes - emittedRunes
			if remaining <= 0 {
				return nil
			}
			runes := []rune(delta)
			if len(runes) > remaining {
				runes = runes[:remaining]
			}
			emittedRunes += len(runes)
			if len(runes) == 0 {
				return nil
			}
			return onDelta(string(runes))
		})
	} else {
		result, err = s.provider.GenerateSkill(ctx, description)
		if err == nil && onDelta != nil {
			bounded, _ := boundedRedactedOutput(result.Text, maxApplicationContentRunes)
			if callbackErr := onDelta(bounded); callbackErr != nil {
				return AIGeneratedSkill{}, callbackErr
			}
		}
	}
	if err != nil {
		return AIGeneratedSkill{}, safeBackendError("AI skill generation failed", err)
	}
	content, truncated := boundedRedactedOutput(result.Text, maxApplicationContentRunes)
	model, _ := boundedRedactedOutput(result.Model, 200)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "skill_generated"})
	return AIGeneratedSkill{Content: content, Model: model, WasTruncated: truncated}, nil
}

func (s *AIAssistantService) ValidateSkill(ctx context.Context, content string, authorization ActionAuthorization) (AISkillValidationView, error) {
	if err := requireActionActor(authorization); err != nil {
		return AISkillValidationView{}, err
	}
	content, err := boundedRedactedInput(content, maxApplicationContentRunes, "Skill content", true)
	if err != nil {
		return AISkillValidationView{}, err
	}
	if s.provider == nil {
		return AISkillValidationView{}, validationError("ai_provider_unavailable", "AI provider is unavailable")
	}
	result, err := s.provider.ValidateSkill(ctx, content)
	if err != nil {
		return AISkillValidationView{}, safeBackendError("AI skill validation failed", err)
	}
	issues, issuesTruncated, _ := boundedRedactedStrings(result.Issues, 50, 1000)
	suggestions, suggestionsTruncated, _ := boundedRedactedStrings(result.Suggestions, 50, 1000)
	summary, summaryTruncated := boundedRedactedOutput(result.Summary, maxApplicationSummaryRunes)
	return AISkillValidationView{Valid: result.Valid, Issues: issues, Suggestions: suggestions, Summary: summary, WasTruncated: issuesTruncated || suggestionsTruncated || summaryTruncated}, nil
}

func (s *AIAssistantService) ExecuteTool(ctx context.Context, request AIToolExecutionRequest) (AIToolExecutionView, error) {
	if err := requireExplicitActionApproval(request.Authorization); err != nil {
		return AIToolExecutionView{}, err
	}
	request.Name = strings.TrimSpace(request.Name)
	if !regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`).MatchString(request.Name) {
		return AIToolExecutionView{}, validationError("invalid_ai_tool_name", "AI tool name is invalid")
	}
	if request.ConversationID < 0 {
		return AIToolExecutionView{}, validationError("invalid_conversation_id", "Conversation ID cannot be negative")
	}
	arguments, err := sanitizeBoundedJSONMap(request.Arguments)
	if err != nil {
		return AIToolExecutionView{}, err
	}
	request.Arguments = arguments
	if s.tools == nil {
		return AIToolExecutionView{}, validationError("ai_tool_backend_unavailable", "AI tool execution backend is unavailable")
	}
	result, err := s.tools.ExecuteAITool(ctx, request)
	if err != nil {
		return AIToolExecutionView{}, safeBackendError("AI tool execution failed", err)
	}
	output, truncated := boundedRedactedOutput(result.Output, maxApplicationOutputRunes)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "tools", Action: "ai_tool_executed", ID: request.ConversationID, Meta: map[string]any{"tool": request.Name, "exit_code": result.ExitCode}})
	return AIToolExecutionView{Status: "completed", Output: output, ExitCode: result.ExitCode, WasTruncated: truncated}, nil
}

func (s *AIAssistantService) AcceptSuggestion(ctx context.Context, id int64, authorization ActionAuthorization) (AISuggestionAcceptance, error) {
	if err := requireActionActor(authorization); err != nil {
		return AISuggestionAcceptance{}, err
	}
	record, err := s.pendingSuggestion(ctx, id)
	if err != nil {
		return AISuggestionAcceptance{}, err
	}
	if record.Risk == ProposalRiskR3 || record.Risk == ProposalRiskR4 {
		if err = requireExplicitActionApproval(authorization); err != nil {
			return AISuggestionAcceptance{}, err
		}
	}
	result, err := s.conversations.AcceptSuggestionAtomic(ctx, id, authorization)
	if err != nil {
		return AISuggestionAcceptance{}, safeBackendError("AI suggestion acceptance failed", err)
	}
	result.Status = defaultStatus(result.Status, "accepted")
	result.Status, _ = boundedRedactedOutput(result.Status, 100)
	result.Message, _ = boundedRedactedOutput(result.Message, maxApplicationSummaryRunes)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "suggestion_accepted", ID: id})
	return result, nil
}

func (s *AIAssistantService) DismissSuggestion(ctx context.Context, id int64, authorization ActionAuthorization) error {
	if err := requireActionActor(authorization); err != nil {
		return err
	}
	if _, err := s.pendingSuggestion(ctx, id); err != nil {
		return err
	}
	if err := s.conversations.DismissSuggestionAtomic(ctx, id, authorization); err != nil {
		return safeBackendError("AI suggestion dismissal failed", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "suggestion_dismissed", ID: id})
	return nil
}

func (s *AIAssistantService) DiscussSuggestion(ctx context.Context, id int64, authorization ActionAuthorization) (AIConversationView, error) {
	if err := requireActionActor(authorization); err != nil {
		return AIConversationView{}, err
	}
	if _, err := s.pendingSuggestion(ctx, id); err != nil {
		return AIConversationView{}, err
	}
	reference, err := s.conversations.DiscussSuggestionAtomic(ctx, id, authorization)
	if err != nil || reference == nil {
		return AIConversationView{}, safeBackendError("Suggestion discussion could not be created", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "suggestion_discussed", ID: id, Meta: map[string]any{"conversation_id": reference.ID}})
	return aiConversationView(reference), nil
}

func (s *AIAssistantService) MarkConversationRead(ctx context.Context, id int64, authorization ActionAuthorization) error {
	if err := requireActionActor(authorization); err != nil {
		return err
	}
	if err := validateBoundedID(id, "Conversation"); err != nil {
		return err
	}
	if s.conversations == nil {
		return validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	if err := s.conversations.MarkConversationRead(ctx, id); err != nil {
		return safeBackendError("AI conversation could not be marked as read", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai", Action: "conversation_read", ID: id})
	return nil
}

func (s *AIAssistantService) pendingSuggestion(ctx context.Context, id int64) (*AISuggestionRecord, error) {
	if err := validateBoundedID(id, "Suggestion"); err != nil {
		return nil, err
	}
	if s.conversations == nil {
		return nil, validationError("ai_backend_unavailable", "AI conversation backend is unavailable")
	}
	record, err := s.conversations.GetSuggestion(ctx, id)
	if err != nil || record == nil {
		return nil, notFoundError("ai_suggestion_not_found", "AI suggestion not found", err)
	}
	if record.Status != "pending" {
		return nil, conflictError("ai_suggestion_already_processed", "AI suggestion has already been processed")
	}
	return record, nil
}

func aiConversationView(reference *AIConversationReference) AIConversationView {
	title, _ := boundedRedactedOutput(reference.Title, maxApplicationTitleRunes)
	source, _ := boundedRedactedOutput(reference.Source, 50)
	proactiveType, _ := boundedRedactedOutput(reference.ProactiveType, 100)
	return AIConversationView{ID: reference.ID, Title: title, Source: source, ProactiveType: proactiveType, IsRead: reference.IsRead}
}
