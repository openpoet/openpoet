package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"openpoet/internal/application"
	"openpoet/internal/updater"
)

// CheckUpdate queries GitHub Releases for a newer version.
func (a *API) CheckUpdate(w http.ResponseWriter, r *http.Request) {
	services, ready := a.platformApplicationServices()
	if !ready || services.Execution.Updates == nil || a.updater == nil {
		respondError(w, http.StatusServiceUnavailable, "updater not initialized")
		return
	}

	status, err := services.Execution.Updates.CheckForUpdate(platformUIContext(r))
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
	services, ready := a.platformApplicationServices()
	if !ready || services.Execution.UpdateMutations == nil || a.updater == nil {
		respondError(w, http.StatusServiceUnavailable, "updater not initialized")
		return
	}

	var body struct {
		Force bool `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	authorization := platformUIAuthorization(r)
	command := application.ApplyUpdateCommand{Force: body.Force, Authorization: authorization}
	if body.Force {
		command.ForceAuthorization = &application.ForceUpdateAuthorization{
			Authorization:              platformUIAuthorization(r),
			AcknowledgesActiveSessions: true,
		}
	}
	result, err := services.Execution.UpdateMutations.Apply(platformUIContext(r), command)
	if err != nil {
		var appErr *application.Error
		if errors.As(err, &appErr) && appErr.Kind == application.ErrorConflict {
			switch appErr.Code {
			case "managed_install":
				mgr := a.updater.DetectPackageManager()
				respondJSON(w, http.StatusConflict, map[string]interface{}{
					"error":   appErr.Code,
					"message": fmt.Sprintf("Binary is managed by %s. Use '%s upgrade openpoet' instead.", mgr, mgr),
					"manager": mgr,
				})
				return
			case "active_sessions":
				count := a.ActiveSessionCount()
				respondJSON(w, http.StatusConflict, map[string]interface{}{
					"error":         appErr.Code,
					"message":       fmt.Sprintf("%d Claude Code session(s) are running. Stop them first or force update.", count),
					"session_count": count,
				})
				return
			}
		}
		respondApplicationError(w, err)
		return
	}
	if result.Status == "already_up_to_date" {
		respondJSON(w, http.StatusOK, map[string]string{"status": result.Status})
		return
	}

	// Respond success
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  result.Status,
		"version": result.Version,
		"message": "Update applied. Restarting...",
	})
}
