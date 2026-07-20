package automation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/database"
)

// Phase 7.3 (Maestro): the durable mission registry, driven by the elected
// coordinator. A mission binds a goal to the coordinated group; workers are
// enrolled at spawn (or attached later) and carry a rolling last_report_ref.
// All mutations are fence-gated like every coordinator mutation.

// missionStore is the mission slice of the database the tier needs.
// *database.DB satisfies it.
type missionStore interface {
	CreateMission(ctx context.Context, goal string, groupTagID int64, coordinatorSessionID string) (*database.Mission, error)
	GetMission(ctx context.Context, id int64) (*database.Mission, error)
	UpdateMissionStatus(ctx context.Context, id int64, status string) error
	UpsertMissionWorker(ctx context.Context, worker *database.MissionWorker) error
	ListMissionWorkers(ctx context.Context, missionID int64) ([]database.MissionWorker, error)
	LatestSessionReportRef(ctx context.Context, sessionID string) (string, error)
	UpdateMissionWorkerReport(ctx context.Context, sessionID, reportRef string) error
	AppendMissionEvent(ctx context.Context, eventType string, missionID int64, payload map[string]any) error
}

func (c *coordinatorAPI) missions() (missionStore, bool) {
	store, ok := c.store.(missionStore)
	return store, ok
}

// missionForGroup loads a mission and fails closed unless it belongs to the
// caller's coordinated group.
func (c *coordinatorAPI) missionForGroup(w http.ResponseWriter, r *http.Request, store missionStore, missionID, group int64) *database.Mission {
	if missionID <= 0 {
		writeError(w, http.StatusBadRequest, "mission_id_required", "a mission_id is required", false)
		return nil
	}
	mission, err := store.GetMission(r.Context(), missionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mission_read_failed", "the mission could not be read", true)
		return nil
	}
	if mission == nil {
		writeError(w, http.StatusNotFound, "mission_not_found", "the mission does not exist", false)
		return nil
	}
	if mission.GroupTagID != group {
		writeError(w, http.StatusForbidden, "mission_group_mismatch", "the mission belongs to another coordination group", false)
		return nil
	}
	return mission
}

type startMissionRequest struct {
	Goal         string `json:"goal"`
	FenceVersion *int64 `json:"fence_version"`
}

func (c *coordinatorAPI) startMission(w http.ResponseWriter, r *http.Request) {
	store, ok := c.missions()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission_store_unavailable", "the mission store is unavailable", true)
		return
	}
	var req startMissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Goal) == "" {
		writeError(w, http.StatusBadRequest, "mission_goal_required", "a mission goal is required", false)
		return
	}
	if len(req.Goal) > 4000 {
		writeError(w, http.StatusBadRequest, "mission_goal_invalid", "the mission goal exceeds 4000 characters", false)
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
	mission, err := store.CreateMission(r.Context(), strings.TrimSpace(req.Goal), group, cs.SessionID)
	if err != nil {
		if errors.Is(err, database.ErrMissionActiveExists) {
			writeError(w, http.StatusConflict, "mission_already_active", "the group already has an active mission — complete or fail it first", false)
			return
		}
		writeError(w, http.StatusInternalServerError, "mission_create_failed", "the mission could not be created", true)
		return
	}
	_ = store.AppendMissionEvent(r.Context(), "mission.created", mission.ID, map[string]any{
		"mission_id": mission.ID, "goal": mission.Goal, "group_tag_id": group,
		"coordinator_session_id": cs.SessionID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"mission_id": mission.ID, "goal": mission.Goal, "group": group,
		"coordinator_session_id": cs.SessionID, "status": mission.Status,
	})
}

func (c *coordinatorAPI) getMission(w http.ResponseWriter, r *http.Request) {
	store, ok := c.missions()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission_store_unavailable", "the mission store is unavailable", true)
		return
	}
	_, group, authorized := c.requireCoordinator(w, r, nil)
	if !authorized {
		return
	}
	missionID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	mission := c.missionForGroup(w, r, store, missionID, group)
	if mission == nil {
		return
	}
	workers, err := store.ListMissionWorkers(r.Context(), mission.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mission_read_failed", "the mission workers could not be read", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mission": mission, "workers": workers})
}

