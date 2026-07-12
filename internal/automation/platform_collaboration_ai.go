package automation

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type aiCollaborationExecutor struct {
	service *application.AIAssistantService
	queries AICollaborationReadPort
}

type aiListPayload struct {
	Limit int `json:"limit,omitempty"`
}

type aiMessagesPayload struct {
	Limit    int `json:"limit,omitempty"`
	MaxBytes int `json:"max_bytes,omitempty"`
}

type aiConnectionPayload struct {
	ProviderType string `json:"provider_type"`
	APIKey       string `json:"api_key,omitempty"`
	Model        string `json:"model,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	ConfigID     *int64 `json:"config_id,omitempty"`
}

type aiChatPayload struct {
	ConversationID int64  `json:"conversation_id,omitempty"`
	ProjectID      int64  `json:"project_id,omitempty"`
	AgentID        *int64 `json:"agent_id,omitempty"`
	Prompt         string `json:"prompt"`
}

type aiProjectPayload struct {
	ProjectID int64 `json:"project_id,omitempty"`
}

type aiTaskCreationPayload struct {
	ProjectID   int64  `json:"project_id,omitempty"`
	ParentID    int64  `json:"parent_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
	Priority    string `json:"priority,omitempty"`
	DueDate     string `json:"due_date,omitempty"`
}

type aiTaskDiscussionPayload struct {
	ProjectID int64 `json:"project_id,omitempty"`
	TaskID    int64 `json:"task_id,omitempty"`
}

type aiSkillCustomizationPayload struct {
	ProjectID int64 `json:"project_id,omitempty"`
	SkillID   int64 `json:"skill_id,omitempty"`
}

type aiTextPayload struct {
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
}

type aiToolPayload struct {
	Name           string         `json:"name"`
	Arguments      map[string]any `json:"arguments,omitempty"`
	ConversationID int64          `json:"conversation_id,omitempty"`
}

type aiProactivePayload struct {
	Level string `json:"level,omitempty"`
}

type aiConversationAutomationView struct {
	ID             int64     `json:"id"`
	Title          string    `json:"title,omitempty"`
	Source         string    `json:"source,omitempty"`
	ProactiveLevel string    `json:"proactive_level,omitempty"`
	ProactiveType  string    `json:"proactive_type,omitempty"`
	IsRead         bool      `json:"is_read"`
	AgentID        *int64    `json:"agent_id,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
	WasTruncated   bool      `json:"was_truncated,omitempty"`
}

type aiMessageAutomationView struct {
	ID             int64     `json:"id"`
	ConversationID int64     `json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	HasToolCalls   bool      `json:"has_tool_calls"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	WasTruncated   bool      `json:"was_truncated,omitempty"`
}

type aiSuggestionAutomationView struct {
	ID             int64     `json:"id"`
	SessionID      string    `json:"session_id,omitempty"`
	ProjectID      int64     `json:"project_id"`
	Type           string    `json:"type"`
	Title          string    `json:"title,omitempty"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	Level          string    `json:"level,omitempty"`
	ConversationID *int64    `json:"conversation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	WasTruncated   bool      `json:"was_truncated,omitempty"`
}

type aiActionAutomationView struct {
	Status         string   `json:"status"`
	ConversationID int64    `json:"conversation_id,omitempty"`
	Title          string   `json:"title,omitempty"`
	Source         string   `json:"source,omitempty"`
	ProactiveType  string   `json:"proactive_type,omitempty"`
	Output         string   `json:"output,omitempty"`
	Content        string   `json:"content,omitempty"`
	Model          string   `json:"model,omitempty"`
	Message        string   `json:"message,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	Configured     bool     `json:"configured,omitempty"`
	ExitCode       int      `json:"exit_code,omitempty"`
	Valid          *bool    `json:"valid,omitempty"`
	Issues         []string `json:"issues,omitempty"`
	Suggestions    []string `json:"suggestions,omitempty"`
	WasTruncated   bool     `json:"was_truncated,omitempty"`
}

type aiStatusAutomationView struct {
	UnreadProactive    int `json:"unread_proactive"`
	PendingSuggestions int `json:"pending_suggestions"`
}

func aiCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationReadCapability("ai.conversations.list", "ai_assistant", "ai:read"),
		collaborationReadCapability("ai.conversations.get", "ai_assistant", "ai:read"),
		collaborationReadCapability("ai.messages.list", "ai_assistant", "ai:read"),
		collaborationReadCapability("ai.suggestions.list", "ai_assistant", "ai:read"),
		collaborationReadCapability("ai.suggestions.get", "ai_assistant", "ai:read"),
		collaborationReadCapability("ai.status", "ai_assistant", "ai:read"),
		collaborationPayloadLimit(platformMutation(collaborationReadCapability("ai.test_connection", "ai_assistant", "ai:use", "credentials:use")), 64<<10),
		collaborationPayloadLimit(collaborationWriteCapability("ai.chat", "ai_assistant", "ai:use"), 256<<10),
		collaborationDestructiveCapability("ai.delete_all_conversations", "ai_assistant", "ai:write"),
		collaborationDestructiveCapability("ai.delete_conversation", "ai_assistant", "ai:write"),
		collaborationWriteCapability("ai.stop_conversation", "ai_assistant", "ai:write"),
		collaborationWriteCapability("ai.initiate_memory_edit", "ai_assistant", "ai:use", "documents:write"),
		collaborationPayloadLimit(collaborationWriteCapability("ai.initiate_task_creation", "ai_assistant", "ai:use", "tasks:write"), 64<<10),
		collaborationWriteCapability("ai.initiate_task_discussion", "ai_assistant", "ai:use", "tasks:read"),
		collaborationWriteCapability("ai.initiate_skill_customization", "ai_assistant", "ai:use", "skills:write"),
		collaborationPayloadLimit(collaborationWriteCapability("ai.generate_skill", "ai_assistant", "ai:use", "skills:write"), 256<<10),
		collaborationPayloadLimit(platformMutation(collaborationReadCapability("ai.validate_skill", "ai_assistant", "ai:use", "skills:read")), 512<<10),
		collaborationPayloadLimit(collaborationUnsafeCapability("ai.execute_tool", "ai_assistant", "ai:use", "tools:execute"), 256<<10),
		collaborationWriteCapability("ai.accept_suggestion", "ai_assistant", "ai:write"),
		platformMutation(collaborationReadCapability("ai.dismiss_suggestion", "ai_assistant", "ai:write")),
		collaborationWriteCapability("ai.discuss_suggestion", "ai_assistant", "ai:use"),
		platformMutation(collaborationReadCapability("ai.mark_conversation_read", "ai_assistant", "ai:write")),
		collaborationWriteCapability("ai.test_proactive", "ai_assistant", "ai:write"),
	}
}

