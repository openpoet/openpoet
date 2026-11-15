package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestMigrateV87AddsFormatAndCanRunAgain(t *testing.T) {
	db, err := sqlx.Connect("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// artifact_versions as V81 created it, with a version stored before V87.
	if _, err := db.Exec(`
		CREATE TABLE artifact_versions (
			artifact_id TEXT NOT NULL, version INTEGER NOT NULL, content TEXT NOT NULL,
			size_bytes INTEGER NOT NULL DEFAULT 0, sha256 TEXT NOT NULL DEFAULT '', change_note TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (artifact_id, version));
		INSERT INTO artifact_versions (artifact_id, version, content) VALUES ('old00001', 1, '<p>before</p>');
	`); err != nil {
		t.Fatal(err)
	}
	migrate := func() {
		t.Helper()
		tx, err := db.Beginx()
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateV87(tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	migrate()
	// A second run, as after schema_version was set back by hand, changes nothing.
	migrate()

	var format string
	if err := db.Get(&format, `SELECT format FROM artifact_versions WHERE artifact_id = 'old00001'`); err != nil || format != ArtifactFormatHTML {
		t.Fatalf("format of a version stored before V87 = %q (err %v), want html", format, err)
	}
	// The insert a binary from before V87 runs still works and is an HTML version.
	if _, err := db.Exec(`INSERT INTO artifact_versions (artifact_id, version, content, size_bytes, sha256, change_note, created_by, session_id)
		VALUES ('old00001', 2, '<p>after</p>', 12, 'h', '', '', '')`); err != nil {
		t.Fatalf("insert without format: %v", err)
	}
	if err := db.Get(&format, `SELECT format FROM artifact_versions WHERE artifact_id = 'old00001' AND version = 2`); err != nil || format != ArtifactFormatHTML {
		t.Fatalf("format of an insert without the column = %q (err %v), want html", format, err)
	}
}

func TestArtifactVersionFormatIsStoredAndListed(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// A version published without a format is an HTML version.
	htmlArtifact, htmlVersion := newTestArtifact("fmt00001", 0, "sess-a")
	if err := db.CreateArtifact(ctx, htmlArtifact, htmlVersion); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetArtifactVersion(ctx, "fmt00001", 1)
	if err != nil || stored.Format != ArtifactFormatHTML {
		t.Fatalf("default format = %q (err %v), want html", stored.Format, err)
	}

	source := "# Title\n\nSome **markdown**.\n"
	artifact, version := newTestArtifact("fmt00002", 0, "sess-a")
	version.Format, version.Content, version.SHA256 = ArtifactFormatMarkdown, source, "hash-md-1"
	if err := db.CreateArtifact(ctx, artifact, version); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetArtifactVersion(ctx, "fmt00002", 1)
	if err != nil || stored.Format != ArtifactFormatMarkdown || stored.Content != source {
		t.Fatalf("markdown version = %+v (err %v)", stored, err)
	}

	// The same content in the same format is not stored again.
	_, created, err := db.AppendArtifactVersion(ctx, "fmt00002", 1, &ArtifactVersion{Format: ArtifactFormatMarkdown, Content: source, SHA256: "hash-md-1"})
	if err != nil || created {
		t.Fatalf("identical markdown append: created=%v err=%v", created, err)
	}
	// The same bytes published as HTML are a different version.
	current, created, err := db.AppendArtifactVersion(ctx, "fmt00002", 1, &ArtifactVersion{Format: ArtifactFormatHTML, Content: source, SHA256: "hash-md-1"})
	if err != nil || !created || current.CurrentVersion != 2 {
		t.Fatalf("same bytes in another format: current=%+v created=%v err=%v", current, created, err)
	}

	versions, err := db.ListArtifactVersions(ctx, "fmt00002")
	if err != nil || len(versions) != 2 || versions[0].Format != ArtifactFormatHTML || versions[1].Format != ArtifactFormatMarkdown {
		t.Fatalf("listed versions = %+v (err %v)", versions, err)
	}
	items, err := db.ListArtifacts(ctx, ArtifactFilter{SessionID: "sess-a"})
	if err != nil {
		t.Fatal(err)
	}
	formats := map[string]string{}
	for _, item := range items {
		formats[item.ID] = item.Format
	}
	if formats["fmt00001"] != ArtifactFormatHTML || formats["fmt00002"] != ArtifactFormatHTML {
		t.Fatalf("listed formats = %v, want the format of each current version", formats)
	}
}
