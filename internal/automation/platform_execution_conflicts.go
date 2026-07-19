package automation

import (
	"context"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// ConflictReadPort is the narrow read surface the conflict capabilities need.
// *database.DB satisfies it directly.
type ConflictReadPort interface {
	ListCoordinatorIncidents(ctx context.Context, projectID int64, state string, limit int) ([]database.CoordinatorIncident, error)
	GetCoordinatorIncident(ctx context.Context, id string) (*database.CoordinatorIncident, error)
	ListSessionFileActivity(ctx context.Context, sessionID string, limit int) ([]database.SessionFileActivity, error)
}

func conflictPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("conflicts.list", "conflicts", "conflicts:read"),
		executionReadCapability("conflicts.get", "conflicts", "conflicts:read"),
		executionReadCapability("sessions.file_activity", "conflicts", "conflicts:read"),
	}
}

type conflictPlatformExecutor struct {
	queries ConflictReadPort
}

type conflictListPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	State     string `json:"state,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type conflictActivityPayload struct {
	Limit int `json:"limit,omitempty"`
}

func (e *conflictPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Capability {
	case "conflicts.list":
		var payload conflictListPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID := payload.ProjectID
		if target.ProjectID > 0 {
			projectID = target.ProjectID
		}
		if projectID < 0 {
			return nil, platformFailure("platform_payload_invalid", "project_id must be positive", false)
		}
		state := strings.TrimSpace(payload.State)
		switch state {
		case "", "open", "acknowledged", "resolved", "expired":
		default:
			return nil, platformFailure("platform_payload_invalid", "state must be open|acknowledged|resolved|expired", false)
		}
		limit := payload.Limit
		if limit == 0 {
			limit = 100
		}
		if limit < 1 || limit > 500 {
			return nil, platformFailure("platform_payload_invalid", "limit must be between 1 and 500", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "state": state, "limit": limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			incidents, err := e.queries.ListCoordinatorIncidents(ctx, projectID, state, limit)
			if err != nil {
				return nil, err
			}
			if incidents == nil {
				incidents = []database.CoordinatorIncident{}
			}
			return map[string]any{"incidents": incidents}, nil
		}}, nil
	case "conflicts.get":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		incidentID, err := executionStringID(target, "conflict id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"incident_id": incidentID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			incident, err := e.queries.GetCoordinatorIncident(ctx, incidentID)
			if err != nil {
				return nil, err
			}
			if incident == nil {
				return nil, platformFailure("conflict_not_found", "conflict incident not found", false)
			}
			return map[string]any{"incident": incident}, nil
		}}, nil
	case "sessions.file_activity":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload conflictActivityPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		limit := payload.Limit
		if limit == 0 {
			limit = 200
		}
		if limit < 1 || limit > 500 {
			return nil, platformFailure("platform_payload_invalid", "limit must be between 1 and 500", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "limit": limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			activity, err := e.queries.ListSessionFileActivity(ctx, sessionID, limit)
			if err != nil {
				return nil, err
			}
			if activity == nil {
				activity = []database.SessionFileActivity{}
			}
			return map[string]any{"session_id": sessionID, "activity": activity}, nil
		}}, nil
	}
	return nil, platformFailure("platform_capability_unknown", "unknown conflict capability", false)
}
