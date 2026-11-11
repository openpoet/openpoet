package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	FindRecentArtifactByContent(ctx context.Context, sessionID, title, sha256 string, window time.Duration) (*database.Artifact, error)
	ArtifactAnnotationStore
	ArtifactQuestionStore
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
	// Questions is how many questions the published version declares for the
	// reader to answer.
	Questions int `json:"questions"`
	// SizeBytes and SHA256 describe the HTML of that version exactly as it is
	// stored, so the publisher can compare them with what it sent. The stored
	// HTML is the input byte for byte unless Redacted is set.
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	// Replacements is how many places a patch changed (0 for a full publish).
	Replacements int `json:"replacements,omitempty"`
	// Redactions lists where text was masked (at most
	// maxArtifactRedactionsReported places); RedactionCount is how many there
	// were in all.
	Redactions     []ArtifactRedaction `json:"redactions,omitempty"`
	RedactionCount int                 `json:"redaction_count,omitempty"`
	// DryRun is set when the request only asked what a publish would do:
	// nothing was stored, and Version is the version it would have created.
	DryRun bool `json:"dry_run,omitempty"`
	// Duplicate is set when a create found that the same session had just
	// published an artifact with the same title and HTML: that artifact is
	// returned and nothing new was created.
	Duplicate bool `json:"duplicate,omitempty"`
}

// artifactCreateRetryWindow is how long after a create the same session
// publishing the same title and HTML gets the artifact it already created. A
// tool call that timed out on the caller's side is retried well within it.
const artifactCreateRetryWindow = 10 * time.Minute

type CreateArtifactCommand struct {
	Title          string
	Summary        string
	Content        string
	ProjectID      *int64
	TaskID         *int64
	ConversationID *int64
	SessionID      string
	// DryRun validates the document and reports what would be stored,
	// without creating anything.
	DryRun        bool
	Authorization ActionAuthorization
}

type UpdateArtifactCommand struct {
	ArtifactID  string
	Content     string
	ChangeNote  string
	BaseVersion int
	SessionID   string
	// Title and Summary, when not nil, replace the artifact's title and
	// summary together with the publish.
	Title   *string
	Summary *string
	// DryRun validates the document and reports what would be stored,
	// without publishing a version.
	DryRun        bool
	Authorization ActionAuthorization
}

// ArtifactEdit replaces one piece of the HTML of a version. Old must occur
// exactly once in the document, unless ReplaceAll is set, in which case every
// occurrence is replaced.
type ArtifactEdit struct {
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// PatchArtifactCommand publishes a new version made of the HTML of
// BaseVersion with Edits applied in order.
type PatchArtifactCommand struct {
	ArtifactID    string
	BaseVersion   int
	Edits         []ArtifactEdit
	ChangeNote    string
	SessionID     string
	Title         *string
	Summary       *string
	DryRun        bool
	Authorization ActionAuthorization
}

// maxArtifactEdits bounds the edits of one patch.
const maxArtifactEdits = 200

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
	// ArtifactAnnotationsChanged fires when an annotation is created, resolved
	// or reopened. It carries no annotation text.
	ArtifactAnnotationsChanged(ctx context.Context, artifact ArtifactView)
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

// singleLine removes control characters and folds line breaks into spaces. A
// title and a change note are shown in listings, cards and tool output, and a
// title is typed into an agent's terminal by "Send to agent": they are one
// line of printable text.
func singleLine(value string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return -1
		}
		return r
	}, value)), " ")
}

