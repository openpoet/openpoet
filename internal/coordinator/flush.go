package coordinator

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"openpoet/internal/database"
)

// flushLoop ticks every 2s and drains dirty state into ONE transaction on the
// single SQLite connection: batched ledger upserts, incident rows, and outbox
// appends together. It uses a detached context — a canceled request context
// must never poison the shared connection (the documented outage).
func (c *Coordinator) flushLoop() {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.quit:
			c.flushOnce()
			return
		case <-ticker.C:
			c.flushOnce()
		}
	}
}

func (c *Coordinator) flushOnce() {
	c.mu.Lock()
	ledger := c.dirtyLedger
	c.dirtyLedger = make(map[ledgerKey]*ledgerDelta)
	var incidents []database.CoordinatorIncident
	for key := range c.dirtyIncidents {
		if inc, ok := c.incidents[key]; ok {
			incidents = append(incidents, inc.toRow())
		}
	}
	c.dirtyIncidents = make(map[string]struct{})
	events := c.pendingEvents
	c.pendingEvents = nil
	c.pruneMemoryLocked()
	c.mu.Unlock()

	if len(ledger) == 0 && len(incidents) == 0 && len(events) == 0 {
		return
	}
	if c.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.persist(ctx, ledger, incidents, events); err != nil {
		// The batch is all-or-nothing; ONE poisoned row (e.g. a ledger delta
		// whose session row was deleted mid-window → FK violation) must not
		// livelock the whole pipeline. Retry per item, dropping rows that fail
		// individually; only still-transient event appends are requeued.
		log.Printf("[Coordinator] batched flush failed, retrying per item: %v", err)
		c.persistPerItem(ctx, ledger, incidents, events)
	}
	// Wake long-poll awaiters: turn_completed / awaiting_input / conflict.detected
	// are now committed and visible. Over-signaling is harmless (readers re-check).
	if len(events) > 0 {
		c.db.NotifyOutboxAppended()
	}
}

// pruneMemoryLocked bounds long-lived maps: quiet incidents fall out of memory
// (the DB row remains) and stale hysteresis stamps go with them.
func (c *Coordinator) pruneMemoryLocked() {
	cutoff := c.now().Add(-incidentMemoryTTL)
	for key, inc := range c.incidents {
		if inc.LastEvidenceAt.Before(cutoff) {
			delete(c.incidents, key)
			delete(c.lastEmit, key)
		}
	}
	emitCutoff := c.now().Add(-2 * hysteresisWindow)
	for key, ts := range c.lastEmit {
		if _, live := c.incidents[key]; !live && ts.Before(emitCutoff) {
			delete(c.lastEmit, key)
		}
	}
}

