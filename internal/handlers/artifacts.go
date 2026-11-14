package handlers

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
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

// artifactPublishResponse is what a create, update or patch answers. "link" is
// the in-app path of the artifact; "url" is that path on the public base URL,
// and is empty when none is configured. "size_bytes" and "sha256" describe the
// stored HTML of the version; "source_path" is the file it was read from, when
// it was published from one.
func (a *API) artifactPublishResponse(ctx context.Context, result application.ArtifactPublishResult, sourcePath string) map[string]any {
	link := application.ArtifactLinkPath(result.Artifact.ID, 0)
	response := map[string]any{
		"id":         result.Artifact.ID,
		"version":    result.Version,
		"created":    result.Created,
		"redacted":   result.Redacted,
		"questions":  result.Questions,
		"size_bytes": result.SizeBytes,
		"sha256":     result.SHA256,
		"link":       link,
		"url":        a.Links().Absolute(ctx, link),
		"artifact":   result.Artifact,
	}
	if sourcePath != "" {
		response["source_path"] = sourcePath
	}
	if result.Replacements > 0 {
		response["replacements"] = result.Replacements
	}
	if result.RedactionCount > 0 {
		response["redaction_count"] = result.RedactionCount
		response["redactions"] = result.Redactions
	}
	if result.DryRun {
		response["dry_run"] = true
	}
	if result.Duplicate {
		response["duplicate"] = true
	}
	return response
}

// artifactDetailResponse is an artifact with the links that open it.
type artifactDetailResponse struct {
	application.ArtifactView
	Link string `json:"link"`
	URL  string `json:"url"`
}

func (a *API) artifactDetailResponse(ctx context.Context, view application.ArtifactView) artifactDetailResponse {
	link := application.ArtifactLinkPath(view.ID, 0)
	return artifactDetailResponse{ArtifactView: view, Link: link, URL: a.Links().Absolute(ctx, link)}
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
		HTMLPath       string     `json:"html_path"`
		ProjectID      flexibleID `json:"project_id"`
		TaskID         flexibleID `json:"task_id"`
		ConversationID flexibleID `json:"conversation_id"`
		SessionID      string     `json:"session_id"`
		DryRun         bool       `json:"dry_run"`
	}
	if !decodeArtifactRequest(w, r, &input) {
		return
	}
	html, sourcePath, err := a.artifactHTMLInput(r, input.HTML, input.HTMLPath, input.SessionID)
	if err != nil {
		respondArtifactFileError(w, err)
		return
	}
	result, err := service.Create(platformUIContext(r), application.CreateArtifactCommand{
		Title: input.Title, Summary: input.Summary, Content: html,
		ProjectID: input.ProjectID.pointer(), TaskID: input.TaskID.pointer(), ConversationID: input.ConversationID.pointer(),
		SessionID: artifactSessionID(r, input.SessionID), DryRun: input.DryRun, Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK // a dry run, or the artifact this session had just created
	}
	respondJSON(w, status, a.artifactPublishResponse(r.Context(), result, sourcePath))
}

// UpdateArtifact publishes a new version of an existing artifact.
func (a *API) UpdateArtifact(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		HTML        string     `json:"html"`
		HTMLPath    string     `json:"html_path"`
		ChangeNote  string     `json:"change_note"`
		BaseVersion flexibleID `json:"base_version"`
		SessionID   string     `json:"session_id"`
		Title       *string    `json:"title"`
		Summary     *string    `json:"summary"`
		DryRun      bool       `json:"dry_run"`
	}
	if !decodeArtifactRequest(w, r, &input) {
		return
	}
	html, sourcePath, err := a.artifactHTMLInput(r, input.HTML, input.HTMLPath, input.SessionID)
	if err != nil {
		respondArtifactFileError(w, err)
		return
	}
	result, err := service.Update(platformUIContext(r), application.UpdateArtifactCommand{
		ArtifactID: chi.URLParam(r, "id"), Content: html, ChangeNote: input.ChangeNote,
		BaseVersion: int(input.BaseVersion), SessionID: artifactSessionID(r, input.SessionID),
		Title: input.Title, Summary: input.Summary, DryRun: input.DryRun,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, a.artifactPublishResponse(r.Context(), result, sourcePath))
}

