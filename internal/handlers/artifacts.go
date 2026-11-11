package handlers

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
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

// ListArtifactAnnotations returns an artifact's annotations
// (?status=open|resolved|all, default all).
func (a *API) ListArtifactAnnotations(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	views, err := service.ListAnnotations(platformUIContext(r), chi.URLParam(r, "id"), r.URL.Query().Get("status"))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, views)
}

// CreateArtifactAnnotation stores the user's note on a text range or element.
// The anchor was computed inside the sandboxed artifact frame, so the service
// treats it as untrusted data.
func (a *API) CreateArtifactAnnotation(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		Version flexibleID                 `json:"version"`
		Anchor  application.ArtifactAnchor `json:"anchor"`
		Body    string                     `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	view, err := service.CreateAnnotation(platformUIContext(r), application.CreateArtifactAnnotationCommand{
		ArtifactID: chi.URLParam(r, "id"), Version: int(input.Version), Anchor: input.Anchor, Body: input.Body,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, view)
}

// ResolveArtifactAnnotation marks an annotation resolved, with an optional note.
func (a *API) ResolveArtifactAnnotation(w http.ResponseWriter, r *http.Request) {
	a.changeArtifactAnnotation(w, r, true)
}

// ReopenArtifactAnnotation puts a resolved annotation back to open.
func (a *API) ReopenArtifactAnnotation(w http.ResponseWriter, r *http.Request) {
	a.changeArtifactAnnotation(w, r, false)
}

func (a *API) changeArtifactAnnotation(w http.ResponseWriter, r *http.Request, resolve bool) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	annotationID, err := strconv.ParseInt(chi.URLParam(r, "annotationId"), 10, 64)
	if err != nil || annotationID <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid annotation ID")
		return
	}
	var input struct {
		ResolutionNote string `json:"resolution_note"`
	}
	// The body is optional: the UI resolves and reopens without one.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	command := application.ResolveArtifactAnnotationCommand{
		ArtifactID: chi.URLParam(r, "id"), AnnotationID: annotationID, ResolutionNote: input.ResolutionNote,
		Authorization: platformUIAuthorization(r),
	}
	var view application.ArtifactAnnotationView
	if resolve {
		view, err = service.ResolveAnnotation(platformUIContext(r), command)
	} else {
		view, err = service.ReopenAnnotation(platformUIContext(r), command)
	}
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}

// artifactDocumentIDPrefix marks an artifact in the document listings and doc
// cards that also carry Docs and plans ("plan:<session>" is the precedent). The
// UI document viewer hands any id with this prefix to the artifact viewer.
const artifactDocumentIDPrefix = "artifact:"

type artifactDocEntry struct {
	ID        string
	Title     string
	Type      string
	Status    string
	CreatedAt time.Time
}

// artifactDocEntries lists artifacts for the session/task document listings.
// The status column carries the version ("v3"), which is what a reader of that
// list needs to tell whether an artifact changed.
func (a *API) artifactDocEntries(r *http.Request, filter database.ArtifactFilter) []artifactDocEntry {
	if filter.SessionID == "" && filter.TaskID == 0 && filter.ProjectID == 0 {
		return nil
	}
	items, err := a.db.ListArtifacts(r.Context(), filter)
	if err != nil {
		return nil
	}
	entries := make([]artifactDocEntry, 0, len(items))
	for _, item := range items {
		entries = append(entries, artifactDocEntry{
			ID: artifactDocumentIDPrefix + item.ID, Title: item.Title, Type: "artifact",
			Status: fmt.Sprintf("v%d", item.CurrentVersion), CreatedAt: item.UpdatedAt,
		})
	}
	return entries
}

// platformArtifactNotifier pushes artifact publishes to connected UIs.
type platformArtifactNotifier struct{ api *API }

func newPlatformArtifactService(api *API, store application.ArtifactStore, effects application.ApplicationEffects) *application.ArtifactService {
	service := application.NewArtifactService(store, effects)
	service.SetNotifier(platformArtifactNotifier{api: api})
	return service
}

func (n platformArtifactNotifier) ArtifactPublished(_ context.Context, artifact application.ArtifactView, isNew bool) {
	if n.api == nil || n.api.hub == nil {
		return
	}
	conversationID := ""
	if artifact.ConversationID != nil {
		conversationID = strconv.FormatInt(*artifact.ConversationID, 10)
	}
	if isNew {
		// Same card the Docs get, so the artifact shows up where the agent is
		// working (floating over the terminal, in the structured view, in chat).
		n.api.hub.BroadcastChatDocCard(map[string]interface{}{
			"doc_id":          artifactDocumentIDPrefix + artifact.ID,
			"type":            "artifact",
			"title":           artifact.Title,
			"summary":         artifact.Summary,
			"conversation_id": conversationID,
			"session_id":      artifact.SessionID,
		})
	}
	update := map[string]interface{}{
		"artifact_id": artifact.ID,
		"version":     artifact.CurrentVersion,
		"title":       artifact.Title,
		"session_id":  artifact.SessionID,
		"is_new":      isNew,
	}
	if artifact.ProjectID != nil {
		update["project_id"] = *artifact.ProjectID
	}
	if artifact.TaskID != nil {
		update["task_id"] = *artifact.TaskID
	}
	n.api.hub.BroadcastArtifactUpdated(update)
}

func (n platformArtifactNotifier) ArtifactAnnotationsChanged(_ context.Context, artifact application.ArtifactView) {
	if n.api == nil || n.api.hub == nil {
		return
	}
	update := map[string]interface{}{"artifact_id": artifact.ID, "session_id": artifact.SessionID}
	if artifact.ProjectID != nil {
		update["project_id"] = *artifact.ProjectID
	}
	n.api.hub.BroadcastArtifactAnnotationsChanged(update)
}

// artifactContentSecurityPolicy is sent with every rendered artifact. The HTML
// is agent-generated and treated as hostile:
//   - sandbox allow-scripts: scripts run, but in an opaque origin (no cookies,
//     no storage, no same-origin access to OpenPoet), with no popups, forms,
//     top navigation or downloads — even when the URL is opened directly in a
//     tab instead of the viewer's sandboxed iframe;
//   - default-src 'none' + connect-src 'none': no network at all, so the page
//     cannot call the OpenPoet API or send anything out by fetch/XHR/WebSocket,
//     and cannot pull scripts, styles, images or fonts from any host;
//   - only inline scripts/styles and data:/blob: media are allowed, which is
//     what a self-contained artifact needs;
//   - webrtc 'block' closes peer connections in browsers that implement the
//     directive (others ignore it).
//
// CSP does not govern navigation: the artifact can still navigate its own
// frame to another URL, carrying only what the artifact itself contains.
const artifactContentSecurityPolicy = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; " +
	"style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; " +
	"connect-src 'none'; form-action 'none'; base-uri 'none'; frame-src 'none'; child-src 'none'; " +
	"worker-src 'none'; object-src 'none'; webrtc 'block'; frame-ancestors 'self'"

func setArtifactRenderHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", artifactContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cache-Control", "no-store")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("X-Frame-Options", "SAMEORIGIN")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), clipboard-read=(), clipboard-write=()")
}

// RenderArtifact serves one version of an artifact as an HTML document for the
// viewer's sandboxed iframe. It is the only artifact route that returns
// text/html, and it always carries the restrictive headers above — on errors
// too, so nothing under this path is ever served without them. The route lives
// under /api/ on purpose: the UI-cookie middleware never plants the UI secret
// on /api/ responses.
func (a *API) RenderArtifact(w http.ResponseWriter, r *http.Request) {
	setArtifactRenderHeaders(w)
	renderError := func(status int, message string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(message))
	}
	services, ready := a.platformApplicationServices()
	if !ready || services.Collaboration.Artifacts == nil {
		renderError(http.StatusServiceUnavailable, "artifact service is unavailable")
		return
	}
	version, valid := parseArtifactVersion(r.URL.Query().Get("v"))
	if !valid {
		renderError(http.StatusBadRequest, "v must be a positive integer")
		return
	}
	view, err := services.Collaboration.Artifacts.GetVersion(platformUIContext(r), chi.URLParam(r, "id"), version)
	if err != nil {
		status := http.StatusInternalServerError
		if application.ErrorIsKind(err, application.ErrorNotFound) {
			status = http.StatusNotFound
		} else if application.ErrorIsKind(err, application.ErrorValidation) {
			status = http.StatusBadRequest
		}
		renderError(status, "artifact not available")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(artifactRenderDocument(view.Content)))
}

// artifactBridgeScript is the annotation bridge appended to an artifact when it
// is served to the viewer. It is never stored with the artifact.
//
//go:embed artifact_bridge.js
var artifactBridgeScript string

// artifactRenderDocument returns the HTML sent to the sandboxed iframe for a
// stored version: the artifact, byte for byte, followed by the annotation
// bridge.
//
// The bridge goes after the whole document rather than before </body>: finding
// "the" closing tag in agent-written markup is guesswork (the text "</body>"
// can sit inside a script string), and a wrong guess would cut one of the
// artifact's own scripts in half. Browsers run a script that follows </html>
// as part of the body.
//
// The bridge runs inside the same sandbox as the artifact and gets no extra
// capability: it only reports selections to the embedding page and draws
// highlights. If an artifact's markup swallows it (a document that ends inside
// an unclosed <script> or comment) the artifact still renders and annotations
// are simply unavailable for that version; the viewer says so.
func artifactRenderDocument(html string) string {
	return html + "\n<script data-openpoet-bridge>" + strings.ReplaceAll(artifactBridgeScript, "</script", "<\\/script") + "</script>"
}
