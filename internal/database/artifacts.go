package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Artifact is a self-contained interactive HTML document published by an agent
// into the database. Its HTML lives in artifact_versions; this row carries the
// links (project, session, task, conversation) and the current version number.
type Artifact struct {
	ID             string        `db:"id" json:"id"`
	ProjectID      sql.NullInt64 `db:"project_id" json:"-"`
	SessionID      string        `db:"session_id" json:"session_id,omitempty"`
	TaskID         sql.NullInt64 `db:"task_id" json:"-"`
	ConversationID sql.NullInt64 `db:"conversation_id" json:"-"`
	Title          string        `db:"title" json:"title"`
	Summary        string        `db:"summary" json:"summary"`
	CurrentVersion int           `db:"current_version" json:"current_version"`
	Status         string        `db:"status" json:"status"`
	CreatedBy      string        `db:"created_by" json:"created_by"`
	CreatedAt      time.Time     `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time     `db:"updated_at" json:"updated_at"`
}

// ArtifactVersion is one published HTML of an artifact. Versions are never
// rewritten or deleted.
type ArtifactVersion struct {
	ArtifactID string    `db:"artifact_id" json:"artifact_id"`
	Version    int       `db:"version" json:"version"`
	Content    string    `db:"content" json:"content,omitempty"`
	SizeBytes  int64     `db:"size_bytes" json:"size_bytes"`
	SHA256     string    `db:"sha256" json:"sha256"`
	ChangeNote string    `db:"change_note" json:"change_note"`
	CreatedBy  string    `db:"created_by" json:"created_by"`
	SessionID  string    `db:"session_id" json:"session_id,omitempty"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// ArtifactListItem is an artifact row plus the figures a listing shows.
type ArtifactListItem struct {
	Artifact
	SizeBytes       int64 `db:"size_bytes" json:"size_bytes"`
	OpenAnnotations int   `db:"open_annotations" json:"open_annotations"`
}

// ArtifactFilter narrows ListArtifacts. Zero values mean "any".
type ArtifactFilter struct {
	ProjectID       int64
	SessionID       string
	TaskID          int64
	IncludeArchived bool
	Limit           int
}

// ErrArtifactVersionConflict is returned by AppendArtifactVersion when the
// caller's base version is no longer the current one.
var ErrArtifactVersionConflict = errors.New("artifact version conflict")

const artifactColumns = `id, project_id, session_id, task_id, conversation_id, title, summary, current_version, status, created_by, created_at, updated_at`

const artifactVersionMetaColumns = `artifact_id, version, size_bytes, sha256, change_note, created_by, session_id, created_at`

// CreateArtifact stores a new artifact together with its version 1.
func (d *DB) CreateArtifact(ctx context.Context, artifact *Artifact, version *ArtifactVersion) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if artifact.Status == "" {
		artifact.Status = "active"
	}
	artifact.CurrentVersion = 1
	version.ArtifactID = artifact.ID
	version.Version = 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts (id, project_id, session_id, task_id, conversation_id, title, summary, current_version, status, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		artifact.ID, artifact.ProjectID, artifact.SessionID, artifact.TaskID, artifact.ConversationID,
		artifact.Title, artifact.Summary, artifact.Status, artifact.CreatedBy); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifact_versions (artifact_id, version, content, size_bytes, sha256, change_note, created_by, session_id)
		 VALUES (?, 1, ?, ?, ?, ?, ?, ?)`,
		version.ArtifactID, version.Content, version.SizeBytes, version.SHA256, version.ChangeNote,
		version.CreatedBy, version.SessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetArtifact returns the artifact row (no HTML).
func (d *DB) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	var artifact Artifact
	if err := d.GetContext(ctx, &artifact, `SELECT `+artifactColumns+` FROM artifacts WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// ListArtifacts returns artifacts matching the filter, most recently updated
// first, without their HTML.
func (d *DB) ListArtifacts(ctx context.Context, filter ArtifactFilter) ([]ArtifactListItem, error) {
	where := []string{"1=1"}
	args := []any{}
	if filter.ProjectID > 0 {
		where = append(where, "a.project_id = ?")
		args = append(args, filter.ProjectID)
	}
	if filter.SessionID != "" {
		where = append(where, "a.session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.TaskID > 0 {
		where = append(where, "a.task_id = ?")
		args = append(args, filter.TaskID)
	}
	if !filter.IncludeArchived {
		where = append(where, "a.status != 'archived'")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	args = append(args, limit)
	query := `SELECT a.id, a.project_id, a.session_id, a.task_id, a.conversation_id, a.title, a.summary,
			a.current_version, a.status, a.created_by, a.created_at, a.updated_at,
			COALESCE((SELECT v.size_bytes FROM artifact_versions v WHERE v.artifact_id = a.id AND v.version = a.current_version), 0) AS size_bytes,
			(SELECT COUNT(*) FROM artifact_annotations n WHERE n.artifact_id = a.id AND n.status = 'open') AS open_annotations
		FROM artifacts a
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY a.updated_at DESC, a.id
		LIMIT ?`
	items := []ArtifactListItem{}
	err := d.SelectContext(ctx, &items, query, args...)
	return items, err
}

// ListArtifactVersions returns every version of an artifact, newest first,
// without the HTML.
func (d *DB) ListArtifactVersions(ctx context.Context, artifactID string) ([]ArtifactVersion, error) {
	versions := []ArtifactVersion{}
	err := d.SelectContext(ctx, &versions,
		`SELECT `+artifactVersionMetaColumns+` FROM artifact_versions WHERE artifact_id = ? ORDER BY version DESC`, artifactID)
	return versions, err
}

// GetArtifactVersion returns one version with its HTML.
func (d *DB) GetArtifactVersion(ctx context.Context, artifactID string, version int) (*ArtifactVersion, error) {
	var v ArtifactVersion
	if err := d.GetContext(ctx, &v,
		`SELECT `+artifactVersionMetaColumns+`, content FROM artifact_versions WHERE artifact_id = ? AND version = ?`,
		artifactID, version); err != nil {
		return nil, err
	}
	return &v, nil
}

// AppendArtifactVersion publishes a new version. baseVersion > 0 must equal the
// current version, otherwise ErrArtifactVersionConflict is returned and nothing
// is written. HTML identical to the current version is not stored again: the
// current version number is returned with created=false. The returned artifact
// reflects the state after the call (also on a conflict, so the caller can
// report the current version).
func (d *DB) AppendArtifactVersion(ctx context.Context, artifactID string, baseVersion int, version *ArtifactVersion) (artifact *Artifact, created bool, err error) {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var current Artifact
	if err := tx.GetContext(ctx, &current, `SELECT `+artifactColumns+` FROM artifacts WHERE id = ?`, artifactID); err != nil {
		return nil, false, err
	}
	if baseVersion > 0 && baseVersion != current.CurrentVersion {
		return &current, false, ErrArtifactVersionConflict
	}
	var currentHash string
	if err := tx.GetContext(ctx, &currentHash,
		`SELECT sha256 FROM artifact_versions WHERE artifact_id = ? AND version = ?`, artifactID, current.CurrentVersion); err != nil {
		return nil, false, err
	}
	if currentHash != "" && currentHash == version.SHA256 {
		return &current, false, nil
	}

	next := current.CurrentVersion + 1
	version.ArtifactID = artifactID
	version.Version = next
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifact_versions (artifact_id, version, content, size_bytes, sha256, change_note, created_by, session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		artifactID, next, version.Content, version.SizeBytes, version.SHA256, version.ChangeNote,
		version.CreatedBy, version.SessionID); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE artifacts SET current_version = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, artifactID); err != nil {
		return nil, false, err
	}
	if err := tx.GetContext(ctx, &current, `SELECT `+artifactColumns+` FROM artifacts WHERE id = ?`, artifactID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &current, true, nil
}

// UpdateArtifactMeta changes the title, summary and/or status. Nil fields are
// left as they are.
func (d *DB) UpdateArtifactMeta(ctx context.Context, artifactID string, title, summary, status *string) error {
	sets := []string{}
	args := []any{}
	if title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *title)
	}
	if summary != nil {
		sets = append(sets, "summary = ?")
		args = append(args, *summary)
	}
	if status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *status)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, artifactID)
	result, err := d.ExecContext(ctx, `UPDATE artifacts SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ArtifactAnnotation is a note the user attached to a text range or an element
// of one artifact version. The agent reads the open ones and resolves them.
type ArtifactAnnotation struct {
	ID             int64        `db:"id" json:"id"`
	ArtifactID     string       `db:"artifact_id" json:"artifact_id"`
	Version        int          `db:"version" json:"version"`
	AnchorJSON     string       `db:"anchor_json" json:"-"`
	Quote          string       `db:"quote" json:"quote"`
	Body           string       `db:"body" json:"body"`
	Status         string       `db:"status" json:"status"`
	ResolutionNote string       `db:"resolution_note" json:"resolution_note"`
	ResolvedBy     string       `db:"resolved_by" json:"resolved_by"`
	ResolvedAt     sql.NullTime `db:"resolved_at" json:"-"`
	CreatedBy      string       `db:"created_by" json:"created_by"`
	CreatedAt      time.Time    `db:"created_at" json:"created_at"`
}

const artifactAnnotationColumns = `id, artifact_id, version, anchor_json, quote, body, status, resolution_note, resolved_by, resolved_at, created_by, created_at`

// CreateArtifactAnnotation stores an open annotation and sets its ID.
func (d *DB) CreateArtifactAnnotation(ctx context.Context, annotation *ArtifactAnnotation) error {
	if annotation.AnchorJSON == "" {
		annotation.AnchorJSON = "{}"
	}
	annotation.Status = "open"
	result, err := d.ExecContext(ctx,
		`INSERT INTO artifact_annotations (artifact_id, version, anchor_json, quote, body, status, created_by)
		 VALUES (?, ?, ?, ?, ?, 'open', ?)`,
		annotation.ArtifactID, annotation.Version, annotation.AnchorJSON, annotation.Quote, annotation.Body, annotation.CreatedBy)
	if err != nil {
		return err
	}
	annotation.ID, err = result.LastInsertId()
	return err
}

// GetArtifactAnnotation returns one annotation by ID.
func (d *DB) GetArtifactAnnotation(ctx context.Context, id int64) (*ArtifactAnnotation, error) {
	var annotation ArtifactAnnotation
	if err := d.GetContext(ctx, &annotation,
		`SELECT `+artifactAnnotationColumns+` FROM artifact_annotations WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &annotation, nil
}

