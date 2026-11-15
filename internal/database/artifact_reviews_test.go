package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
)

func inTestTx(t *testing.T, db *DB, run func(tx *sqlx.Tx) error) error {
	t.Helper()
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if err := run(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nil
}

func newReviewArtifact(t *testing.T, db *DB, id, kind string, conversationID, messageID int64) {
	t.Helper()
	artifact, version := newTestArtifact(id, 0, "")
	artifact.Summary = "summary of " + id
	version.Format = ArtifactFormatMarkdown
	if conversationID > 0 {
		artifact.ConversationID = sql.NullInt64{Int64: conversationID, Valid: true}
	}
	if kind != "" {
		artifact.Review = &ArtifactReview{Kind: kind, MessageID: messageID}
	}
	if err := db.CreateArtifact(context.Background(), artifact, version); err != nil {
		t.Fatal(err)
	}
}

func newTestConversation(t *testing.T, db *DB, title string) int64 {
	t.Helper()
	conversation := &AIConversation{Title: title}
	if err := db.CreateAIConversation(context.Background(), conversation); err != nil {
		t.Fatal(err)
	}
	return conversation.ID
}

func countRows(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.Get(&count, query, args...); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMigrateV88CreatesTheReviewTablesAndCanRunAgain(t *testing.T) {
	db := setupTestDB(t)
	for _, table := range []string{"artifact_reviews", "artifact_legacy_docs", "ai_chat_file_cards"} {
		if countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table) != 1 {
			t.Fatalf("table %s missing after migrations", table)
		}
	}
	newReviewArtifact(t, db, "rev00001", ArtifactReviewTask, 0, 0)
	// A second run, as after schema_version was set back by hand, keeps the data.
	if err := inTestTx(t, db, func(tx *sqlx.Tx) error { return migrateV88(tx) }); err != nil {
		t.Fatal(err)
	}
	if review, err := db.GetArtifactReview(context.Background(), "rev00001"); err != nil || review == nil || review.Status != ArtifactReviewPending {
		t.Fatalf("review after a second run = %+v err=%v", review, err)
	}
}

func TestArtifactReviewIsDecidedOnce(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	newReviewArtifact(t, db, "rev00001", ArtifactReviewSkill, 0, 0)
	newReviewArtifact(t, db, "doc00001", "", 0, 0)

	// A plain document has no review.
	if review, err := db.GetArtifactReview(ctx, "doc00001"); err != nil || review != nil {
		t.Fatalf("review of a plain document = %+v err=%v", review, err)
	}
	review, err := db.GetArtifactReview(ctx, "rev00001")
	if err != nil || review == nil || review.Kind != ArtifactReviewSkill || review.Status != ArtifactReviewPending || review.DecidedAt.Valid {
		t.Fatalf("new review = %+v err=%v", review, err)
	}

	// What a decision applies rolls back with it.
	failure := errors.New("the proposal could not be applied")
	if err := inTestTx(t, db, func(tx *sqlx.Tx) error {
		if err := ClaimArtifactReviewTx(ctx, tx, "rev00001", ArtifactReviewApproved); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("err = %v", err)
	}
	if review, _ = db.GetArtifactReview(ctx, "rev00001"); review.Status != ArtifactReviewPending {
		t.Fatalf("a rolled back decision left the review %s", review.Status)
	}

	claim := func(id, status string) error {
		return inTestTx(t, db, func(tx *sqlx.Tx) error { return ClaimArtifactReviewTx(ctx, tx, id, status) })
	}
	if err := claim("rev00001", ArtifactReviewApproved); err != nil {
		t.Fatal(err)
	}
	if review, _ = db.GetArtifactReview(ctx, "rev00001"); review.Status != ArtifactReviewApproved || !review.DecidedAt.Valid {
		t.Fatalf("decided review = %+v", review)
	}
	// Decided once: neither a second approval nor a rejection applies.
	for _, status := range []string{ArtifactReviewApproved, ArtifactReviewRejected} {
		if err := claim("rev00001", status); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("second decision (%s) err = %v", status, err)
		}
	}
	if err := claim("doc00001", ArtifactReviewApproved); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deciding a plain document err = %v", err)
	}
}