func (e *aiCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeCollaborationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "ai.conversations.list":
		var payload aiListPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if err := normalizeAIListLimit(&payload.Limit); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.queries.ListAIConversations(ctx)
			if err != nil {
				return nil, err
			}
			if len(items) > payload.Limit {
				items = items[:payload.Limit]
			}
			views := make([]aiConversationAutomationView, len(items))
			for i := range items {
				views[i] = aiConversationFromDatabase(items[i])
			}
			return views, nil
		}}, nil
	case "ai.conversations.get":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationIntegerID(target, 0, "conversation id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"conversation_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			item, err := e.queries.GetAIConversation(ctx, id)
			if err != nil {
				return nil, err
			}
			if item == nil {
				return nil, platformFailure("ai_conversation_not_found", "AI conversation not found", false)
			}
			return aiConversationFromDatabase(*item), nil
		}}, nil
	case "ai.messages.list":
		id, err := collaborationIntegerID(target, 0, "conversation id")
		if err != nil {
			return nil, err
		}
		var payload aiMessagesPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if err := normalizeAIListLimit(&payload.Limit); err != nil {
			return nil, err
		}
		if payload.MaxBytes == 0 {
			payload.MaxBytes = 32 << 10
		}
		if payload.MaxBytes < 1 || payload.MaxBytes > maxCollaborationMessageBytes {
			return nil, platformFailure("platform_payload_invalid", "AI message max_bytes is invalid", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"conversation_id": id, "limit": payload.Limit, "max_bytes": payload.MaxBytes}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.queries.ListAIMessages(ctx, id)
			if err != nil {
				return nil, err
			}
			if len(items) > payload.Limit {
				items = items[len(items)-payload.Limit:]
			}
			return aiMessagesForAutomation(items, payload.MaxBytes), nil
		}}, nil
	case "ai.suggestions.list":
		var payload aiListPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if err := normalizeAIListLimit(&payload.Limit); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.queries.ListPendingAISuggestions(ctx)
			if err != nil {
				return nil, err
			}
			if len(items) > payload.Limit {
				items = items[:payload.Limit]
			}
			views := make([]aiSuggestionAutomationView, len(items))
			for i := range items {
				views[i] = aiSuggestionFromDatabase(items[i])
			}
			return views, nil
		}}, nil
	case "ai.suggestions.get":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationIntegerID(target, 0, "suggestion id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"suggestion_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			item, err := e.queries.GetAISuggestion(ctx, id)
			if err != nil {
				return nil, err
			}
			if item == nil {
				return nil, platformFailure("ai_suggestion_not_found", "AI suggestion not found", false)
			}
			return aiSuggestionFromDatabase(*item), nil
		}}, nil
	case "ai.status":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			unread, err := e.queries.CountUnreadProactive(ctx)
			if err != nil {
				return nil, err
			}
			pending, err := e.queries.ListPendingAISuggestions(ctx)
			if err != nil {
				return nil, err
			}
			return aiStatusAutomationView{UnreadProactive: max(0, unread), PendingSuggestions: min(len(pending), maxCollaborationListItems)}, nil
		}}, nil
	case "ai.test_connection":
		var payload aiConnectionPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.ProviderType, err = boundedCollaborationInput(payload.ProviderType, "provider type", 100, true); err != nil {
			return nil, err
		}
		if payload.Model, err = boundedCollaborationInput(payload.Model, "model", 200, false); err != nil {
			return nil, err
		}
		if payload.BaseURL, err = boundedCollaborationInput(payload.BaseURL, "base URL", 2048, false); err != nil {
			return nil, err
		}
		if utf8RuneCount(payload.APIKey) > 16<<10 || hasCollaborationControlRune(payload.APIKey) {
			return nil, platformFailure("platform_payload_invalid", "API key exceeds its bounded limit", false)
		}
		if payload.ConfigID != nil && *payload.ConfigID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "AI config id must be positive", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"provider_type": payload.ProviderType, "has_api_key": strings.TrimSpace(payload.APIKey) != "", "has_config": payload.ConfigID != nil,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.TestConnection(ctx, application.AIConnectionTestRequest{
				ProviderType: payload.ProviderType, APIKey: payload.APIKey, Model: payload.Model, BaseURL: payload.BaseURL, ConfigID: payload.ConfigID,
			}, authorization)
			if err != nil {
				return nil, err
			}
			return aiConnectionForAutomation(view), nil
		}}, nil
	case "ai.chat":
		var payload aiChatPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.ConversationID < 0 || payload.ProjectID < 0 || payload.AgentID != nil && *payload.AgentID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "AI chat references are invalid", false)
		}
		if payload.Prompt, err = boundedCollaborationInput(payload.Prompt, "AI prompt", maxCollaborationInputRunes, true); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"conversation_id": payload.ConversationID, "project_id": payload.ProjectID, "prompt_bytes": len(payload.Prompt), "has_agent": payload.AgentID != nil,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.Chat(ctx, application.AIChatCommand{
				ConversationID: payload.ConversationID, ProjectID: payload.ProjectID, AgentID: payload.AgentID,
				Prompt: payload.Prompt, Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return aiChatForAutomation(view), nil
		}}, nil
	case "ai.delete_all_conversations":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			deleted, err := e.service.DeleteAllConversations(ctx, authorization)
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": max(int64(0), deleted)}, nil
		}}, nil
	case "ai.delete_conversation", "ai.stop_conversation", "ai.mark_conversation_read":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationIntegerID(target, 0, "conversation id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"conversation_id": id}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			var err error
			switch input.Handler {
			case "ai.delete_conversation":
				err = e.service.DeleteConversation(ctx, id, authorization)
			case "ai.stop_conversation":
				err = e.service.StopConversation(ctx, id, authorization)
			case "ai.mark_conversation_read":
				err = e.service.MarkConversationRead(ctx, id, authorization)
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"conversation_id": id, "status": aiConversationMutationStatus(input.Handler)}, nil
		}}, nil
	case "ai.initiate_memory_edit":
		var payload aiProjectPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := collaborationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.InitiateMemoryDocEdit(ctx, projectID, authorization)
			if err != nil {
				return nil, err
			}
			return aiConversationFromApplication(view, "initiated"), nil
		}}, nil
	case "ai.initiate_task_creation":
		var payload aiTaskCreationPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := collaborationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		if payload.ParentID < 0 {
			return nil, platformFailure("platform_payload_invalid", "parent id cannot be negative", false)
		}
		if payload.Title, err = boundedCollaborationInput(payload.Title, "task title", 1000, false); err != nil {
			return nil, err
		}
		if payload.Description, err = boundedCollaborationInput(payload.Description, "task description", 16<<10, false); err != nil {
			return nil, err
		}
		for label, value := range map[string]string{"status": payload.Status, "priority": payload.Priority, "due date": payload.DueDate} {
			if _, err := boundedCollaborationInput(value, label, 100, false); err != nil {
				return nil, err
			}
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"project_id": projectID, "has_parent": payload.ParentID > 0}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.InitiateTaskCreation(ctx, application.AITaskCreationCommand{
				ProjectID: projectID, ParentID: payload.ParentID, Title: payload.Title, Description: payload.Description,
				Status: payload.Status, Priority: payload.Priority, DueDate: payload.DueDate, Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return aiConversationFromApplication(view, "initiated"), nil
		}}, nil
	case "ai.initiate_task_discussion":
		var payload aiTaskDiscussionPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID := target.ProjectID
		if projectID == 0 {
			projectID = payload.ProjectID
		}
		taskID, err := collaborationIntegerID(target, payload.TaskID, "task id")
		if err != nil || projectID <= 0 {
			return nil, platformFailure("platform_target_invalid", "project id and task id are required", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"project_id": projectID, "task_id": taskID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.InitiateTaskDiscussion(ctx, projectID, taskID, authorization)
			if err != nil {
				return nil, err
			}
			return aiConversationFromApplication(view, "initiated"), nil
		}}, nil
	case "ai.initiate_skill_customization":
		var payload aiSkillCustomizationPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID := target.ProjectID
		if projectID == 0 {
			projectID = payload.ProjectID
		}
		skillID, err := collaborationIntegerID(target, payload.SkillID, "skill id")
		if err != nil || projectID <= 0 {
			return nil, platformFailure("platform_target_invalid", "project id and skill id are required", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"project_id": projectID, "skill_id": skillID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.InitiateSkillCustomization(ctx, projectID, skillID, authorization)
			if err != nil {
				return nil, err
			}
			return aiConversationFromApplication(view, "initiated"), nil
		}}, nil
	case "ai.generate_skill", "ai.validate_skill":
		var payload aiTextPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		field := payload.Description
		label := "skill description"
		if input.Handler == "ai.validate_skill" {
			field, label = payload.Content, "skill content"
		}
		field, err = boundedCollaborationInput(field, label, maxCollaborationInputRunes, true)
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"input_bytes": len(field)}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if input.Handler == "ai.generate_skill" {
				view, err := e.service.GenerateSkill(ctx, field, authorization)
				if err != nil {
					return nil, err
				}
				return aiGeneratedSkillForAutomation(view), nil
			}
			view, err := e.service.ValidateSkill(ctx, field, authorization)
			if err != nil {
				return nil, err
			}
			return aiSkillValidationForAutomation(view), nil
		}}, nil
	case "ai.execute_tool":
		var payload aiToolPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.Name, err = boundedCollaborationInput(payload.Name, "tool name", 200, true); err != nil {
			return nil, err
		}
		if payload.ConversationID < 0 || len(payload.Arguments) > 100 {
			return nil, platformFailure("platform_payload_invalid", "AI tool references or arguments are invalid", false)
		}
		encodedArguments, err := json.Marshal(payload.Arguments)
		if err != nil || len(encodedArguments) > 128<<10 {
			return nil, platformFailure("platform_payload_invalid", "AI tool arguments exceed their bounded limit", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"tool": payload.Name, "conversation_id": payload.ConversationID, "argument_count": len(payload.Arguments),
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.ExecuteTool(ctx, application.AIToolExecutionRequest{
				Name: payload.Name, Arguments: payload.Arguments, ConversationID: payload.ConversationID, Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return aiToolExecutionForAutomation(view), nil
		}}, nil
	case "ai.accept_suggestion", "ai.dismiss_suggestion", "ai.discuss_suggestion":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationIntegerID(target, 0, "suggestion id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"suggestion_id": id}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			switch input.Handler {
			case "ai.accept_suggestion":
				view, err := e.service.AcceptSuggestion(ctx, id, authorization)
				if err != nil {
					return nil, err
				}
				status, statusTruncated := boundedCollaborationOutput(view.Status, 100)
				message, messageTruncated := boundedCollaborationOutput(view.Message, maxCollaborationSummaryBytes)
				return aiActionAutomationView{Status: status, Message: message, WasTruncated: statusTruncated || messageTruncated}, nil
			case "ai.dismiss_suggestion":
				if err := e.service.DismissSuggestion(ctx, id, authorization); err != nil {
					return nil, err
				}
				return aiActionAutomationView{Status: "dismissed"}, nil
			default:
				view, err := e.service.DiscussSuggestion(ctx, id, authorization)
				if err != nil {
					return nil, err
				}
				return aiConversationFromApplication(view, "discussion_started"), nil
			}
		}}, nil
	case "ai.test_proactive":
		var payload aiProactivePayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.Level, err = boundedCollaborationInput(payload.Level, "proactive level", 100, false); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"level": payload.Level}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.TestProactive(ctx, payload.Level, authorization)
			if err != nil {
				return nil, err
			}
			return aiConversationFromApplication(view, "initiated"), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the AI capability handler is unsupported", false)
	}
}

