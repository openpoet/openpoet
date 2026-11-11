package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"openpoet/internal/database"
)

// MaxArtifactHTMLBytes bounds the HTML of one artifact version.
const MaxArtifactHTMLBytes = 2 << 20

const maxArtifactChangeNoteRunes = 1000

// artifactHTMLPattern is a cheap guard against publishing markdown or plain
// text as an artifact: the content must contain at least one HTML element.
var artifactHTMLPattern = regexp.MustCompile(`(?i)<\s*(!doctype|html|head|body|main|div|section|article|header|nav|svg|canvas|table|h[1-6]|p|style|script)\b`)

type ArtifactStore interface {
	GetProject(context.Context, int64) (*database.Project, error)
	GetSession(context.Context, string) (*database.Session, error)
	GetTask(context.Context, int64) (*database.ProjectTask, error)
	CreateArtifact(context.Context, *database.Artifact, *database.ArtifactVersion) error
	GetArtifact(context.Context, string) (*database.Artifact, error)
	ListArtifacts(context.Context, database.ArtifactFilter) ([]database.ArtifactListItem, error)
	ListArtifactVersions(context.Context, string) ([]database.ArtifactVersion, error)
	GetArtifactVersion(context.Context, string, int) (*database.ArtifactVersion, error)
	AppendArtifactVersion(context.Context, string, int, *database.ArtifactVersion) (*database.Artifact, bool, error)
	UpdateArtifactMeta(context.Context, string, *string, *string, *string) error
}

