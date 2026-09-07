package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"openpoet/internal/database"
)

// Phase 7.4 (Maestro): the spawn safety net. Group limits live in
// tags.settings_json (the V65 substrate) and every coordinator spawn passes the
// parallelism cap.

// GroupSettings is the parsed tags.settings_json vocabulary.
type GroupSettings struct {
	// MaxParallel caps live sessions across the group's projects (0 = default).
	MaxParallel int `json:"max_parallel"`
}

// Safe default for a dev machine that also hosts production on :8081.
const defaultGroupMaxParallel = 3

// groupSafetyStore is the safety-net slice of the database.
// *database.DB satisfies it.
type groupSafetyStore interface {
	GetTag(ctx context.Context, id int64) (*database.Tag, error)
	CountActiveSessionsInProjects(ctx context.Context, projectIDs []int64) (int, error)
}

func parseGroupSettings(raw string) GroupSettings {
	settings := GroupSettings{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return settings
	}
	_ = json.Unmarshal([]byte(raw), &settings)
	return settings
}

func (g GroupSettings) maxParallel() int {
	if g.MaxParallel > 0 {
		return g.MaxParallel
	}
	return defaultGroupMaxParallel
}

// safetyViolation is a typed refusal from the safety net.
type safetyViolation struct {
	Code    string
	Message string
}

func (v *safetyViolation) Error() string { return v.Message }

// checkSpawnSafety enforces the parallelism cap for the group. Read-only; the
// caller maps a violation to a typed HTTP refusal.
func (c *coordinatorAPI) checkSpawnSafety(ctx context.Context, group int64) (*safetyViolation, error) {
	safety, ok := c.store.(groupSafetyStore)
	if !ok {
		return nil, nil // no safety substrate wired (bare test fixtures)
	}
	tag, err := safety.GetTag(ctx, group)
	if err != nil || tag == nil {
		return nil, err
	}
	settings := parseGroupSettings(tag.SettingsJSON.String)

	members, err := c.store.ProjectIDsForTags(ctx, []int64{group})
	if err != nil {
		return nil, err
	}
	live, err := safety.CountActiveSessionsInProjects(ctx, members)
	if err != nil {
		return nil, err
	}
	if live >= settings.maxParallel() {
		return &safetyViolation{Code: "coordinator_parallel_cap",
			Message: fmt.Sprintf("the group already runs %d live session(s) (max_parallel=%d) — wait for a worker to finish or raise the group's max_parallel", live, settings.maxParallel())}, nil
	}
	return nil, nil
}

// enforceSpawnSafety runs the net and writes the typed refusal itself.
// Returns false when the spawn must not proceed (response already written).
func (c *coordinatorAPI) enforceSpawnSafety(w http.ResponseWriter, r *http.Request, group int64) bool {
	violation, err := c.checkSpawnSafety(r.Context(), group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "coordinator_safety_check_failed", "the spawn safety net could not be evaluated", true)
		return false
	}
	if violation == nil {
		return true
	}
	writeError(w, http.StatusTooManyRequests, violation.Code, violation.Message, false)
	return false
}

// Spawn idempotency (Phase 7.4): the fence for retried/replayed start_worker
// calls rides the group blackboard — zero new schema. The key is RESERVED
// before the spawn (CAS at version 0) and completed with the session id after;
// a replay reads the recorded session instead of double-spawning.

const (
	spawnKeyTTLSeconds = 24 * 60 * 60
	// A PENDING reservation (reserve→crash before complete) must not jam the
	// key for a day — it expires in 15 minutes; completion extends to 24h.
	spawnKeyPendingTTLSeconds = 15 * 60
)

type spawnKeyValue struct {
	SessionID string `json:"session_id"`
	Pending   bool   `json:"pending,omitempty"`
}

// reserveSpawnKey claims the idempotency key. Returns (existingSessionID,
// reservedVersion): a non-empty session id means "replay — return this
// session"; reservedVersion > 0 means "you own the reservation, spawn and
// complete it". Both zero with pending=true error means a concurrent spawn is
// mid-flight.
func (c *coordinatorAPI) reserveSpawnKey(ctx context.Context, group int64, key, sessionID string) (string, int64, error) {
	blackboardKey := "spawn:" + key
	expected := int64(0)
	value, _ := json.Marshal(spawnKeyValue{Pending: true})
	version, err := c.store.BlackboardPut(ctx, database.BlackboardPutInput{
		ScopeType: "group", ScopeID: group, Key: blackboardKey,
		ValueJSON: string(value), ExpectedVersion: &expected,
		TTLSeconds: spawnKeyPendingTTLSeconds, Actor: "session:" + sessionID,
	})
	if err == nil {
		return "", version, nil
	}
	// Reservation lost: someone spawned (or is spawning) with this key.
	entry, getErr := c.store.BlackboardGet(ctx, "group", group, blackboardKey)
	if getErr != nil || entry == nil {
		return "", 0, err
	}
	var recorded spawnKeyValue
	_ = json.Unmarshal([]byte(entry.ValueJSON), &recorded)
	if recorded.SessionID != "" {
		return recorded.SessionID, 0, nil
	}
	return "", 0, &safetyViolation{Code: "spawn_in_flight", Message: "a spawn with this idempotency_key is already in flight — retry shortly"}
}

// completeSpawnKey records the spawned session id under the reservation.
func (c *coordinatorAPI) completeSpawnKey(ctx context.Context, group int64, key, sessionID string, reservedVersion int64) {
	value, _ := json.Marshal(spawnKeyValue{SessionID: sessionID})
	expected := reservedVersion
	_, _ = c.store.BlackboardPut(ctx, database.BlackboardPutInput{
		ScopeType: "group", ScopeID: group, Key: "spawn:" + key,
		ValueJSON: string(value), ExpectedVersion: &expected,
		TTLSeconds: spawnKeyTTLSeconds, Actor: "session:" + sessionID,
	})
}

// releaseSpawnKey expires a reservation whose spawn failed, so a retry with
// the same key can run.
func (c *coordinatorAPI) releaseSpawnKey(ctx context.Context, group int64, key, sessionID string, reservedVersion int64) {
	expected := reservedVersion
	_, _ = c.store.BlackboardPut(ctx, database.BlackboardPutInput{
		ScopeType: "group", ScopeID: group, Key: "spawn:" + key,
		ValueJSON: `{}`, ExpectedVersion: &expected,
		TTLSeconds: -1, Actor: "session:" + sessionID,
	})
}