func (c *Coordinator) persist(ctx context.Context, ledger map[ledgerKey]*ledgerDelta, incidents []database.CoordinatorIncident, events []database.EventOutboxAppend) error {
	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for key, delta := range ledger {
		err := database.UpsertSessionFileActivityTx(tx, database.SessionFileActivityUpsert{
			SessionID:    key.sessionID,
			ProjectID:    delta.projectID,
			Path:         key.path,
			Kind:         key.kind,
			FirstTouchAt: delta.firstTs,
			LastTouchAt:  delta.lastTs,
			TouchCount:   delta.count,
			LastTool:     delta.lastTool,
			Source:       "hook",
		})
		if err != nil {
			return err
		}
	}
	for _, inc := range incidents {
		if err := database.UpsertCoordinatorIncidentTx(tx, inc); err != nil {
			return err
		}
	}
	for _, ev := range events {
		if _, err := database.AppendEventOutbox(ctx, tx, ev); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// persistPerItem is the degraded path after a failed batch: each ledger delta
// and incident gets its own small transaction and is DROPPED (logged) if it
// fails again — a row that cannot persist twice is poisoned (dead FK), not
// transient. Event appends that fail are requeued (bounded) since our events
// reference no foreign keys and their failures are lock-transient.
func (c *Coordinator) persistPerItem(ctx context.Context, ledger map[ledgerKey]*ledgerDelta, incidents []database.CoordinatorIncident, events []database.EventOutboxAppend) {
	inOwnTx := func(fn func(tx *sqlx.Tx) error) error {
		tx, err := c.db.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	for key, delta := range ledger {
		key, delta := key, delta
		err := inOwnTx(func(tx *sqlx.Tx) error {
			return database.UpsertSessionFileActivityTx(tx, database.SessionFileActivityUpsert{
				SessionID: key.sessionID, ProjectID: delta.projectID, Path: key.path, Kind: key.kind,
				FirstTouchAt: delta.firstTs, LastTouchAt: delta.lastTs, TouchCount: delta.count,
				LastTool: delta.lastTool, Source: "hook",
			})
		})
		if err != nil {
			log.Printf("[Coordinator] dropping unpersistable ledger row (session=%s path=%s): %v", key.sessionID, key.path, err)
		}
	}
	for _, inc := range incidents {
		inc := inc
		err := inOwnTx(func(tx *sqlx.Tx) error { return database.UpsertCoordinatorIncidentTx(tx, inc) })
		if err != nil {
			log.Printf("[Coordinator] dropping unpersistable incident %s: %v", inc.ID, err)
		}
	}
	var failedEvents []database.EventOutboxAppend
	for _, ev := range events {
		ev := ev
		err := inOwnTx(func(tx *sqlx.Tx) error {
			_, appendErr := database.AppendEventOutbox(ctx, tx, ev)
			return appendErr
		})
		if err != nil {
			failedEvents = append(failedEvents, ev)
		}
	}
	if len(failedEvents) > 0 {
		log.Printf("[Coordinator] requeueing %d event(s) after per-item flush", len(failedEvents))
		c.mu.Lock()
		merged := append(failedEvents, c.pendingEvents...)
		if len(merged) > maxPendingEvents {
			c.droppedEvents += int64(len(merged) - maxPendingEvents)
			merged = merged[len(merged)-maxPendingEvents:]
		}
		c.pendingEvents = merged
		c.mu.Unlock()
	}
}

func conflictDetectedEvent(inc Incident, ts time.Time) database.EventOutboxAppend {
	payload, _ := json.Marshal(map[string]interface{}{
		"incident_id": inc.ID,
		"rule":        inc.Rule,
		"severity":    inc.Severity,
		"project_id":  inc.ProjectID,
		"scope_key":   inc.ScopeKey,
		"sessions":    inc.Sessions,
	})
	return database.EventOutboxAppend{
		EventID:       uuid.NewString(),
		EventType:     "conflict.detected",
		AggregateType: "conflict",
		AggregateID:   inc.ID,
		Actor:         "coordinator",
		SchemaVersion: 1,
		PayloadJSON:   string(payload),
		OccurredAt:    ts.UTC(),
	}
}

func awaitingInputEvent(sessionID, kind, excerpt string, ts time.Time) database.EventOutboxAppend {
	payload, _ := json.Marshal(map[string]interface{}{
		"session_id": sessionID,
		"kind":       kind,
		"excerpt":    excerpt,
	})
	return database.EventOutboxAppend{
		EventID:       uuid.NewString(),
		EventType:     "session.awaiting_input",
		AggregateType: "session",
		AggregateID:   sessionID,
		Actor:         "coordinator",
		SchemaVersion: 1,
		PayloadJSON:   string(payload),
		OccurredAt:    ts.UTC(),
	}
}

func turnCompletedEvent(sessionID string, projectID int64, files []string, ts time.Time) database.EventOutboxAppend {
	if files == nil {
		files = []string{}
	}
	payload, _ := json.Marshal(map[string]interface{}{"session_id": sessionID, "project_id": projectID, "files_touched": files})
	return database.EventOutboxAppend{
		EventID:       uuid.NewString(),
		EventType:     "session.turn_completed",
		AggregateType: "session",
		AggregateID:   sessionID,
		Actor:         "coordinator",
		SchemaVersion: 1,
		PayloadJSON:   string(payload),
		OccurredAt:    ts.UTC(),
	}
}
