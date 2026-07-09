package automation

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"openpoet/internal/database"
)

const defaultSnapshotNotificationLimit = 100
const maxSnapshotNotificationLimit = 500

type SnapshotStore interface {
	ReadAutomationSnapshot(ctx context.Context, notificationLimit int) (*database.AutomationSnapshot, error)
}

type snapshotResponse struct {
	APIVersion    string                  `json:"api_version"`
	Cursor        string                  `json:"cursor"`
	GeneratedAt   time.Time               `json:"generated_at"`
	Projects      []database.Project      `json:"projects"`
	Tasks         []database.ProjectTask  `json:"tasks"`
	TaskSummary   map[string]int          `json:"task_summary"`
	Sessions      []database.Session      `json:"sessions"`
	Notifications []database.Notification `json:"notifications"`
}

func (a *commandAPI) getSnapshot(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable", "the automation snapshot is unavailable", true)
		return
	}
	limit := defaultSnapshotNotificationLimit
	if raw := r.URL.Query().Get("notification_limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > maxSnapshotNotificationLimit {
			writeError(w, http.StatusBadRequest, "notification_limit_invalid", "notification_limit must be between 1 and 500", false)
			return
		}
		limit = parsed
	}

	snapshot, err := a.snapshot.ReadAutomationSnapshot(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_failed", "the consistent automation snapshot could not be loaded", true)
		return
	}
	writeJSON(w, http.StatusOK, snapshotResponse{
		APIVersion: APIVersion, Cursor: strconv.FormatInt(snapshot.Cursor, 10), GeneratedAt: a.now().UTC(),
		Projects: snapshot.Projects, Tasks: snapshot.Tasks, TaskSummary: snapshot.TaskSummary,
		Sessions: snapshot.Sessions, Notifications: snapshot.Notifications,
	})
}
