package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"openpoet/internal/database"
)

const (
	ProjectTaskEventCreated              = "project_task.created"
	ProjectTaskEventUpdated              = "project_task.updated"
	ProjectTaskEventStatusChanged        = "project_task.status_changed"
	ProjectTaskEventDeleted              = "project_task.deleted"
	ProjectTaskEventDuplicated           = "project_task.duplicated"
	ProjectTaskEventReordered            = "project_task.reordered"
	ProjectTasksEventReorderedGlobal     = "project_tasks.reordered_global"
	ProjectTaskEventVerificationApproved = "project_task.verification_approved"
	ProjectTaskEventVerificationRejected = "project_task.verification_rejected"
	ProjectTaskEventSessionLinked        = "project_task.session_linked"
	ProjectTaskEventSessionUnlinked      = "project_task.session_unlinked"
	ProjectTaskEventCommentAdded         = "project_task.comment_added"
)

type ProjectTaskEvent struct {
	EventType     string
	AggregateType string
	AggregateID   string
	Actor         Actor
	CorrelationID string
	Payload       map[string]any
}

type ProjectTaskEventPublisher interface {
	PublishProjectTaskEvent(ctx context.Context, tx *database.ProjectTaskTx, event ProjectTaskEvent) error
}

type TransactionalProjectTaskEventPublisher struct{}

func NewTransactionalProjectTaskEventPublisher() *TransactionalProjectTaskEventPublisher {
	return &TransactionalProjectTaskEventPublisher{}
}

func (p *TransactionalProjectTaskEventPublisher) PublishProjectTaskEvent(
	ctx context.Context,
	tx *database.ProjectTaskTx,
	event ProjectTaskEvent,
) error {
	payload := event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.AppendEventOutbox(ctx, database.EventOutboxAppend{
		EventID:       uuid.NewString(),
		EventType:     event.EventType,
		AggregateType: event.AggregateType,
		AggregateID:   event.AggregateID,
		Actor:         event.Actor.eventValue(),
		CorrelationID: strings.TrimSpace(event.CorrelationID),
		SchemaVersion: 1,
		PayloadJSON:   string(payloadJSON),
		OccurredAt:    time.Now().UTC(),
	})
	return err
}

type EventMetadata struct {
	Actor         Actor
	CorrelationID string
}

type eventMetadataContextKey struct{}

// WithEventMetadata lets request adapters identify the automation actor and
// correlation without widening service method signatures that predate the API.
func WithEventMetadata(ctx context.Context, metadata EventMetadata) context.Context {
	return context.WithValue(ctx, eventMetadataContextKey{}, metadata)
}

func eventMetadataFromContext(ctx context.Context) EventMetadata {
	metadata, _ := ctx.Value(eventMetadataContextKey{}).(EventMetadata)
	return metadata
}

func (a Actor) eventValue() string {
	actorType := strings.TrimSpace(a.Type)
	if actorType == "" {
		actorType = "system"
	}
	actorID := strings.TrimSpace(a.ID)
	if actorID == "" {
		return actorType
	}
	return actorType + ":" + actorID
}
