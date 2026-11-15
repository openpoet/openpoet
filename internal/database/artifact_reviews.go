package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

// Kinds of artifact review: what the user decides on.
const (
	ArtifactReviewVerification = "verification" // the verification document of a task
	ArtifactReviewMemory       = "memory"       // a change to a project's memory document
	ArtifactReviewTask         = "task"         // tasks to create, change or delete
	ArtifactReviewSkill        = "skill"        // a project skill to create or change
	ArtifactReviewTool         = "tool"         // a project tool to run
)

// States of an artifact review.
const (
	ArtifactReviewPending  = "pending"
	ArtifactReviewApproved = "approved"
	ArtifactReviewRejected = "rejected"
)

// ArtifactReview is the approval state of an artifact that waits for the
// user's decision (see migrateV88). An artifact with no review is a plain
// document.
type ArtifactReview struct {
	ArtifactID  string       `db:"artifact_id" json:"-"`
	Kind        string       `db:"kind" json:"kind"`
	Status      string       `db:"status" json:"status"`
	MessageID   int64        `db:"message_id" json:"-"`
	FeedbackAck bool         `db:"feedback_ack" json:"-"`
	DecidedAt   sql.NullTime `db:"decided_at" json:"-"`
	CreatedAt   time.Time    `db:"created_at" json:"-"`
}

const artifactReviewColumns = `artifact_id, kind, status, message_id, feedback_ack, decided_at, created_at`

func insertArtifactReview(ctx context.Context, tx *sqlx.Tx, artifactID string, review *ArtifactReview) error {
	if review.Status == "" {
		review.Status = ArtifactReviewPending
	}
	review.ArtifactID = artifactID
	_, err := tx.ExecContext(ctx,
		`INSERT INTO artifact_reviews (artifact_id, kind, status, message_id) VALUES (?, ?, ?, ?)`,
		artifactID, review.Kind, review.Status, review.MessageID)
	return err
}

