package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// toolsChangedHub fans out a "the effective tool list may have changed" signal
// (a share, a project or a global tool policy was written) to every open MCP
// session served by this process. The signal carries no payload: each session
// recomputes its own list and tells its client only when that list differs.
type toolsChangedHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

var toolsChanged = &toolsChangedHub{subs: make(map[chan struct{}]struct{})}

// NotifyToolsChanged signals every subscriber. Never blocks: a subscriber that
// has not consumed the previous signal already has one pending.
func NotifyToolsChanged() {
	toolsChanged.mu.Lock()
	defer toolsChanged.mu.Unlock()
	for ch := range toolsChanged.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// subscribeToolsChanged returns a channel that receives a signal after each
// NotifyToolsChanged, and the function that unsubscribes it.
func subscribeToolsChanged() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	toolsChanged.mu.Lock()
	toolsChanged.subs[ch] = struct{}{}
	toolsChanged.mu.Unlock()
	return ch, func() {
		toolsChanged.mu.Lock()
		delete(toolsChanged.subs, ch)
		toolsChanged.mu.Unlock()
	}
}

// toolListChangedNotification is the MCP notification sent when a session's
// effective tool list changed; the client answers with a fresh tools/list.
var toolListChangedNotification = []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)

// ToolChangesStream serves GET /api/mcp/tool-changes: an SSE stream with one
// "data: tools_changed" frame per NotifyToolsChanged. The stdio transport runs
// in a separate process (mcp-serve), so it subscribes here to learn about
// changes made in the server.
func ToolChangesStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	signal, unsubscribe := subscribeToolsChanged()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("[MCP] tool-changes stream: clear read deadline: %v", err)
	}
	write := func(frame string) error {
		if err := rc.SetWriteDeadline(time.Now().Add(mcpStreamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := io.WriteString(w, frame); err != nil {
			return err
		}
		return rc.Flush()
	}

	w.WriteHeader(http.StatusOK)
	if err := write(": tool-changes stream open\n\n"); err != nil {
		return
	}
	ticker := time.NewTicker(mcpStreamPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-signal:
			if err := write("data: tools_changed\n\n"); err != nil {
				return
			}
		case <-ticker.C:
			if err := write(": ping\n\n"); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// Reconnect backoff of watchToolChanges.
var (
	toolChangesRetryMin = time.Second
	toolChangesRetryMax = 30 * time.Second
)

// watchToolChanges keeps a subscription to the server's tool-changes stream
// and calls onChange on every signal and on every (re)connection, since a
// change may have happened while the stream was down (e.g. a server restart).
// Runs until ctx is done.
func watchToolChanges(ctx context.Context, client *APIClient, onChange func()) {
	backoff := toolChangesRetryMin
	for {
		resp, err := client.Stream(ctx, "/api/mcp/tool-changes")
		if err == nil {
			backoff = toolChangesRetryMin
			onChange()
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "data:") {
					onChange()
				}
			}
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > toolChangesRetryMax {
			backoff = toolChangesRetryMax
		}
	}
}
