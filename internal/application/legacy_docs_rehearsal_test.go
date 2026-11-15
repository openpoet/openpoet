package application

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"openpoet/internal/database"
)

// TestLegacyDocMigrationRehearsal runs the copy of Docs into artifacts against
// a database file named by OPENPOET_LEGACY_DOCS_REHEARSAL_DB, with the mask the
// server uses, and prints the report: counts only, never a title or a content.
// It is how the copy is rehearsed on a COPY of a real database before a
// release; it migrates the schema of that file and writes to it, so never
// point it at a database in use.
func TestLegacyDocMigrationRehearsal(t *testing.T) {
	path := os.Getenv("OPENPOET_LEGACY_DOCS_REHEARSAL_DB")
	if path == "" {
		t.Skip("OPENPOET_LEGACY_DOCS_REHEARSAL_DB is not set")
	}
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := time.Now()
	report, err := db.MigrateLegacyDocsToArtifacts(context.Background(), MaskArtifactText)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("ran in %s\n%s", time.Since(started).Round(time.Millisecond), encoded)
	if report.Pending != 0 || report.MismatchCount != 0 || len(report.Failed) != 0 || report.VerificationDocsResolved != report.VerificationDocs {
		t.Fatalf("the copy is not complete or not faithful")
	}
	again, err := db.MigrateLegacyDocsToArtifacts(context.Background(), MaskArtifactText)
	if err != nil || again.Copied != 0 || again.Remasked != 0 {
		t.Fatalf("a second run copied %d Docs and masked %d (err %v)", again.Copied, again.Remasked, err)
	}
}

// The mask the copy uses is the one artifacts are stored with.
func TestMaskArtifactTextMatchesTheMaskOfStoredArtifacts(t *testing.T) {
	for _, text := range []string{
		"plain text, nothing to hide\n",
		"password=hunter2hunter2\nnext line\n",
		"Authorization: Bearer hidden-token\n",
		"api_key=artifact-secret-value\nand password = abc12345\n",
	} {
		stored, _ := maskArtifactSecrets(text)
		if masked := MaskArtifactText(text); masked != stored {
			t.Fatalf("MaskArtifactText(%q) = %q, an artifact stores %q", text, masked, stored)
		}
	}
}