type ArtifactView struct {
	ID              string                `json:"id"`
	Title           string                `json:"title"`
	Summary         string                `json:"summary,omitempty"`
	Status          string                `json:"status"`
	ProjectID       *int64                `json:"project_id,omitempty"`
	TaskID          *int64                `json:"task_id,omitempty"`
	ConversationID  *int64                `json:"conversation_id,omitempty"`
	SessionID       string                `json:"session_id,omitempty"`
	CurrentVersion  int                   `json:"current_version"`
	SizeBytes       int64                 `json:"size_bytes,omitempty"`
	OpenAnnotations int                   `json:"open_annotations"`
	CreatedBy       string                `json:"created_by,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
	Versions        []ArtifactVersionView `json:"versions,omitempty"`
}

type ArtifactVersionView struct {
	ArtifactID string    `json:"artifact_id"`
	Version    int       `json:"version"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	ChangeNote string    `json:"change_note,omitempty"`
	CreatedBy  string    `json:"created_by,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Content    string    `json:"content,omitempty"`
}

// ArtifactPublishResult reports what a create or update did. Created is false
// when the HTML was identical to the current version and nothing was stored.
// Redacted is true when credential-looking text was masked in the stored HTML
// (which can change scripts, so the publisher needs to know).
type ArtifactPublishResult struct {
	Artifact ArtifactView `json:"artifact"`
	Version  int          `json:"version"`
	Created  bool         `json:"created"`
	Redacted bool         `json:"redacted,omitempty"`
}

type CreateArtifactCommand struct {
	Title          string
	Summary        string
	Content        string
	ProjectID      *int64
	TaskID         *int64
	ConversationID *int64
	SessionID      string
	Authorization  ActionAuthorization
}

type UpdateArtifactCommand struct {
	ArtifactID    string
	Content       string
	ChangeNote    string
	BaseVersion   int
	SessionID     string
	Authorization ActionAuthorization
}

type UpdateArtifactMetaCommand struct {
	ArtifactID    string
	Title         *string
	Summary       *string
	Status        *string
	Authorization ActionAuthorization
}

type ArtifactListQuery struct {
	ProjectID       int64
	SessionID       string
	TaskID          int64
	IncludeArchived bool
	Limit           int
}

// ArtifactNotifier is a post-commit effect that tells connected UIs about a
// publish, so an open viewer can offer the new version and a new artifact gets
// its card. It carries no HTML.
type ArtifactNotifier interface {
	ArtifactPublished(ctx context.Context, artifact ArtifactView, isNew bool)
}

type ArtifactService struct {
	store    ArtifactStore
	effects  ApplicationEffects
	notifier ArtifactNotifier
}

func NewArtifactService(store ArtifactStore, effects ApplicationEffects) *ArtifactService {
	return &ArtifactService{store: store, effects: effects}
}

// SetNotifier installs the UI notifier. It is optional: without one, publishes
// only emit the application change event.
func (s *ArtifactService) SetNotifier(notifier ArtifactNotifier) {
	s.notifier = notifier
}

func (s *ArtifactService) notifyPublished(ctx context.Context, artifact ArtifactView, isNew bool) {
	if s.notifier != nil {
		s.notifier.ArtifactPublished(ctx, artifact, isNew)
	}
}

func (s *ArtifactService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("artifacts")
}

func (s *ArtifactService) Create(ctx context.Context, command CreateArtifactCommand) (ArtifactPublishResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactPublishResult{}, err
	}
	title, err := boundedRedactedInput(command.Title, maxApplicationTitleRunes, "Artifact title", false)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	if title == "" {
		title = "Artifact"
	}
	summary, err := boundedRedactedInput(command.Summary, maxApplicationSummaryRunes, "Artifact summary", false)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	content, redacted, err := normalizeArtifactHTML(command.Content)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	sessionID := strings.TrimSpace(command.SessionID)
	if len(sessionID) > 200 {
		return ArtifactPublishResult{}, validationError("invalid_session_id", "Session ID exceeds its bounded limit")
	}
	if command.ConversationID != nil && *command.ConversationID <= 0 {
		return ArtifactPublishResult{}, validationError("invalid_conversation_id", "Conversation ID must be positive")
	}

	// Links: an explicit project or task must exist; whatever is missing is
	// derived from the task, then from the publishing session.
	var projectID, taskID sql.NullInt64
	if command.ProjectID != nil {
		if err := validateBoundedID(*command.ProjectID, "Project"); err != nil {
			return ArtifactPublishResult{}, err
		}
		if project, err := s.store.GetProject(ctx, *command.ProjectID); err != nil || project == nil {
			return ArtifactPublishResult{}, notFoundError("project_not_found", "Project not found", err)
		}
		projectID = sql.NullInt64{Int64: *command.ProjectID, Valid: true}
	}
	if command.TaskID != nil {
		if err := validateBoundedID(*command.TaskID, "Task"); err != nil {
			return ArtifactPublishResult{}, err
		}
		task, err := s.store.GetTask(ctx, *command.TaskID)
		if err != nil || task == nil {
			return ArtifactPublishResult{}, notFoundError("task_not_found", "Task not found", err)
		}
		if projectID.Valid && task.ProjectID != projectID.Int64 {
			return ArtifactPublishResult{}, validationError("task_project_mismatch", "Task does not belong to the given project")
		}
		taskID = sql.NullInt64{Int64: task.ID, Valid: true}
		projectID = sql.NullInt64{Int64: task.ProjectID, Valid: true}
	}
	if sessionID != "" && (!projectID.Valid || !taskID.Valid) {
		if session, err := s.store.GetSession(ctx, sessionID); err == nil && session != nil && session.ID != "" {
			if !projectID.Valid && session.ProjectID > 0 {
				projectID = sql.NullInt64{Int64: session.ProjectID, Valid: true}
			}
			if !taskID.Valid && session.TaskID.Valid && session.TaskID.Int64 > 0 &&
				(!projectID.Valid || projectID.Int64 == session.ProjectID) {
				taskID = session.TaskID
			}
		}
	}

	actor := command.Authorization.Actor.eventValue()
	artifact := &database.Artifact{
		ID: uuid.NewString()[:8], ProjectID: projectID, SessionID: sessionID, TaskID: taskID,
		Title: title, Summary: summary, CreatedBy: actor,
	}
	if command.ConversationID != nil {
		artifact.ConversationID = sql.NullInt64{Int64: *command.ConversationID, Valid: true}
	}
	version := &database.ArtifactVersion{
		Content: content, SizeBytes: int64(len(content)), SHA256: artifactContentHash(content),
		CreatedBy: actor, SessionID: sessionID,
	}
	if err := s.store.CreateArtifact(ctx, artifact, version); err != nil {
		return ArtifactPublishResult{}, err
	}
	stored, err := s.store.GetArtifact(ctx, artifact.ID)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: "created", Meta: artifactChangeMeta(stored)})
	view := artifactView(stored)
	view.SizeBytes = version.SizeBytes
	s.notifyPublished(ctx, view, true)
	return ArtifactPublishResult{Artifact: view, Version: 1, Created: true, Redacted: redacted}, nil
}

func (s *ArtifactService) Update(ctx context.Context, command UpdateArtifactCommand) (ArtifactPublishResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactPublishResult{}, err
	}
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	if command.BaseVersion < 0 {
		return ArtifactPublishResult{}, validationError("invalid_base_version", "base_version cannot be negative")
	}
	content, redacted, err := normalizeArtifactHTML(command.Content)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	note, err := boundedRedactedInput(command.ChangeNote, maxArtifactChangeNoteRunes, "Artifact change note", false)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	sessionID := strings.TrimSpace(command.SessionID)
	if len(sessionID) > 200 {
		return ArtifactPublishResult{}, validationError("invalid_session_id", "Session ID exceeds its bounded limit")
	}
	version := &database.ArtifactVersion{
		Content: content, SizeBytes: int64(len(content)), SHA256: artifactContentHash(content),
		ChangeNote: note, CreatedBy: command.Authorization.Actor.eventValue(), SessionID: sessionID,
	}
	stored, created, err := s.store.AppendArtifactVersion(ctx, id, command.BaseVersion, version)
	if err != nil {
		if errors.Is(err, database.ErrArtifactVersionConflict) && stored != nil {
			return ArtifactPublishResult{}, conflictError("artifact_version_conflict",
				fmt.Sprintf("Artifact %s is at version %d, not base_version %d. Read the current version, reapply your change and update again.", id, stored.CurrentVersion, command.BaseVersion))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ArtifactPublishResult{}, notFoundError("artifact_not_found", "Artifact not found", err)
		}
		return ArtifactPublishResult{}, err
	}
	view := artifactView(stored)
	view.SizeBytes = version.SizeBytes
	if created {
		publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: "version_published", Meta: artifactChangeMeta(stored)})
		s.notifyPublished(ctx, view, false)
	}
	return ArtifactPublishResult{Artifact: view, Version: stored.CurrentVersion, Created: created, Redacted: redacted}, nil
}

func (s *ArtifactService) UpdateMeta(ctx context.Context, command UpdateArtifactMetaCommand) (ArtifactView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactView{}, err
	}
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return ArtifactView{}, err
	}
	var title, summary, status *string
	if command.Title != nil {
		value, err := boundedRedactedInput(*command.Title, maxApplicationTitleRunes, "Artifact title", true)
		if err != nil {
			return ArtifactView{}, err
		}
		title = &value
	}
	if command.Summary != nil {
		value, err := boundedRedactedInput(*command.Summary, maxApplicationSummaryRunes, "Artifact summary", false)
		if err != nil {
			return ArtifactView{}, err
		}
		summary = &value
	}
	if command.Status != nil {
		value := strings.TrimSpace(*command.Status)
		if value != "active" && value != "archived" {
			return ArtifactView{}, validationError("invalid_artifact_status", "Artifact status must be active or archived")
		}
		status = &value
	}
	if err := s.store.UpdateArtifactMeta(ctx, id, title, summary, status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ArtifactView{}, notFoundError("artifact_not_found", "Artifact not found", err)
		}
		return ArtifactView{}, err
	}
	stored, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactView{}, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: "updated", Meta: artifactChangeMeta(stored)})
	return artifactView(stored), nil
}

// Get returns the artifact with the metadata of every version (no HTML).
func (s *ArtifactService) Get(ctx context.Context, artifactID string) (ArtifactView, error) {
	id, err := normalizeArtifactID(artifactID)
	if err != nil {
		return ArtifactView{}, err
	}
	stored, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactView{}, err
	}
	versions, err := s.store.ListArtifactVersions(ctx, id)
	if err != nil {
		return ArtifactView{}, err
	}
	view := artifactView(stored)
	for i := range versions {
		view.Versions = append(view.Versions, artifactVersionView(&versions[i], false))
		if versions[i].Version == stored.CurrentVersion {
			view.SizeBytes = versions[i].SizeBytes
		}
	}
	return view, nil
}

// GetVersion returns one version with its HTML. version 0 means the current one.
func (s *ArtifactService) GetVersion(ctx context.Context, artifactID string, version int) (ArtifactVersionView, error) {
	id, err := normalizeArtifactID(artifactID)
	if err != nil {
		return ArtifactVersionView{}, err
	}
	if version < 0 {
		return ArtifactVersionView{}, validationError("invalid_artifact_version", "Artifact version cannot be negative")
	}
	if version == 0 {
		stored, err := s.artifact(ctx, id)
		if err != nil {
			return ArtifactVersionView{}, err
		}
		version = stored.CurrentVersion
	}
	stored, err := s.store.GetArtifactVersion(ctx, id, version)
	if err != nil || stored == nil {
		return ArtifactVersionView{}, notFoundError("artifact_version_not_found", "Artifact version not found", err)
	}
	return artifactVersionView(stored, true), nil
}

func (s *ArtifactService) List(ctx context.Context, query ArtifactListQuery) ([]ArtifactView, error) {
	if query.ProjectID < 0 || query.TaskID < 0 {
		return nil, validationError("invalid_id", "Project and task IDs must be positive")
	}
	sessionID := strings.TrimSpace(query.SessionID)
	if len(sessionID) > 200 {
		return nil, validationError("invalid_session_id", "Session ID exceeds its bounded limit")
	}
	items, err := s.store.ListArtifacts(ctx, database.ArtifactFilter{
		ProjectID: query.ProjectID, SessionID: sessionID, TaskID: query.TaskID,
		IncludeArchived: query.IncludeArchived, Limit: query.Limit,
	})
	if err != nil {
		return nil, err
	}
	views := make([]ArtifactView, 0, len(items))
	for i := range items {
		view := artifactView(&items[i].Artifact)
		view.SizeBytes = items[i].SizeBytes
		view.OpenAnnotations = items[i].OpenAnnotations
		views = append(views, view)
	}
	return views, nil
}

func (s *ArtifactService) artifact(ctx context.Context, id string) (*database.Artifact, error) {
	stored, err := s.store.GetArtifact(ctx, id)
	if err != nil || stored == nil {
		return nil, notFoundError("artifact_not_found", "Artifact not found", err)
	}
	return stored, nil
}

func normalizeArtifactID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 100 {
		return "", validationError("invalid_artifact_id", "Artifact ID is invalid")
	}
	return id, nil
}

// normalizeArtifactHTML validates the HTML of a version and masks credentials
// in it. It reports whether masking changed the content.
func normalizeArtifactHTML(content string) (string, bool, error) {
	if strings.TrimSpace(content) == "" {
		return "", false, validationError("content_required", "Artifact HTML is required")
	}
	if len(content) > MaxArtifactHTMLBytes {
		return "", false, validationError("content_too_large",
			fmt.Sprintf("Artifact HTML is %d bytes; the limit is %d bytes per version", len(content), MaxArtifactHTMLBytes))
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return "", false, validationError("invalid_artifact_html", "Artifact HTML must be valid UTF-8 text")
	}
	if !artifactHTMLPattern.MatchString(content) {
		return "", false, validationError("invalid_artifact_html", "Artifact content must be an HTML document (use a Doc for markdown or plain text)")
	}
	trimmed := strings.TrimSpace(content)
	masked := redactReportSecrets(trimmed)
	return masked, masked != trimmed, nil
}

func artifactContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func artifactChangeMeta(artifact *database.Artifact) map[string]any {
	meta := map[string]any{
		"artifact_id": artifact.ID, "version": artifact.CurrentVersion,
		"title": artifact.Title, "session_id": artifact.SessionID,
	}
	if artifact.ProjectID.Valid {
		meta["project_id"] = artifact.ProjectID.Int64
	}
	if artifact.TaskID.Valid {
		meta["task_id"] = artifact.TaskID.Int64
	}
	return meta
}

func artifactView(artifact *database.Artifact) ArtifactView {
	view := ArtifactView{
		ID: artifact.ID, Title: artifact.Title, Summary: artifact.Summary, Status: artifact.Status,
		SessionID: artifact.SessionID, CurrentVersion: artifact.CurrentVersion, CreatedBy: artifact.CreatedBy,
		CreatedAt: artifact.CreatedAt, UpdatedAt: artifact.UpdatedAt,
	}
	if artifact.ProjectID.Valid {
		id := artifact.ProjectID.Int64
		view.ProjectID = &id
	}
	if artifact.TaskID.Valid {
		id := artifact.TaskID.Int64
		view.TaskID = &id
	}
	if artifact.ConversationID.Valid {
		id := artifact.ConversationID.Int64
		view.ConversationID = &id
	}
	return view
}

func artifactVersionView(version *database.ArtifactVersion, includeContent bool) ArtifactVersionView {
	view := ArtifactVersionView{
		ArtifactID: version.ArtifactID, Version: version.Version, SizeBytes: version.SizeBytes, SHA256: version.SHA256,
		ChangeNote: version.ChangeNote, CreatedBy: version.CreatedBy, SessionID: version.SessionID, CreatedAt: version.CreatedAt,
	}
	if includeContent {
		view.Content = version.Content
	}
	return view
}
