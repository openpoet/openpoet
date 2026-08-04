package automation

import (
	"context"
	"errors"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func workspacePlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("workspaces.list", "workspaces", "workspaces:read"),
		executionReadCapability("workspaces.get", "workspaces", "workspaces:read"),
		// Read-only: plans an integration ORDER for a project's open lanes without
		// touching any tree.
		executionReadCapability("workspaces.plan_merges", "workspaces", "workspaces:read"),
		executionWriteCapability("workspaces.create", "workspaces", "workspaces:write"),
		executionDestructiveCapability("workspaces.remove", "workspaces", "workspaces:write"),
		executionDestructiveCapability("workspaces.merge", "workspaces", "workspaces:write"),
		executionDestructiveCapability("workspaces.discard", "workspaces", "workspaces:write"),
	}
}

// mapWorkspaceCreateError surfaces the Phase-6 environment-provision failures as
// typed dispatch errors so the caller sees manifest_approval_required (with the
// SHA to approve) or port_reserved instead of a redacted generic message.
func mapWorkspaceCreateError(err error) error {
	var approval *application.ManifestApprovalRequiredError
	if errors.As(err, &approval) {
		return platformFailure("manifest_approval_required", "content_sha256="+approval.SHA, false)
	}
	if strings.Contains(err.Error(), "port_reserved") {
		return platformFailure("port_reserved", "the manifest demands a reserved port", false)
	}
	return err
}

type workspacePlatformExecutor struct {
	service *application.WorkspaceService
}

type workspaceCreatePayload struct {
	Name       string `json:"name,omitempty"`
	BaseRef    string `json:"base_ref,omitempty"`
	TaskID     *int64 `json:"task_id,omitempty"`
	KeepOnExit bool   `json:"keep_on_exit,omitempty"`
}

type workspaceListPayload struct {
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// workspaceAutomationView keeps the surface stable and small; raw rows carry
// SQL null wrappers that serialize awkwardly.
type workspaceAutomationView struct {
	ID                string `json:"id"`
	ProjectID         int64  `json:"project_id"`
	Kind              string `json:"kind"`
	Name              string `json:"name"`
	Branch            string `json:"branch"`
	BaseRef           string `json:"base_ref"`
	Path              string `json:"path"`
	Status            string `json:"status"`
	TaskID            int64  `json:"task_id,omitempty"`
	LeasedBySessionID string `json:"leased_by_session_id,omitempty"`
	Version           int64  `json:"version"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

func workspaceView(ws database.Workspace) workspaceAutomationView {
	view := workspaceAutomationView{
		ID: ws.ID, ProjectID: ws.ProjectID, Kind: ws.Kind, Name: ws.Name,
		Branch: ws.Branch, BaseRef: ws.BaseRef, Path: ws.Path, Status: ws.Status,
		Version:   ws.Version,
		CreatedAt: ws.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: ws.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if ws.TaskID.Valid {
		view.TaskID = ws.TaskID.Int64
	}
	if ws.LeasedBySessionID.Valid {
		view.LeasedBySessionID = ws.LeasedBySessionID.String
	}
	return view
}

func (e *workspacePlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Capability {
	case "workspaces.create":
		var payload workspaceCreatePayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		if len(payload.Name) > 64 || len(payload.BaseRef) > 200 {
			return nil, platformFailure("platform_payload_invalid", "workspace name or base_ref is too large", false)
		}
		if payload.TaskID != nil && *payload.TaskID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "task_id must be positive", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "name": payload.Name, "base_ref": payload.BaseRef}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			ws, err := e.service.Create(ctx, application.CreateWorkspaceCommand{
				ProjectID:     projectID,
				Name:          payload.Name,
				BaseRef:       payload.BaseRef,
				TaskID:        payload.TaskID,
				KeepOnExit:    payload.KeepOnExit,
				Authorization: authorization,
			})
			if err != nil {
				return nil, mapWorkspaceCreateError(err)
			}
			return map[string]any{"workspace": workspaceView(*ws)}, nil
		}}, nil
	case "workspaces.list":
		var payload workspaceListPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID := target.ProjectID
		if projectID < 0 {
			return nil, platformFailure("platform_target_invalid", "project_id must be positive", false)
		}
		status := strings.TrimSpace(payload.Status)
		switch status {
		case "", "provisioning", "ready", "leased", "dirty", "tearing_down", "failed", "merged", "removed":
		default:
			return nil, platformFailure("platform_payload_invalid", "unknown workspace status filter", false)
		}
		limit := payload.Limit
		if limit == 0 {
			limit = 100
		}
		if limit < 1 || limit > 500 {
			return nil, platformFailure("platform_payload_invalid", "limit must be between 1 and 500", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "status": status, "limit": limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			rows, err := e.service.List(ctx, projectID, status, limit)
			if err != nil {
				return nil, err
			}
			views := make([]workspaceAutomationView, 0, len(rows))
			for _, ws := range rows {
				views = append(views, workspaceView(ws))
			}
			return map[string]any{"workspaces": views}, nil
		}}, nil
	case "workspaces.plan_merges":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID := target.ProjectID
		if projectID <= 0 {
			return nil, platformFailure("platform_target_invalid", "project_id must be positive", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.PlanMerges(ctx, projectID)
		}}, nil
	case "workspaces.get":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		workspaceID, err := executionStringID(target, "workspace id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"workspace_id": workspaceID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			ws, err := e.service.Get(ctx, workspaceID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"workspace": workspaceView(*ws)}, nil
		}}, nil
	case "workspaces.remove":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		workspaceID, err := executionStringID(target, "workspace id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"workspace_id": workspaceID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			ws, err := e.service.Remove(ctx, application.RemoveWorkspaceCommand{
				WorkspaceID:   workspaceID,
				Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"workspace": workspaceView(*ws)}, nil
		}}, nil
	case "workspaces.discard":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		workspaceID, err := executionStringID(target, "workspace id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"workspace_id": workspaceID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			ws, err := e.service.Discard(ctx, application.RemoveWorkspaceCommand{
				WorkspaceID:   workspaceID,
				Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"workspace": workspaceView(*ws), "discarded": true}, nil
		}}, nil
	case "workspaces.merge":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		workspaceID, err := executionStringID(target, "workspace id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"workspace_id": workspaceID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			result, err := e.service.Merge(ctx, application.MergeWorkspaceCommand{
				WorkspaceID:   workspaceID,
				Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			out := map[string]any{"merged": result.Merged}
			if result.Workspace != nil {
				out["workspace"] = workspaceView(*result.Workspace)
			}
			if !result.Merged {
				out["code"] = "workspace_merge_conflict"
				out["conflict_files"] = result.ConflictFiles
			}
			return out, nil
		}}, nil
	}
	return nil, platformFailure("platform_capability_unknown", "unknown workspace capability", false)
}
