package automation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// BlackboardPort is the narrow surface the blackboard capabilities need.
// *database.DB satisfies it directly.
type BlackboardPort interface {
	BlackboardGet(ctx context.Context, scopeType string, scopeID int64, key string) (*database.BlackboardEntry, error)
	BlackboardPut(ctx context.Context, in database.BlackboardPutInput) (int64, error)
}

func blackboardPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("blackboard.get", "blackboard", "blackboard:read"),
		executionWriteCapability("blackboard.put", "blackboard", "blackboard:write"),
	}
}

type blackboardPlatformExecutor struct {
	port BlackboardPort
}

type blackboardGetPayload struct {
	Scope   string `json:"scope"`
	ScopeID int64  `json:"scope_id,omitempty"`
	Key     string `json:"key"`
}

type blackboardFencePayload struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
}

type blackboardPutPayload struct {
	Scope           string                  `json:"scope"`
	ScopeID         int64                   `json:"scope_id,omitempty"`
	Key             string                  `json:"key"`
	Value           json.RawMessage         `json:"value,omitempty"`
	ExpectedVersion *int64                  `json:"expected_version,omitempty"`
	TTLSeconds      int                     `json:"ttl_seconds,omitempty"`
	Fence           *blackboardFencePayload `json:"fence,omitempty"`
}

// validBlackboardScope validates the scope enum AND confines it to the acting
// client's project_filter: a scoped client may only touch a project/group board
// inside its filter. The global board is the designated shared coordination
// space and stays open to any client (isolation there is by key convention).
func validBlackboardScope(scope string, scopeID int64, ps *ProjectScopeSet) error {
	switch scope {
	case "global":
		return nil
	case "project":
		if scopeID <= 0 {
			return platformFailure("platform_payload_invalid", "scope_id is required for project scope", false)
		}
		if !ps.Allows(scopeID) {
			return platformFailure("blackboard_out_of_scope", "the client is not scoped to this project board", false)
		}
		return nil
	case "group":
		if scopeID <= 0 {
			return platformFailure("platform_payload_invalid", "scope_id is required for group scope", false)
		}
		if !ps.AllowsTag(scopeID) {
			return platformFailure("blackboard_out_of_scope", "the client is not scoped to this group board", false)
		}
		return nil
	default:
		return platformFailure("platform_payload_invalid", "scope must be global, group, or project", false)
	}
}

func (e *blackboardPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	switch input.Capability {
	case "blackboard.get":
		var payload blackboardGetPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload.Key) == "" {
			return nil, platformFailure("platform_payload_invalid", "key is required", false)
		}
		if err := validBlackboardScope(payload.Scope, payload.ScopeID, input.ProjectScope); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"scope": payload.Scope, "key": payload.Key}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			entry, err := e.port.BlackboardGet(ctx, payload.Scope, payload.ScopeID, payload.Key)
			if err != nil {
				return nil, err
			}
			if entry == nil {
				return map[string]any{"found": false, "key": payload.Key, "version": 0}, nil
			}
			return map[string]any{"found": true, "entry": entry, "version": entry.Version}, nil
		}}, nil

	case "blackboard.put":
		var payload blackboardPutPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload.Key) == "" {
			return nil, platformFailure("platform_payload_invalid", "key is required", false)
		}
		if err := validBlackboardScope(payload.Scope, payload.ScopeID, input.ProjectScope); err != nil {
			return nil, err
		}
		put := database.BlackboardPutInput{
			ScopeType:       payload.Scope,
			ScopeID:         payload.ScopeID,
			Key:             payload.Key,
			ValueJSON:       string(payload.Value),
			ExpectedVersion: payload.ExpectedVersion,
			TTLSeconds:      payload.TTLSeconds,
		}
		if payload.Fence != nil {
			if strings.TrimSpace(payload.Fence.Key) == "" {
				return nil, platformFailure("platform_payload_invalid", "fence.key is required when fence is set", false)
			}
			put.FenceKey = payload.Fence.Key
			put.FenceVersion = payload.Fence.Version
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"scope": payload.Scope, "key": payload.Key}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			put.Actor = authorization.Actor.ID
			version, err := e.port.BlackboardPut(ctx, put)
			switch {
			case errors.Is(err, database.ErrBlackboardCASConflict):
				return nil, platformFailure("blackboard_cas_conflict", "expected_version does not match the current version", false)
			case errors.Is(err, database.ErrBlackboardFenceStale):
				return nil, platformFailure("blackboard_fence_stale", "the fenced lease is stale (a newer holder exists)", false)
			case err != nil:
				return nil, err
			}
			return map[string]any{"key": payload.Key, "version": version}, nil
		}}, nil
	}
	return nil, platformFailure("platform_handler_unsupported", "the blackboard operation handler is unsupported", false)
}