func (s *ArtifactService) Create(ctx context.Context, command CreateArtifactCommand) (ArtifactPublishResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactPublishResult{}, err
	}
	title, err := boundedRedactedInput(singleLine(command.Title), maxApplicationTitleRunes, "Artifact title", false)
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
	content, redactions, err := normalizeArtifactHTML(command.Content)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	questions, err := artifactQuestionsOf(content)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	sessionID := strings.TrimSpace(command.SessionID)
	if len(sessionID) > 200 {
		return ArtifactPublishResult{}, validationError("invalid_session_id", "Session ID exceeds its bounded limit")
	}
	stored := ArtifactPublishResult{Version: 1, Questions: len(questions), SizeBytes: int64(len(content)), SHA256: artifactContentHash(content)}
	stored.setRedactions(redactions)
	if command.DryRun {
		stored.DryRun = true
		return stored, nil
	}
	// A create repeated by the same session (a retry after a timeout on the
	// caller's side) returns the artifact the first call created.
	if sessionID != "" {
		if existing, err := s.store.FindRecentArtifactByContent(ctx, sessionID, title, stored.SHA256, artifactCreateRetryWindow); err == nil && existing != nil {
			stored.Artifact, stored.Version, stored.Duplicate = artifactView(existing), existing.CurrentVersion, true
			stored.Artifact.SizeBytes = stored.SizeBytes
			return stored, nil
		}
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
		CreatedBy: actor, SessionID: sessionID, Questions: questions,
	}
	if err := s.store.CreateArtifact(ctx, artifact, version); err != nil {
		return ArtifactPublishResult{}, err
	}
	created, err := s.store.GetArtifact(ctx, artifact.ID)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: "created", Meta: artifactChangeMeta(created)})
	view := artifactView(created)
	view.SizeBytes = version.SizeBytes
	s.notifyPublished(ctx, view, true)
	stored.Artifact, stored.Created = view, true
	return stored, nil
}

// setRedactions records where masking changed the document.
func (r *ArtifactPublishResult) setRedactions(redactions []ArtifactRedaction) {
	r.Redacted = len(redactions) > 0
	r.RedactionCount = len(redactions)
	if len(redactions) > maxArtifactRedactionsReported {
		redactions = redactions[:maxArtifactRedactionsReported]
	}
	r.Redactions = redactions
}

// artifactMetaInput validates a title and a summary that may accompany a
// publish. Nil stays nil.
func artifactMetaInput(title, summary *string) (*string, *string, error) {
	if title != nil {
		value, err := boundedRedactedInput(singleLine(*title), maxApplicationTitleRunes, "Artifact title", true)
		if err != nil {
			return nil, nil, err
		}
		title = &value
	}
	if summary != nil {
		value, err := boundedRedactedInput(*summary, maxApplicationSummaryRunes, "Artifact summary", false)
		if err != nil {
			return nil, nil, err
		}
		summary = &value
	}
	return title, summary, nil
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
	content, redactions, err := normalizeArtifactHTML(command.Content)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	questions, err := artifactQuestionsOf(content)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	note, err := boundedRedactedInput(singleLine(command.ChangeNote), maxArtifactChangeNoteRunes, "Artifact change note", false)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	title, summary, err := artifactMetaInput(command.Title, command.Summary)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	sessionID := strings.TrimSpace(command.SessionID)
	if len(sessionID) > 200 {
		return ArtifactPublishResult{}, validationError("invalid_session_id", "Session ID exceeds its bounded limit")
	}
	result := ArtifactPublishResult{Questions: len(questions), SizeBytes: int64(len(content)), SHA256: artifactContentHash(content)}
	result.setRedactions(redactions)
	if command.DryRun {
		return s.updateDryRun(ctx, id, command.BaseVersion, result)
	}
	version := &database.ArtifactVersion{
		Content: content, SizeBytes: int64(len(content)), SHA256: artifactContentHash(content),
		ChangeNote: note, CreatedBy: command.Authorization.Actor.eventValue(), SessionID: sessionID,
		Questions: questions,
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
	if title != nil || summary != nil {
		if err := s.store.UpdateArtifactMeta(ctx, id, title, summary, nil); err != nil {
			return ArtifactPublishResult{}, err
		}
		if stored, err = s.artifact(ctx, id); err != nil {
			return ArtifactPublishResult{}, err
		}
		if !created {
			publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: "updated", Meta: artifactChangeMeta(stored)})
		}
	}
	view := artifactView(stored)
	view.SizeBytes = version.SizeBytes
	if created {
		publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: "version_published", Meta: artifactChangeMeta(stored)})
		s.notifyPublished(ctx, view, false)
	}
	result.Artifact, result.Version, result.Created = view, stored.CurrentVersion, created
	return result, nil
}

