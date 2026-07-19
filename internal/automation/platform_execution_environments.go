package automation

import (
	"context"
	"strings"

	"openpoet/internal/application"
)

// EnvironmentManifestApprover is the narrow surface the environments capability
// needs (*application.EnvironmentService satisfies it).
type EnvironmentManifestApprover interface {
	ApproveManifest(ctx context.Context, projectID int64, providedSHA, approvedBy string) (map[string]any, error)
}

func environmentPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		// UNSAFE/explicit: approving a manifest lets its repo-authored commands run
		// as the server user. Automation-plane + human/warden grant only.
		executionUnsafeCapability("environments.approve_manifest", "environments", "environments:manage"),
	}
}

type environmentPlatformExecutor struct {
	service EnvironmentManifestApprover
}

type approveManifestPayload struct {
	ContentSHA256 string `json:"content_sha256"`
}

func (e *environmentPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Capability {
	case "environments.approve_manifest":
		var payload approveManifestPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		sha := strings.TrimSpace(payload.ContentSHA256)
		if len(sha) > 64 {
			return nil, platformFailure("platform_payload_invalid", "content_sha256 is too large", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.ApproveManifest(ctx, projectID, sha, authorization.Actor.ID)
		}}, nil
	}
	return nil, platformFailure("platform_handler_unsupported", "the environments operation handler is unsupported", false)
}
