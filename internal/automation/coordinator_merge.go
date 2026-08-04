package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// Phase 7.5 (Maestro integration): the conversational merge gate. Merging a
// lane is destructive human-tier authority; the coordinator spends a
// mission-scoped multi-use grant (V73) that the user pre-issued — no grant, no
// merge, and the typed refusal tells the coordinator exactly what to ask for.
// Prediction (git merge-tree) is a free read so the coordinator negotiates
// with facts ("lane A merges clean, lane B collides on util.go").

// MergePredictor is the prediction slice of the workspace service.
type MergePredictor interface {
	PredictMerge(ctx context.Context, workspaceID string) (*application.MergePrediction, error)
}

// missionGrantStore is the V73 grant ledger. *database.DB satisfies it.
type missionGrantStore interface {
	PeekMissionGrant(ctx context.Context, missionID int64, capability string) error
	ConsumeMissionGrantUse(ctx context.Context, missionID int64, capability string) (int64, error)
	RefundMissionGrantUse(ctx context.Context, grantID int64) error
}

// workspaceProjectStore resolves a workspace's project for scope checks.
type workspaceProjectStore interface {
	GetWorkspace(ctx context.Context, id string) (*database.Workspace, error)
}

// resolveGroupWorkspace loads the workspace and fails closed unless its
// project belongs to the coordinated group.
func (c *coordinatorAPI) resolveGroupWorkspace(w http.ResponseWriter, r *http.Request, cs coordinatorSession, group int64, workspaceID string) *database.Workspace {
	store, ok := c.store.(workspaceProjectStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "workspace_store_unavailable", "the workspace store is unavailable", true)
		return nil
	}
	ws, err := store.GetWorkspace(r.Context(), strings.TrimSpace(workspaceID))
	if err != nil || ws == nil {
		writeError(w, http.StatusNotFound, "workspace_not_found", "the workspace does not exist", false)
		return nil
	}
	actor := coordinatorSessionActor(cs.SessionID, group)
	scope := resolveActorProjectScope(r.Context(), c.api.scopeStore, actor)
	if scope.Restricted() && !scope.Allowed[ws.ProjectID] {
		writeError(w, http.StatusForbidden, "platform_project_out_of_scope", "the workspace belongs to a project outside the coordinated group", false)
		return nil
	}
	return ws
}

