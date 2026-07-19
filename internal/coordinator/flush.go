package coordinator

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"

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
		log.Printf("[Coordinator] flush failed, requeueing: %v", err)
		c.requeue(ledger, incidents, events)
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

func (c *Coordinator) requeue(ledger map[ledgerKey]*ledgerDelta, incidents []database.CoordinatorIncident, events []database.EventOutboxAppend) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, delta := range ledger {
		if cur, ok := c.dirtyLedger[key]; ok {
			cur.count += delta.count
			if delta.firstTs.Before(cur.firstTs) {
				cur.firstTs = delta.firstTs
			}
		} else {
			c.dirtyLedger[key] = delta
		}
	}
	for _, inc := range incidents {
		key := inc.Rule + "|" + inc.ScopeKey
		if _, ok := c.incidents[key]; ok {
			c.dirtyIncidents[key] = struct{}{}
		}
	}
	c.pendingEvents = append(events, c.pendingEvents...)
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

func turnCompletedEvent(sessionID string, ts time.Time) database.EventOutboxAppend {
	payload, _ := json.Marshal(map[string]interface{}{"session_id": sessionID})
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