// ListArtifactAnnotations returns an artifact's annotations in creation order.
// status "" returns all of them; otherwise only "open" or "resolved".
func (d *DB) ListArtifactAnnotations(ctx context.Context, artifactID, status string) ([]ArtifactAnnotation, error) {
	annotations := []ArtifactAnnotation{}
	query := `SELECT ` + artifactAnnotationColumns + ` FROM artifact_annotations WHERE artifact_id = ?`
	args := []any{artifactID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id`
	err := d.SelectContext(ctx, &annotations, query, args...)
	return annotations, err
}

// CountOpenArtifactAnnotations returns how many annotations of an artifact are
// still open.
func (d *DB) CountOpenArtifactAnnotations(ctx context.Context, artifactID string) (int, error) {
	var count int
	err := d.GetContext(ctx, &count,
		`SELECT COUNT(*) FROM artifact_annotations WHERE artifact_id = ? AND status = 'open'`, artifactID)
	return count, err
}

// ResolveArtifactAnnotation marks an annotation resolved, recording who
// resolved it and their note. Resolving an already resolved annotation updates
// the note and resolver.
func (d *DB) ResolveArtifactAnnotation(ctx context.Context, id int64, note, resolvedBy string) error {
	result, err := d.ExecContext(ctx,
		`UPDATE artifact_annotations SET status = 'resolved', resolution_note = ?, resolved_by = ?, resolved_at = CURRENT_TIMESTAMP WHERE id = ?`,
		note, resolvedBy, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ReopenArtifactAnnotation puts a resolved annotation back to open and clears
// its resolution.
func (d *DB) ReopenArtifactAnnotation(ctx context.Context, id int64) error {
	result, err := d.ExecContext(ctx,
		`UPDATE artifact_annotations SET status = 'open', resolution_note = '', resolved_by = '', resolved_at = NULL WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