func normalizeAIListLimit(limit *int) error {
	if *limit == 0 {
		*limit = 50
	}
	if *limit < 1 || *limit > maxCollaborationListItems {
		return platformFailure("platform_payload_invalid", "AI list limit is invalid", false)
	}
	return nil
}

func aiConversationFromDatabase(item database.AIConversation) aiConversationAutomationView {
	title, titleTruncated := boundedCollaborationOutput(item.Title, 1000)
	source, sourceTruncated := boundedCollaborationOutput(item.Source, 100)
	level, levelTruncated := boundedCollaborationOutput(item.ProactiveLevel, 100)
	proactiveType, typeTruncated := boundedCollaborationOutput(item.ProactiveType, 100)
	var agentID *int64
	if item.AgentID.Valid {
		value := item.AgentID.Int64
		agentID = &value
	}
	return aiConversationAutomationView{
		ID: item.ID, Title: title, Source: source, ProactiveLevel: level, ProactiveType: proactiveType,
		IsRead: item.IsRead, AgentID: agentID, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		WasTruncated: titleTruncated || sourceTruncated || levelTruncated || typeTruncated,
	}
}

func aiConversationFromApplication(item application.AIConversationView, status string) aiActionAutomationView {
	title, titleTruncated := boundedCollaborationOutput(item.Title, 1000)
	source, sourceTruncated := boundedCollaborationOutput(item.Source, 100)
	proactiveType, typeTruncated := boundedCollaborationOutput(item.ProactiveType, 100)
	return aiActionAutomationView{
		Status: status, ConversationID: item.ID, Title: title, Source: source, ProactiveType: proactiveType,
		WasTruncated: titleTruncated || sourceTruncated || typeTruncated,
	}
}