type missionStatusRequest struct {
	Status       string `json:"status"`
	FenceVersion *int64 `json:"fence_version"`
}

var missionStatusVocabulary = map[string]bool{
	"active": true, "paused": true, "completed": true, "failed": true, "archived": true,
}

func (c *coordinatorAPI) updateMissionStatus(w http.ResponseWriter, r *http.Request) {
	store, ok := c.missions()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission_store_unavailable", "the mission store is unavailable", true)
		return
	}
	var req missionStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !missionStatusVocabulary[req.Status] {
		writeError(w, http.StatusBadRequest, "mission_status_invalid", "status must be one of active, paused, completed, failed, archived", false)
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
	missionID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	mission := c.missionForGroup(w, r, store, missionID, group)
	if mission == nil {
		return
	}
	if err := store.UpdateMissionStatus(r.Context(), mission.ID, req.Status); err != nil {
		writeError(w, http.StatusInternalServerError, "mission_update_failed", "the mission status could not be updated", true)
		return
	}
	_ = store.AppendMissionEvent(r.Context(), "mission.status_changed", mission.ID, map[string]any{
		"mission_id": mission.ID, "status": req.Status, "by_session_id": cs.SessionID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"mission_id": mission.ID, "status": req.Status})
}

type attachWorkerRequest struct {
	MissionID    int64  `json:"mission_id"`
	SessionID    string `json:"session_id"`
	Role         string `json:"role"`
	FenceVersion *int64 `json:"fence_version"`
}

// attachWorker adopts an EXISTING session into the mission roster (workers
// spawned via start_worker enroll automatically). The rolling last_report_ref
// is backfilled from the session's latest dense report.
func (c *coordinatorAPI) attachWorker(w http.ResponseWriter, r *http.Request) {
	store, ok := c.missions()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission_store_unavailable", "the mission store is unavailable", true)
		return
	}
	var req attachWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		writeError(w, http.StatusBadRequest, "mission_worker_required", "a mission_id and session_id are required", false)
		return
	}
	if len(req.Role) > 100 {
		writeError(w, http.StatusBadRequest, "mission_worker_invalid", "role must not exceed 100 characters", false)
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
	mission := c.missionForGroup(w, r, store, req.MissionID, group)
	if mission == nil {
		return
	}
	targetID := strings.TrimSpace(req.SessionID)
	if !c.sessionInScope(r.Context(), cs, group, targetID, w) {
		return
	}
	sess, err := c.store.GetSession(r.Context(), targetID)
	if err != nil || sess == nil {
		writeError(w, http.StatusNotFound, "session_not_found", "the target session does not exist", false)
		return
	}
	reportRef, _ := store.LatestSessionReportRef(r.Context(), targetID)
	status := "running"
	if sess.Status == "stopped" || sess.Status == "completed" || sess.Status == "error" {
		status = "stopped"
	}
	worker := &database.MissionWorker{
		MissionID: mission.ID, ProjectID: sess.ProjectID, Backend: sess.Backend,
		SessionID: targetID, WorkspaceID: sess.WorkspaceID.String,
		Role: strings.TrimSpace(req.Role), Status: status, LastReportRef: reportRef,
	}
	if err := store.UpsertMissionWorker(r.Context(), worker); err != nil {
		writeError(w, http.StatusInternalServerError, "mission_update_failed", "the worker could not be attached", true)
		return
	}
	_ = store.AppendMissionEvent(r.Context(), "mission.worker_attached", mission.ID, map[string]any{
		"mission_id": mission.ID, "session_id": targetID, "role": worker.Role, "project_id": sess.ProjectID,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"mission_id": mission.ID, "session_id": targetID, "role": worker.Role,
		"last_report_ref": reportRef,
	})
}