// PatchArtifactContent publishes a new version made of the HTML of
// base_version with text replacements applied: POST /api/artifacts/{id}/patch
// {base_version, edits:[{old, new, replace_all}], change_note}. A small change
// then costs its own size instead of the whole document.
func (a *API) PatchArtifactContent(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		BaseVersion flexibleID                 `json:"base_version"`
		Edits       []application.ArtifactEdit `json:"edits"`
		ChangeNote  string                     `json:"change_note"`
		SessionID   string                     `json:"session_id"`
		Title       *string                    `json:"title"`
		Summary     *string                    `json:"summary"`
		DryRun      bool                       `json:"dry_run"`
	}
	if !decodeArtifactRequest(w, r, &input) {
		return
	}
	result, err := service.Patch(platformUIContext(r), application.PatchArtifactCommand{
		ArtifactID: chi.URLParam(r, "id"), BaseVersion: int(input.BaseVersion), Edits: input.Edits,
		ChangeNote: input.ChangeNote, SessionID: artifactSessionID(r, input.SessionID),
		Title: input.Title, Summary: input.Summary, DryRun: input.DryRun,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, a.artifactPublishResponse(r.Context(), result, ""))
}

// ExportArtifactVersion writes the stored HTML of one version, byte for byte,
// to a file in the calling session's workspace:
// POST /api/artifacts/{id}/versions/{version}/export {path}. It answers with
// the path written and the size and SHA-256 of the HTML, and never returns the
// HTML itself.
func (a *API) ExportArtifactVersion(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	version, valid := parseArtifactVersion(chi.URLParam(r, "version"))
	if !valid {
		respondError(w, http.StatusBadRequest, "version must be a positive integer or \"current\"")
		return
	}
	var input struct {
		Path      string `json:"path"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	view, err := service.GetVersion(platformUIContext(r), chi.URLParam(r, "id"), version)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	workspace, err := a.artifactWorkspaceFor(r, input.SessionID)
	if err != nil {
		respondArtifactFileError(w, err)
		return
	}
	defer workspace.close()
	written, err := workspace.write(input.Path, view.Content)
	if err != nil {
		respondArtifactFileError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"artifact_id": view.ArtifactID, "version": view.Version, "path": written,
		"size_bytes": view.SizeBytes, "sha256": view.SHA256,
	})
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
	respondJSON(w, http.StatusOK, a.artifactDetailResponse(r.Context(), view))
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
// (?status=open|resolved|all, default all). With ?placement=1 each note made
// on an earlier version also says whether what it points at is still in the
// current version, and each resolved one the version it was resolved on.
func (a *API) ListArtifactAnnotations(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	list := service.ListAnnotations
	if placement := r.URL.Query().Get("placement"); placement == "1" || placement == "true" {
		list = service.ListAnnotationsWithPlacement
	}
	views, err := list(platformUIContext(r), chi.URLParam(r, "id"), r.URL.Query().Get("status"))
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

// SendArtifactAnnotationsToAgent tells the session that published the artifact
// about its open annotations and records the send on each of them:
// POST /api/artifacts/{id}/send-to-agent. It answers with the session that was
// told, when, and the open annotations as they are stored now. When the session
// cannot take input the request fails and nothing is marked as sent.
func (a *API) SendArtifactAnnotationsToAgent(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	result, err := service.SendAnnotationsToAgent(platformUIContext(r), application.SendArtifactAnnotationsCommand{
		ArtifactID: chi.URLParam(r, "id"), Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// ListArtifactQuestions returns the questions one version declares
// (?v=<n>, default the current version).
func (a *API) ListArtifactQuestions(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	version, valid := parseArtifactVersion(r.URL.Query().Get("v"))
	if !valid {
		respondError(w, http.StatusBadRequest, "v must be a positive integer")
		return
	}
	view, err := service.Questions(platformUIContext(r), chi.URLParam(r, "id"), version)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}

// SaveArtifactAnswers stores the reader's answers to questions of one version:
// PUT /api/artifacts/{id}/answers {version, answers:[{question, options, text,
// comment}]}. The request is built by the OpenPoet page from what the user
// confirmed there; the service checks every answer against the questions the
// version declared, so nothing the artifact's own script says can add to them.
func (a *API) SaveArtifactAnswers(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	var input struct {
		Version flexibleID                        `json:"version"`
		Answers []application.ArtifactAnswerInput `json:"answers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	views, err := service.AnswerQuestions(platformUIContext(r), application.AnswerArtifactQuestionsCommand{
		ArtifactID: chi.URLParam(r, "id"), Version: int(input.Version), Answers: input.Answers,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, views)
}

