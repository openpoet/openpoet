package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
)

// maxArtifactRequestBytes bounds a create/update request body. The HTML limit
// itself (application.MaxArtifactHTMLBytes) is enforced by the service; this
// leaves room for JSON string escaping on top of it.
const maxArtifactRequestBytes = 4*application.MaxArtifactHTMLBytes + 64<<10

// flexibleID accepts a JSON number or a numeric string, because MCP tool
// callers pass ids as strings while the UI passes numbers.
type flexibleID int64

func (f *flexibleID) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer")
	}
	*f = flexibleID(value)
	return nil
}

func (f flexibleID) pointer() *int64 {
	if f <= 0 {
		return nil
	}
	value := int64(f)
	return &value
}

// artifactSessionID returns the session an artifact request is attributed to.
// A session-authenticated caller is always attributed to its own session, so a
// session cannot publish under another session's identity.
func artifactSessionID(r *http.Request, claimed string) string {
	if actor := requestActor(r); actor.Type == "session" && actor.ID != "" {
		return actor.ID
	}
	return strings.TrimSpace(claimed)
}

func artifactLink(id string) string {
	return "/app/artifact/" + id
}

func artifactPublishResponse(result application.ArtifactPublishResult) map[string]any {
	return map[string]any{
		"id":       result.Artifact.ID,
		"version":  result.Version,
		"created":  result.Created,
		"redacted": result.Redacted,
		"link":     artifactLink(result.Artifact.ID),
		"artifact": result.Artifact,
	}
}

func (a *API) artifactService(w http.ResponseWriter) (*application.ArtifactService, bool) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return nil, false
	}
	if services.Collaboration.Artifacts == nil {
		respondError(w, http.StatusServiceUnavailable, "artifact service is unavailable")
		return nil, false
	}
	return services.Collaboration.Artifacts, true
}

func decodeArtifactRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxArtifactRequestBytes)).Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("Request body is too large; artifact HTML is limited to %d bytes per version", application.MaxArtifactHTMLBytes),
				"code":  "content_too_large",
			})
			return false
		}
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return false
	}
	return true
}

// CreateArtifact publishes a new HTML artifact (version 1).
func (a *API) CreateArtifact(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		Title          string     `json:"title"`
		Summary        string     `json:"summary"`
		HTML           string     `json:"html"`
		ProjectID      flexibleID `json:"project_id"`
		TaskID         flexibleID `json:"task_id"`
		ConversationID flexibleID `json:"conversation_id"`
		SessionID      string     `json:"session_id"`
	}
	if !decodeArtifactRequest(w, r, &input) {
		return
	}
	result, err := service.Create(platformUIContext(r), application.CreateArtifactCommand{
		Title: input.Title, Summary: input.Summary, Content: input.HTML,
		ProjectID: input.ProjectID.pointer(), TaskID: input.TaskID.pointer(), ConversationID: input.ConversationID.pointer(),
		SessionID: artifactSessionID(r, input.SessionID), Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, artifactPublishResponse(result))
}

// UpdateArtifact publishes a new version of an existing artifact.
func (a *API) UpdateArtifact(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		HTML        string     `json:"html"`
		ChangeNote  string     `json:"change_note"`
		BaseVersion flexibleID `json:"base_version"`
		SessionID   string     `json:"session_id"`
	}
	if !decodeArtifactRequest(w, r, &input) {
		return
	}
	result, err := service.Update(platformUIContext(r), application.UpdateArtifactCommand{
		ArtifactID: chi.URLParam(r, "id"), Content: input.HTML, ChangeNote: input.ChangeNote,
		BaseVersion: int(input.BaseVersion), SessionID: artifactSessionID(r, input.SessionID),
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, artifactPublishResponse(result))
}

// PatchArtifact changes the title, summary or status (active/archived).
func (a *API) PatchArtifact(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		Title   *string `json:"title"`
		Summary *string `json:"summary"`
		Status  *string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	view, err := service.UpdateMeta(platformUIContext(r), application.UpdateArtifactMetaCommand{
		ArtifactID: chi.URLParam(r, "id"), Title: input.Title, Summary: input.Summary, Status: input.Status,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}

// ListArtifacts lists artifacts (no HTML) filtered by project, session or task.
func (a *API) ListArtifacts(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	projectID, _ := strconv.ParseInt(query.Get("project_id"), 10, 64)
	taskID, _ := strconv.ParseInt(query.Get("task_id"), 10, 64)
	limit, _ := strconv.Atoi(query.Get("limit"))
	views, err := service.List(platformUIContext(r), application.ArtifactListQuery{
		ProjectID: projectID, TaskID: taskID, SessionID: query.Get("session_id"),
		IncludeArchived: query.Get("include_archived") == "1" || query.Get("include_archived") == "true",
		Limit:           limit,
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, views)
}

// GetArtifact returns an artifact's metadata and version history (no HTML).
func (a *API) GetArtifact(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	view, err := service.Get(platformUIContext(r), chi.URLParam(r, "id"))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}

// GetArtifactVersion returns one version with its HTML as JSON data. This is
// the endpoint agents and the download button read; it never serves text/html.
func (a *API) GetArtifactVersion(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	version, valid := parseArtifactVersion(chi.URLParam(r, "version"))
	if !valid {
		respondError(w, http.StatusBadRequest, "version must be a positive integer or \"current\"")
		return
	}
	view, err := service.GetVersion(platformUIContext(r), chi.URLParam(r, "id"), version)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}

// parseArtifactVersion maps "", "current" and "latest" to 0 (the current
// version) and otherwise requires a positive integer.
func parseArtifactVersion(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "current" || raw == "latest" {
		return 0, true
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version <= 0 {
		return 0, false
	}
	return version, true
}
