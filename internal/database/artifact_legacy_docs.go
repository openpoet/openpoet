package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Docs (rows of temp_documents) are copied into artifacts, so every document
// has one format. The copy never changes or deletes a Doc: temp_documents stays
// as it is, and artifact_legacy_docs records which artifact each Doc became.
//
// What a Doc becomes:
//
//   - a markdown artifact with one version: the content byte for byte, with its
//     title, summary, conversation, task and session, and the Doc's creation
//     time as the time the artifact and the version were created;
//   - the Doc's id as the artifact's id, unless an artifact already has it (a
//     new id is drawn then). Ids a Doc is known by elsewhere (a task's
//     verification_doc_id, a stored /app/doc/<id> link) therefore keep working,
//     through the map in any case;
//   - the project of its task, or else of its session, when either still
//     exists. A Doc has no project of its own;
//   - "session:<id>" as author when the Doc names a session, and
//     "system:doc-migration" otherwise. A Doc has no author of its own;
//   - a review, when the Doc was a proposal or a verification document (the
//     Docs table told them apart by the prefix of the title), with the status
//     the Doc had.
//
// File cards (status "file") are not documents and are not copied.

// LegacyDocMigrationActor is the author of an artifact copied from a Doc that
// names no session.
const LegacyDocMigrationActor = "system:doc-migration"

// LegacyDocMigrationReport says what a run of the copy did, or what a check of
// it found.
type LegacyDocMigrationReport struct {
	// Docs is every row of temp_documents; FileCards the ones that are file
	// cards, which are not copied; Eligible the rest.
	Docs      int `db:"docs" json:"docs"`
	FileCards int `db:"file_cards" json:"file_cards"`
	Eligible  int `db:"eligible" json:"eligible"`
	// Mapped is how many Docs have an artifact; Pending how many eligible ones
	// do not yet; Copied how many this run copied.
	Mapped  int `db:"mapped" json:"mapped"`
	Pending int `db:"pending" json:"pending"`
	Copied  int `db:"-" json:"copied"`
	// ByKind counts the mapped Docs by what they became: "document", or the
	// kind of their review.
	ByKind map[string]int `db:"-" json:"by_kind"`
	// NewIDs is how many Docs got an artifact with another id than theirs.
	NewIDs int `db:"new_ids" json:"new_ids"`
	// Mismatches lists the mapped Docs whose artifact differs from them in
	// content, title, summary, creation time, task, session or conversation
	// (at most 50; MismatchCount is exact). A copy that is right has none.
	Mismatches    []LegacyDocMismatch `db:"-" json:"mismatches"`
	MismatchCount int                 `db:"-" json:"mismatch_count"`
	// VerificationDocs is how many tasks name a verification document, and
	// VerificationDocsResolved how many of those ids lead to a document.
	VerificationDocs         int `db:"verification_docs" json:"verification_docs"`
	VerificationDocsResolved int `db:"verification_docs_resolved" json:"verification_docs_resolved"`
	// Failed lists the Docs this run could not copy (id and reason).
	Failed []LegacyDocFailure `db:"-" json:"failed,omitempty"`
}

// LegacyDocMismatch is a Doc whose artifact is not a faithful copy.
type LegacyDocMismatch struct {
	DocID      string `json:"doc_id"`
	ArtifactID string `json:"artifact_id"`
	Field      string `json:"field"`
}

// LegacyDocFailure is a Doc that could not be copied.
type LegacyDocFailure struct {
	DocID  string `json:"doc_id"`
	Reason string `json:"reason"`
}

// legacyMemoryViewTitle is the title of a Doc that was a snapshot of a memory
// document for viewing ("Memory Doc: <project> (v3)"), not a proposal.
var legacyMemoryViewTitle = regexp.MustCompile(`^Memory Doc: .* \(v\d+\)$`)

// legacyDocReviewKind is the kind of review a Doc becomes, from the prefix of
// its title, or "" for a plain document.
func legacyDocReviewKind(title string) string {
	switch {
	case strings.HasPrefix(title, "Verification:"):
		return ArtifactReviewVerification
	case legacyMemoryViewTitle.MatchString(title):
		return ""
	case strings.HasPrefix(title, "Memory Doc:"), title == "Memory document proposal":
		return ArtifactReviewMemory
	case strings.HasPrefix(title, "Task:"), strings.HasPrefix(title, "Planning:"),
		strings.HasPrefix(title, "Update Task:"), strings.HasPrefix(title, "Delete Task:"):
		return ArtifactReviewTask
	case strings.HasPrefix(title, "Skill:"):
		return ArtifactReviewSkill
	case strings.HasPrefix(title, "Tool:"):
		return ArtifactReviewTool
	}
	return ""
}

func legacyDocContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// MigrateLegacyDocsToArtifacts copies every Doc that has no artifact yet into
// one. It can run any number of times: a Doc is copied once, each in its own
// transaction, so a run that stops halfway leaves whole copies and the next
// run goes on. A Doc that cannot be copied is reported and left for the next
// run; it stops nothing.
func (d *DB) MigrateLegacyDocsToArtifacts(ctx context.Context) (LegacyDocMigrationReport, error) {
	var pending []string
	if err := d.SelectContext(ctx, &pending,
		`SELECT t.id FROM temp_documents t LEFT JOIN artifact_legacy_docs m ON m.doc_id = t.id
		 WHERE m.doc_id IS NULL AND t.status != 'file' ORDER BY t.created_at, t.id`); err != nil {
		return LegacyDocMigrationReport{}, err
	}
	copied := 0
	var failed []LegacyDocFailure
	for _, id := range pending {
		if err := ctx.Err(); err != nil {
			return LegacyDocMigrationReport{}, err
		}
		if err := d.copyLegacyDoc(ctx, id); err != nil {
			failed = append(failed, LegacyDocFailure{DocID: id, Reason: err.Error()})
			continue
		}
		copied++
	}
	report, err := d.VerifyLegacyDocMigration(ctx)
	report.Copied, report.Failed = copied, failed
	return report, err
}

// copyLegacyDoc copies one Doc into an artifact, in one transaction.
func (d *DB) copyLegacyDoc(ctx context.Context, docID string) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var doc struct {
		Title     string        `db:"title"`
		Content   string        `db:"content"`
		Status    string        `db:"status"`
		SessionID string        `db:"session_id"`
		TaskID    sql.NullInt64 `db:"task_id"`
	}
	if err := tx.GetContext(ctx, &doc, `SELECT title, content, status, session_id, task_id FROM temp_documents WHERE id = ?`, docID); err != nil {
		return err
	}
	var mapped int
	if err := tx.GetContext(ctx, &mapped, `SELECT COUNT(*) FROM artifact_legacy_docs WHERE doc_id = ?`, docID); err != nil {
		return err
	}
	if mapped > 0 {
		return nil // copied by another run
	}

	// The Doc's id, unless an artifact already has it.
	artifactID := docID
	for attempt := 0; ; attempt++ {
		var taken int
		if err := tx.GetContext(ctx, &taken, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, artifactID); err != nil {
			return err
		}
		if taken == 0 {
			break
		}
		if attempt > 20 {
			return fmt.Errorf("no free artifact id")
		}
		artifactID = uuid.NewString()[:8]
	}

	author := LegacyDocMigrationActor
	if doc.SessionID != "" {
		author = "session:" + doc.SessionID
	}
	// Everything a Doc has is copied by the database itself, so times and text
	// arrive exactly as they are stored. The project is the task's, or else
	// the session's, when it still exists.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts (id, project_id, session_id, task_id, conversation_id, title, summary, current_version, status, created_by, created_at, updated_at)
		 SELECT ?, COALESCE(
				(SELECT p.id FROM project_tasks k JOIN projects p ON p.id = k.project_id WHERE k.id = t.task_id),
				(SELECT p.id FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id = t.session_id AND t.session_id != '')),
			t.session_id, t.task_id, t.conversation_id, t.title, t.summary, 1, 'active', ?, t.created_at, t.created_at
		 FROM temp_documents t WHERE t.id = ?`, artifactID, author, docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifact_versions (artifact_id, version, format, content, size_bytes, sha256, change_note, created_by, session_id, created_at)
		 SELECT ?, 1, ?, t.content, ?, ?, '', ?, t.session_id, t.created_at FROM temp_documents t WHERE t.id = ?`,
		artifactID, ArtifactFormatMarkdown, len(doc.Content), legacyDocContentHash(doc.Content), author, docID); err != nil {
		return err
	}
	if kind := legacyDocReviewKind(doc.Title); kind != "" {
		status := doc.Status
		if status != ArtifactReviewApproved && status != ArtifactReviewRejected {
			status = ArtifactReviewPending
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO artifact_reviews (artifact_id, kind, status, message_id, feedback_ack, created_at)
			 SELECT ?, ?, ?, t.message_id, t.feedback_ack, t.created_at FROM temp_documents t WHERE t.id = ?`,
			artifactID, kind, status, docID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_legacy_docs (doc_id, artifact_id) VALUES (?, ?)`, docID, artifactID); err != nil {
		return err
	}
	return tx.Commit()
}

const maxLegacyDocMismatchesReported = 50

