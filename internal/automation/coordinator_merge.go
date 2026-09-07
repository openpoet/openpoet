package automation

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// Phase 7.5: merge FORECASTING for the coordinator tier. Prediction (git
// merge-tree) and ordering are free reads, so a coordinator negotiates
// integration with facts ("lane A merges clean, lane B collides on util.go").
//
// Performing the merge is deliberately NOT here: it is destructive and needs
// human authority. Mission grants used to supply it; with missions retired
// (V74) the merge itself runs through the platform's approved capability
// surface (workspaces.merge from the UI), not from a session's own initiative.

// MergePredictor is the prediction slice of the workspace service.
type MergePredictor interface {
	PredictMerge(ctx context.Context, workspaceID string) (*application.MergePrediction, error)
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
