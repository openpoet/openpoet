package automation

import (
	"context"

	"openpoet/internal/application"
)

func configurationPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("settings.get", "configuration", "settings:read"),
		readConfigurationCapability("tools.get_policies", "configuration", "tools:read"),
		readConfigurationCapability("tools.get_project_policy", "configuration", "tools:read", "projects:read"),
		readConfigurationCapability("projects.get_shares", "configuration", "projects:read"),
		readConfigurationCapability("mcp_api_key.status", "configuration", "credentials:read"),
		unsafeConfigurationCapability("settings.update", "configuration", "settings:write"),
		unsafeConfigurationCapability("projects.sync_config", "configuration", "projects:write", "config:write"),
		unsafeConfigurationCapability("config.sync_all", "configuration", "config:write"),
		unsafeConfigurationCapability("mcp_api_key.generate", "configuration", "credentials:write"),
		unsafeConfigurationCapability("mcp_api_key.revoke", "configuration", "credentials:write"),
		unsafeConfigurationCapability("tools.update_policies", "configuration", "tools:policy"),
		unsafeConfigurationCapability("tools.update_project_policy", "configuration", "tools:policy", "projects:write"),
		writeConfigurationCapability("projects.update_shares", "configuration", "projects:write"),
	}
}

type configurationPlatformExecutor struct {
	service *application.ConfigurationService
}

type settingsUpdatePayload struct {
	Settings map[string]string `json:"settings"`
}
type toolPoliciesPayload struct {
	Policies map[string]string `json:"policies"`
}
type projectPolicyPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Policy    string `json:"policy"`
}
type projectSharesPayload struct {
	ProjectID        int64   `json:"project_id,omitempty"`
	SharedProjectIDs []int64 `json:"shared_project_ids"`
}

func (e *configurationPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "settings.get":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Settings(ctx)
		}}, nil
	case "tools.get_policies":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ToolPolicies(ctx)
		}}, nil
	case "tools.get_project_policy":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			policy, err := e.service.ProjectToolPolicy(ctx, projectID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"project_id": projectID, "policy": policy}, nil
		}}, nil
	case "projects.get_shares":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ProjectShares(ctx, projectID)
		}}, nil
	case "mcp_api_key.status":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.MCPAPIKeyStatus(ctx)
		}}, nil
	case "settings.update":
		var payload settingsUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if len(payload.Settings) == 0 || len(payload.Settings) > 200 {
			return nil, platformFailure("platform_payload_invalid", "settings update must contain between 1 and 200 settings", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"setting_count": len(payload.Settings)}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.UpdateSettings(ctx, auth, payload.Settings); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true, "setting_count": len(payload.Settings)}, nil
		}}, nil
	case "projects.sync_config":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.SyncProject(ctx, auth, projectID); err != nil {
				return nil, err
			}
			return map[string]any{"synced": true, "project_id": projectID}, nil
		}}, nil
	case "config.sync_all":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.SyncAll(ctx, auth); err != nil {
				return nil, err
			}
			return map[string]any{"synced": true}, nil
		}}, nil
	case "mcp_api_key.generate":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.GenerateMCPAPIKey(ctx, auth)
		}}, nil
	case "mcp_api_key.revoke":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.RevokeMCPAPIKey(ctx, auth); err != nil {
				return nil, err
			}
			return map[string]any{"revoked": true}, nil
		}}, nil
	case "tools.update_policies":
		var payload toolPoliciesPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if len(payload.Policies) == 0 || len(payload.Policies) > 3 {
			return nil, platformFailure("platform_payload_invalid", "tool policies must contain between 1 and 3 contexts", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"policy_count": len(payload.Policies)}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.UpdateToolPolicies(ctx, auth, payload.Policies); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true}, nil
		}}, nil
	case "tools.update_project_policy":
		var payload projectPolicyPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.UpdateProjectToolPolicy(ctx, auth, projectID, payload.Policy); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true, "project_id": projectID}, nil
		}}, nil
	case "projects.update_shares":
		var payload projectSharesPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		if len(payload.SharedProjectIDs) > 1000 {
			return nil, platformFailure("platform_payload_invalid", "project shares exceed 1000 entries", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "share_count": len(payload.SharedProjectIDs)}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.ReplaceProjectShares(ctx, projectID, payload.SharedProjectIDs); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true, "project_id": projectID, "share_count": len(payload.SharedProjectIDs)}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the configuration capability handler is unsupported", false)
	}
}