// updateDryRun reports what an update would do without publishing: the
// conflict it would hit, or the version it would create (none when the HTML is
// the current version's).
func (s *ArtifactService) updateDryRun(ctx context.Context, id string, baseVersion int, result ArtifactPublishResult) (ArtifactPublishResult, error) {
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	if baseVersion > 0 && baseVersion != artifact.CurrentVersion {
		return ArtifactPublishResult{}, conflictError("artifact_version_conflict",
			fmt.Sprintf("Artifact %s is at version %d, not base_version %d. Read the current version, reapply your change and update again.", id, artifact.CurrentVersion, baseVersion))
	}
	current, err := s.store.GetArtifactVersion(ctx, id, artifact.CurrentVersion)
	if err != nil || current == nil {
		return ArtifactPublishResult{}, notFoundError("artifact_version_not_found", "Artifact version not found", err)
	}
	result.Artifact, result.DryRun = artifactView(artifact), true
	result.Artifact.SizeBytes = current.SizeBytes
	result.Version = artifact.CurrentVersion
	if current.SHA256 != result.SHA256 {
		result.Version++
		result.Created = true // a version would be created
	}
	return result, nil
}

// Patch publishes a new version by applying text replacements to the HTML of
// the version the caller read, so a small change does not resend the whole
// document. base_version is required and must be the current version: the
// edits are located in that exact text. Each edit's old text has to match the
// stored HTML exactly and (unless replace_all) exactly once; when one does not,
// nothing is published and the error says which edit and how many matches.
func (s *ArtifactService) Patch(ctx context.Context, command PatchArtifactCommand) (ArtifactPublishResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactPublishResult{}, err
	}
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	if command.BaseVersion <= 0 {
		return ArtifactPublishResult{}, validationError("base_version_required", "base_version is required for a patch: pass the version whose HTML your edits were written against")
	}
	if len(command.Edits) == 0 {
		return ArtifactPublishResult{}, validationError("edits_required", "A patch needs at least one edit ({old, new})")
	}
	if len(command.Edits) > maxArtifactEdits {
		return ArtifactPublishResult{}, validationError("too_many_edits", fmt.Sprintf("A patch takes at most %d edits", maxArtifactEdits))
	}
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	if artifact.CurrentVersion != command.BaseVersion {
		return ArtifactPublishResult{}, conflictError("artifact_version_conflict",
			fmt.Sprintf("Artifact %s is at version %d, not base_version %d. Read the current version, reapply your change and patch again.", id, artifact.CurrentVersion, command.BaseVersion))
	}
	base, err := s.store.GetArtifactVersion(ctx, id, command.BaseVersion)
	if err != nil || base == nil {
		return ArtifactPublishResult{}, notFoundError("artifact_version_not_found", "Artifact version not found", err)
	}
	content, replacements, err := applyArtifactEdits(base.Content, command.Edits)
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	// Update checks base_version again inside the write transaction, so a
	// version published between the read above and the write is a conflict.
	result, err := s.Update(ctx, UpdateArtifactCommand{
		ArtifactID: id, Content: content, ChangeNote: command.ChangeNote, BaseVersion: command.BaseVersion,
		SessionID: command.SessionID, Title: command.Title, Summary: command.Summary, DryRun: command.DryRun,
		Authorization: command.Authorization,
	})
	if err != nil {
		return ArtifactPublishResult{}, err
	}
	result.Replacements = replacements
	return result, nil
}

// applyArtifactEdits applies the edits in order, each one to the result of the
// ones before it, and returns the new text and how many places changed.
func applyArtifactEdits(content string, edits []ArtifactEdit) (string, int, error) {
	replacements := 0
	for i, edit := range edits {
		number := i + 1
		if edit.Old == "" {
			return "", 0, validationError("invalid_edit", fmt.Sprintf("Edit %d: old is empty. Give the exact text to replace", number))
		}
		if edit.Old == edit.New {
			return "", 0, validationError("invalid_edit", fmt.Sprintf("Edit %d: old and new are the same text", number))
		}
		old, replacement := edit.Old, edit.New
		count := strings.Count(content, old)
		// A document stored with CRLF line breaks is still matched by an edit
		// written with LF, which is what a tool argument usually carries.
		if count == 0 && strings.Contains(content, "\r\n") && !strings.Contains(old, "\r") {
			if crlf := strings.ReplaceAll(old, "\n", "\r\n"); crlf != old && strings.Contains(content, crlf) {
				old, replacement = crlf, strings.ReplaceAll(strings.ReplaceAll(replacement, "\r\n", "\n"), "\n", "\r\n")
				count = strings.Count(content, old)
			}
		}
		switch {
		case count == 0:
			return "", 0, validationError("edit_not_found", fmt.Sprintf(
				"Edit %d: old text not found (%s). It has to match the stored HTML exactly, including whitespace, line breaks and entities, and edits apply in order to the result of the previous ones. Nothing was published.",
				number, describeEditText(edit.Old)))
		case count > 1 && !edit.ReplaceAll:
			return "", 0, validationError("edit_ambiguous", fmt.Sprintf(
				"Edit %d: old text occurs %d times (%s). Add surrounding text so it is unique, or set replace_all. Nothing was published.",
				number, count, describeEditText(edit.Old)))
		}
		if edit.ReplaceAll {
			content = strings.ReplaceAll(content, old, replacement)
			replacements += count
		} else {
			content = strings.Replace(content, old, replacement, 1)
			replacements++
		}
	}
	return content, replacements, nil
}