// VerifyLegacyDocMigration checks the copy against the Docs, without changing
// anything: how many Docs there are and how many have an artifact, and, for
// every Doc that has one, whether version 1 of the artifact holds the Doc's
// content and the artifact its title, summary, time and links. It is meant for
// right after a copy: an artifact that was renamed since is reported too.
func (d *DB) VerifyLegacyDocMigration(ctx context.Context) (LegacyDocMigrationReport, error) {
	report := LegacyDocMigrationReport{ByKind: map[string]int{}, Mismatches: []LegacyDocMismatch{}}
	if err := d.GetContext(ctx, &report, `SELECT
			(SELECT COUNT(*) FROM temp_documents) AS docs,
			(SELECT COUNT(*) FROM temp_documents WHERE status = 'file') AS file_cards,
			(SELECT COUNT(*) FROM temp_documents WHERE status != 'file') AS eligible,
			(SELECT COUNT(*) FROM artifact_legacy_docs) AS mapped,
			(SELECT COUNT(*) FROM temp_documents t LEFT JOIN artifact_legacy_docs m ON m.doc_id = t.id WHERE m.doc_id IS NULL AND t.status != 'file') AS pending,
			(SELECT COUNT(*) FROM artifact_legacy_docs WHERE doc_id != artifact_id) AS new_ids,
			(SELECT COUNT(*) FROM project_tasks WHERE verification_doc_id != '') AS verification_docs,
			(SELECT COUNT(*) FROM project_tasks k WHERE k.verification_doc_id != '' AND (
				EXISTS (SELECT 1 FROM artifact_legacy_docs m JOIN artifacts a ON a.id = m.artifact_id WHERE m.doc_id = k.verification_doc_id)
				OR EXISTS (SELECT 1 FROM temp_documents t WHERE t.id = k.verification_doc_id)
				OR EXISTS (SELECT 1 FROM artifacts a WHERE a.id = k.verification_doc_id))) AS verification_docs_resolved`); err != nil {
		return report, err
	}
	var kinds []struct {
		Kind  string `db:"kind"`
		Count int    `db:"count"`
	}
	if err := d.SelectContext(ctx, &kinds, `SELECT COALESCE(r.kind, 'document') AS kind, COUNT(*) AS count
		FROM artifact_legacy_docs m LEFT JOIN artifact_reviews r ON r.artifact_id = m.artifact_id GROUP BY 1`); err != nil {
		return report, err
	}
	for _, kind := range kinds {
		report.ByKind[kind.Kind] = kind.Count
	}

	// Compare each mapped Doc that still exists with version 1 of its artifact.
	rows, err := d.QueryxContext(ctx, `SELECT m.doc_id, m.artifact_id, t.content, t.title, t.summary, t.created_at AS doc_created_at,
			t.task_id AS doc_task_id, t.session_id AS doc_session_id, t.conversation_id AS doc_conversation_id,
			a.title AS artifact_title, a.summary AS artifact_summary, a.created_at AS artifact_created_at,
			a.task_id AS artifact_task_id, a.session_id AS artifact_session_id, a.conversation_id AS artifact_conversation_id,
			COALESCE(v.sha256, '') AS sha256, COALESCE(v.size_bytes, -1) AS size_bytes, COALESCE(v.format, '') AS format,
			COALESCE(v.content = t.content, 0) AS same_content
		FROM artifact_legacy_docs m
		JOIN temp_documents t ON t.id = m.doc_id
		LEFT JOIN artifacts a ON a.id = m.artifact_id
		LEFT JOIN artifact_versions v ON v.artifact_id = m.artifact_id AND v.version = 1
		ORDER BY m.doc_id`)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var row struct {
			DocID                  string         `db:"doc_id"`
			ArtifactID             string         `db:"artifact_id"`
			Content                string         `db:"content"`
			Title                  string         `db:"title"`
			Summary                string         `db:"summary"`
			DocCreatedAt           time.Time      `db:"doc_created_at"`
			DocTaskID              sql.NullInt64  `db:"doc_task_id"`
			DocSessionID           string         `db:"doc_session_id"`
			DocConversationID      sql.NullInt64  `db:"doc_conversation_id"`
			ArtifactTitle          sql.NullString `db:"artifact_title"`
			ArtifactSummary        sql.NullString `db:"artifact_summary"`
			ArtifactCreatedAt      sql.NullTime   `db:"artifact_created_at"`
			ArtifactTaskID         sql.NullInt64  `db:"artifact_task_id"`
			ArtifactSessionID      sql.NullString `db:"artifact_session_id"`
			ArtifactConversationID sql.NullInt64  `db:"artifact_conversation_id"`
			SHA256                 string         `db:"sha256"`
			SizeBytes              int64          `db:"size_bytes"`
			Format                 string         `db:"format"`
			SameContent            bool           `db:"same_content"`
		}
		if err := rows.StructScan(&row); err != nil {
			return report, err
		}
		field := ""
		switch {
		case !row.ArtifactTitle.Valid:
			field = "artifact missing"
		case !row.SameContent || row.SHA256 != legacyDocContentHash(row.Content) || row.SizeBytes != int64(len(row.Content)) || row.Format != ArtifactFormatMarkdown:
			field = "content"
		case row.ArtifactTitle.String != row.Title:
			field = "title"
		case row.ArtifactSummary.String != row.Summary:
			field = "summary"
		case !row.ArtifactCreatedAt.Valid || !row.ArtifactCreatedAt.Time.Equal(row.DocCreatedAt):
			field = "created_at"
		case row.ArtifactTaskID != row.DocTaskID:
			field = "task"
		case row.ArtifactSessionID.String != row.DocSessionID:
			field = "session"
		case row.ArtifactConversationID != row.DocConversationID:
			field = "conversation"
		}
		if field == "" {
			continue
		}
		report.MismatchCount++
		if len(report.Mismatches) < maxLegacyDocMismatchesReported {
			report.Mismatches = append(report.Mismatches, LegacyDocMismatch{DocID: row.DocID, ArtifactID: row.ArtifactID, Field: field})
		}
	}
	return report, rows.Err()
}
