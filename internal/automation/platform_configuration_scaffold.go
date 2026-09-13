package automation

import (
	"context"

	"openpoet/internal/application"
)

func projectScaffoldPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("projects.scaffold_root", "project_scaffold", "projects:read"),
		// Creating a directory and writing a compose manifest onto the host is
		// unsafe by construction — it is the file the container runtime will later
		// execute, so it carries the same weight as projects.create.
		unsafeConfigurationCapability("projects.scaffold", "project_scaffold", "projects:write"),
	}
}

type projectScaffoldPlatformExecutor struct {
	service *application.ProjectScaffoldService
}

func (e *projectScaffoldPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	switch input.Handler {
	case "projects.scaffold_root":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, nil),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				root, err := e.service.Root(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"root": root}, nil
			},
		}, nil
	case "projects.scaffold":
		var payload application.ScaffoldProjectInput
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{
			preview: configurationPreview(input.Handler, map[string]any{
				"name":       payload.Name,
				"dir_name":   payload.DirName,
				"git_init":   payload.GitInit,
				"file_count": len(payload.Files) + 1,
			}),
			execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
				return e.service.Create(ctx, payload)
			},
		}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the project scaffold handler is unsupported", false)
	}
}