// describeEditText identifies an edit's text in an error without echoing all of it.
func describeEditText(text string) string {
	oneLine := strings.Join(strings.Fields(text), " ")
	if runes := []rune(oneLine); len(runes) > 60 {
		oneLine = string(runes[:60]) + "…"
	}
	return fmt.Sprintf("%d bytes, starting %q", len(text), oneLine)
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
		value, err := boundedRedactedInput(singleLine(*command.Title), maxApplicationTitleRunes, "Artifact title", true)
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
	if view.OpenAnnotations, err = s.store.CountOpenArtifactAnnotations(ctx, id); err != nil {
		return ArtifactView{}, err
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
// in it. It reports each place where masking changed the content.
func normalizeArtifactHTML(content string) (string, []ArtifactRedaction, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil, validationError("content_required", "Artifact HTML is required")
	}
	if len(content) > MaxArtifactHTMLBytes {
		return "", nil, validationError("content_too_large",
			fmt.Sprintf("Artifact HTML is %d bytes; the limit is %d bytes per version", len(content), MaxArtifactHTMLBytes))
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return "", nil, validationError("invalid_artifact_html", "Artifact HTML must be valid UTF-8 text")
	}
	if !artifactHTMLPattern.MatchString(content) {
		return "", nil, validationError("invalid_artifact_html", "Artifact content must be an HTML document (use a Doc for markdown or plain text)")
	}
	// The document is stored as it was sent: its size and SHA-256 are reported
	// back so the publisher can compare them with its own copy. Masking is the
	// only thing that changes it.
	masked, redactions := maskArtifactSecrets(content)
	return masked, redactions, nil
}

// CheckArtifactHTML reports whether content would be accepted as an artifact
// version, and whether storing it would mask credential-looking text in it.
func CheckArtifactHTML(content string) (redacted bool, err error) {
	normalized, redactions, err := normalizeArtifactHTML(content)
	if err != nil {
		return false, err
	}
	_, err = artifactQuestionsOf(normalized)
	return len(redactions) > 0, err
}

// artifactQuestionsOf returns the rows for the questions a document declares.
// It reads the HTML as it will be stored, after masking.
func artifactQuestionsOf(content string) ([]database.ArtifactQuestion, error) {
	questions, err := ParseArtifactQuestions(content)
	if err != nil {
		return nil, err
	}
	return artifactQuestionRows(questions)
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

// ---- Annotations ----

const (
	maxArtifactAnnotationBodyRunes     = 4000
	maxArtifactAnnotationQuoteRunes    = 2000
	maxArtifactAnnotationContextRunes  = 200
	maxArtifactAnnotationSelectorRunes = 1000
	maxArtifactAnnotationLabelRunes    = 300
)

// ArtifactAnchor locates an annotation inside one version of an artifact. It
// is produced inside the sandboxed artifact frame, so every field is untrusted
// input: bounded, stored as data and never interpreted by the server.
//
// A "text" anchor is a quote (Exact, with the text just before and after it and
// its offsets inside the element at Selector). An "element" anchor is a whole
// element (Selector, with its tag and a short label).
type ArtifactAnchor struct {
	Type     string `json:"type"`
	Selector string `json:"selector,omitempty"`
	Exact    string `json:"exact,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Suffix   string `json:"suffix,omitempty"`
	Start    int    `json:"start,omitempty"`
	End      int    `json:"end,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Label    string `json:"label,omitempty"`
	// Question is set on the anchor of an answer (Type "question"): the id of
	// the question it answers. Such an anchor is only ever written by
	// AnswerQuestions; CreateAnnotation refuses the type.
	Question string `json:"question,omitempty"`
	// Near names the closest element, at or above the annotated one, that
	// carries an id or a data-id: "#stage-2" or `[data-id="TM8"]`. A page made
	// of tabs or panels usually marks each one that way, so it tells the agent
	// which panel a note is about, and lets the viewer place the note only
	// while that panel is in the page.
	Near string `json:"near,omitempty"`
}

// artifactAnchorNearPattern is the only form Near is stored in: a selector for
// one id or one data-id, made of characters that need no escaping.
var artifactAnchorNearPattern = regexp.MustCompile(`^(#[A-Za-z][\w.:-]{0,79}|\[data-id="[\w.:-]{1,80}"\])$`)

// ArtifactAnnotationPlacement says whether an annotation made on an earlier
// version still has something to point at in the current one. The server does
// not render the page: Found is a check of the stored HTML (the quoted text is
// in the document's text, or the element named by Near or by the selector's id
// is in the markup), which is what the agent needs to know before acting on an
// old note. Checked is false when there was nothing the server could look for.
type ArtifactAnnotationPlacement struct {
	Version int  `json:"version"`
	Checked bool `json:"checked"`
	Found   bool `json:"found"`
}

type ArtifactAnnotationView struct {
	ID             int64          `json:"id"`
	ArtifactID     string         `json:"artifact_id"`
	Version        int            `json:"version"`
	Anchor         ArtifactAnchor `json:"anchor"`
	Quote          string         `json:"quote,omitempty"`
	Body           string         `json:"body"`
	Status         string         `json:"status"`
	ResolutionNote string         `json:"resolution_note,omitempty"`
	ResolvedBy     string         `json:"resolved_by,omitempty"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
	CreatedBy      string         `json:"created_by,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	// Kind is "note" or "answer". For an answer, QuestionID names the
	// question, Quote is its prompt, Answer the options chosen, and Body the
	// comment (or the text of a free-text answer).
	Kind       string              `json:"kind"`
	QuestionID string              `json:"question_id,omitempty"`
	Answer     *ArtifactAnswerView `json:"answer,omitempty"`
	UpdatedAt  *time.Time          `json:"updated_at,omitempty"`
	// Placement is filled by ListAnnotationsWithPlacement for a note made on a
	// version that is no longer the current one.
	Placement *ArtifactAnnotationPlacement `json:"placement,omitempty"`
	// ResolvedOnVersion is the version that was current when the annotation
	// was resolved (filled by ListAnnotationsWithPlacement).
	ResolvedOnVersion int `json:"resolved_on_version,omitempty"`
}

type CreateArtifactAnnotationCommand struct {
	ArtifactID    string
	Version       int // 0 means the current version
	Anchor        ArtifactAnchor
	Body          string
	Authorization ActionAuthorization
}

type ResolveArtifactAnnotationCommand struct {
	// ArtifactID is optional. When set, the annotation must belong to it.
	ArtifactID     string
	AnnotationID   int64
	ResolutionNote string
	Authorization  ActionAuthorization
}

// ArtifactAnnotationStore is the persistence the annotation methods need.
type ArtifactAnnotationStore interface {
	CreateArtifactAnnotation(context.Context, *database.ArtifactAnnotation) error
	GetArtifactAnnotation(context.Context, int64) (*database.ArtifactAnnotation, error)
	ListArtifactAnnotations(context.Context, string, string) ([]database.ArtifactAnnotation, error)
	CountOpenArtifactAnnotations(context.Context, string) (int, error)
	ResolveArtifactAnnotation(context.Context, int64, string, string) error
	ReopenArtifactAnnotation(context.Context, int64) error
}

// CreateAnnotation attaches a note to a text range or element of one version.
func (s *ArtifactService) CreateAnnotation(ctx context.Context, command CreateArtifactAnnotationCommand) (ArtifactAnnotationView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactAnnotationView{}, err
	}
	// Annotations are the reviewer's notes, and agents are told to treat them
	// as the user's feedback. A session (an agent) therefore cannot write one:
	// it would be putting words in the user's mouth for the next agent.
	if strings.TrimSpace(command.Authorization.Actor.Type) == "session" {
		return ArtifactAnnotationView{}, validationError("annotation_author_not_allowed", "Annotations are written by the user reviewing the artifact, not by a session")
	}
	store := s.store
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	body, err := boundedRedactedInput(command.Body, maxArtifactAnnotationBodyRunes, "Annotation text", true)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	anchor, quote, err := normalizeArtifactAnchor(command.Anchor)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	version := command.Version
	if version == 0 {
		version = artifact.CurrentVersion
	}
	if version < 1 || version > artifact.CurrentVersion {
		return ArtifactAnnotationView{}, notFoundError("artifact_version_not_found", "Artifact version not found", nil)
	}
	encoded, err := json.Marshal(anchor)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	annotation := &database.ArtifactAnnotation{
		ArtifactID: id, Version: version, AnchorJSON: string(encoded), Quote: quote, Body: body,
		CreatedBy: command.Authorization.Actor.eventValue(),
	}
	if err := store.CreateArtifactAnnotation(ctx, annotation); err != nil {
		return ArtifactAnnotationView{}, err
	}
	stored, err := store.GetArtifactAnnotation(ctx, annotation.ID)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	s.annotationsChanged(ctx, artifact, "annotation_created")
	return artifactAnnotationView(stored), nil
}

// ListAnnotations returns an artifact's annotations in creation order. status
// is "open", "resolved" or "" / "all" for both.
func (s *ArtifactService) ListAnnotations(ctx context.Context, artifactID, status string) ([]ArtifactAnnotationView, error) {
	store := s.store
	id, err := normalizeArtifactID(artifactID)
	if err != nil {
		return nil, err
	}
	status = strings.TrimSpace(status)
	switch status {
	case "all":
		status = ""
	case "", "open", "resolved":
	default:
		return nil, validationError("invalid_annotation_status", "Annotation status must be open, resolved or all")
	}
	if _, err := s.artifact(ctx, id); err != nil {
		return nil, err
	}
	annotations, err := store.ListArtifactAnnotations(ctx, id, status)
	if err != nil {
		return nil, err
	}
	views := make([]ArtifactAnnotationView, 0, len(annotations))
	for i := range annotations {
		views = append(views, artifactAnnotationView(&annotations[i]))
	}
	return views, nil
}

// ListAnnotationsWithPlacement is ListAnnotations with, for each note made on
// an earlier version, whether what it points at is still in the current
// version, and for each resolved annotation the version it was resolved on. It
// reads the current version's HTML, so it is for the agent tools, not for the
// viewer (which places annotations in the rendered page itself).
func (s *ArtifactService) ListAnnotationsWithPlacement(ctx context.Context, artifactID, status string) ([]ArtifactAnnotationView, error) {
	views, err := s.ListAnnotations(ctx, artifactID, status)
	if err != nil || len(views) == 0 {
		return views, err
	}
	id := strings.TrimSpace(artifactID)
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return nil, err
	}
	var versions []database.ArtifactVersion
	var markup, text string
	loaded := false
	for i := range views {
		view := &views[i]
		if view.ResolvedAt != nil {
			if versions == nil {
				if versions, err = s.store.ListArtifactVersions(ctx, id); err != nil {
					return nil, err
				}
			}
			// Versions come newest first: the first one published before the
			// resolution was the current one at that moment.
			for _, version := range versions {
				if !version.CreatedAt.After(*view.ResolvedAt) {
					view.ResolvedOnVersion = version.Version
					break
				}
			}
		}
		if view.Kind != database.ArtifactAnnotationKindNote || view.Version >= artifact.CurrentVersion {
			continue
		}
		if !loaded {
			current, err := s.store.GetArtifactVersion(ctx, id, artifact.CurrentVersion)
			if err != nil || current == nil {
				return nil, notFoundError("artifact_version_not_found", "Artifact version not found", err)
			}
			markup, loaded = current.Content, true
			text = artifactPlainText(markup)
		}
		view.Placement = artifactAnnotationPlacement(view.Anchor, artifact.CurrentVersion, markup, text)
	}
	return views, nil
}

