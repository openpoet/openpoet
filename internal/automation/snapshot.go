package automation

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

const defaultSnapshotNotificationLimit = 100
const maxSnapshotNotificationLimit = 500

type SnapshotStore interface {
	ListProjects(ctx context.Context) ([]database.Project, error)
	ListSessions(ctx context.Context) ([]database.Session, error)
	ListNotifications(ctx context.Context, limit int) ([]database.Notification, error)
}

type snapshotResponse struct {
	APIVersion    string                  `json:"api_version"`
	GeneratedAt   time.Time               `json:"generated_at"`
	Projects      []database.Project      `json:"projects"`
	Tasks         []database.ProjectTask  `json:"tasks"`
	TaskSummary   map[string]int          `json:"task_summary"`
	Sessions      []database.Session      `json:"sessions"`
	Notifications []database.Notification `json:"notifications"`
}

func (a *commandAPI) getSnapshot(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.snapshot == nil || a.capabilities == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable", "the automation snapshot is unavailable", true)
		return
	}
	taskService, err := projectTaskServiceFor(a.capabilities, application.CapabilityTasksList)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable", err.Error(), true)
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

	projects, err := a.snapshot.ListProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_projects_failed", "projects could not be loaded", true)
		return
	}
	tasks, err := taskService.ListAll(r.Context(), database.TaskFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_tasks_failed", "tasks could not be loaded", true)
		return
	}
	sessions, err := a.snapshot.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_sessions_failed", "sessions could not be loaded", true)
		return
	}
	notifications, err := a.snapshot.ListNotifications(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_notifications_failed", "notifications could not be loaded", true)
		return
	}
	if projects == nil {
		projects = []database.Project{}
	}
	if sessions == nil {
		sessions = []database.Session{}
	}
	if notifications == nil {
		notifications = []database.Notification{}
	}
	if tasks.Summary == nil {
		tasks.Summary = map[string]int{}
	}
	writeJSON(w, http.StatusOK, snapshotResponse{
		APIVersion: APIVersion, GeneratedAt: a.now().UTC(), Projects: projects,
		Tasks: tasks.Tasks, TaskSummary: tasks.Summary, Sessions: sessions, Notifications: notifications,
	})
}
