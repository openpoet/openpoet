package automation

import (
	"context"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type ProjectAutomationView struct {
	ID                         int64  `json:"id"`
	Name                       string `json:"name"`
	Path                       string `json:"path"`
	Type                       string `json:"type"`
	SSHHost                    string `json:"ssh_host,omitempty"`
	SSHPort                    int64  `json:"ssh_port,omitempty"`
	SSHUser                    string `json:"ssh_user,omitempty"`
	SSHAuthType                string `json:"ssh_auth_type,omitempty"`
	HasCredential              bool   `json:"has_credential"`
	ToolPolicy                 string `json:"tool_policy,omitempty"`
	SkillPolicy                string `json:"skill_policy,omitempty"`
	DangerouslySkipPermissions bool   `json:"dangerously_skip_permissions"`
	Backend                    string `json:"backend"`
	HasBackendConfig           bool   `json:"has_backend_config"`
}

func projectAutomationView(project database.Project) ProjectAutomationView {
	return ProjectAutomationView{
		ID: project.ID, Name: project.Name, Path: project.Path, Type: project.Type,
		SSHHost: project.SSHHost.String, SSHPort: project.SSHPort.Int64,
		SSHUser: project.SSHUser.String, SSHAuthType: project.SSHAuthType.String,
		HasCredential: project.HasCredential || project.SSHCredentialEncrypted.Valid,
		ToolPolicy:    project.ToolPolicy, SkillPolicy: project.SkillPolicy,
		DangerouslySkipPermissions: project.DangerouslySkipPermissions,
		Backend:                    project.Backend, HasBackendConfig: project.BackendConfig != "" && project.BackendConfig != "{}",
	}
}

func projectAutomationViews(projects []database.Project) []ProjectAutomationView {
	result := make([]ProjectAutomationView, len(projects))
	for i := range projects {
		result[i] = projectAutomationView(projects[i])
	}
	return result
}

func projectPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("projects.list", "projects", "projects:read"),
		readConfigurationCapability("projects.get", "projects", "projects:read"),
		unsafeConfigurationCapability("projects.create", "projects", "projects:write", "credentials:write"),
		unsafeConfigurationCapability("projects.update", "projects", "projects:write", "credentials:write"),
		destructiveConfigurationCapability("projects.delete", "projects", "projects:write"),
		unsafeConfigurationCapability("projects.duplicate", "projects", "projects:write", "credentials:write"),
	}
}

type projectPlatformExecutor struct{ service *application.ProjectService }

func (e *projectPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "projects.list":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, nil),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				projects, err := e.service.List(ctx)
				if err != nil {
					return nil, err
				}
				return projectAutomationViews(projects), nil
			},
		}, nil
	case "projects.get":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "project id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"project_id": id}),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				project, err := e.service.Get(ctx, id)
				if err != nil {
					return nil, err
				}
				return projectAutomationView(*project), nil
			},
		}, nil
	case "projects.create":
		var payload database.ProjectInput
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"project_type": payload.Type, "has_credential": payload.SSHCredential != ""}),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				project, err := e.service.Create(ctx, payload)
				if err != nil {
					return nil, err
				}
				return projectAutomationView(*project), nil
			},
		}, nil
	case "projects.update":
		id, err := configurationTargetID(target, 0, "project id")
		if err != nil {
			return nil, err
		}
		var payload database.ProjectInput
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"project_id": id, "has_credential": payload.SSHCredential != ""}),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				project, err := e.service.Update(ctx, id, payload)
				if err != nil {
					return nil, err
				}
				return projectAutomationView(*project), nil
			},
		}, nil
	case "projects.delete":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "project id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"project_id": id}),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				if err := e.service.Delete(ctx, id); err != nil {
					return nil, err
				}
				return deletedConfigurationResult("project", id, 0), nil
			},
		}, nil
	case "projects.duplicate":
		id, err := configurationTargetID(target, 0, "project id")
		if err != nil {
			return nil, err
		}
		var overrides database.ProjectInput
		if err := decodeConfigurationPayload(input.Payload, &overrides); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"project_id": id, "has_credential_override": overrides.SSHCredential != ""}),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				project, err := e.service.Duplicate(ctx, application.DuplicateProjectCommand{ProjectID: id, Overrides: overrides})
				if err != nil {
					return nil, err
				}
				return projectAutomationView(*project), nil
			},
		}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the project capability handler is unsupported", false)
	}
}

func projectOperationPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		platformMutation(readConfigurationCapability("projects.validate", "project_operations", "projects:read")),
		unsafeConfigurationCapability("projects.browse_remote", "project_operations", "projects:read", "credentials:use"),
	}
}

type projectOperationPlatformExecutor struct {
	service *application.ProjectOperationService
}

type remoteBrowsePayload struct {
	SSHHost       string `json:"ssh_host"`
	SSHPort       int    `json:"ssh_port,omitempty"`
	SSHUser       string `json:"ssh_user"`
	SSHAuthType   string `json:"ssh_auth_type,omitempty"`
	SSHCredential string `json:"ssh_credential,omitempty"`
	Path          string `json:"path,omitempty"`
}

func (e *projectOperationPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "projects.validate":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "project id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"project_id": id}),
			execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
				return e.service.Validate(ctx, application.ValidateProjectCommand{ProjectID: id, Authorization: authorization})
			},
		}, nil
	case "projects.browse_remote":
		var payload remoteBrowsePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		connection := application.RemoteBrowseConnection{
			Host: payload.SSHHost, Port: payload.SSHPort, User: payload.SSHUser,
			AuthType: payload.SSHAuthType, Credential: payload.SSHCredential, Path: payload.Path,
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{"host": payload.SSHHost, "path": payload.Path, "has_credential": payload.SSHCredential != ""}),
			execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
				return e.service.BrowseRemote(ctx, application.BrowseRemoteProjectCommand{Connection: connection, Authorization: authorization})
			},
		}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the project operation handler is unsupported", false)
	}
}

func tagPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("tags.list", "tags", "tags:read"),
		readConfigurationCapability("tags.list_project", "tags", "tags:read", "projects:read"),
		writeConfigurationCapability("tags.create", "tags", "tags:write"),
		writeConfigurationCapability("tags.update", "tags", "tags:write"),
		destructiveConfigurationCapability("tags.delete", "tags", "tags:write"),
		writeConfigurationCapability("tags.assign_project", "tags", "tags:write", "projects:write"),
	}
}

type tagPlatformExecutor struct{ service *application.TagService }

type tagCreatePayload struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type tagUpdatePayload struct {
	ID    int64   `json:"id,omitempty"`
	Name  *string `json:"name,omitempty"`
	Color *string `json:"color,omitempty"`
}

type tagIDsPayload struct {
	ProjectID int64   `json:"project_id,omitempty"`
	TagIDs    []int64 `json:"tag_ids"`
}

func (e *tagPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "tags.list":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.List(ctx)
		}}, nil
	case "tags.list_project":
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
	case "tags.create":
		var payload tagCreatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Create(ctx, payload.Name, payload.Color)
		}}, nil
	case "tags.update":
		var payload tagUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "tag id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"tag_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Update(ctx, application.UpdateTagCommand{ID: id, Name: payload.Name, Color: payload.Color})
		}}, nil
	case "tags.delete":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "tag id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"tag_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.Delete(ctx, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("tag", id, 0), nil
		}}, nil
	case "tags.assign_project":
		var payload tagIDsPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "tag_count": len(payload.TagIDs)}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.ReplaceProject(ctx, projectID, payload.TagIDs); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true, "project_id": projectID, "tag_count": len(payload.TagIDs)}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the tag capability handler is unsupported", false)
	}
}