var artifactSelectorIDPattern = regexp.MustCompile(`^#([A-Za-z][\w-]*)`)

// artifactAnnotationPlacement checks one anchor against the HTML of a version.
func artifactAnnotationPlacement(anchor ArtifactAnchor, version int, markup, text string) *ArtifactAnnotationPlacement {
	placement := &ArtifactAnnotationPlacement{Version: version}
	if anchor.Type == "text" {
		if quote := foldArtifactWhitespace(anchor.Exact); quote != "" {
			placement.Checked, placement.Found = true, strings.Contains(text, quote)
		}
		return placement
	}
	// An element: the id or data-id it sits in, or the id its selector starts at.
	attribute, value := "", ""
	switch {
	case strings.HasPrefix(anchor.Near, "#"):
		attribute, value = "id", anchor.Near[1:]
	case strings.HasPrefix(anchor.Near, `[data-id="`):
		attribute, value = "data-id", strings.TrimSuffix(strings.TrimPrefix(anchor.Near, `[data-id="`), `"]`)
	default:
		if match := artifactSelectorIDPattern.FindStringSubmatch(anchor.Selector); match != nil {
			attribute, value = "id", match[1]
		}
	}
	if attribute == "" {
		return placement
	}
	pattern, err := regexp.Compile(`(?i)[\s"']` + attribute + `\s*=\s*("` + regexp.QuoteMeta(value) + `"|'` + regexp.QuoteMeta(value) + `'|` + regexp.QuoteMeta(value) + `[\s/>])`)
	if err != nil {
		return placement
	}
	placement.Checked, placement.Found = true, pattern.MatchString(markup)
	return placement
}

