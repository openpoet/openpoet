package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func newTestArtifact(id string, projectID int64, sessionID string) (*Artifact, *ArtifactVersion) {
	artifact := &Artifact{ID: id, Title: "Plan " + id, SessionID: sessionID, CreatedBy: "session:" + sessionID}
	if projectID > 0 {
		artifact.ProjectID = sql.NullInt64{Int64: projectID, Valid: true}
	}
	html := "<html><body>" + id + " v1</body></html>"
	return artifact, &ArtifactVersion{Content: html, SizeBytes: int64(len(html)), SHA256: "hash-" + id + "-1", CreatedBy: artifact.CreatedBy, SessionID: sessionID}
}

func TestMigrationV81CreatesArtifactTables(t *testing.T) {
	db := setupTestDB(t)
	for _, table := range []string{"artifacts", "artifact_versions", "artifact_annotations"} {
		var name string
		if err := db.Get(&name, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table); err != nil {
			t.Fatalf("table %s missing after migrations: %v", table, err)
		}
	}
	var version int
	if err := db.Get(&version, `SELECT version FROM schema_version LIMIT 1`); err != nil || version < 81 {
		t.Fatalf("schema version = %d (err %v), want >= 81", version, err)
	}
}

func TestArtifactVersioningConflictAndDedup(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	artifact, v1 := newTestArtifact("art00001", 0, "sess-a")
	if err := db.CreateArtifact(ctx, artifact, v1); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetArtifact(ctx, "art00001")
	if err != nil || stored.CurrentVersion != 1 || stored.Status != "active" {
		t.Fatalf("stored artifact = %+v err=%v", stored, err)
	}

	// Stale base version: nothing is written, the current state is reported.
	stale := &ArtifactVersion{Content: "<p>stale</p>", SHA256: "hash-stale"}
	current, created, err := db.AppendArtifactVersion(ctx, "art00001", 7, stale)
	if !errors.Is(err, ErrArtifactVersionConflict) || created || current == nil || current.CurrentVersion != 1 {
		t.Fatalf("stale append: current=%+v created=%v err=%v", current, created, err)
	}

	// Identical HTML (same hash as the current version) is not stored again.
	same := &ArtifactVersion{Content: v1.Content, SHA256: v1.SHA256}
	current, created, err = db.AppendArtifactVersion(ctx, "art00001", 1, same)
	if err != nil || created || current.CurrentVersion != 1 {
		t.Fatalf("identical append: current=%+v created=%v err=%v", current, created, err)
	}

	v2 := &ArtifactVersion{Content: "<p>v2</p>", SizeBytes: 9, SHA256: "hash-2", ChangeNote: "second", CreatedBy: "session:sess-b", SessionID: "sess-b"}
	current, created, err = db.AppendArtifactVersion(ctx, "art00001", 1, v2)
	if err != nil || !created || current.CurrentVersion != 2 {
		t.Fatalf("append v2: current=%+v created=%v err=%v", current, created, err)
	}
	// base version 0 means "whatever is current".
	v3 := &ArtifactVersion{Content: "<p>v3</p>", SizeBytes: 9, SHA256: "hash-3"}
	if current, created, err = db.AppendArtifactVersion(ctx, "art00001", 0, v3); err != nil || !created || current.CurrentVersion != 3 {
		t.Fatalf("append v3: current=%+v created=%v err=%v", current, created, err)
	}

	versions, err := db.ListArtifactVersions(ctx, "art00001")
	if err != nil || len(versions) != 3 || versions[0].Version != 3 || versions[2].Version != 1 {
		t.Fatalf("versions = %+v err=%v", versions, err)
	}
	if versions[0].Content != "" {
		t.Fatal("version listing must not carry the HTML")
	}
	// Every version stays readable with its own HTML.
	first, err := db.GetArtifactVersion(ctx, "art00001", 1)
	if err != nil || first.Content != v1.Content {
		t.Fatalf("v1 = %+v err=%v", first, err)
	}
	second, err := db.GetArtifactVersion(ctx, "art00001", 2)
	if err != nil || second.Content != "<p>v2</p>" || second.ChangeNote != "second" || second.SessionID != "sess-b" {
		t.Fatalf("v2 = %+v err=%v", second, err)
	}
	if _, err := db.GetArtifactVersion(ctx, "art00001", 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing version err = %v", err)
	}
	if _, _, err := db.AppendArtifactVersion(ctx, "missing", 0, v3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("append to missing artifact err = %v", err)
	}
}

func TestListArtifactsFiltersAndMeta(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	a1, v := newTestArtifact("art0000a", 1, "sess-a")
	a1.TaskID = sql.NullInt64{Int64: 5, Valid: true}
	if err := db.CreateArtifact(ctx, a1, v); err != nil {
		t.Fatal(err)
	}
	a2, v := newTestArtifact("art0000b", 1, "sess-b")
	if err := db.CreateArtifact(ctx, a2, v); err != nil {
		t.Fatal(err)
	}
	a3, v := newTestArtifact("art0000c", 2, "sess-c")
	if err := db.CreateArtifact(ctx, a3, v); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifact_annotations (artifact_id, version, body, status) VALUES ('art0000a', 1, 'fix', 'open'), ('art0000a', 1, 'done', 'resolved')`); err != nil {
		t.Fatal(err)
	}

	count := func(filter ArtifactFilter) int {
		t.Helper()
		items, err := db.ListArtifacts(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	if got := count(ArtifactFilter{}); got != 3 {
		t.Fatalf("all = %d", got)
	}
	if got := count(ArtifactFilter{ProjectID: 1}); got != 2 {
		t.Fatalf("project 1 = %d", got)
	}
	if got := count(ArtifactFilter{SessionID: "sess-c"}); got != 1 {
		t.Fatalf("session c = %d", got)
	}
	items, err := db.ListArtifacts(ctx, ArtifactFilter{TaskID: 5})
	if err != nil || len(items) != 1 || items[0].ID != "art0000a" || items[0].OpenAnnotations != 1 || items[0].SizeBytes == 0 {
		t.Fatalf("task 5 = %+v err=%v", items, err)
	}

	archived, title := "archived", "Renamed"
	if err := db.UpdateArtifactMeta(ctx, "art0000b", &title, nil, &archived); err != nil {
		t.Fatal(err)
	}
	if got := count(ArtifactFilter{ProjectID: 1}); got != 1 {
		t.Fatalf("project 1 without archived = %d", got)
	}
	if got := count(ArtifactFilter{ProjectID: 1, IncludeArchived: true}); got != 2 {
		t.Fatalf("project 1 with archived = %d", got)
	}
	renamed, err := db.GetArtifact(ctx, "art0000b")
	if err != nil || renamed.Title != "Renamed" || renamed.Status != "archived" {
		t.Fatalf("renamed = %+v err=%v", renamed, err)
	}
	if err := db.UpdateArtifactMeta(ctx, "missing", &title, nil, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("meta update on missing artifact err = %v", err)
	}
}
