package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"openpoet/internal/updater"
)

// CheckUpdate queries GitHub Releases for a newer version.
func (a *API) CheckUpdate(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		respondError(w, http.StatusServiceUnavailable, "updater not initialized")
		return
	}

	status, err := a.updater.CheckForUpdate(r.Context())
	if err != nil {
		respondJSON(w, http.StatusOK, &updater.UpdateStatus{
			CurrentVersion: a.updater.CurrentVersion,
			Error:          err.Error(),
			Managed:        a.updater.DetectPackageManager(),
		})
		return
	}

	respondJSON(w, http.StatusOK, status)
}

// ApplyUpdate downloads and installs the latest release, then restarts the process.
func (a *API) ApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		respondError(w, http.StatusServiceUnavailable, "updater not initialized")
		return
	}

	var body struct {
		Force bool `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	// Check for package manager
	if mgr := a.updater.DetectPackageManager(); mgr != "" {
		respondJSON(w, http.StatusConflict, map[string]interface{}{
			"error":   "managed_install",
			"message": fmt.Sprintf("Binary is managed by %s. Use '%s upgrade openpoet' instead.", mgr, mgr),
			"manager": mgr,
		})
		return
	}

	// Check for active sessions
	runningSessions := a.sessionMgr.ListRunningSessions()
	if len(runningSessions) > 0 && !body.Force {
		respondJSON(w, http.StatusConflict, map[string]interface{}{
			"error":         "active_sessions",
			"message":       fmt.Sprintf("%d Claude Code session(s) are running. Stop them first or force update.", len(runningSessions)),
			"session_count": len(runningSessions),
		})
		return
	}

	// Check for available update
	status, err := a.updater.CheckForUpdate(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !status.Available {
		respondJSON(w, http.StatusOK, map[string]string{"status": "already_up_to_date"})
		return
	}

	// Download and apply
	if err := a.updater.DownloadAndApply(r.Context(), status); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Respond success
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "applied",
		"version": status.LatestVersion,
		"message": "Update applied. Restarting...",
	})

	// Broadcast restart notification to all connected clients
	a.hub.BroadcastStateUpdate("update", map[string]interface{}{
		"action":  "restarting",
		"version": status.LatestVersion,
	})

	// Schedule restart after response is flushed
	go func() {
		time.Sleep(1 * time.Second) // let response flush to client
		updater.RestartSelf()
	}()
}
