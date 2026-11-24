package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/devboxpublish"
)

// SetDevboxPublisher wires the devbox port publisher (nil or disabled: the
// routes answer that the feature is not configured).
func (a *API) SetDevboxPublisher(s *devboxpublish.Service) { a.devboxPublisher = s }

// devboxCaller resolves the calling session and its project. Only a session
// (opst1 bearer) may publish: the container is derived from its project.
func (a *API) devboxCaller(w http.ResponseWriter, r *http.Request) (devboxpublish.Caller, bool) {
	if !a.devboxPublisher.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "publishing devbox ports is not configured on this OpenPoet instance")
		return devboxpublish.Caller{}, false
	}
	info, ok := resolvedActorFromContext(r.Context())
	if !ok || info.actor.Type != "session" || info.actor.ID == "" {
		respondError(w, http.StatusForbidden, "only an agent session running in a devbox can publish ports (call the tool from that session)")
		return devboxpublish.Caller{}, false
	}
	sess, err := a.db.GetSession(r.Context(), info.actor.ID)
	if err != nil || sess == nil {
		respondError(w, http.StatusForbidden, "the calling session was not found")
		return devboxpublish.Caller{}, false
	}
	project, err := a.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil || project == nil {
		respondError(w, http.StatusForbidden, "the calling session's project was not found")
		return devboxpublish.Caller{}, false
	}
	if project.Type != "remote" || !project.SSHHost.Valid || !project.SSHPort.Valid {
		respondError(w, http.StatusForbidden, "this session's project is not a devbox container on the OpenPoet host, so it cannot publish ports")
		return devboxpublish.Caller{}, false
	}
	return devboxpublish.Caller{
		SessionID: sess.ID, ProjectID: project.ID,
		SSHHost: project.SSHHost.String, SSHPort: int(project.SSHPort.Int64),
	}, true
}

func respondDevboxError(w http.ResponseWriter, err error) {
	var ue *devboxpublish.Error
	if errors.As(err, &ue) {
		respondError(w, ue.Status, ue.Msg)
		return
	}
	respondError(w, http.StatusInternalServerError, err.Error())
}

// ListDevboxPorts handles GET /api/devbox/ports.
func (a *API) ListDevboxPorts(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.devboxCaller(w, r)
	if !ok {
		return
	}
	views, err := a.devboxPublisher.List(r.Context(), caller)
	if err != nil {
		respondDevboxError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ports": views})
}

// PublishDevboxPort handles POST /api/devbox/ports {app, port, mode}.
func (a *API) PublishDevboxPort(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.devboxCaller(w, r)
	if !ok {
		return
	}
	var req struct {
		App  string `json:"app"`
		Port int    `json:"port"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: expected {app, port, mode}")
		return
	}
	view, err := a.devboxPublisher.Publish(r.Context(), caller, req.App, req.Port, req.Mode)
	if err != nil {
		respondDevboxError(w, err)
		return
	}
	status := http.StatusOK
	if view.Status == "created" {
		status = http.StatusCreated
	}
	respondJSON(w, status, view)
}

// UnpublishDevboxPort handles DELETE /api/devbox/ports/{app}.
func (a *API) UnpublishDevboxPort(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.devboxCaller(w, r)
	if !ok {
		return
	}
	view, err := a.devboxPublisher.Unpublish(r.Context(), caller, chi.URLParam(r, "app"))
	if err != nil {
		respondDevboxError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}
