package automation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"openpoet/internal/database"
)

const (
	defaultEventConsumer = "events"
	defaultEventPageSize = 100
	maxEventPageSize     = 500
)

var eventConsumerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

type EventStore interface {
	ReadEventOutboxPage(ctx context.Context, afterSequence int64, limit int) ([]database.EventOutboxEvent, int64, error)
	GetEventOutboxConsumerCursor(ctx context.Context, automationClientID, consumer string) (*database.EventOutboxConsumerCursor, error)
	AckEventOutbox(ctx context.Context, automationClientID, consumer string, sequence int64) (*database.EventOutboxConsumerCursor, error)
}

type eventEntityRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type eventActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type automationEvent struct {
	Sequence      int64          `json:"sequence"`
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	Aggregate     eventEntityRef `json:"aggregate"`
	Actor         eventActor     `json:"actor"`
	OccurredAt    time.Time      `json:"occurred_at"`
	SchemaVersion int            `json:"schema_version"`
	Payload       any            `json:"payload"`
}

type eventPage struct {
	Events     []automationEvent `json:"events"`
	NextCursor *string           `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

type eventAckRequest struct {
	Through  string `json:"through"`
	Consumer string `json:"consumer,omitempty"`
}

type eventAckResponse struct {
	Acknowledged string `json:"acknowledged"`
}

func (a *commandAPI) listEvents(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.events == nil {
		writeError(w, http.StatusServiceUnavailable, "event_transport_unavailable", "the event transport is unavailable", true)
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	consumer, err := parseEventConsumer(r.URL.Query().Get("consumer"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "event_consumer_invalid", err.Error(), false)
		return
	}
	registered, err := a.events.GetEventOutboxConsumerCursor(r.Context(), actor.ClientID, consumer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "event_consumer_failed", "the event consumer could not be registered", true)
		return
	}
	after := registered.CursorSequence
	if rawAfter := strings.TrimSpace(r.URL.Query().Get("after")); rawAfter != "" {
		after, err = parseEventCursor(rawAfter)
		if err != nil {
			writeError(w, http.StatusBadRequest, "event_cursor_invalid", err.Error(), false)
			return
		}
	}
	limit := defaultEventPageSize
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		limit, err = strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 || limit > maxEventPageSize {
			writeError(w, http.StatusBadRequest, "event_limit_invalid", "limit must be between 1 and 500", false)
			return
		}
	}
	storedEvents, _, err := a.events.ReadEventOutboxPage(r.Context(), after, limit+1)
	if err != nil {
		if errors.Is(err, database.ErrEventOutboxCursorAhead) {
			writeError(w, http.StatusConflict, "event_cursor_ahead", "after is ahead of the current event cursor", false)
			return
		}
		writeError(w, http.StatusInternalServerError, "event_page_failed", "events could not be loaded", true)
		return
	}
	hasMore := len(storedEvents) > limit
	if hasMore {
		storedEvents = storedEvents[:limit]
	}
	// Honor the client's project_filter on the POLL path too — not only on the
	// SSE push path — so a scoped client cannot read another group's events by
	// simply choosing GET /events over GET /events/stream.
	scope := resolveActorProjectScope(r.Context(), a.scopeStore, actor)
	events := make([]automationEvent, 0, len(storedEvents))
	for _, stored := range storedEvents {
		event, err := mapAutomationEvent(stored)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "event_payload_invalid", "a persisted event payload is invalid", false)
			return
		}
		if !scope.Allows(eventProjectID(event)) {
			continue
		}
		events = append(events, event)
	}
	// The cursor advances past ALL rows read (even filtered ones) so a scoped
	// client never re-scans events it is not allowed to see.
	var nextCursor *string
	if len(storedEvents) > 0 {
		value := strconv.FormatInt(storedEvents[len(storedEvents)-1].Sequence, 10)
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, eventPage{Events: events, NextCursor: nextCursor, HasMore: hasMore})
}

func (a *commandAPI) ackEvents(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.events == nil {
		writeError(w, http.StatusServiceUnavailable, "event_transport_unavailable", "the event transport is unavailable", true)
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input eventAckRequest
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "event_ack_invalid", "through is required as a decimal cursor", false)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "event_ack_invalid", "the event acknowledgement body is invalid", false)
		return
	}
	queryConsumer := strings.TrimSpace(r.URL.Query().Get("consumer"))
	if queryConsumer != "" && strings.TrimSpace(input.Consumer) != "" && queryConsumer != strings.TrimSpace(input.Consumer) {
		writeError(w, http.StatusBadRequest, "event_consumer_conflict", "consumer in query and body must match", false)
		return
	}
	consumerValue := input.Consumer
	if queryConsumer != "" {
		consumerValue = queryConsumer
	}
	consumer, err := parseEventConsumer(consumerValue)
	if err != nil {
		writeError(w, http.StatusBadRequest, "event_consumer_invalid", err.Error(), false)
		return
	}
	through, err := parseEventCursor(input.Through)
	if err != nil {
		writeError(w, http.StatusBadRequest, "event_cursor_invalid", err.Error(), false)
		return
	}
	cursor, err := a.events.AckEventOutbox(r.Context(), actor.ClientID, consumer, through)
	if err != nil {
		if errors.Is(err, database.ErrEventOutboxCursorAhead) {
			writeError(w, http.StatusConflict, "event_cursor_ahead", "through is ahead of the current event cursor", false)
			return
		}
		writeError(w, http.StatusInternalServerError, "event_ack_failed", "the event cursor could not be acknowledged", true)
		return
	}
	writeJSON(w, http.StatusOK, eventAckResponse{Acknowledged: strconv.FormatInt(cursor.CursorSequence, 10)})
}

func mapAutomationEvent(stored database.EventOutboxEvent) (automationEvent, error) {
	var payload any
	if err := json.Unmarshal([]byte(stored.PayloadJSON), &payload); err != nil {
		return automationEvent{}, err
	}
	actorType, actorID, found := strings.Cut(stored.Actor, ":")
	if !found {
		actorType = stored.Actor
		actorID = ""
	}
	return automationEvent{
		Sequence: stored.Sequence, EventID: stored.EventID, EventType: stored.EventType,
		Aggregate: eventEntityRef{Type: stored.AggregateType, ID: stored.AggregateID},
		Actor:     eventActor{Type: actorType, ID: actorID}, OccurredAt: stored.OccurredAt,
		SchemaVersion: stored.SchemaVersion, Payload: payload,
	}, nil
}

func parseEventConsumer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultEventConsumer
	}
	if !eventConsumerPattern.MatchString(value) {
		return "", errors.New("consumer must match [A-Za-z0-9][A-Za-z0-9._:-]{0,63}")
	}
	return value, nil
}

func parseEventCursor(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("cursor must be a non-negative decimal string")
	}
	cursor, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("cursor must be a non-negative decimal string")
	}
	return cursor, nil
}
