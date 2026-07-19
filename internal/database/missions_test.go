package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

// TestMissionGrantConsumeAndExhaust: peek distinguishes never-granted from
// exhausted; consume decrements atomically; expiry counts as absent.
func TestMissionGrantConsumeAndExhaust(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "grants.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	mission, err := db.CreateMission(ctx, "g", 5, "sess")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.PeekMissionGrant(ctx, mission.ID, "workspaces.merge"); !errors.Is(err, ErrMissionGrantRequired) {
		t.Fatalf("never-granted must be required, got %v", err)
	}
	grant := &MissionGrant{MissionID: mission.ID, Capability: "workspaces.merge",
		UsesRemaining: 2, ExpiresAt: time.Now().Add(time.Hour), GrantedBy: "user"}
	if err := db.CreateMissionGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	if err := db.PeekMissionGrant(ctx, mission.ID, "workspaces.merge"); err != nil {
		t.Fatalf("live grant must peek clean: %v", err)
	}
	grantID1, err := db.ConsumeMissionGrantUse(ctx, mission.ID, "workspaces.merge")
	if err != nil || grantID1 != grant.ID {
		t.Fatalf("first consume: id=%d err=%v", grantID1, err)
	}
	if _, err := db.ConsumeMissionGrantUse(ctx, mission.ID, "workspaces.merge"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeMissionGrantUse(ctx, mission.ID, "workspaces.merge"); !errors.Is(err, ErrMissionGrantExhausted) {
		t.Fatalf("third consume must exhaust, got %v", err)
	}
	// Refund (conflict costs nothing) re-arms exactly one use.
	if err := db.RefundMissionGrantUse(ctx, grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeMissionGrantUse(ctx, mission.ID, "workspaces.merge"); err != nil {
		t.Fatalf("refunded use must be spendable: %v", err)
	}
	if err := db.PeekMissionGrant(ctx, mission.ID, "workspaces.merge"); !errors.Is(err, ErrMissionGrantExhausted) {
		t.Fatalf("exhausted peek must be typed, got %v", err)
	}

	// Expired grant reads as never-granted-alive → required.
	expired := &MissionGrant{MissionID: mission.ID, Capability: "workspaces.remove",
		UsesRemaining: 5, ExpiresAt: time.Now().Add(-time.Hour), GrantedBy: "user"}
	if err := db.CreateMissionGrant(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if err := db.PeekMissionGrant(ctx, mission.ID, "workspaces.remove"); !errors.Is(err, ErrMissionGrantRequired) {
		t.Fatalf("expired grant must read as required, got %v", err)
	}
}
