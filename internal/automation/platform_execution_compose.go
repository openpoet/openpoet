package automation

import (
	"context"
	"strings"

	"openpoet/internal/application"
)

// ComposeLifecyclePort is the narrow surface the compose capabilities need
// (*application.ComposeApplicationService satisfies it).
type ComposeLifecyclePort interface {
	Status(ctx context.Context, projectID int64) (*application.ComposeStatus, error)
	Approve(ctx context.Context, projectID int64, expectedSHA, approvedBy string) (*application.ComposeStatus, error)
	Up(ctx context.Context, projectID int64) (*application.ComposeActionResult, error)
	Down(ctx context.Context, projectID int64) (*application.ComposeActionResult, error)
	Restart(ctx context.Context, projectID int64) (*application.ComposeActionResult, error)
	Logs(ctx context.Context, projectID int64, service string, tail int) (string, error)
}

func composePlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("compose.status", "compose", "environments:read"),
		executionReadCapability("compose.logs", "compose", "environments:read"),
		// UNSAFE/explicit: approving a compose file and starting it both put the
		// manifest's contents in front of the container runtime as the server user.
		executionUnsafeCapability("compose.approve", "compose", "environments:manage"),
		executionUnsafeCapability("compose.up", "compose", "environments:manage"),
		// Stopping is destructive but not privilege-granting.
		executionDestructiveCapability("compose.down", "compose", "environments:manage"),
		executionWriteCapability("compose.restart", "compose", "environments:manage"),
	}
}

type composePlatformExecutor struct {
	service ComposeLifecyclePort
}

type composeApprovePayload struct {
	ContentSHA256 string `json:"content_sha256"`
}

type composeLogsPayload struct {
	Service string `json:"service"`
	Tail    int    `json:"tail"`
}

func (e *composePlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	projectID, err := executionProjectID(target, 0)
	if err != nil {
		return nil, err
	}
	preview := func(extra map[string]any) map[string]any {
		fields := map[string]any{"project_id": projectID}
		for k, v := range extra {
			fields[k] = v
		}
		return fields
	}

	switch input.Capability {
	case "compose.status":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, preview(nil)), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Status(ctx, projectID)
		}}, nil

	case "compose.logs":
		var payload composeLogsPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		service := strings.TrimSpace(payload.Service)
		if len(service) > 128 {
			return nil, platformFailure("platform_payload_invalid", "service name is too large", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, preview(map[string]any{"service": service, "tail": payload.Tail})), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			output, err := e.service.Logs(ctx, projectID, service, payload.Tail)
			if err != nil {
				return nil, err
			}
			return map[string]any{"logs": output}, nil
		}}, nil

	case "compose.approve":
		var payload composeApprovePayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		sha := strings.TrimSpace(payload.ContentSHA256)
		if len(sha) > 64 {
			return nil, platformFailure("platform_payload_invalid", "content_sha256 is too large", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, preview(nil)), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.Approve(ctx, projectID, sha, authorization.Actor.ID)
		}}, nil

	case "compose.up":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, preview(nil)), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Up(ctx, projectID)
		}}, nil

	case "compose.down":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, preview(nil)), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Down(ctx, projectID)
		}}, nil

	case "compose.restart":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, preview(nil)), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Restart(ctx, projectID)
		}}, nil
	}
	return nil, platformFailure("platform_handler_unsupported", "the compose operation handler is unsupported", false)
}