// GetArtifactReview returns the review of an artifact, or nil when the
// artifact is a plain document.
func (d *DB) GetArtifactReview(ctx context.Context, artifactID string) (*ArtifactReview, error) {
	var review ArtifactReview
	err := d.GetContext(ctx, &review, `SELECT `+artifactReviewColumns+` FROM artifact_reviews WHERE artifact_id = ?`, artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &review, nil
}

// ClaimArtifactReviewTx decides a pending review, in the transaction that
// applies what was decided. It returns sql.ErrNoRows when the review is not
// pending anymore (or does not exist), so a decision is applied once.
func ClaimArtifactReviewTx(ctx context.Context, tx *sqlx.Tx, artifactID, status string) error {
	result, err := tx.ExecContext(ctx,
		`UPDATE artifact_reviews SET status = ?, decided_at = CURRENT_TIMESTAMP WHERE artifact_id = ? AND status = 'pending'`,
		status, artifactID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ConversationReviewArtifact is a reviewed artifact of a chat conversation, as
// the chat needs it to show a card or tell the assistant about a decision.
type ConversationReviewArtifact struct {
	ID        string    `db:"id"`
	Title     string    `db:"title"`
	Summary   string    `db:"summary"`
	Kind      string    `db:"kind"`
	Status    string    `db:"status"`
	MessageID int64     `db:"message_id"`
	CreatedAt time.Time `db:"created_at"`
}

const conversationReviewArtifactQuery = `SELECT a.id, a.title, a.summary, r.kind, r.status, r.message_id, a.created_at
	FROM artifacts a JOIN artifact_reviews r ON r.artifact_id = a.id
	WHERE a.conversation_id = ?`

// ListConversationReviewArtifacts returns the reviewed artifacts (proposals)
// of a chat conversation, oldest first.
func (d *DB) ListConversationReviewArtifacts(ctx context.Context, conversationID int64) ([]ConversationReviewArtifact, error) {
	artifacts := []ConversationReviewArtifact{}
	err := d.SelectContext(ctx, &artifacts, conversationReviewArtifactQuery+` ORDER BY a.created_at, a.id`, conversationID)
	return artifacts, err
}

// ListConversationDocumentArtifacts returns the documents a chat conversation
// published (its artifacts that are not proposals), oldest first. An artifact
// does not record the message it was published from, so MessageID is the
// assistant message that was being written at the time: the latest one of the
// conversation created at or before the artifact (0 when there is none).
func (d *DB) ListConversationDocumentArtifacts(ctx context.Context, conversationID int64) ([]ConversationReviewArtifact, error) {
	artifacts := []ConversationReviewArtifact{}
	err := d.SelectContext(ctx, &artifacts, `SELECT a.id, a.title, a.summary, '' AS kind, '' AS status, a.created_at,
			COALESCE((SELECT m.id FROM ai_messages m
				WHERE m.conversation_id = a.conversation_id AND m.role = 'assistant' AND m.created_at <= a.created_at
				ORDER BY m.created_at DESC, m.id DESC LIMIT 1), 0) AS message_id
		FROM artifacts a
		WHERE a.conversation_id = ? AND a.status != 'archived'
		  AND NOT EXISTS (SELECT 1 FROM artifact_reviews r WHERE r.artifact_id = a.id)
		ORDER BY a.created_at, a.id`, conversationID)
	return artifacts, err
}

// TakeArtifactReviewFeedbackTx returns the proposals of a conversation that
// were decided and not yet reported to the assistant, oldest first, and marks
// them as reported, in the transaction that builds the assistant's next turn.
func TakeArtifactReviewFeedbackTx(ctx context.Context, tx *sqlx.Tx, conversationID int64) ([]ConversationReviewArtifact, error) {
	decided := []ConversationReviewArtifact{}
	if err := tx.SelectContext(ctx, &decided,
		conversationReviewArtifactQuery+` AND r.status IN ('approved','rejected') AND r.feedback_ack = 0 ORDER BY a.created_at, a.id`,
		conversationID); err != nil {
		return nil, err
	}
	for _, artifact := range decided {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_reviews SET feedback_ack = 1 WHERE artifact_id = ? AND feedback_ack = 0`, artifact.ID); err != nil {
			return nil, err
		}
	}
	return decided, nil
}

// artifactExecer is a *sqlx.DB or a *sqlx.Tx.
type artifactExecer interface {
	sqlx.ExtContext
}

// deleteConversationArtifacts removes what a chat conversation leaves behind
// besides its messages: its proposals (reviewed artifacts, with their
// versions, annotations and questions) and its file cards. A proposal only
// means something next to the conversation that made it, so it goes with it;
// the documents a conversation published stay.
//
// conversations selects the conversation ids, as the right side of
// "conversation_id IN (...)": a subquery or placeholders, with its args.
func deleteConversationArtifacts(ctx context.Context, q artifactExecer, conversations string, args ...any) error {
	selected := `SELECT a.id FROM artifacts a JOIN artifact_reviews r ON r.artifact_id = a.id WHERE a.conversation_id IN (` + conversations + `)`
	var ids []string
	if err := sqlx.SelectContext(ctx, q, &ids, selected, args...); err != nil {
		return err
	}
	for _, id := range ids {
		for _, stmt := range []string{
			`DELETE FROM artifact_versions WHERE artifact_id = ?`,
			`DELETE FROM artifact_annotations WHERE artifact_id = ?`,
			`DELETE FROM artifact_questions WHERE artifact_id = ?`,
			`DELETE FROM artifact_reviews WHERE artifact_id = ?`,
			`DELETE FROM artifacts WHERE id = ?`,
		} {
			if _, err := q.ExecContext(ctx, stmt, id); err != nil {
				return err
			}
		}
	}
	_, err := q.ExecContext(ctx, `DELETE FROM ai_chat_file_cards WHERE conversation_id IN (`+conversations+`)`, args...)
	return err
}

// DeleteConversationArtifactsTx is deleteConversationArtifacts for one
// conversation, in the transaction that deletes it.
func DeleteConversationArtifactsTx(ctx context.Context, tx *sqlx.Tx, conversationID int64) error {
	return deleteConversationArtifacts(ctx, tx, `?`, conversationID)
}

// DeleteAllConversationArtifactsTx is deleteConversationArtifacts for every
// conversation, in the transaction that deletes them all.
func DeleteAllConversationArtifactsTx(ctx context.Context, tx *sqlx.Tx) error {
	return deleteConversationArtifacts(ctx, tx, `SELECT id FROM ai_conversations`)
}

// ChatFileCard is a "file" card of a chat message: a file of a project the
// assistant pointed the user at. It is not a document.
type ChatFileCard struct {
	ID             string    `db:"id"`
	ConversationID int64     `db:"conversation_id"`
	MessageID      int64     `db:"message_id"`
	ProjectID      int64     `db:"project_id"`
	Path           string    `db:"path"`
	CreatedAt      time.Time `db:"created_at"`
}

// CreateChatFileCard stores a file card of a conversation.
func (d *DB) CreateChatFileCard(ctx context.Context, card *ChatFileCard) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO ai_chat_file_cards (id, conversation_id, message_id, project_id, path) VALUES (?, ?, ?, ?, ?)`,
		card.ID, card.ConversationID, card.MessageID, card.ProjectID, card.Path)
	return err
}

// ListChatFileCards returns the file cards of a conversation, oldest first.
func (d *DB) ListChatFileCards(ctx context.Context, conversationID int64) ([]ChatFileCard, error) {
	cards := []ChatFileCard{}
	err := d.SelectContext(ctx, &cards,
		`SELECT id, conversation_id, message_id, project_id, path, created_at FROM ai_chat_file_cards WHERE conversation_id = ? ORDER BY created_at, id`,
		conversationID)
	return cards, err
}

// DocumentArtifact is the artifact a document id stands for, with the content
// of its current version.
type DocumentArtifact struct {
	Artifact Artifact
	Version  ArtifactVersion
	Review   *ArtifactReview
}

// ResolveDocumentArtifactID returns the id of the artifact a document id
// stands for, or "" when the id is a Doc that is still only a Doc, or nothing.
//
// A document id reaches an artifact in two ways. A Doc that was copied into an
// artifact is in artifact_legacy_docs, which wins. Otherwise, an id that is
// not a Doc and is the id of an artifact is that artifact: proposals and
// verification documents are artifacts that are still named by a bare id where
// a Doc id used to be (a task's verification_doc_id, a chat card, a
// /app/doc/<id> link). A Doc that was not copied stays a Doc, even if an
// artifact happens to share its id.
func (d *DB) ResolveDocumentArtifactID(ctx context.Context, documentID string) (string, error) {
	var artifactID string
	err := d.GetContext(ctx, &artifactID, `SELECT artifact_id FROM artifact_legacy_docs WHERE doc_id = ?`, documentID)
	if err == nil {
		return artifactID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var found int
	if err := d.GetContext(ctx, &found, `SELECT COUNT(*) FROM temp_documents WHERE id = ?`, documentID); err != nil {
		return "", err
	}
	if found > 0 {
		return "", nil
	}
	if err := d.GetContext(ctx, &found, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, documentID); err != nil {
		return "", err
	}
	if found > 0 {
		return documentID, nil
	}
	return "", nil
}

// ResolveDocumentArtifact returns the artifact a document id stands for (see
// ResolveDocumentArtifactID), or nil when it stands for none.
func (d *DB) ResolveDocumentArtifact(ctx context.Context, documentID string) (*DocumentArtifact, error) {
	artifactID, err := d.ResolveDocumentArtifactID(ctx, documentID)
	if err != nil || artifactID == "" {
		return nil, err
	}
	artifact, err := d.GetArtifact(ctx, artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	version, err := d.GetArtifactVersion(ctx, artifactID, artifact.CurrentVersion)
	if err != nil {
		return nil, err
	}
	review, err := d.GetArtifactReview(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	return &DocumentArtifact{Artifact: *artifact, Version: *version, Review: review}, nil
}

// SetDocumentReviewStatusTx records the outcome of a task's verification on
// its verification document: the review of the artifact the id stands for.
// (A Doc id is handled by the caller, which still updates temp_documents.) It
// does nothing when the id is not a reviewed artifact.
func SetDocumentReviewStatusTx(ctx context.Context, tx *sqlx.Tx, documentID, status string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE artifact_reviews SET status = ?, decided_at = CURRENT_TIMESTAMP
		 WHERE artifact_id = COALESCE((SELECT artifact_id FROM artifact_legacy_docs WHERE doc_id = ?), ?)`,
		status, documentID, documentID)
	return err
}