// WithdrawArtifactAnswer removes the open answer to one question.
func (a *API) WithdrawArtifactAnswer(w http.ResponseWriter, r *http.Request) {
	service, ok := a.artifactService(w)
	if !ok {
		return
	}
	if err := service.WithdrawAnswer(platformUIContext(r), application.WithdrawArtifactAnswerCommand{
		ArtifactID: chi.URLParam(r, "id"), QuestionID: chi.URLParam(r, "questionId"),
		Authorization: platformUIAuthorization(r),
	}); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// newPlatformArtifactService builds the artifact service. agentInput is how
// "Send to agent" types into a session (the session service).
func newPlatformArtifactService(api *API, store application.ArtifactStore, effects application.ApplicationEffects, agentInput application.ArtifactAgentInput) *application.ArtifactService {
	service := application.NewArtifactService(store, effects)
	service.SetNotifier(platformArtifactNotifier{api: api})
	if agentInput != nil {
		service.SetAgentInput(agentInput)
	}
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
//   - webrtc 'block' is kept as defence in depth for browsers that implement
//     the directive, but Chromium ignores it, so peer connections are actually
//     closed by the guard script injected first in every rendered document (see
//     artifactRenderDocument); the CSP is not the only line of defence here.
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

// artifactGuardScript neuters the WebRTC peer-connection constructors. It must
// run before any agent-written script, so artifactRenderDocument injects it as
// the first script of the rendered document. It is never stored with the
// artifact.
//
//go:embed artifact_guard.js
var artifactGuardScript string

// artifactScriptBlock wraps embedded JavaScript in an inline <script> the
// agent's markup cannot break out of: the only way an inline script ends is a
// literal "</script" (any case), which is escaped so the browser keeps the
// whole source inside this element.
func artifactScriptBlock(attr, source string) string {
	return "<script " + attr + ">" + reCloseScript.ReplaceAllString(source, "<\\/script") + "</script>"
}

var reCloseScript = regexp.MustCompile(`(?i)</script`)

// artifactRenderDocument returns the HTML sent to the sandboxed iframe for a
// stored version: the WebRTC guard first, then the artifact, then the
// annotation bridge.
//
// The guard goes first because it has to run before any script the artifact
// carries (a reference grabbed before the guard runs would survive). It is
// injected right after a leading <!doctype>, when there is one, so a
// standards-mode document stays in standards mode — a script placed before the
// doctype would force quirks mode. A document with no doctype is already in
// quirks mode, so the guard is simply prepended and the mode does not change.
//
// The bridge goes after the whole document rather than before </body>: finding
// "the" closing tag in agent-written markup is guesswork (the text "</body>"
// can sit inside a script string), and a wrong guess would cut one of the
// artifact's own scripts in half. Browsers run a script that follows </html>
// as part of the body.
//
// Both run inside the same sandbox as the artifact and get no extra capability.
// If an artifact's markup swallows the bridge (a document that ends inside an
// unclosed <script> or comment) the artifact still renders and annotations are
// simply unavailable for that version; the viewer says so. The guard cannot be
// swallowed the same way: it precedes the artifact's markup.
func artifactRenderDocument(html string) string {
	guard := artifactScriptBlock("data-openpoet-guard", artifactGuardScript)
	bridge := artifactScriptBlock("data-openpoet-bridge", artifactBridgeScript)
	return injectAfterDoctype(html, guard) + "\n" + bridge
}

// injectAfterDoctype inserts snippet right after a leading <!doctype ...>
// declaration, past any leading whitespace or BOM, so a standards-mode document
// keeps its doctype as the first thing and stays in standards mode. A document
// with no leading doctype is already in quirks mode; the snippet is prepended,
// which keeps it in quirks mode. A doctype has no '>' inside it, so the first
// '>' after "<!doctype" always closes it.
func injectAfterDoctype(html, snippet string) string {
	rest := strings.TrimLeft(html, " \t\r\n\f\v")
	rest = strings.TrimPrefix(rest, "\ufeff")
	rest = strings.TrimLeft(rest, " \t\r\n\f\v")
	lead := len(html) - len(rest)
	if len(rest) >= len("<!doctype") && strings.EqualFold(rest[:len("<!doctype")], "<!doctype") {
		if gt := strings.IndexByte(rest, '>'); gt >= 0 {
			end := lead + gt + 1
			return html[:end] + "\n" + snippet + html[end:]
		}
	}
	return snippet + "\n" + html
}