// predictMerge is the free pre-merge forecast (read-only, no fence needed).
func (c *coordinatorAPI) predictMerge(w http.ResponseWriter, r *http.Request) {
	cs, group, ok := c.requireCoordinator(w, r, nil)
	if !ok {
		return
	}
	if c.predictor == nil {
		writeError(w, http.StatusServiceUnavailable, "merge_predictor_unavailable", "merge prediction is unavailable", true)
		return
	}
	workspaceID := strings.TrimSpace(chi.URLParam(r, "id"))
	if c.resolveGroupWorkspace(w, r, cs, group, workspaceID) == nil {
		return
	}
	prediction, err := c.predictor.PredictMerge(r.Context(), workspaceID)
	if err != nil {
		writeApplicationReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace_id": workspaceID, "clean": prediction.Clean, "conflict_files": prediction.ConflictFiles})
}

// MergePlanner is the integration-ordering slice of the workspace service.
type MergePlanner interface {
	PlanMerges(ctx context.Context, projectID int64) (*application.MergePlan, error)
}

// planMerges orders a project's open lanes for integration. Free read: it runs
// only read-only git and touches no tree. It answers what per-lane prediction
// cannot — two lanes that each rewrote the same file both predict clean against
// main while being guaranteed to collide with each other.
func (c *coordinatorAPI) planMerges(w http.ResponseWriter, r *http.Request) {
	cs, group, ok := c.requireCoordinator(w, r, nil)
	if !ok {
		return
	}
	planner, ok := c.predictor.(MergePlanner)
	if !ok || c.predictor == nil {
		writeError(w, http.StatusServiceUnavailable, "merge_planner_unavailable", "merge planning is unavailable", true)
		return
	}
	projectID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "projectID")), 10, 64)
	if err != nil || projectID <= 0 {
		writeError(w, http.StatusBadRequest, "project_id_required", "a positive project id is required", false)
		return
	}
	// Fail closed outside the coordinated group, exactly like every other
	// project-scoped coordinator read.
	actor := coordinatorSessionActor(cs.SessionID, group)
	scope := resolveActorProjectScope(r.Context(), c.api.scopeStore, actor)
	if scope.Restricted() && !scope.Allowed[projectID] {
		writeError(w, http.StatusForbidden, "platform_project_out_of_scope", "the project is outside the coordinated group", false)
		return
	}
	plan, err := planner.PlanMerges(r.Context(), projectID)
	if err != nil {
		writeApplicationReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type mergeWorkspaceRequest struct {
	MissionID    int64  `json:"mission_id"`
	FenceVersion *int64 `json:"fence_version"`
}

// mergeWorkspace is the conversational merge gate: fence-checked, grant-spent,
// conflict-safe (the underlying Merge aborts and reports files on conflict —
// and a conflict does NOT spend a grant use).
func (c *coordinatorAPI) mergeWorkspace(w http.ResponseWriter, r *http.Request) {
	var req mergeWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MissionID <= 0 {
		writeError(w, http.StatusBadRequest, "mission_id_required", "a mission_id is required (merge authority is mission-scoped)", false)
		return
	}
	if req.FenceVersion == nil {
		writeError(w, http.StatusBadRequest, "coordinator_fence_required", "a fence_version is required for coordinator mutations", false)
		return
	}
	cs, group, ok := c.requireCoordinator(w, r, req.FenceVersion)
	if !ok {
		return
	}
	missions, ok := c.missions()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission_store_unavailable", "the mission store is unavailable", true)
		return
	}
	mission := c.missionForGroup(w, r, missions, req.MissionID, group)
	if mission == nil {
		return
	}
	if mission.Status != "active" {
		writeError(w, http.StatusConflict, "mission_not_active", "the mission is not active — grants only spend on running missions", false)
		return
	}
	workspaceID := strings.TrimSpace(chi.URLParam(r, "id"))
	ws := c.resolveGroupWorkspace(w, r, cs, group, workspaceID)
	if ws == nil {
		return
	}
	grants, ok := c.store.(missionGrantStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission_grant_store_unavailable", "the mission grant ledger is unavailable", true)
		return
	}
	// CONSUME BEFORE the effect (no peek-then-act TOCTOU: two concurrent
	// merges with one use left race the atomic decrement, exactly one wins).
	// A non-effect outcome (conflict, dispatch failure) refunds the use.
	grantID, err := grants.ConsumeMissionGrantUse(r.Context(), req.MissionID, "workspaces.merge")
	if err != nil {
		writeMissionGrantError(w, err)
		return
	}

	// The mission grant IS the validated human approval for this destructive
	// capability — attributed as such in the audit trail.
	approval, err := NewValidatedPlatformApproval(fmt.Sprintf("mission-grant:%d", req.MissionID))
	if err != nil {
		_ = grants.RefundMissionGrantUse(r.Context(), grantID)
		writeError(w, http.StatusInternalServerError, "mission_grant_store_unavailable", "the merge approval could not be constructed", true)
		return
	}
	actor := coordinatorSessionActor(cs.SessionID, group)
	target, _ := json.Marshal(map[string]any{"type": "workspace", "id": workspaceID})
	result, err := DispatchPlatformCapability(r.Context(), c.api.platform, PlatformDispatchRequest{
		Capability: application.CapabilityName("workspaces.merge"),
		Target:     target, Payload: json.RawMessage(`{}`),
		Actor: actor, Reason: fmt.Sprintf("mission %d integration merge (mission grant)", req.MissionID),
		Approval: approval,
	})
	if err != nil {
		_ = grants.RefundMissionGrantUse(r.Context(), grantID)
		writeCoordinatorDispatchError(w, err)
		return
	}
	encoded, _ := json.Marshal(result.Result)
	view := map[string]any{}
	_ = json.Unmarshal(encoded, &view)
	merged, _ := view["merged"].(bool)
	if merged {
		_ = missions.AppendMissionEvent(r.Context(), "mission.merge_completed", req.MissionID, map[string]any{
			"mission_id": req.MissionID, "workspace_id": workspaceID, "project_id": ws.ProjectID,
		})
	} else {
		// A conflict costs nothing — the use goes back.
		_ = grants.RefundMissionGrantUse(r.Context(), grantID)
	}
	writeJSON(w, http.StatusOK, view)
}

func writeMissionGrantError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrMissionGrantRequired):
		writeError(w, http.StatusForbidden, "mission_grant_required", err.Error(), false)
	case errors.Is(err, database.ErrMissionGrantExhausted):
		writeError(w, http.StatusForbidden, "mission_grant_exhausted", err.Error(), false)
	default:
		writeError(w, http.StatusInternalServerError, "mission_grant_check_failed", "the mission grant could not be checked", true)
	}
}
