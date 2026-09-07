package automation

import (
	"context"
	"testing"
)

// TestCoordinatorParallelCap: the parallelism cap counts live sessions in the
// group's projects; the default applies when settings_json is absent, and
// raising max_parallel releases it.
func TestCoordinatorParallelCap(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	api := &coordinatorAPI{store: f.db}

	// No settings: default max_parallel=3; two live sessions → under cap.
	f.mintSession(t, f.memberProjectID)
	f.mintSession(t, f.memberProjectID)
	violation, err := api.checkSpawnSafety(ctx, f.tagID)
	if err != nil || violation != nil {
		t.Fatalf("2 live sessions must pass the default cap: v=%v err=%v", violation, err)
	}
	// Third live session hits the default cap.
	f.mintSession(t, f.memberProjectID)
	violation, err = api.checkSpawnSafety(ctx, f.tagID)
	if err != nil || violation == nil || violation.Code != "coordinator_parallel_cap" {
		t.Fatalf("default cap (3) must trip: v=%v err=%v", violation, err)
	}
	// Raising max_parallel releases it.
	if _, err := f.db.Exec(`UPDATE tags SET settings_json='{"max_parallel":10}' WHERE id=?`, f.tagID); err != nil {
		t.Fatal(err)
	}
	violation, err = api.checkSpawnSafety(ctx, f.tagID)
	if err != nil || violation != nil {
		t.Fatalf("raised cap must pass: v=%v err=%v", violation, err)
	}
}

// TestSpawnIdempotencyDedupe: reserve → complete → replay returns the recorded
// session; a failed spawn releases the key for retry.
func TestSpawnIdempotencyDedupe(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	api := &coordinatorAPI{store: f.db}

	existing, version, err := api.reserveSpawnKey(ctx, f.tagID, "k1", "coord-s")
	if err != nil || existing != "" || version == 0 {
		t.Fatalf("first reserve must own the key: existing=%q v=%d err=%v", existing, version, err)
	}
	// Concurrent second reserve while pending → typed in-flight error.
	if _, _, err := api.reserveSpawnKey(ctx, f.tagID, "k1", "coord-s"); err == nil {
		t.Fatal("pending reservation must refuse a second reserve")
	}
	api.completeSpawnKey(ctx, f.tagID, "k1", "sess-spawned", version)

	// Replay: the recorded session comes back, no new reservation.
	existing, version2, err := api.reserveSpawnKey(ctx, f.tagID, "k1", "coord-s")
	if err != nil || existing != "sess-spawned" || version2 != 0 {
		t.Fatalf("replay must return the recorded session: existing=%q v=%d err=%v", existing, version2, err)
	}

	// Failure path: reserve then release → key reusable.
	_, version3, err := api.reserveSpawnKey(ctx, f.tagID, "k2", "coord-s")
	if err != nil || version3 == 0 {
		t.Fatalf("reserve k2: v=%d err=%v", version3, err)
	}
	api.releaseSpawnKey(ctx, f.tagID, "k2", "coord-s", version3)
	existing, version4, err := api.reserveSpawnKey(ctx, f.tagID, "k2", "coord-s")
	if err != nil || existing != "" || version4 == 0 {
		t.Fatalf("released key must be reusable: existing=%q v=%d err=%v", existing, version4, err)
	}
}
