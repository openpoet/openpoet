package automation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// streamEvents is the Server-Sent Events push of the outbox (Phase 5). It is the
// same durable, cursor-resumable feed as GET /events, but pushed: it holds the
// connection open, woken by the outbox commit signal (never polling), and emits
// one `id:`/`event:`/`data:` frame per event. Resumption is native — a client
// reconnecting with `Last-Event-ID: <sequence>` resumes from that cursor, and
// because the scan is strictly `sequence > after`, the last-seen frame is never
// re-delivered. A client project_filter is honored on this PUSH path too, so a
// scoped client never receives another group's events.
func (a *commandAPI) streamEvents(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.events == nil {
		writeError(w, http.StatusServiceUnavailable, "event_transport_unavailable", "the event transport is unavailable", true)
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", "streaming is not supported by the server", false)
		return
	}

	// Resume cursor precedence: Last-Event-ID header → ?after= → registered
	// consumer cursor. Mirrors listEvents so a poller and a streamer agree.
	consumer, err := parseEventConsumer(r.URL.Query().Get("consumer"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "event_consumer_invalid", err.Error(), false)
		return
	}
	after := int64(0)
	if reg, regErr := a.events.GetEventOutboxConsumerCursor(r.Context(), actor.ClientID, consumer); regErr == nil {
		after = reg.CursorSequence
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		if v, perr := parseEventCursor(raw); perr == nil {
			after = v
		}
	}
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if v, perr := parseEventCursor(raw); perr == nil {
			after = v // resume exactly past the last frame the client saw
		}
	}
	filter := strings.TrimSpace(r.URL.Query().Get("filter"))
	scope := resolveActorProjectScope(r.Context(), a.scopeStore, actor)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // defeat proxy/compress buffering so frames flush
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": ok\n\n") // open the stream immediately (first frame)
	flusher.Flush()

	// A server-side lifetime cap so an idle stream cannot pin a goroutine forever;
	// clients reconnect with Last-Event-ID. Heartbeats keep proxies from timing out.
	lifetime := time.After(10 * time.Minute)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		var wake <-chan struct{}
		if a.waiter != nil {
			wake = a.waiter.OutboxWait()
		}
		matched, next, caughtUp, scanErr := a.scanUntilMatchOrHead(r.Context(), after, filter)
		if scanErr == nil {
			after = next
			wrote := false
			for _, ev := range matched {
				if !scope.Allows(eventProjectID(ev)) {
					continue // filter honored on the push path (never leak other groups)
				}
				writeSSEEvent(w, ev)
				wrote = true
			}
			if wrote {
				flusher.Flush()
			}
		}
		// Keep draining a backlog before parking — but ONLY on a clean scan. On a
		// scan error caughtUp is false; a bare `continue` there would busy-loop at
		// 100% CPU and never check the context/lifetime. Fall through to the select
		// so the error path backs off and honors cancellation.
		if scanErr == nil && !caughtUp {
			continue
		}
		if wake == nil {
			wake = timeAfterChan(250 * time.Millisecond)
		}
		select {
		case <-wake:
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-lifetime:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// writeSSEEvent renders one outbox event as an SSE frame. The `id:` line carries
// the outbox sequence — the exact value a client echoes back as Last-Event-ID.
func writeSSEEvent(w http.ResponseWriter, ev automationEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\n", ev.Sequence)
	fmt.Fprintf(w, "event: %s\n", ev.EventType)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// filterEventsByScope drops events outside the actor's project_filter. Used by
// the long-poll and page endpoints so every event surface honors the filter, not
// just the SSE push path.
func filterEventsByScope(events []automationEvent, scope *ProjectScopeSet) []automationEvent {
	if scope == nil || scope.Unrestricted {
		return events
	}
	out := make([]automationEvent, 0, len(events))
	for _, ev := range events {
		if scope.Allows(eventProjectID(ev)) {
			out = append(out, ev)
		}
	}
	return out
}

// eventProjectID extracts the project id from an event payload (0 when absent),
// so project_filter can be applied on the push path.
func eventProjectID(ev automationEvent) int64 {
	m, ok := ev.Payload.(map[string]any)
	if !ok {
		return 0
	}
	switch v := m["project_id"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}
