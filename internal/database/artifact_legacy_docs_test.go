package database

import (
	"context"
	"database/sql"
	"strings"
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

	report, err := db.MigrateLegacyDocsToArtifacts(ctx, nil)
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
	again, err := db.MigrateLegacyDocsToArtifacts(ctx, nil)
	if err != nil || again.Copied != 0 || again.Mapped != 9 || again.MismatchCount != 0 {
		t.Fatalf("second run = %+v err=%v", again, err)
	}
	if err := db.CreateTempDocument(ctx, &TempDocument{ID: "late0001", Title: "Late", Content: "late"}); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.VerifyLegacyDocMigration(ctx, nil); err != nil || pending.Pending != 1 {
		t.Fatalf("pending before the third run = %+v err=%v", pending, err)
	}
	if third, err := db.MigrateLegacyDocsToArtifacts(ctx, nil); err != nil || third.Copied != 1 || third.Mapped != 10 || third.Pending != 0 {
		t.Fatalf("third run = %+v err=%v", third, err)
	}

	// The check tells a copy that no longer matches its Doc.
	if _, err := db.Exec(`UPDATE artifact_versions SET content = 'tampered' WHERE artifact_id = 'plain001' AND version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET created_at = '2026-03-01 00:00:00' WHERE id = 'task0001'`); err != nil {
		t.Fatal(err)
	}
	checked, err := db.VerifyLegacyDocMigration(ctx, nil)
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

// testLegacyDocMask stands in for the application's mask: it hides one word.
func testLegacyDocMask(text string) string {
	return strings.ReplaceAll(text, "hunter2", "[REDACTED]")
}

// A Doc was stored as written and masked when read; its artifact is masked
// when stored, like any artifact. The Doc keeps the original.
func TestLegacyDocsAreMaskedWhenCopied(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	for _, doc := range []TempDocument{
		{ID: "secr0001", Title: "Deploy with hunter2", Summary: "uses hunter2", Content: "run with password hunter2\nand keep going\n"},
		{ID: "clean001", Title: "Clean", Summary: "nothing to hide", Content: "nothing to hide\n"},
	} {
		doc := doc
		if err := db.CreateTempDocument(ctx, &doc); err != nil {
			t.Fatal(err)
		}
	}
	report, err := db.MigrateLegacyDocsToArtifacts(ctx, testLegacyDocMask)
	if err != nil || report.Copied != 2 || report.Masked != 1 || report.Remasked != 0 || report.MismatchCount != 0 {
		t.Fatalf("report = %+v err=%v", report, err)
	}
	artifact, err := db.GetArtifact(ctx, "secr0001")
	if err != nil || artifact.Title != "Deploy with [REDACTED]" || artifact.Summary != "uses [REDACTED]" {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	version, err := db.GetArtifactVersion(ctx, "secr0001", 1)
	want := "run with password [REDACTED]\nand keep going\n"
	if err != nil || version.Content != want || version.SizeBytes != int64(len(want)) || version.SHA256 != legacyDocContentHash(want) {
		t.Fatalf("version = %+v err=%v", version, err)
	}
	if clean, err := db.GetArtifactVersion(ctx, "clean001", 1); err != nil || clean.Content != "nothing to hide\n" {
		t.Fatalf("a Doc with nothing to hide was changed: %+v err=%v", clean, err)
	}
	// The Doc keeps what was written.
	if doc, err := db.GetTempDocument(ctx, "secr0001"); err != nil || doc.Content != "run with password hunter2\nand keep going\n" || doc.Title != "Deploy with hunter2" {
		t.Fatalf("the Doc was changed: %+v err=%v", doc, err)
	}
}

// Copies made before the copy masked are masked by the next run, as long as
// they still hold exactly what their Doc holds.
func TestLegacyDocCopiesAreMaskedAfterTheFact(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	for _, doc := range []TempDocument{
		{ID: "secr0001", Title: "Deploy with hunter2", Summary: "uses hunter2", Content: "password hunter2\n"},
		{ID: "secr0002", Title: "Edited since", Content: "token hunter2\n"},
		{ID: "clean001", Title: "Clean", Content: "nothing to hide\n"},
	} {
		doc := doc
		if err := db.CreateTempDocument(ctx, &doc); err != nil {
			t.Fatal(err)
		}
	}
	// The copy as it was first released: no masking.
	if report, err := db.MigrateLegacyDocsToArtifacts(ctx, nil); err != nil || report.Copied != 3 {
		t.Fatalf("unmasked copy = %+v err=%v", report, err)
	}
	// One of the artifacts got a second version since; version 1 is history.
	if _, _, err := db.AppendArtifactVersion(ctx, "secr0002", 1, &ArtifactVersion{Format: ArtifactFormatMarkdown, Content: "rewritten\n", SHA256: "h2"}); err != nil {
		t.Fatal(err)
	}

	// The check, with the mask, tells the copies that still show the word.
	if checked, err := db.VerifyLegacyDocMigration(ctx, testLegacyDocMask); err != nil || checked.MismatchCount != 2 || checked.Masked != 2 {
		t.Fatalf("check before masking = %+v err=%v", checked, err)
	}
	report, err := db.MigrateLegacyDocsToArtifacts(ctx, testLegacyDocMask)
	if err != nil || report.Remasked != 2 || report.Copied != 0 || report.MismatchCount != 0 || report.Masked != 2 {
		t.Fatalf("run that masks = %+v err=%v", report, err)
	}
	for id, want := range map[string]string{"secr0001": "password [REDACTED]\n", "secr0002": "token [REDACTED]\n", "clean001": "nothing to hide\n"} {
		version, err := db.GetArtifactVersion(ctx, id, 1)
		if err != nil || version.Content != want || version.SHA256 != legacyDocContentHash(want) || version.SizeBytes != int64(len(want)) {
			t.Errorf("version 1 of %s = %+v err=%v", id, version, err)
		}
	}
	if artifact, err := db.GetArtifact(ctx, "secr0001"); err != nil || artifact.Title != "Deploy with [REDACTED]" || artifact.Summary != "uses [REDACTED]" {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	// What was published since is not touched.
	if current, err := db.GetArtifactVersion(ctx, "secr0002", 2); err != nil || current.Content != "rewritten\n" {
		t.Fatalf("version 2 = %+v err=%v", current, err)
	}
	// Nothing left to do.
	if again, err := db.MigrateLegacyDocsToArtifacts(ctx, testLegacyDocMask); err != nil || again.Remasked != 0 {
		t.Fatalf("second masking run = %+v err=%v", again, err)
	}
	if count := countRows(t, db, `SELECT COUNT(*) FROM temp_documents WHERE content LIKE '%hunter2%'`); count != 2 {
		t.Fatalf("%d Docs still hold the original, want 2", count)
	}
}