// ResolveAnnotation marks an annotation resolved with a note saying what was
// done about it.
func (s *ArtifactService) ResolveAnnotation(ctx context.Context, command ResolveArtifactAnnotationCommand) (ArtifactAnnotationView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactAnnotationView{}, err
	}
	note, err := boundedRedactedInput(command.ResolutionNote, maxArtifactAnnotationBodyRunes, "Resolution note", false)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	return s.changeAnnotation(ctx, command, "annotation_resolved", func(store ArtifactAnnotationStore) error {
		return store.ResolveArtifactAnnotation(ctx, command.AnnotationID, note, command.Authorization.Actor.eventValue())
	})
}

// ReopenAnnotation puts a resolved annotation back to open.
func (s *ArtifactService) ReopenAnnotation(ctx context.Context, command ResolveArtifactAnnotationCommand) (ArtifactAnnotationView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactAnnotationView{}, err
	}
	return s.changeAnnotation(ctx, command, "annotation_reopened", func(store ArtifactAnnotationStore) error {
		return store.ReopenArtifactAnnotation(ctx, command.AnnotationID)
	})
}

func (s *ArtifactService) changeAnnotation(ctx context.Context, command ResolveArtifactAnnotationCommand, action string, change func(ArtifactAnnotationStore) error) (ArtifactAnnotationView, error) {
	store := s.store
	if command.AnnotationID <= 0 {
		return ArtifactAnnotationView{}, validationError("invalid_annotation_id", "Annotation ID must be positive")
	}
	existing, err := store.GetArtifactAnnotation(ctx, command.AnnotationID)
	if err != nil || existing == nil {
		return ArtifactAnnotationView{}, notFoundError("annotation_not_found", "Annotation not found", err)
	}
	if expected := strings.TrimSpace(command.ArtifactID); expected != "" && expected != existing.ArtifactID {
		return ArtifactAnnotationView{}, notFoundError("annotation_not_found", "Annotation not found on this artifact", nil)
	}
	if err := change(store); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ArtifactAnnotationView{}, notFoundError("annotation_not_found", "Annotation not found", err)
		}
		if errors.Is(err, database.ErrArtifactAnswerSuperseded) {
			return ArtifactAnnotationView{}, conflictError("answer_superseded", "This question already has a newer open answer; change that one instead")
		}
		return ArtifactAnnotationView{}, err
	}
	stored, err := store.GetArtifactAnnotation(ctx, command.AnnotationID)
	if err != nil {
		return ArtifactAnnotationView{}, err
	}
	if artifact, err := s.store.GetArtifact(ctx, stored.ArtifactID); err == nil && artifact != nil {
		s.annotationsChanged(ctx, artifact, action)
	}
	return artifactAnnotationView(stored), nil
}

