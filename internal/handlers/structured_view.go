package handlers

import (
	"log"
	"net/http"
	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/websocket"
	"os"
	"sync"

	"github.com/go-chi/chi/v5"
)

// StructuredViewHandler manages JSONL file watchers for the structured view.
type StructuredViewHandler struct {
	db  *database.DB
	hub *websocket.Hub

	mu       sync.Mutex
	watchers map[string]*watcherEntry // sessionID → watcher
}

type watcherEntry struct {
	watcher  *jsonlview.Watcher
	stopFunc func()
}

func NewStructuredViewHandler(db *database.DB, hub *websocket.Hub) *StructuredViewHandler {
	return &StructuredViewHandler{
		db:       db,
		hub:      hub,
		watchers: make(map[string]*watcherEntry),
	}
}

// GetSessionEvents returns parsed JSONL events for a session's structured view.
func (h *StructuredViewHandler) GetSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	jsonlPath, reason := h.resolveJSONLPath(r, sessionID)
	if reason != "" {
		respondJSON(w, http.StatusOK, map[string]any{
			"events": []any{},
			"reason": reason,
		})
		return
	}

	events, err := jsonlview.ParseFile(jsonlPath)
	if err != nil {
		respondJSON(w, http.StatusOK, []any{})
		return
	}
	if events == nil {
		events = []*jsonlview.SessionEvent{}
	}

	respondJSON(w, http.StatusOK, events)
}

// StartWatching begins streaming new JSONL events for a session via WebSocket.
func (h *StructuredViewHandler) StartWatching(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	h.mu.Lock()
	if _, exists := h.watchers[sessionID]; exists {
		h.mu.Unlock()
		respondJSON(w, http.StatusOK, map[string]string{"status": "already_watching"})
		return
	}
	h.mu.Unlock()

	jsonlPath, reason := h.resolveJSONLPath(r, sessionID)
	if reason != "" {
		respondJSON(w, http.StatusOK, map[string]any{
			"status": "unavailable",
			"reason": reason,
		})
		return
	}

	// Get file size for offset (start watching from end, since initial load already fetched history)
	var offset int64
	if info, err := os.Stat(jsonlPath); err == nil {
		offset = info.Size()
	}

	watcher := jsonlview.NewWatcher(jsonlPath, offset)
	watcher.Start()

	// Goroutine to forward events from watcher to WebSocket hub
	stopCh := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case events, ok := <-watcher.Events():
				if !ok {
					return
				}
				for _, event := range events {
					h.hub.BroadcastToChannel("session:"+sessionID, &websocket.Message{
						Type: websocket.MsgTypeSessionEvent,
						Data: event,
					})
				}
			}
		}
	}()

	h.mu.Lock()
	h.watchers[sessionID] = &watcherEntry{
		watcher: watcher,
		stopFunc: func() {
			watcher.Stop()
			close(stopCh)
		},
	}
	h.mu.Unlock()

	log.Printf("[StructuredView] Started watching session %s", sessionID)
	respondJSON(w, http.StatusOK, map[string]string{"status": "watching"})
}

// StopWatching stops the JSONL file watcher for a session.
func (h *StructuredViewHandler) StopWatching(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	h.stopWatcher(sessionID)
	respondJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// StopSessionWatcher stops the watcher for a specific session (called on session end).
func (h *StructuredViewHandler) StopSessionWatcher(sessionID string) {
	h.stopWatcher(sessionID)
}

// StopAllWatchers stops all active watchers (called on shutdown).
func (h *StructuredViewHandler) StopAllWatchers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sid, entry := range h.watchers {
		entry.stopFunc()
		delete(h.watchers, sid)
	}
}

func (h *StructuredViewHandler) stopWatcher(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry, exists := h.watchers[sessionID]; exists {
		entry.stopFunc()
		delete(h.watchers, sessionID)
		log.Printf("[StructuredView] Stopped watching session %s", sessionID)
	}
}

// resolveJSONLPath resolves the JSONL file path for a session.
// Returns (path, "") on success, or ("", reason) on failure.
func (h *StructuredViewHandler) resolveJSONLPath(r *http.Request, sessionID string) (string, string) {
	sess, err := h.db.GetSession(r.Context(), sessionID)
	if err != nil {
		return "", "not_found"
	}

	project, err := h.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		return "", "not_found"
	}

	if project.Type != "local" {
		return "", "remote"
	}
	if sess.Backend == "copilot" {
		return "", "unsupported_backend"
	}

	return jsonlview.ResolveJSONLPath(project.Path, sessionID), ""
}