func TestListArtifactsLeavesReviewedArtifactsOutByDefault(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	newReviewArtifact(t, db, "doc00001", "", 0, 0)
	newReviewArtifact(t, db, "rev00001", ArtifactReviewVerification, 0, 0)

	documents, err := db.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil || len(documents) != 1 || documents[0].ID != "doc00001" || documents[0].ReviewKind != "" {
		t.Fatalf("default listing = %+v err=%v", documents, err)
	}
	all, err := db.ListArtifacts(ctx, ArtifactFilter{IncludeReviewed: true})
	if err != nil || len(all) != 2 {
		t.Fatalf("listing with reviewed = %+v err=%v", all, err)
	}
	for _, item := range all {
		if item.ID == "rev00001" && (item.ReviewKind != ArtifactReviewVerification || item.ReviewStatus != ArtifactReviewPending) {
			t.Fatalf("reviewed item = %+v", item)
		}
	}
}

func TestConversationProposalsFeedbackAndCleanup(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	kept := newTestConversation(t, db, "kept")
	gone := newTestConversation(t, db, "gone")

	newReviewArtifact(t, db, "task0001", ArtifactReviewTask, gone, 7)
	newReviewArtifact(t, db, "tool0001", ArtifactReviewTool, gone, 8)
	newReviewArtifact(t, db, "note0001", "", gone, 0) // a document the conversation published
	newReviewArtifact(t, db, "skil0001", ArtifactReviewSkill, kept, 3)
	for _, card := range []ChatFileCard{
		{ID: "file0001", ConversationID: gone, MessageID: 7, ProjectID: 2, Path: "docs/plan.md"},
		{ID: "file0002", ConversationID: kept, MessageID: 3, ProjectID: 2, Path: "README.md"},
	} {
		card := card
		if err := db.CreateChatFileCard(ctx, &card); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateArtifactAnnotation(ctx, &ArtifactAnnotation{ArtifactID: "task0001", Version: 1, AnchorJSON: `{}`, Body: "note", CreatedBy: "user:ui"}); err != nil {
		t.Fatal(err)
	}

	proposals, err := db.ListConversationReviewArtifacts(ctx, gone)
	if err != nil || len(proposals) != 2 || proposals[0].ID != "task0001" || proposals[0].MessageID != 7 || proposals[0].Kind != ArtifactReviewTask || proposals[0].Summary != "summary of task0001" {
		t.Fatalf("proposals of the conversation = %+v err=%v", proposals, err)
	}
	cards, err := db.ListChatFileCards(ctx, gone)
	if err != nil || len(cards) != 1 || cards[0].Path != "docs/plan.md" || cards[0].ProjectID != 2 {
		t.Fatalf("file cards = %+v err=%v", cards, err)
	}

	// A decision is reported to the assistant once.
	if err := inTestTx(t, db, func(tx *sqlx.Tx) error { return ClaimArtifactReviewTx(ctx, tx, "task0001", ArtifactReviewRejected) }); err != nil {
		t.Fatal(err)
	}
	take := func() []ConversationReviewArtifact {
		var decided []ConversationReviewArtifact
		if err := inTestTx(t, db, func(tx *sqlx.Tx) error {
			var err error
			decided, err = TakeArtifactReviewFeedbackTx(ctx, tx, gone)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return decided
	}
	if decided := take(); len(decided) != 1 || decided[0].ID != "task0001" || decided[0].Status != ArtifactReviewRejected {
		t.Fatalf("feedback = %+v", decided)
	}
	if decided := take(); len(decided) != 0 {
		t.Fatalf("feedback reported twice: %+v", decided)
	}

	// The proposals and file cards of a conversation go with it; the document
	// it published and the other conversation's proposals stay.
	if err := db.DeleteAIConversation(ctx, gone); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM artifacts WHERE id IN ('task0001','tool0001')`:                  0,
		`SELECT COUNT(*) FROM artifact_versions WHERE artifact_id IN ('task0001','tool0001')`: 0,
		`SELECT COUNT(*) FROM artifact_reviews WHERE artifact_id IN ('task0001','tool0001')`:  0,
		`SELECT COUNT(*) FROM artifact_annotations WHERE artifact_id = 'task0001'`:            0,
		`SELECT COUNT(*) FROM ai_chat_file_cards WHERE id = 'file0001'`:                       0,
		`SELECT COUNT(*) FROM artifacts WHERE id = 'note0001'`:                                1,
		`SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = 'note0001'`:               1,
		`SELECT COUNT(*) FROM artifacts WHERE id = 'skil0001'`:                                1,
		`SELECT COUNT(*) FROM artifact_reviews WHERE artifact_id = 'skil0001'`:                1,
		`SELECT COUNT(*) FROM ai_chat_file_cards WHERE id = 'file0002'`:                       1,
	} {
		if got := countRows(t, db, query); got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}

	// Deleting every conversation takes the remaining proposals too.
	if err := db.DeleteAllAIConversations(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM artifacts WHERE id = 'skil0001'`) + countRows(t, db, `SELECT COUNT(*) FROM ai_chat_file_cards`); got != 0 {
		t.Fatalf("%d proposal(s) or file card(s) left after deleting every conversation", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM artifacts WHERE id = 'note0001'`); got != 1 {
		t.Fatal("the document a conversation published was deleted with it")
	}
}

func TestPruningConversationsTakesTheirProposals(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	old := newTestConversation(t, db, "old")
	recent := newTestConversation(t, db, "recent")
	if _, err := db.Exec(`UPDATE ai_conversations SET updated_at = datetime('now', '-1 day') WHERE id = ?`, old); err != nil {
		t.Fatal(err)
	}
	newReviewArtifact(t, db, "old00001", ArtifactReviewMemory, old, 1)
	newReviewArtifact(t, db, "new00001", ArtifactReviewMemory, recent, 1)
	if err := db.PruneOldAIConversations(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM artifacts WHERE id = 'old00001'`) != 0 || countRows(t, db, `SELECT COUNT(*) FROM artifacts WHERE id = 'new00001'`) != 1 {
		t.Fatal("pruning did not remove exactly the proposals of the pruned conversation")
	}
}

func TestDocumentIDResolvesToTheArtifactItStandsFor(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	newReviewArtifact(t, db, "veri0001", ArtifactReviewVerification, 0, 0) // named by its bare id
	newReviewArtifact(t, db, "copy0001", "", 0, 0)                         // the copy of the Doc "olddoc01"
	newReviewArtifact(t, db, "same0001", "", 0, 0)                         // shares its id with a Doc by chance
	for _, doc := range []TempDocument{
		{ID: "olddoc01", Title: "Copied", Content: "x"},
		{ID: "same0001", Title: "Still a Doc", Content: "x"},
		{ID: "plain001", Title: "Only a Doc", Content: "x"},
	} {
		doc := doc
		if err := db.CreateTempDocument(ctx, &doc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO artifact_legacy_docs (doc_id, artifact_id) VALUES ('olddoc01', 'copy0001')`); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{
		"veri0001": "veri0001", // an artifact, and not a Doc
		"olddoc01": "copy0001", // a Doc copied into an artifact
		"same0001": "",         // a Doc that was not copied stays a Doc
		"plain001": "",
		"missing0": "",
	} {
		got, err := db.ResolveDocumentArtifactID(ctx, id)
		if err != nil || got != want {
			t.Errorf("ResolveDocumentArtifactID(%q) = %q err=%v, want %q", id, got, err, want)
		}
	}

	resolved, err := db.ResolveDocumentArtifact(ctx, "veri0001")
	if err != nil || resolved == nil || resolved.Artifact.ID != "veri0001" || resolved.Version.Format != ArtifactFormatMarkdown ||
		resolved.Version.Content == "" || resolved.Review == nil || resolved.Review.Kind != ArtifactReviewVerification {
		t.Fatalf("resolved = %+v err=%v", resolved, err)
	}
	if resolved, err = db.ResolveDocumentArtifact(ctx, "plain001"); err != nil || resolved != nil {
		t.Fatalf("a Doc resolved to %+v err=%v", resolved, err)
	}

	// The outcome of a verification is recorded on the document a task names,
	// whether the id is the artifact's or a copied Doc's.
	newReviewArtifact(t, db, "copy0002", ArtifactReviewVerification, 0, 0)
	if _, err := db.Exec(`INSERT INTO artifact_legacy_docs (doc_id, artifact_id) VALUES ('olddoc02', 'copy0002')`); err != nil {
		t.Fatal(err)
	}
	for documentID, artifactID := range map[string]string{"veri0001": "veri0001", "olddoc02": "copy0002"} {
		if err := inTestTx(t, db, func(tx *sqlx.Tx) error { return SetDocumentReviewStatusTx(ctx, tx, documentID, ArtifactReviewApproved) }); err != nil {
			t.Fatal(err)
		}
		if review, err := db.GetArtifactReview(ctx, artifactID); err != nil || review.Status != ArtifactReviewApproved {
			t.Fatalf("review of %s after the task was approved = %+v err=%v", artifactID, review, err)
		}
	}
}
