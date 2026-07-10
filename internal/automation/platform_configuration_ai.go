package automation

import (
	"context"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func agentPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("agents.list", "ai_agents", "agents:read"),
		writeConfigurationCapability("agents.create", "ai_agents", "agents:write"),
		writeConfigurationCapability("agents.update", "ai_agents", "agents:write"),
		destructiveConfigurationCapability("agents.delete", "ai_agents", "agents:write"),
	}
}

type agentPlatformExecutor struct{ service *application.AIAgentService }

type agentCreatePayload struct {
	Name          string `json:"name"`
	SystemPrompt  string `json:"system_prompt"`
	ToolPolicy    string `json:"tool_policy,omitempty"`
	ProjectFilter string `json:"project_filter,omitempty"`
	Enabled       bool   `json:"enabled"`
}

type agentUpdatePayload struct {
	ID            int64   `json:"id,omitempty"`
	Name          *string `json:"name,omitempty"`
	SystemPrompt  *string `json:"system_prompt,omitempty"`
	ToolPolicy    *string `json:"tool_policy,omitempty"`
	ProjectFilter *string `json:"project_filter,omitempty"`
	Enabled       *bool   `json:"enabled,omitempty"`
}

func (e *agentPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "agents.list":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) { return e.service.List(ctx) }}, nil
	case "agents.create":
		var payload agentCreatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		command := application.AIAgentInput{Name: payload.Name, SystemPrompt: payload.SystemPrompt, ToolPolicy: payload.ToolPolicy, ProjectFilter: payload.ProjectFilter, Enabled: payload.Enabled}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Create(ctx, command)
		}}, nil
	case "agents.update":
		var payload agentUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "agent id")
		if err != nil {
			return nil, err
		}
		command := application.UpdateAIAgentCommand{ID: id, Name: payload.Name, SystemPrompt: payload.SystemPrompt, ToolPolicy: payload.ToolPolicy, ProjectFilter: payload.ProjectFilter, Enabled: payload.Enabled}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"agent_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Update(ctx, command)
		}}, nil
	case "agents.delete":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "agent id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"agent_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.Delete(ctx, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("agent", id, 0), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the AI agent capability handler is unsupported", false)
	}
}

type AIConfigAutomationView struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	ProviderType  string    `json:"provider_type"`
	APIKeyPreview string    `json:"api_key_preview,omitempty"`
	HasAPIKey     bool      `json:"has_api_key"`
	Model         string    `json:"model"`
	BaseURL       string    `json:"base_url"`
	ExtraJSON     string    `json:"extra_json"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func aiConfigAutomationView(item database.AIConfig) AIConfigAutomationView {
	return AIConfigAutomationView{
		ID: item.ID, Name: item.Name, ProviderType: item.ProviderType,
		APIKeyPreview: item.APIKeyPreview, HasAPIKey: item.APIKeyEncrypted != "",
		Model: item.Model, BaseURL: item.BaseURL, ExtraJSON: item.ExtraJSON,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func aiConfigAutomationViews(items []database.AIConfig) []AIConfigAutomationView {
	result := make([]AIConfigAutomationView, len(items))
	for i := range items {
		result[i] = aiConfigAutomationView(items[i])
	}
	return result
}

func aiConfigPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("ai_configs.list", "ai_configs", "ai_configs:read"),
		readConfigurationCapability("ai_configs.list_assignments", "ai_configs", "ai_configs:read"),
		unsafeConfigurationCapability("ai_configs.create", "ai_configs", "ai_configs:write", "credentials:write"),
		unsafeConfigurationCapability("ai_configs.update", "ai_configs", "ai_configs:write", "credentials:write"),
		unsafeConfigurationCapability("ai_configs.delete", "ai_configs", "ai_configs:write", "credentials:write"),
		unsafeConfigurationCapability("ai_configs.assign", "ai_configs", "ai_configs:write"),
	}
}

type aiConfigPlatformExecutor struct{ service *application.AIConfigService }

type aiConfigCreatePayload struct {
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	APIKey       string `json:"api_key,omitempty"`
	Model        string `json:"model,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	ExtraJSON    string `json:"extra_json,omitempty"`
}

type aiConfigUpdatePayload struct {
	ID           int64   `json:"id,omitempty"`
	Name         *string `json:"name,omitempty"`
	ProviderType *string `json:"provider_type,omitempty"`
	APIKey       *string `json:"api_key,omitempty"`
	Model        *string `json:"model,omitempty"`
	BaseURL      *string `json:"base_url,omitempty"`
	ExtraJSON    *string `json:"extra_json,omitempty"`
}

type aiConfigAssignmentsPayload struct {
	Assignments map[string]*int64 `json:"assignments"`
}

func (e *aiConfigPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "ai_configs.list":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.service.List(ctx)
			if err != nil {
				return nil, err
			}
			return aiConfigAutomationViews(items), nil
		}}, nil
	case "ai_configs.list_assignments":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListAssignments(ctx)
		}}, nil
	case "ai_configs.create":
		var payload aiConfigCreatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		command := application.AIConfigInput{Name: payload.Name, ProviderType: payload.ProviderType, APIKey: payload.APIKey, Model: payload.Model, BaseURL: payload.BaseURL, ExtraJSON: payload.ExtraJSON}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"has_api_key": payload.APIKey != ""}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			item, err := e.service.Create(ctx, auth, command)
			if err != nil {
				return nil, err
			}
			return aiConfigAutomationView(*item), nil
		}}, nil
	case "ai_configs.update":
		var payload aiConfigUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "AI config id")
		if err != nil {
			return nil, err
		}
		command := application.UpdateAIConfigCommand{ID: id, Name: payload.Name, ProviderType: payload.ProviderType, APIKey: payload.APIKey, Model: payload.Model, BaseURL: payload.BaseURL, ExtraJSON: payload.ExtraJSON}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"ai_config_id": id, "updates_api_key": payload.APIKey != nil && *payload.APIKey != ""}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			item, err := e.service.Update(ctx, auth, command)
			if err != nil {
				return nil, err
			}
			return aiConfigAutomationView(*item), nil
		}}, nil
	case "ai_configs.delete":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "AI config id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"ai_config_id": id}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.Delete(ctx, auth, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("ai_config", id, 0), nil
		}}, nil
	case "ai_configs.assign":
		var payload aiConfigAssignmentsPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if len(payload.Assignments) > 20 {
			return nil, platformFailure("platform_payload_invalid", "AI config assignments exceed 20 slots", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"assignment_count": len(payload.Assignments)}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.Assign(ctx, auth, payload.Assignments); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true, "assignment_count": len(payload.Assignments)}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the AI config capability handler is unsupported", false)
	}
}