func aiMessagesForAutomation(items []database.AIMessage, maximumBytes int) []aiMessageAutomationView {
	views := make([]aiMessageAutomationView, 0, len(items))
	remaining := maximumBytes
	for _, item := range items {
		if remaining <= 0 {
			break
		}
		content, contentTruncated := boundedCollaborationOutput(item.Content, min(remaining, 16<<10))
		role, roleTruncated := boundedCollaborationOutput(item.Role, 50)
		status, statusTruncated := boundedCollaborationOutput(item.Status, 100)
		errorInfo, errorTruncated := boundedCollaborationOutput(item.ErrorInfo, min(remaining, 2000))
		remaining -= len(content) + len(errorInfo)
		views = append(views, aiMessageAutomationView{
			ID: item.ID, ConversationID: item.ConversationID, Role: role, Content: content, Status: status,
			Error: errorInfo, HasToolCalls: strings.TrimSpace(item.ToolCalls) != "" && strings.TrimSpace(item.ToolCalls) != "[]",
			CreatedAt: item.CreatedAt, WasTruncated: contentTruncated || roleTruncated || statusTruncated || errorTruncated,
		})
	}
	return views
}

func aiSuggestionFromDatabase(item database.AISuggestion) aiSuggestionAutomationView {
	sessionID, sessionTruncated := boundedCollaborationOutput(item.SessionID, 200)
	typeName, typeTruncated := boundedCollaborationOutput(item.Type, 100)
	title, titleTruncated := boundedCollaborationOutput(item.Title, 1000)
	description, descriptionTruncated := boundedCollaborationOutput(item.Description, maxCollaborationSummaryBytes)
	status, statusTruncated := boundedCollaborationOutput(item.Status, 100)
	level, levelTruncated := boundedCollaborationOutput(item.Level, 100)
	var conversationID *int64
	if item.ConversationID.Valid {
		value := item.ConversationID.Int64
		conversationID = &value
	}
	return aiSuggestionAutomationView{
		ID: item.ID, SessionID: sessionID, ProjectID: item.ProjectID, Type: typeName, Title: title,
		Description: description, Status: status, Level: level, ConversationID: conversationID, CreatedAt: item.CreatedAt,
		WasTruncated: sessionTruncated || typeTruncated || titleTruncated || descriptionTruncated || statusTruncated || levelTruncated,
	}
}

