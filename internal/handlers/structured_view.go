package handlers

import (
	"context"
	"log"
	"net/http"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/jsonlview"
	"openpoet/internal/websocket"
	"os"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// StructuredViewHandler manages JSONL file watchers for the structured view.
type StructuredViewHandler struct {
	db          *database.DB
	hub         *websocket.Hub
	decryptFunc func(string, string) (string, error)

	mu                    sync.Mutex
	watchers              map[string]*watcherEntry // sessionID → watcher
	nextWatcherGeneration uint64
}

type watcherEntry struct {
	stopFunc   func()
	generation uint64
}

// jsonlSource describes where the JSONL file for a session lives.
type jsonlSource struct {
	isRemote  bool
	localPath string            // set when isRemote=false
	project   *database.Project // set when isRemote=true
	sessionID string
}

func NewStructuredViewHandler(db *database.DB, hub *websocket.Hub, decryptFunc func(string, string) (string, error)) *StructuredViewHandler {
	return &StructuredViewHandler{
		db:          db,
		hub:         hub,
		decryptFunc: decryptFunc,
		watchers:    make(map[string]*watcherEntry),
	}
}

// GetSessionEvents returns parsed JSONL events for a session's structured view.
func (h *StructuredViewHandler) GetSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	source, reason := h.resolveJSONLSource(r, sessionID)
	if reason != "" {
		respondJSON(w, http.StatusOK, map[string]any{
			"events": []any{},
			"reason": reason,
		})
		return
	}

	var events []*jsonlview.SessionEvent
	var err error

	if source.isRemote {
		events, err = h.readRemoteEvents(source)
	} else {
		events, err = jsonlview.ParseFile(source.localPath)
	}

	if err != nil {
		log.Printf("[StructuredView] Error reading events for session %s: %v", sessionID, err)
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

	source, reason := h.resolveJSONLSource(r, sessionID)
	if reason != "" {
		respondJSON(w, http.StatusOK, map[string]any{
			"status": "unavailable",
			"reason": reason,
		})
		return
	}

	if source.isRemote {
		h.startRemoteWatching(sessionID, source)
	} else {
		h.startLocalWatching(sessionID, source.localPath)
	}

	log.Printf("[StructuredView] Started watching session %s (remote=%v)", sessionID, source.isRemote)
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

// HandleSessionSourceChange follows a Claude Code transcript switch (notably
// /resume) without requiring the user to close or reopen Structured View. If a
// watcher is active, the replacement watcher replays the resumed JSONL from the
// beginning after telling connected clients to reset their rendered timeline.
func (h *StructuredViewHandler) HandleSessionSourceChange(sessionID, providerSessionID string) {
	source, reason := h.resolveJSONLSourceContext(context.Background(), sessionID)
	if reason != "" {
		return
	}

	wasWatching := h.stopWatcher(sessionID)
	broadcastReset := func(replaying bool) {
		h.hub.BroadcastToChannel("session:"+sessionID, &websocket.Message{
			Type: websocket.MsgTypeStructuredViewSourceChanged,
			Data: map[string]interface{}{
				"session_id":          sessionID,
				"provider_session_id": providerSessionID,
				"replaying":           replaying,
			},
		})
	}

	if !wasWatching {
		// A client may have loaded the view while watching was unavailable. It
		// can refetch the now-correct source when it receives this notification.
		broadcastReset(false)
		return
	}

	var started bool
	if source.isRemote {
		started = h.startRemoteWatchingAtOffset(sessionID, source, 0, func() { broadcastReset(true) })
	} else {
		started = h.startLocalWatchingAtOffset(sessionID, source.localPath, 0, func() { broadcastReset(true) })
	}
	if !started {
		broadcastReset(false)
		return
	}
	log.Printf("[StructuredView] Switched session %s to Claude transcript %s", sessionID, source.sessionID)
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

func (h *StructuredViewHandler) stopWatcher(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry, exists := h.watchers[sessionID]; exists {
		entry.stopFunc()
		delete(h.watchers, sessionID)
		log.Printf("[StructuredView] Stopped watching session %s", sessionID)
		return true
	}
	return false
}

// resolveJSONLSource resolves where the JSONL file lives for a session.
// Returns (source, "") on success, or (nil, reason) on failure.
func (h *StructuredViewHandler) resolveJSONLSource(r *http.Request, sessionID string) (*jsonlSource, string) {
	return h.resolveJSONLSourceContext(r.Context(), sessionID)
}

func (h *StructuredViewHandler) resolveJSONLSourceContext(ctx context.Context, sessionID string) (*jsonlSource, string) {
	sess, err := h.db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, "not_found"
	}

	project, err := h.db.GetProject(ctx, sess.ProjectID)
	if err != nil {
		return nil, "not_found"
	}

	if sess.Backend == "copilot" || sess.Backend == "codex" || sess.Backend == "opencode" {
		return nil, "unsupported_backend"
	}
	providerSessionID := strings.TrimSpace(sess.ProviderSessionID)
	if providerSessionID == "" {
		providerSessionID = sessionID
	}

	if project.Type == "local" {
		return &jsonlSource{
			isRemote:  false,
			localPath: jsonlview.ResolveJSONLPath(project.Path, providerSessionID),
			sessionID: providerSessionID,
		}, ""
	}

	// Remote project
	return &jsonlSource{
		isRemote:  true,
		project:   project,
		sessionID: providerSessionID,
	}, ""
}

// readRemoteEvents reads and parses the full JSONL file from a remote host via SFTP.
func (h *StructuredViewHandler) readRemoteEvents(source *jsonlSource) ([]*jsonlview.SessionEvent, error) {
	fm := files.NewRemoteFileManager(source.project, h.decryptFunc)
	connector := fm.NewSFTPConnector()

	sshClient, sftpClient, err := connector.Connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	homeDir, err := sftpClient.Getwd()
	if err != nil {
		return nil, err
	}

	remotePath := jsonlview.ResolveRemoteJSONLPath(source.project.Path, source.sessionID, homeDir)

	file, err := sftpClient.Open(remotePath)
	if err != nil {
		return nil, nil // File doesn't exist yet
	}
	defer file.Close()

	return jsonlview.ParseReader(file)
}

// startLocalWatching starts a local file watcher for a session.
func (h *StructuredViewHandler) startLocalWatching(sessionID, jsonlPath string) {
	var offset int64
	if info, err := os.Stat(jsonlPath); err == nil {
		offset = info.Size()
	}
	h.startLocalWatchingAtOffset(sessionID, jsonlPath, offset, nil)
}

func (h *StructuredViewHandler) startLocalWatchingAtOffset(sessionID, jsonlPath string, offset int64, beforeStart func()) bool {
	watcher := jsonlview.NewWatcher(jsonlPath, offset)
	stopCh := make(chan struct{})
	generation := h.registerWatcher(sessionID, &watcherEntry{
		stopFunc: func() {
			watcher.Stop()
			close(stopCh)
		},
	})
	if beforeStart != nil {
		beforeStart()
	}
	go h.forwardEvents(sessionID, generation, watcher.Events(), stopCh)
	watcher.Start()
	return true
}

// startRemoteWatching starts an SFTP-based remote watcher for a session.
func (h *StructuredViewHandler) startRemoteWatching(sessionID string, source *jsonlSource) {
	h.startRemoteWatchingAtOffset(sessionID, source, -1, nil)
}

func (h *StructuredViewHandler) startRemoteWatchingAtOffset(sessionID string, source *jsonlSource, requestedOffset int64, beforeStart func()) bool {
	fm := files.NewRemoteFileManager(source.project, h.decryptFunc)
	connector := fm.NewSFTPConnector()

	// Resolve remote JSONL path (need home dir from a one-shot connection)
	sshClient, sftpClient, err := connector.Connect()
	if err != nil {
		log.Printf("[StructuredView] Failed to connect for remote session %s: %v", sessionID, err)
		return false
	}

	homeDir, err := sftpClient.Getwd()
	if err != nil {
		sftpClient.Close()
		sshClient.Close()
		log.Printf("[StructuredView] Failed to get remote home dir for session %s: %v", sessionID, err)
		return false
	}

	remotePath := jsonlview.ResolveRemoteJSONLPath(source.project.Path, source.sessionID, homeDir)

	// Get initial file size for offset
	offset := requestedOffset
	if offset < 0 {
		offset = 0
		if info, err := sftpClient.Stat(remotePath); err == nil {
			offset = info.Size()
		}
	}

	sftpClient.Close()
	sshClient.Close()

	rw := jsonlview.NewRemoteWatcher(connector, remotePath, offset)
	stopCh := make(chan struct{})
	generation := h.registerWatcher(sessionID, &watcherEntry{
		stopFunc: func() {
			rw.Stop()
			close(stopCh)
		},
	})
	if beforeStart != nil {
		beforeStart()
	}
	go h.forwardEvents(sessionID, generation, rw.Events(), stopCh)
	rw.Start()
	return true
}

// forwardEvents reads events from either watcher type and broadcasts them via WebSocket.
func (h *StructuredViewHandler) forwardEvents(sessionID string, generation uint64, eventsCh <-chan []*jsonlview.SessionEvent, stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case events, ok := <-eventsCh:
			if !ok {
				return
			}
			for _, event := range events {
				if !h.isCurrentWatcher(sessionID, generation) {
					return
				}
				h.hub.BroadcastToChannel("session:"+sessionID, &websocket.Message{
					Type: websocket.MsgTypeSessionEvent,
					Data: event,
				})
			}
		}
	}
}

func (h *StructuredViewHandler) registerWatcher(sessionID string, entry *watcherEntry) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextWatcherGeneration++
	entry.generation = h.nextWatcherGeneration
	h.watchers[sessionID] = entry
	return entry.generation
}

func (h *StructuredViewHandler) isCurrentWatcher(sessionID string, generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.watchers[sessionID]
	return ok && entry.generation == generation
}
