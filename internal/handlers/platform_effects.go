package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/websocket"
)

// platformEffects is deliberately best-effort. Application services invoke it
// only after their mutation succeeds, so an unavailable websocket or audit
// sink must never turn a committed mutation into a reported failure.
type platformEffects struct {
	api *API
	db  *database.DB
	hub *websocket.Hub
}

func (e *platformEffects) PublishApplicationChange(ctx context.Context, change application.ApplicationChange) {
	e.publish(ctx, change.Domain, change.Action, numericAggregateID(change.ID), application.Actor{})
}

func (e *platformEffects) PublishProjectChange(change application.ProjectChange) {
	e.publish(context.Background(), "project", change.Action, numericAggregateID(change.ID), application.Actor{})
}

func (e *platformEffects) PublishProjectOperation(ctx context.Context, change application.ProjectOperationChange) {
	e.publish(ctx, "project_operation", change.Action, numericAggregateID(change.ProjectID), change.Actor)
}

func (e *platformEffects) PublishSessionChange(ctx context.Context, change application.SessionChange) {
	e.publish(ctx, "session", change.Action, change.ID, change.Actor)
}

func (e *platformEffects) PublishSessionWatcherChange(ctx context.Context, change application.SessionWatcherChange) {
	e.publish(ctx, "session_watcher", change.Action, change.SessionID, change.Actor)
}

func (e *platformEffects) RecordSessionTaskSuggestion(ctx context.Context, entry application.SessionTaskSuggestionAuditEntry) {
	// Only bounded metadata crosses this boundary. The terminal context, prompt,
	// provider output and provider error are intentionally not retained.
	e.publish(ctx, "session_task_suggestion", entry.Event, entry.SessionID, entry.Actor)
}

func (e *platformEffects) PublishFileMutation(ctx context.Context, change application.FileMutationChange) {
	id := numericAggregateID(change.ProjectID)
	if strings.TrimSpace(change.SessionID) != "" {
		id = change.SessionID
	}
	e.publish(ctx, "file", change.Action, id, change.Actor)
}

func (e *platformEffects) PublishGitMutation(ctx context.Context, change application.GitMutationChange) {
	e.publish(ctx, "git", change.Action, numericAggregateID(change.ProjectID), change.Actor)
}

func (e *platformEffects) PublishHookResponse(ctx context.Context, change application.HookResponseChange) {
	e.publish(ctx, "hook_response", change.Action, change.SessionID, change.Actor)
}

func (e *platformEffects) PublishTunnelMutation(ctx context.Context, change application.TunnelMutationChange) {
	e.publish(ctx, "tunnel", change.Action, nonEmptyAggregateID(change.DeviceID), change.Actor)
}

func (e *platformEffects) PublishUpdateMutation(ctx context.Context, change application.UpdateMutationChange) {
	if e.api != nil {
		// Preserve the existing post-update restart semantics. PublishUpdateMutation
		// itself is asynchronous and cannot falsify the successful update result.
		e.api.PublishUpdateMutation(ctx, change)
	}
	e.audit(ctx, "update", change.Action, nonEmptyAggregateID(change.Version), change.Actor)
}

func (e *platformEffects) publish(ctx context.Context, domain, action, aggregateID string, actor application.Actor) {
	domain = boundedEffectName(domain, "platform")
	action = boundedEffectName(action, "changed")
	aggregateID = nonEmptyAggregateID(aggregateID)
	if e.hub != nil {
		// Do not forward application Meta or mutation payloads: they may contain
		// credentials. Consumers receive a refresh signal with bounded identity.
		e.hub.BroadcastStateUpdate(domain, map[string]string{
			"action": action,
			"id":     aggregateID,
		})
	}
	e.audit(ctx, domain, action, aggregateID, actor)
}

func (e *platformEffects) audit(ctx context.Context, domain, action, aggregateID string, actor application.Actor) {
	if e.db == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	metadata := application.EventMetadataFromContext(ctx)
	if strings.TrimSpace(actor.Type) == "" && strings.TrimSpace(actor.ID) == "" {
		actor = metadata.Actor
	}
	payload, _ := json.Marshal(map[string]string{"action": action})
	tx, err := e.db.BeginTxx(ctx, nil)
	if err != nil {
		log.Printf("[Automation] platform audit unavailable domain=%s action=%s: %v", domain, action, err)
		return
	}
	defer tx.Rollback()
	_, err = database.AppendEventOutbox(ctx, tx, database.EventOutboxAppend{
		EventID:       uuid.NewString(),
		EventType:     "platform." + domain + "." + action,
		AggregateType: domain,
		AggregateID:   aggregateID,
		Actor:         application.EventActorValue(actor),
		CorrelationID: strings.TrimSpace(metadata.CorrelationID),
		SchemaVersion: 1,
		PayloadJSON:   string(payload),
		OccurredAt:    time.Now().UTC(),
	})
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.Printf("[Automation] platform audit append failed domain=%s action=%s: %v", domain, action, err)
	}
}

func numericAggregateID(id int64) string {
	if id <= 0 {
		return "global"
	}
	return strconv.FormatInt(id, 10)
}

func nonEmptyAggregateID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "global"
	}
	if len(id) > 256 {
		return id[:256]
	}
	return id
}

func boundedEffectName(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	if len(value) > 80 {
		return value[:80]
	}
	return value
}
