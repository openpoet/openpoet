package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openpoet/internal/database"
)

// Phase 7.4 (Maestro): mission safety nets. Group limits live in
// tags.settings_json (the V65 substrate); every spawn passes the parallelism
// cap, and mission-bound spawns also pass the budget (tokens + worker count +
// wall clock — never USD, which is blind under OAuth). A blown budget doesn't
// just refuse: it AUTO-PAUSES the mission (anomaly brake) and announces it on
// the outbox so the coordinator and the panel see it immediately.

// GroupSettings is the parsed tags.settings_json vocabulary.
type GroupSettings struct {
	// MaxParallel caps live sessions across the group's projects (0 = default).
	MaxParallel int `json:"max_parallel"`
	// MaxWorkers caps the mission roster size (0 = default).
	MaxWorkers int `json:"max_workers"`
	// TokenBudget caps summed input+output tokens across the mission's
	// workers (0 = unlimited).
	TokenBudget int64 `json:"token_budget"`
	// WallClockMinutes caps mission age. 0 or absent = unlimited (consistent
	// with the other budgets); negative = already expired (test hook).
	WallClockMinutes *int `json:"wall_clock_minutes"`
}

const (
	// Safe defaults for a dev machine that also hosts production on :8081.
	defaultGroupMaxParallel = 3
	defaultMissionWorkers   = 12
)

// groupSafetyStore is the safety-net slice of the database.
// *database.DB satisfies it.
type groupSafetyStore interface {
	GetTag(ctx context.Context, id int64) (*database.Tag, error)
	CountActiveSessionsInProjects(ctx context.Context, projectIDs []int64) (int, error)
	SumTokensForSessions(ctx context.Context, sessionIDs []string) (int64, error)
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

func (g GroupSettings) maxWorkers() int {
	if g.MaxWorkers > 0 {
		return g.MaxWorkers
	}
	return defaultMissionWorkers
}

// safetyViolation is a typed refusal from the safety nets.
type safetyViolation struct {
	Code    string
	Message string
}

func (v *safetyViolation) Error() string { return v.Message }

// checkSpawnSafety enforces the parallelism cap for the group and, when the
// spawn is mission-bound, the mission budget. Read-only; the caller maps a
// violation to a typed HTTP refusal (and auto-pauses the mission on budget).
func (c *coordinatorAPI) checkSpawnSafety(ctx context.Context, group int64, mission *database.Mission, now time.Time) (*safetyViolation, error) {
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
		return &safetyViolation{Code: "mission_parallel_cap",
			Message: fmt.Sprintf("the group already runs %d live session(s) (max_parallel=%d) — wait for a worker to finish or raise the group's max_parallel", live, settings.maxParallel())}, nil
	}

	if mission == nil {
		return nil, nil
	}
	missions, ok := c.missions()
	if !ok {
		return nil, nil
	}
	workers, err := missions.ListMissionWorkers(ctx, mission.ID)
	if err != nil {
		return nil, err
	}
	if len(workers) >= settings.maxWorkers() {
		return &safetyViolation{Code: "mission_budget_exceeded",
			Message: fmt.Sprintf("the mission roster already holds %d workers (max_workers=%d)", len(workers), settings.maxWorkers())}, nil
	}
	if settings.WallClockMinutes != nil && *settings.WallClockMinutes != 0 {
		age := now.Sub(mission.CreatedAt)
		if age > time.Duration(*settings.WallClockMinutes)*time.Minute {
			return &safetyViolation{Code: "mission_budget_exceeded",
				Message: fmt.Sprintf("the mission exceeded its wall-clock budget (%d min, running for %s)", *settings.WallClockMinutes, age.Round(time.Second))}, nil
		}
	}
	if settings.TokenBudget > 0 && len(workers) > 0 {
		sessionIDs := make([]string, 0, len(workers))
		for _, worker := range workers {
			if worker.SessionID != "" {
				sessionIDs = append(sessionIDs, worker.SessionID)
			}
		}
		spent, err := safety.SumTokensForSessions(ctx, sessionIDs)
		if err != nil {
			return nil, err
		}
		if spent > settings.TokenBudget {
			return &safetyViolation{Code: "mission_budget_exceeded",
				Message: fmt.Sprintf("the mission spent %d tokens (budget %d)", spent, settings.TokenBudget)}, nil
		}
	}
	return nil, nil
}

// enforceSpawnSafety runs the nets and, on a budget violation, pulls the
// anomaly brake: mission → paused + mission.paused_anomaly on the outbox.
// Returns false when the spawn must not proceed (response already written).
// allowPause gates the anomaly brake: only a REAL spawn may pause the mission —
// a dry_run is a declared side-effect-free probe.
func (c *coordinatorAPI) enforceSpawnSafety(w http.ResponseWriter, r *http.Request, group int64, mission *database.Mission, allowPause bool) bool {
	violation, err := c.checkSpawnSafety(r.Context(), group, mission, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mission_safety_check_failed", "the mission safety nets could not be evaluated", true)
		return false
	}
	if violation == nil {
		return true
	}
	if allowPause && violation.Code == "mission_budget_exceeded" && mission != nil && mission.Status == "active" {
		if store, ok := c.missions(); ok {
			if err := store.UpdateMissionStatus(r.Context(), mission.ID, "paused"); err == nil {
				_ = store.AppendMissionEvent(r.Context(), "mission.paused_anomaly", mission.ID, map[string]any{
					"mission_id": mission.ID, "reason": violation.Message,
				})
			}
		}
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