func (s *ArtifactService) annotationsChanged(ctx context.Context, artifact *database.Artifact, action string) {
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "artifacts", Action: action, Meta: artifactChangeMeta(artifact)})
	if s.notifier != nil {
		s.notifier.ArtifactAnnotationsChanged(ctx, artifactView(artifact))
	}
}

// normalizeArtifactAnchor bounds an anchor coming from the artifact frame and
// returns it with the quote stored next to the annotation (what the agent
// reads to know which part of the document the note is about).
func normalizeArtifactAnchor(anchor ArtifactAnchor) (ArtifactAnchor, string, error) {
	clean := func(value string, limit int) string {
		value = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
				return r
			}
			return -1
		}, value)
		value = redactReportSecrets(value)
		if runes := []rune(value); len(runes) > limit {
			value = string(runes[:limit])
		}
		return value
	}
	normalized := ArtifactAnchor{
		Type:     strings.TrimSpace(anchor.Type),
		Selector: strings.TrimSpace(clean(anchor.Selector, maxArtifactAnnotationSelectorRunes)),
		Tag:      strings.ToLower(strings.TrimSpace(clean(anchor.Tag, 40))),
		Label:    strings.TrimSpace(clean(anchor.Label, maxArtifactAnnotationLabelRunes)),
	}
	// Near is kept only in its one known form; anything else is dropped
	// rather than refused, since the annotation works without it.
	if near := strings.TrimSpace(anchor.Near); artifactAnchorNearPattern.MatchString(near) {
		normalized.Near = near
	}
	switch normalized.Type {
	case "text":
		normalized.Exact = clean(anchor.Exact, maxArtifactAnnotationQuoteRunes)
		if strings.TrimSpace(normalized.Exact) == "" {
			return ArtifactAnchor{}, "", validationError("invalid_annotation_anchor", "A text annotation needs the selected text")
		}
		normalized.Prefix = clean(anchor.Prefix, maxArtifactAnnotationContextRunes)
		normalized.Suffix = clean(anchor.Suffix, maxArtifactAnnotationContextRunes)
		if anchor.Start < 0 || anchor.End < anchor.Start || anchor.End > 64<<20 {
			return ArtifactAnchor{}, "", validationError("invalid_annotation_anchor", "The annotation text range is invalid")
		}
		normalized.Start, normalized.End = anchor.Start, anchor.End
		return normalized, strings.TrimSpace(normalized.Exact), nil
	case "element":
		if normalized.Selector == "" {
			return ArtifactAnchor{}, "", validationError("invalid_annotation_anchor", "An element annotation needs the element selector")
		}
		quote := normalized.Label
		if quote == "" && normalized.Tag != "" {
			quote = "<" + normalized.Tag + ">"
		}
		return normalized, quote, nil
	default:
		return ArtifactAnchor{}, "", validationError("invalid_annotation_anchor", "Annotation anchor type must be text or element")
	}
}

