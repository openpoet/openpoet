package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BlackboardEntry is one row of the shared CAS/TTL key-value board (V66).
type BlackboardEntry struct {
	ID             int64        `db:"id" json:"-"`
	ScopeType      string       `db:"scope_type" json:"scope_type"`
	ScopeID        int64        `db:"scope_id" json:"scope_id"`
	Key            string       `db:"key" json:"key"`
	ValueJSON      string       `db:"value_json" json:"value"`
	Version        int64        `db:"version" json:"version"`
	UpdatedByActor string       `db:"updated_by_actor" json:"updated_by_actor"`
	CreatedAt      time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time    `db:"updated_at" json:"updated_at"`
	ExpiresAt      sql.NullTime `db:"expires_at" json:"expires_at,omitempty"`
}

// ErrBlackboardCASConflict is returned when expected_version != the live version
// (optimistic concurrency loss). ErrBlackboardFenceStale is returned when a
// fenced mutation carries a lease version that no longer matches — a stale lease
// holder's action fails closed.
var (
	ErrBlackboardCASConflict = errors.New("blackboard version conflict")
	ErrBlackboardFenceStale  = errors.New("blackboard fence is stale")
)

// BlackboardGet returns the LIVE entry for (scope,key). An expired entry (TTL
// passed) reads as absent (nil, nil) — it will be reclaimed on the next put.
func (d *DB) BlackboardGet(ctx context.Context, scopeType string, scopeID int64, key string) (*BlackboardEntry, error) {
	var e BlackboardEntry
	err := d.GetContext(ctx, &e,
		"SELECT * FROM blackboard_entries WHERE scope_type=? AND scope_id=? AND key=?",
		scopeType, scopeID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if e.ExpiresAt.Valid && e.ExpiresAt.Time.Before(time.Now().UTC()) {
		return nil, nil // expired = absent
	}
	return &e, nil
}

// BlackboardPutInput is one compare-and-swap write.
type BlackboardPutInput struct {
	ScopeType string
	ScopeID   int64
	Key       string
	ValueJSON string
	// ExpectedVersion, when non-nil, must equal the live version (0 = expect
	// absent). nil = unconditional set.
	ExpectedVersion *int64
	TTLSeconds      int
	// FenceKey/FenceVersion, when FenceKey != "", gate the write on a lease: the
	// fence key's live version must equal FenceVersion, else the write fails closed.
	FenceKey     string
	FenceVersion int64
	Actor        string
}

// BlackboardPut applies a CAS write atomically: it resolves the live version of
// the key (an expired row counts as version 0), optionally validates a fence
// lease and an expected version, then upserts value with version = live+1. All
// in one transaction on the single connection so concurrent puts serialize.
func (d *DB) BlackboardPut(ctx context.Context, in BlackboardPutInput) (int64, error) {
	now := time.Now().UTC()
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	liveVersion := func(key string) (int64, error) {
		var e BlackboardEntry
		qerr := tx.GetContext(ctx, &e,
			"SELECT * FROM blackboard_entries WHERE scope_type=? AND scope_id=? AND key=?",
			in.ScopeType, in.ScopeID, key)
		if errors.Is(qerr, sql.ErrNoRows) {
			return 0, nil
		}
		if qerr != nil {
			return 0, qerr
		}
		if e.ExpiresAt.Valid && e.ExpiresAt.Time.Before(now) {
			return 0, nil // expired reads as absent → version 0
		}
		return e.Version, nil
	}

	if in.FenceKey != "" {
		fenceLive, ferr := liveVersion(in.FenceKey)
		if ferr != nil {
			return 0, ferr
		}
		if fenceLive != in.FenceVersion {
			return 0, ErrBlackboardFenceStale
		}
	}

	current, cerr := liveVersion(in.Key)
	if cerr != nil {
		return 0, cerr
	}
	if in.ExpectedVersion != nil && current != *in.ExpectedVersion {
		return 0, ErrBlackboardCASConflict
	}

	newVersion := current + 1
	var expires interface{}
	if in.TTLSeconds != 0 {
		// Positive TTL → future expiry; a negative TTL yields a past expiry
		// (immediate expiration), useful for lease-release semantics. Zero =
		// persistent (no expiry).
		expires = now.Add(time.Duration(in.TTLSeconds) * time.Second)
	}
	value := in.ValueJSON
	if value == "" {
		value = "{}"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO blackboard_entries (scope_type, scope_id, key, value_json, version, updated_by_actor, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope_type, scope_id, key) DO UPDATE SET
			value_json=excluded.value_json, version=excluded.version,
			updated_by_actor=excluded.updated_by_actor, updated_at=excluded.updated_at,
			expires_at=excluded.expires_at`,
		in.ScopeType, in.ScopeID, in.Key, value, newVersion, in.Actor, now, now, expires); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVersion, nil
}
