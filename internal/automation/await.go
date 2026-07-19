package automation

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/database"
)

// OutboxWaiter lets the long-poll handlers block until the next outbox commit
// instead of busy-polling. *database.DB implements it.
type OutboxWaiter interface {
	OutboxWait() <-chan struct{}
}

// SessionStateStore reads a session row for wait_for_session.
type SessionStateStore interface {
	GetSession(ctx context.Context, id string) (*database.Session, error)
}

const (
	awaitMaxTimeout     = 60 * time.Second
	awaitDefaultTimeout = 20 * time.Second
	awaitScanLimit      = 500
)

func parseAwaitTimeout(raw string) time.Duration {
	if raw == "" {
		return awaitDefaultTimeout
	}
	secs, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || secs <= 0 {
		return awaitDefaultTimeout
	}
	d := time.Duration(secs) * time.Second
	if d > awaitMaxTimeout {
		return awaitMaxTimeout
	}
	return d
}

// awaitEvents is an HTTP long-poll over the outbox: it returns as soon as an
// event (optionally filtered by an event_type prefix) exists past the cursor,
// woken by the commit signal — not by polling. On timeout it returns an empty
// page with the unchanged cursor so the caller can retry.
func (a *commandAPI) awaitEvents(w http.ResponseWriter, r *http.Request) {
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
	filter := strings.TrimSpace(r.URL.Query().Get("filter"))
	timeout := parseAwaitTimeout(r.URL.Query().Get("timeout"))
	deadline := time.After(timeout)

	for {
		// Take the wake channel BEFORE reading, so a commit racing the read is
		// never missed (we re-read after waking).
		var wake <-chan struct{}
		if a.waiter != nil {
			wake = a.waiter.OutboxWait()
		}
		matched, next, err := a.scanFiltered(r.Context(), after, filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "event_page_failed", "events could not be loaded", true)
			return
		}
		if len(matched) > 0 {
			cursor := strconv.FormatInt(next, 10)
			writeJSON(w, http.StatusOK, eventPage{Events: matched, NextCursor: &cursor, HasMore: false})
			return
		}
		if next > after {
			after = next // advance past scanned-but-unmatched rows
		}
		if wake == nil {
			// No notifier wired: degrade to a short poll interval.
			wake = timeAfterChan(250 * time.Millisecond)
		}
		select {
		case <-wake:
		case <-deadline:
			cursor := strconv.FormatInt(after, 10)
			writeJSON(w, http.StatusOK, eventPage{Events: []automationEvent{}, NextCursor: &cursor, HasMore: false})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// scanFiltered reads forward from `after`, applying the event_type prefix
// filter client-side, and returns the matches plus the highest sequence
// scanned (so the caller can advance past unmatched rows).
func (a *commandAPI) scanFiltered(ctx context.Context, after int64, filter string) ([]automationEvent, int64, error) {
	stored, _, err := a.events.ReadEventOutboxPage(ctx, after, awaitScanLimit)
	if err != nil {
		return nil, after, err
	}
	highest := after
	var matched []automationEvent
	for _, ev := range stored {
		if ev.Sequence > highest {
			highest = ev.Sequence
		}
		if filter != "" && !strings.HasPrefix(ev.EventType, filter) {
			continue
		}
		mapped, mapErr := mapAutomationEvent(ev)
		if mapErr != nil {
			continue
		}
		matched = append(matched, mapped)
	}
	return matched, highest, nil
}

func timeAfterChan(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() { time.Sleep(d); close(ch) }()
	return ch
}

// waitForSession blocks until the session settles (turn_completed /
// awaiting_input) or the timeout, then reports its state and how confident that
// signal is: `exact` when derived from a coordinator turn/attention event or a
// terminal status, `heuristic` when it fell back to the polled runtime status.
func (a *commandAPI) waitForSession(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session_transport_unavailable", "session state is unavailable", true)
		return
	}
	if _, ok := ActorFromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id_required", "session id is required", false)
		return
	}
	sess, err := a.sessions.GetSession(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeError(w, http.StatusNotFound, "session_not_found", "session not found", false)
		return
	}
	if state, terminal := terminalSessionState(sess.Status); terminal {
		writeJSON(w, http.StatusOK, sessionWaitResponse{SessionID: sessionID, State: state, SignalQuality: "exact"})
		return
	}
	timeout := parseAwaitTimeout(r.URL.Query().Get("timeout"))
	deadline := time.After(timeout)
	// Baseline cursor: only events AFTER the call count as "settled now".
	_, after, err := a.scanFiltered(r.Context(), 0, "")
	if err != nil {
		after = 0
	}
	for {
		var wake <-chan struct{}
		if a.waiter != nil {
			wake = a.waiter.OutboxWait()
		}
		matched, next, scanErr := a.scanFiltered(r.Context(), after, "session.")
		if scanErr == nil {
			for _, ev := range matched {
				if ev.Aggregate.ID != sessionID {
					continue
				}
				switch ev.EventType {
				case "session.turn_completed":
					writeJSON(w, http.StatusOK, sessionWaitResponse{SessionID: sessionID, State: "turn_complete", SignalQuality: "exact"})
					return
				case "session.awaiting_input":
					writeJSON(w, http.StatusOK, sessionWaitResponse{SessionID: sessionID, State: "awaiting_input", SignalQuality: "exact"})
					return
				}
			}
			if next > after {
				after = next
			}
		}
		if wake == nil {
			wake = timeAfterChan(250 * time.Millisecond)
		}
		select {
		case <-wake:
		case <-deadline:
			// No settle signal in time: fall back to the polled status.
			cur, _ := a.sessions.GetSession(r.Context(), sessionID)
			state := "running"
			quality := "heuristic"
			if cur != nil {
				if ts, terminal := terminalSessionState(cur.Status); terminal {
					state, quality = ts, "exact"
				}
			}
			writeJSON(w, http.StatusOK, sessionWaitResponse{SessionID: sessionID, State: state, SignalQuality: quality})
			return
		case <-r.Context().Done():
			return
		}
	}
}

type sessionWaitResponse struct {
	SessionID     string `json:"session_id"`
	State         string `json:"state"`
	SignalQuality string `json:"signal_quality"`
}

func terminalSessionState(status string) (string, bool) {
	switch status {
	case "stopped", "completed", "error":
		return "stopped", true
	}
	return "", false
}