func artifactAnnotationView(annotation *database.ArtifactAnnotation) ArtifactAnnotationView {
	view := ArtifactAnnotationView{
		ID: annotation.ID, ArtifactID: annotation.ArtifactID, Version: annotation.Version,
		Quote: annotation.Quote, Body: annotation.Body, Status: annotation.Status,
		ResolutionNote: annotation.ResolutionNote, ResolvedBy: annotation.ResolvedBy,
		CreatedBy: annotation.CreatedBy, CreatedAt: annotation.CreatedAt,
	}
	// A stored anchor that no longer decodes is shown without an anchor rather
	// than failing the whole listing.
	_ = json.Unmarshal([]byte(annotation.AnchorJSON), &view.Anchor)
	view.Kind = annotation.Kind
	if view.Kind == "" {
		view.Kind = database.ArtifactAnnotationKindNote
	}
	if view.Kind == database.ArtifactAnnotationKindAnswer {
		view.QuestionID = annotation.QuestionID
		var answer ArtifactAnswerView
		if json.Unmarshal([]byte(annotation.AnswerJSON), &answer) == nil {
			view.Answer = &answer
		}
	}
	if annotation.UpdatedAt.Valid {
		updatedAt := annotation.UpdatedAt.Time
		view.UpdatedAt = &updatedAt
	}
	if annotation.ResolvedAt.Valid {
		resolvedAt := annotation.ResolvedAt.Time
		view.ResolvedAt = &resolvedAt
	}
	return view
}
