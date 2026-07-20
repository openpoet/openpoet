package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func blackboardTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "bb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func ptr(v int64) *int64 { return &v }

// TestBlackboardCASConflict: put at v0 → v1; a second put still expecting v0
// loses with a CAS conflict; expecting v1 succeeds → v2.
func TestBlackboardCASConflict(t *testing.T) {
	db := blackboardTestDB(t)
	ctx := context.Background()
	v, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "global", Key: "k", ValueJSON: `{"n":1}`, ExpectedVersion: ptr(0)})
	if err != nil || v != 1 {
		t.Fatalf("first put: v=%d err=%v (want 1, nil)", v, err)
	}
	if _, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "global", Key: "k", ValueJSON: `{"n":2}`, ExpectedVersion: ptr(0)}); !errors.Is(err, ErrBlackboardCASConflict) {
		t.Fatalf("stale expected_version should conflict, got %v", err)
	}
	v, err = db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "global", Key: "k", ValueJSON: `{"n":2}`, ExpectedVersion: ptr(1)})
	if err != nil || v != 2 {
		t.Fatalf("cas at v1: v=%d err=%v (want 2, nil)", v, err)
	}
	got, err := db.BlackboardGet(ctx, "global", 0, "k")
	if err != nil || got == nil || got.Version != 2 {
		t.Fatalf("get after cas: %+v err=%v", got, err)
	}
}

// TestBlackboardTTLExpiry: a TTL'd entry reads as absent after expiry, and a CAS
// at v0 then reclaims it (version resets to 1). The clock is not mockable here,
// so use TTLSeconds negative to force an already-expired entry.
func TestBlackboardTTLExpiry(t *testing.T) {
	db := blackboardTestDB(t)
	ctx := context.Background()
	// Write an entry whose TTL has already elapsed (negative ttl → past expiry).
	if _, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "global", Key: "lease", ValueJSON: `{"h":"A"}`, ExpectedVersion: ptr(0), TTLSeconds: -1}); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	// Expired → Get returns absent.
	if got, err := db.BlackboardGet(ctx, "global", 0, "lease"); err != nil || got != nil {
		t.Fatalf("expired entry should read absent, got %+v err=%v", got, err)
	}
	// CAS at v0 succeeds because the live version is 0 (expired), version resets to 1.
	v, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "global", Key: "lease", ValueJSON: `{"h":"B"}`, ExpectedVersion: ptr(0)})
	if err != nil || v != 1 {
		t.Fatalf("reclaim expired lease: v=%d err=%v (want 1, nil)", v, err)
	}
}

// TestBlackboardFencingStale: A holds lease v1; B bumps to v2; a fenced mutation
// carrying the stale fence v1 fails closed, while the current fence v2 succeeds.
func TestBlackboardFencingStale(t *testing.T) {
	db := blackboardTestDB(t)
	ctx := context.Background()
	if _, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "group", ScopeID: 1, Key: "coordinator-lease", ExpectedVersion: ptr(0)}); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	if _, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "group", ScopeID: 1, Key: "coordinator-lease", ExpectedVersion: ptr(1)}); err != nil {
		t.Fatalf("B bump: %v", err)
	}
	// Stale holder A (fence v1) → rejected.
	if _, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "group", ScopeID: 1, Key: "work", ValueJSON: `{"by":"A"}`, FenceKey: "coordinator-lease", FenceVersion: 1}); !errors.Is(err, ErrBlackboardFenceStale) {
		t.Fatalf("stale fence should fail closed, got %v", err)
	}
	// Current holder B (fence v2) → accepted.
	if v, err := db.BlackboardPut(ctx, BlackboardPutInput{ScopeType: "group", ScopeID: 1, Key: "work", ValueJSON: `{"by":"B"}`, FenceKey: "coordinator-lease", FenceVersion: 2}); err != nil || v != 1 {
		t.Fatalf("current fence write: v=%d err=%v (want 1, nil)", v, err)
	}
}