func aiConnectionForAutomation(view application.AIConnectionTestResult) aiActionAutomationView {
	provider, providerTruncated := boundedCollaborationOutput(view.Provider, 100)
	model, modelTruncated := boundedCollaborationOutput(view.Model, 200)
	message, messageTruncated := boundedCollaborationOutput(view.Message, maxCollaborationSummaryBytes)
	return aiActionAutomationView{Status: "tested", Provider: provider, Model: model, Message: message, Configured: view.Configured, WasTruncated: providerTruncated || modelTruncated || messageTruncated}
}

func aiChatForAutomation(view application.AIChatResult) aiActionAutomationView {
	output, outputTruncated := boundedCollaborationOutput(view.Output, maxCollaborationOutputBytes)
	model, modelTruncated := boundedCollaborationOutput(view.Model, 200)
	return aiActionAutomationView{Status: "completed", ConversationID: view.ConversationID, Output: output, Model: model, WasTruncated: view.WasTruncated || outputTruncated || modelTruncated}
}

func aiGeneratedSkillForAutomation(view application.AIGeneratedSkill) aiActionAutomationView {
	content, contentTruncated := boundedCollaborationOutput(view.Content, maxCollaborationOutputBytes)
	model, modelTruncated := boundedCollaborationOutput(view.Model, 200)
	return aiActionAutomationView{Status: "generated", Content: content, Model: model, WasTruncated: view.WasTruncated || contentTruncated || modelTruncated}
}

func aiSkillValidationForAutomation(view application.AISkillValidationView) aiActionAutomationView {
	issues, issuesTruncated := boundedCollaborationStrings(view.Issues, 50, maxCollaborationSummaryBytes)
	suggestions, suggestionsTruncated := boundedCollaborationStrings(view.Suggestions, 50, maxCollaborationSummaryBytes)
	summary, summaryTruncated := boundedCollaborationOutput(view.Summary, maxCollaborationSummaryBytes)
	valid := view.Valid
	return aiActionAutomationView{Status: "validated", Valid: &valid, Issues: issues, Suggestions: suggestions, Message: summary, WasTruncated: view.WasTruncated || issuesTruncated || suggestionsTruncated || summaryTruncated}
}

func aiToolExecutionForAutomation(view application.AIToolExecutionView) aiActionAutomationView {
	status, statusTruncated := boundedCollaborationOutput(view.Status, 100)
	output, outputTruncated := boundedCollaborationOutput(view.Output, maxCollaborationOutputBytes)
	return aiActionAutomationView{Status: status, Output: output, ExitCode: view.ExitCode, WasTruncated: view.WasTruncated || statusTruncated || outputTruncated}
}

func aiConversationMutationStatus(handler application.CapabilityHandler) string {
	switch handler {
	case "ai.delete_conversation":
		return "deleted"
	case "ai.stop_conversation":
		return "stopped"
	default:
		return "read"
	}
}

func utf8RuneCount(value string) int {
	return len([]rune(value))
}
