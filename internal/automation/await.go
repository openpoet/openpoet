package automation

import (
	"context"
	"errors"
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
	// Honor project_filter on the long-poll path too (parity with GET /events and
	// the SSE stream): a scoped client is never woken by another group's event.
	scope := resolveActorProjectScope(r.Context(), a.scopeStore, actor)

	for {
		// Take the wake channel BEFORE reading, so a commit racing the read is
		// never missed (we re-read after waking).
		var wake <-chan struct{}
		if a.waiter != nil {
			wake = a.waiter.OutboxWait()
		}
		matched, next, caughtUp, err := a.scanUntilMatchOrHead(r.Context(), after, filter)
		if err != nil {
			if errors.Is(err, database.ErrEventOutboxCursorAhead) {
				writeError(w, http.StatusConflict, "event_cursor_ahead", "after is ahead of the current event cursor", false)
				return
			}
			writeError(w, http.StatusInternalServerError, "event_page_failed", "events could not be loaded", true)
			return
		}
		after = next
		matched = filterEventsByScope(matched, scope)
		if len(matched) > 0 {
			cursor := strconv.FormatInt(next, 10)
			writeJSON(w, http.StatusOK, eventPage{Events: matched, NextCursor: &cursor, HasMore: false})
			return
		}
		// Only park once we have scanned all the way to the committed head:
		// otherwise an already-committed matching event beyond the 500-row
		// window would wait a full timeout for a notify that may never come.
		if !caughtUp {
			continue
		}
		if wake == nil {
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

// scanUntilMatchOrHead pages forward from `after`, applying the prefix filter,
// until it either finds matches or reaches the committed head-of-outbox. It
// returns the matches, the new cursor, and whether the scan is caught up to
// head. Bounded by maxAwaitScanPages so a huge backlog can't spin a request
// forever — a not-caught-up return then asks the caller to continue.
func (a *commandAPI) scanUntilMatchOrHead(ctx context.Context, after int64, filter string) ([]automationEvent, int64, bool, error) {
	for page := 0; page < maxAwaitScanPages; page++ {
		stored, head, err := a.events.ReadEventOutboxPage(ctx, after, awaitScanLimit)
		if err != nil {
			return nil, after, false, err
		}
		var matched []automationEvent
		for _, ev := range stored {
			if ev.Sequence > after {
				after = ev.Sequence
			}
			if filter != "" && !strings.HasPrefix(ev.EventType, filter) {
				continue
			}
			if mapped, mapErr := mapAutomationEvent(ev); mapErr == nil {
				matched = append(matched, mapped)
			}
		}
		if len(matched) > 0 {
			return matched, after, after >= head, nil
		}
		if after >= head || len(stored) < awaitScanLimit {
			return nil, maxInt64(after, head), true, nil
		}
	}
	return nil, after, false, nil // more backlog to scan; caller continues
}

const maxAwaitScanPages = 40 // 40 * 500 = 20k rows per request before yielding

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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
	// Baseline = the committed head-of-outbox at call time, so only events that
	// arrive AFTER the call count as "settled now". Reading the true head (not
	// the 500th-oldest row) is what stops a stale historic turn_completed from
	// masquerading as a fresh settle.
	after, err := a.outboxHead(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "event_page_failed", "events could not be loaded", true)
		return
	}
	for {
		var wake <-chan struct{}
		if a.waiter != nil {
			wake = a.waiter.OutboxWait()
		}
		matched, next, _, scanErr := a.scanUntilMatchOrHead(r.Context(), after, "session.")
		if scanErr == nil {
			after = next
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
		}
		// A session that stops mid-wait emits no outbox event — re-check the
		// row on every wake so we report 'stopped' promptly, not at timeout.
		if cur, _ := a.sessions.GetSession(r.Context(), sessionID); cur != nil {
			if ts, terminal := terminalSessionState(cur.Status); terminal {
				writeJSON(w, http.StatusOK, sessionWaitResponse{SessionID: sessionID, State: ts, SignalQuality: "exact"})
				return
			}
		}
		if wake == nil {
			wake = timeAfterChan(250 * time.Millisecond)
		}
		select {
		case <-wake:
		case <-deadline:
			writeJSON(w, http.StatusOK, sessionWaitResponse{SessionID: sessionID, State: "running", SignalQuality: "heuristic"})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// outboxHead returns the highest committed sequence, paging past the scan-limit
// so a large backlog does not cap the baseline at the 500th-oldest row.
func (a *commandAPI) outboxHead(ctx context.Context) (int64, error) {
	after := int64(0)
	for page := 0; page < maxAwaitScanPages; page++ {
		stored, head, err := a.events.ReadEventOutboxPage(ctx, after, awaitScanLimit)
		if err != nil {
			return 0, err
		}
		if head > after {
			after = head
		}
		if len(stored) < awaitScanLimit || after >= head {
			return after, nil
		}
	}
	return after, nil
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
