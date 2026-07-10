package automation

import (
	"context"

	"openpoet/internal/application"
)

func mcpPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("mcp.list", "mcp_servers", "mcp:read"),
		readConfigurationCapability("mcp.list_project", "mcp_servers", "mcp:read", "projects:read"),
		unsafeConfigurationCapability("mcp.create", "mcp_servers", "mcp:write", "credentials:write"),
		unsafeConfigurationCapability("mcp.update", "mcp_servers", "mcp:write", "credentials:write"),
		unsafeConfigurationCapability("mcp.delete", "mcp_servers", "mcp:write", "credentials:write"),
		unsafeConfigurationCapability("mcp.create_project", "mcp_servers", "mcp:write", "projects:write", "credentials:write"),
		unsafeConfigurationCapability("mcp.update_project", "mcp_servers", "mcp:write", "projects:write", "credentials:write"),
		unsafeConfigurationCapability("mcp.delete_project", "mcp_servers", "mcp:write", "projects:write", "credentials:write"),
	}
}

type mcpPlatformExecutor struct{ service *application.MCPService }

type mcpCreatePayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	Args      string `json:"args,omitempty"`
	Env       string `json:"env,omitempty"`
	Enabled   bool   `json:"enabled"`
}

type mcpUpdatePayload struct {
	ID        int64   `json:"id,omitempty"`
	ProjectID int64   `json:"project_id,omitempty"`
	Name      *string `json:"name,omitempty"`
	Command   *string `json:"command,omitempty"`
	Args      *string `json:"args,omitempty"`
	Env       *string `json:"env,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty"`
}

func mcpInput(payload mcpCreatePayload) application.MCPServerInput {
	return application.MCPServerInput{Name: payload.Name, Command: payload.Command, Args: payload.Args, Env: payload.Env, Enabled: payload.Enabled}
}

func mcpUpdateCommand(id int64, payload mcpUpdatePayload) application.UpdateMCPServerCommand {
	return application.UpdateMCPServerCommand{ID: id, Name: payload.Name, Command: payload.Command, Args: payload.Args, Env: payload.Env, Enabled: payload.Enabled}
}

func (e *mcpPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "mcp.list":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListGlobal(ctx)
		}}, nil
	case "mcp.list_project":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListProject(ctx, projectID)
		}}, nil
	case "mcp.create":
		var payload mcpCreatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		command := mcpInput(payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"has_command": payload.Command != "", "has_env": payload.Env != ""}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.CreateGlobal(ctx, auth, command)
		}}, nil
	case "mcp.update":
		var payload mcpUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "MCP server id")
		if err != nil {
			return nil, err
		}
		command := mcpUpdateCommand(id, payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"mcp_id": id, "updates_command": payload.Command != nil, "updates_env": payload.Env != nil}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.UpdateGlobal(ctx, auth, command)
		}}, nil
	case "mcp.delete":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "MCP server id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"mcp_id": id}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.DeleteGlobal(ctx, auth, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("mcp", id, 0), nil
		}}, nil
	case "mcp.create_project":
		var payload mcpCreatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		command := mcpInput(payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "has_command": payload.Command != "", "has_env": payload.Env != ""}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.CreateProject(ctx, auth, projectID, command)
		}}, nil
	case "mcp.update_project":
		var payload mcpUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "project MCP server id")
		if err != nil {
			return nil, err
		}
		command := mcpUpdateCommand(id, payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "mcp_id": id, "updates_command": payload.Command != nil, "updates_env": payload.Env != nil}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.UpdateProject(ctx, auth, projectID, command)
		}}, nil
	case "mcp.delete_project":
		var payload struct {
			ProjectID int64 `json:"project_id,omitempty"`
		}
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "project MCP server id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "mcp_id": id}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.DeleteProject(ctx, auth, projectID, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("project_mcp", id, projectID), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the MCP capability handler is unsupported", false)
	}
}

func customToolPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("tools.list_project", "custom_tools", "tools:read", "projects:read"),
		unsafeConfigurationCapability("tools.create_project", "custom_tools", "tools:write", "projects:write"),
		unsafeConfigurationCapability("tools.update_project", "custom_tools", "tools:write", "projects:write"),
		unsafeConfigurationCapability("tools.delete_project", "custom_tools", "tools:write", "projects:write"),
	}
}

type customToolPlatformExecutor struct {
	service *application.CustomToolService
}

type customToolCreatePayload struct {
	ProjectID   int64  `json:"project_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Command     string `json:"command"`
	Parameters  string `json:"parameters,omitempty"`
	Confirm     bool   `json:"confirm"`
	WorkingDir  string `json:"working_dir,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type customToolUpdatePayload struct {
	ID          int64   `json:"id,omitempty"`
	ProjectID   int64   `json:"project_id,omitempty"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Command     *string `json:"command,omitempty"`
	Parameters  *string `json:"parameters,omitempty"`
	Confirm     *bool   `json:"confirm,omitempty"`
	WorkingDir  *string `json:"working_dir,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

func (e *customToolPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "tools.list_project":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.List(ctx, projectID)
		}}, nil
	case "tools.create_project":
		var payload customToolCreatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		command := application.CustomToolInput{Name: payload.Name, Description: payload.Description, Command: payload.Command, Parameters: payload.Parameters, Confirm: payload.Confirm, WorkingDir: payload.WorkingDir, Enabled: payload.Enabled}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "has_command": payload.Command != ""}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.Create(ctx, auth, projectID, command)
		}}, nil
	case "tools.update_project":
		var payload customToolUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "custom tool id")
		if err != nil {
			return nil, err
		}
		command := application.UpdateCustomToolCommand{ID: id, Name: payload.Name, Description: payload.Description, Command: payload.Command, Parameters: payload.Parameters, Confirm: payload.Confirm, WorkingDir: payload.WorkingDir, Enabled: payload.Enabled}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "tool_id": id, "updates_command": payload.Command != nil}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			return e.service.Update(ctx, auth, projectID, command)
		}}, nil
	case "tools.delete_project":
		var payload struct {
			ProjectID int64 `json:"project_id,omitempty"`
		}
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "custom tool id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "tool_id": id}), execute: func(ctx context.Context, auth application.ActionAuthorization) (any, error) {
			if err := e.service.Delete(ctx, auth, projectID, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("custom_tool", id, projectID), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the custom tool capability handler is unsupported", false)
	}
}
