package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestTempDocumentMissionIDRoundTrip: V69's mission_id persists when set and
// stays NULL when absent (the link is optional).
func TestTempDocumentMissionIDRoundTrip(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "docs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	linked := &TempDocument{ID: "doc-miss", Title: "mission doc", Content: "x",
		MissionID: sql.NullInt64{Int64: 4242, Valid: true}}
	if err := db.CreateTempDocument(ctx, linked); err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	if err := db.Get(&got, "SELECT mission_id FROM temp_documents WHERE id='doc-miss'"); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != 4242 {
		t.Fatalf("mission_id did not persist: %+v", got)
	}

	loose := &TempDocument{ID: "doc-loose", Title: "loose doc", Content: "y"}
	if err := db.CreateTempDocument(ctx, loose); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&got, "SELECT mission_id FROM temp_documents WHERE id='doc-loose'"); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("mission_id must stay NULL when absent, got %+v", got)
	}
}
