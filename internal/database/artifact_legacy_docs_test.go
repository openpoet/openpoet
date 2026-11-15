package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLegacyDocsAreCopiedIntoArtifactsOnce(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	project := &Project{Name: "legacy-docs", Path: "/tmp/legacy-docs", Type: "local"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	other := &Project{Name: "legacy-docs-other", Path: "/tmp/legacy-docs-other", Type: "local"}
	if err := db.CreateProject(ctx, other); err != nil {
		t.Fatal(err)
	}
	task := &ProjectTask{ProjectID: project.ID, Title: "Ship it", Status: "awaiting_approval", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, &Session{ID: "sess-legacy", ProjectID: other.ID, Status: "stopped", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	conversation := newTestConversation(t, db, "legacy")
	// An artifact already has the id of one of the Docs.
	newReviewArtifact(t, db, "taken001", "", 0, 0)

	valid := func(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: true} }
	docs := []TempDocument{
		{ID: "plain001", Title: "Notes", Content: "# Notes\n\nWith **markdown**.\n", Summary: "Some notes", SessionID: "sess-legacy"},
		{ID: "task0001", Title: "Design", Content: "design", TaskID: valid(task.ID), SessionID: "sess-legacy"},
		{ID: "veri0001", Title: "Verification: Ship it", Content: "# Verification\n", TaskID: valid(task.ID), Status: "approved"},
		{ID: "memo0001", Title: "Memory document proposal", Content: "memory", Status: "rejected"},
		{ID: "view0001", Title: "Memory Doc: legacy-docs (v3)", Content: "snapshot"},
		{ID: "skil0001", Title: "Skill: deploy", Content: "skill", ConversationID: valid(conversation), MessageID: 5, FeedbackAck: true},
		{ID: "gone0001", Title: "Orphan", Content: "orphan", TaskID: valid(999999), SessionID: "sess-deleted"},
		{ID: "taken001", Title: "Same id as an artifact", Content: "collision"},
		{ID: "odd00001", Title: "", Content: "bytes \xff and a\ttab\r\nand CRLF"},
		{ID: "file0001", Title: "File:plan.md", Content: `{"project_id":"1","path":"plan.md"}`, ConversationID: valid(conversation), Status: "file"},
	}
	for i := range docs {
		if err := db.CreateTempDocument(ctx, &docs[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Docs are old: their time is what the artifacts must carry.
	if _, err := db.Exec(`UPDATE temp_documents SET created_at = '2026-02-12 08:30:15', feedback_ack = CASE id WHEN 'skil0001' THEN 1 ELSE 0 END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskVerificationDoc(ctx, task.ID, "veri0001"); err != nil {
		t.Fatal(err)
	}
	var before []TempDocument
	if err := db.Select(&before, `SELECT * FROM temp_documents ORDER BY id`); err != nil {
		t.Fatal(err)
	}

	report, err := db.MigrateLegacyDocsToArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Docs != 10 || report.FileCards != 1 || report.Eligible != 9 || report.Copied != 9 || report.Mapped != 9 || report.Pending != 0 ||
		report.MismatchCount != 0 || len(report.Failed) != 0 || report.NewIDs != 1 || report.VerificationDocs != 1 || report.VerificationDocsResolved != 1 {
		t.Fatalf("report = %+v", report)
	}
	wantKinds := map[string]int{"document": 6, ArtifactReviewVerification: 1, ArtifactReviewMemory: 1, ArtifactReviewSkill: 1}
	for kind, want := range wantKinds {
		if report.ByKind[kind] != want {
			t.Errorf("by kind %s = %d, want %d (%v)", kind, report.ByKind[kind], want, report.ByKind)
		}
	}

	// The Docs table is untouched.
	var after []TempDocument
	if err := db.Select(&after, `SELECT * FROM temp_documents ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("temp_documents has %d rows after the copy, had %d", len(after), len(before))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("Doc %s changed: %+v -> %+v", before[i].ID, before[i], after[i])
		}
	}

	wantTime := time.Date(2026, 2, 12, 8, 30, 15, 0, time.UTC)
	check := func(docID string, projectID int64, author string) (*Artifact, *ArtifactVersion) {
		t.Helper()
		artifactID, err := db.ResolveDocumentArtifactID(ctx, docID)
		if err != nil || artifactID == "" {
			t.Fatalf("%s resolves to %q err=%v", docID, artifactID, err)
		}
		artifact, err := db.GetArtifact(ctx, artifactID)
		if err != nil {
			t.Fatalf("%s: %v", docID, err)
		}
		version, err := db.GetArtifactVersion(ctx, artifactID, 1)
		if err != nil {
			t.Fatalf("%s: %v", docID, err)
		}
		if artifact.ProjectID.Int64 != projectID || artifact.CreatedBy != author || version.CreatedBy != author ||
			!artifact.CreatedAt.Equal(wantTime) || !artifact.UpdatedAt.Equal(wantTime) || !version.CreatedAt.Equal(wantTime) ||
			artifact.Status != "active" || artifact.CurrentVersion != 1 || version.Format != ArtifactFormatMarkdown {
			t.Fatalf("%s: artifact %+v, version %+v", docID, artifact, version)
		}
		return artifact, version
	}

	// A plain Doc: content byte for byte, title, summary, session; the project of its session.
	artifact, version := check("plain001", other.ID, "session:sess-legacy")
	if artifact.ID != "plain001" || artifact.Title != "Notes" || artifact.Summary != "Some notes" || artifact.SessionID != "sess-legacy" ||
		version.Content != "# Notes\n\nWith **markdown**.\n" || version.SizeBytes != int64(len(version.Content)) || version.SHA256 != legacyDocContentHash(version.Content) {
		t.Fatalf("plain Doc: artifact %+v, version %+v", artifact, version)
	}
	if review, _ := db.GetArtifactReview(ctx, "plain001"); review != nil {
		t.Fatalf("a plain Doc got a review: %+v", review)
	}
	// The task's project wins over the session's.
	if artifact, _ := check("task0001", project.ID, "session:sess-legacy"); artifact.TaskID.Int64 != task.ID {
		t.Fatalf("task Doc: %+v", artifact)
	}
	// Links to a task and a session that are gone are kept as they were; there is no project to derive.
	if artifact, _ := check("gone0001", 0, "session:sess-deleted"); artifact.ProjectID.Valid || artifact.TaskID.Int64 != 999999 {
		t.Fatalf("orphan Doc: %+v", artifact)
	}
	// Odd bytes arrive as they are.
	if _, version := check("odd00001", 0, LegacyDocMigrationActor); version.Content != "bytes \xff and a\ttab\r\nand CRLF" {
		t.Fatalf("odd content = %q", version.Content)
	}

	// Proposals and verification documents keep their state as a review.
	for docID, want := range map[string]ArtifactReview{
		"veri0001": {Kind: ArtifactReviewVerification, Status: ArtifactReviewApproved},
		"memo0001": {Kind: ArtifactReviewMemory, Status: ArtifactReviewRejected},
		"skil0001": {Kind: ArtifactReviewSkill, Status: ArtifactReviewPending, MessageID: 5, FeedbackAck: true},
	} {
		review, err := db.GetArtifactReview(ctx, docID)
		if err != nil || review == nil || review.Kind != want.Kind || review.Status != want.Status || review.MessageID != want.MessageID || review.FeedbackAck != want.FeedbackAck {
			t.Errorf("review of %s = %+v err=%v, want %+v", docID, review, err, want)
		}
	}
	// A snapshot of a memory document for viewing is a document, not a proposal.
	if review, _ := db.GetArtifactReview(ctx, "view0001"); review != nil {
		t.Fatalf("a memory view got a review: %+v", review)
	}
	if artifact, _ := check("skil0001", 0, LegacyDocMigrationActor); artifact.ConversationID.Int64 != conversation {
		t.Fatalf("conversation of the skill proposal = %+v", artifact.ConversationID)
	}

	// The Doc whose id was taken got another one, and its own id still leads to it.
	collided, err := db.ResolveDocumentArtifactID(ctx, "taken001")
	if err != nil || collided == "" || collided == "taken001" {
		t.Fatalf("collided Doc resolves to %q err=%v", collided, err)
	}
	if version, err := db.GetArtifactVersion(ctx, collided, 1); err != nil || version.Content != "collision" {
		t.Fatalf("collided Doc content = %+v err=%v", version, err)
	}
	if untouched, err := db.GetArtifact(ctx, "taken001"); err != nil || untouched.Title != "Plan taken001" {
		t.Fatalf("the artifact that had the id = %+v err=%v", untouched, err)
	}

	// The file card is not a document.
	if resolved, _ := db.ResolveDocumentArtifactID(ctx, "file0001"); resolved != "" {
		t.Fatalf("a file card was copied into artifact %q", resolved)
	}

	// A second run copies nothing; a Doc created since is picked up by the next.
	again, err := db.MigrateLegacyDocsToArtifacts(ctx)
	if err != nil || again.Copied != 0 || again.Mapped != 9 || again.MismatchCount != 0 {
		t.Fatalf("second run = %+v err=%v", again, err)
	}
	if err := db.CreateTempDocument(ctx, &TempDocument{ID: "late0001", Title: "Late", Content: "late"}); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.VerifyLegacyDocMigration(ctx); err != nil || pending.Pending != 1 {
		t.Fatalf("pending before the third run = %+v err=%v", pending, err)
	}
	if third, err := db.MigrateLegacyDocsToArtifacts(ctx); err != nil || third.Copied != 1 || third.Mapped != 10 || third.Pending != 0 {
		t.Fatalf("third run = %+v err=%v", third, err)
	}

	// The check tells a copy that no longer matches its Doc.
	if _, err := db.Exec(`UPDATE artifact_versions SET content = 'tampered' WHERE artifact_id = 'plain001' AND version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET created_at = '2026-03-01 00:00:00' WHERE id = 'task0001'`); err != nil {
		t.Fatal(err)
	}
	checked, err := db.VerifyLegacyDocMigration(ctx)
	if err != nil || checked.MismatchCount != 2 {
		t.Fatalf("check after tampering = %+v err=%v", checked, err)
	}
	fields := map[string]string{}
	for _, mismatch := range checked.Mismatches {
		fields[mismatch.DocID] = mismatch.Field
	}
	if fields["plain001"] != "content" || fields["task0001"] != "created_at" {
		t.Fatalf("mismatches = %+v", checked.Mismatches)
	}
}

// TestLegacyDocMigrationRehearsal runs the copy against a database file named
// by OPENPOET_LEGACY_DOCS_REHEARSAL_DB and prints the report: counts only,
// never a title or a content. It is how the copy is rehearsed on a COPY of a
// real database before a release; it migrates the schema of that file and
// writes to it, so never point it at a database in use.
func TestLegacyDocMigrationRehearsal(t *testing.T) {
	path := os.Getenv("OPENPOET_LEGACY_DOCS_REHEARSAL_DB")
	if path == "" {
		t.Skip("OPENPOET_LEGACY_DOCS_REHEARSAL_DB is not set")
	}
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := time.Now()
	report, err := db.MigrateLegacyDocsToArtifacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("copied in %s\n%s", time.Since(started).Round(time.Millisecond), encoded)
	if report.Pending != 0 || report.MismatchCount != 0 || len(report.Failed) != 0 || report.VerificationDocsResolved != report.VerificationDocs {
		t.Fatalf("the copy is not complete or not faithful")
	}
	again, err := db.MigrateLegacyDocsToArtifacts(context.Background())
	if err != nil || again.Copied != 0 {
		t.Fatalf("a second run copied %d Docs (err %v)", again.Copied, err)
	}
}
