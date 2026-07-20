package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func coordinatorTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project := &Project{Name: "p1", Path: "/tmp/p1", Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	session := &Session{
		ID: "sess-coord-1", ProjectID: project.ID, Status: "running",
		StartTime: time.Now(), Backend: "claude_code",
		Model: "unknown", RequestedModel: "default", Effort: "default",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	return db, session.ID
}

// TestSessionFileActivityUpsertAccumulates proves the UNIQUE(session,path,kind)
// contract the whole ledger rests on: repeats bump touch_count on ONE row.
func TestSessionFileActivityUpsertAccumulates(t *testing.T) {
	db, sessionID := coordinatorTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	apply := func(count int64, ts time.Time, tool string) {
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = UpsertSessionFileActivityTx(tx, SessionFileActivityUpsert{
			SessionID: sessionID, ProjectID: 1, Path: "shared.go", Kind: "write",
			FirstTouchAt: ts, LastTouchAt: ts, TouchCount: count, LastTool: tool,
		})
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, base, "Edit")
	apply(3, base.Add(time.Minute), "Write")

	rows, err := db.ListSessionFileActivity(ctx, sessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (UNIQUE violated)", len(rows))
	}
	if rows[0].TouchCount != 4 {
		t.Fatalf("touch_count = %d, want 4", rows[0].TouchCount)
	}
	if rows[0].LastTool != "Write" {
		t.Fatalf("last_tool = %q, want Write", rows[0].LastTool)
	}
}

// TestCoordinatorIncidentUpsertKeepsIdentity proves re-evidence refreshes
// counters without minting a second row or changing the incident id.
func TestCoordinatorIncidentUpsertKeepsIdentity(t *testing.T) {
	db, _ := coordinatorTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	apply := func(inc CoordinatorIncident) {
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := UpsertCoordinatorIncidentTx(tx, inc); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	first := CoordinatorIncident{
		ID: "C-abc123", Rule: "file_overlap", Severity: "critical", ProjectID: 1,
		ScopeKey: "local|/p|shared.go|sa+sb", SessionsJSON: `["sa","sb"]`, State: "open",
		FirstDetectedAt: base, LastEvidenceAt: base, EvidenceCount: 1, DetailsJSON: `{}`,
	}
	apply(first)
	second := first
	second.ID = "C-should-not-replace"
	second.LastEvidenceAt = base.Add(time.Minute)
	second.EvidenceCount = 7
	apply(second)

	incidents, err := db.ListCoordinatorIncidents(ctx, 1, "open", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incidents))
	}
	if incidents[0].ID != "C-abc123" {
		t.Fatalf("id = %q, want the original C-abc123", incidents[0].ID)
	}
	if incidents[0].EvidenceCount != 7 {
		t.Fatalf("evidence_count = %d, want 7", incidents[0].EvidenceCount)
	}
	got, err := db.GetCoordinatorIncident(ctx, "C-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Rule != "file_overlap" {
		t.Fatalf("GetCoordinatorIncident = %+v", got)
	}
	missing, err := db.GetCoordinatorIncident(ctx, "C-missing")
	if err != nil || missing != nil {
		t.Fatalf("missing incident should be (nil, nil), got (%+v, %v)", missing, err)
	}
}
