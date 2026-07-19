package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestMissionSingleActivePerGroup: the partial unique index allows exactly one
// ACTIVE mission per group; completing it frees the slot; another group is
// unaffected.
func TestMissionSingleActivePerGroup(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "missions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	first, err := db.CreateMission(ctx, "goal A", 10, "sess-c")
	if err != nil || first.Status != "active" {
		t.Fatalf("first mission: %+v err=%v", first, err)
	}
	if _, err := db.CreateMission(ctx, "goal B", 10, "sess-c"); !errors.Is(err, ErrMissionActiveExists) {
		t.Fatalf("second active mission in group must be refused, got %v", err)
	}
	if _, err := db.CreateMission(ctx, "goal other-group", 11, "sess-x"); err != nil {
		t.Fatalf("another group must be free to run its own mission: %v", err)
	}
	if err := db.UpdateMissionStatus(ctx, first.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	done, _ := db.GetMission(ctx, first.ID)
	if done.Status != "completed" || !done.CompletedAt.Valid {
		t.Fatalf("completion not stamped: %+v", done)
	}
	if _, err := db.CreateMission(ctx, "goal C", 10, "sess-c"); err != nil {
		t.Fatalf("completed mission must free the active slot: %v", err)
	}

	active, err := db.ListActiveMissions(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("expected 2 active missions, got %d (%v)", len(active), err)
	}
}
