package handlers

import (
	"log"
	"net/http"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/jsonlview"
	"openpoet/internal/websocket"
	"os"
	"sync"

	"github.com/go-chi/chi/v5"
)

// StructuredViewHandler manages JSONL file watchers for the structured view.
type StructuredViewHandler struct {
	db          *database.DB
	hub         *websocket.Hub
	decryptFunc func(string, string) (string, error)

	mu       sync.Mutex
	watchers map[string]*watcherEntry // sessionID → watcher
}

type watcherEntry struct {
	stopFunc func()
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

// resolveJSONLSource resolves where the JSONL file lives for a session.
// Returns (source, "") on success, or (nil, reason) on failure.
func (h *StructuredViewHandler) resolveJSONLSource(r *http.Request, sessionID string) (*jsonlSource, string) {
	sess, err := h.db.GetSession(r.Context(), sessionID)
	if err != nil {
		return nil, "not_found"
	}

	project, err := h.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		return nil, "not_found"
	}

	if sess.Backend == "copilot" || sess.Backend == "codex" {
		return nil, "unsupported_backend"
	}

	if project.Type == "local" {
		return &jsonlSource{
			isRemote:  false,
			localPath: jsonlview.ResolveJSONLPath(project.Path, sessionID),
			sessionID: sessionID,
		}, ""
	}

	// Remote project
	return &jsonlSource{
		isRemote:  true,
		project:   project,
		sessionID: sessionID,
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

	watcher := jsonlview.NewWatcher(jsonlPath, offset)
	watcher.Start()

	stopCh := make(chan struct{})
	go h.forwardEvents(sessionID, watcher.Events(), stopCh)

	h.mu.Lock()
	h.watchers[sessionID] = &watcherEntry{
		stopFunc: func() {
			watcher.Stop()
			close(stopCh)
		},
	}
	h.mu.Unlock()
}

// startRemoteWatching starts an SFTP-based remote watcher for a session.
func (h *StructuredViewHandler) startRemoteWatching(sessionID string, source *jsonlSource) {
	fm := files.NewRemoteFileManager(source.project, h.decryptFunc)
	connector := fm.NewSFTPConnector()

	// Resolve remote JSONL path (need home dir from a one-shot connection)
	sshClient, sftpClient, err := connector.Connect()
	if err != nil {
		log.Printf("[StructuredView] Failed to connect for remote session %s: %v", sessionID, err)
		return
	}

	homeDir, err := sftpClient.Getwd()
	if err != nil {
		sftpClient.Close()
		sshClient.Close()
		log.Printf("[StructuredView] Failed to get remote home dir for session %s: %v", sessionID, err)
		return
	}

	remotePath := jsonlview.ResolveRemoteJSONLPath(source.project.Path, source.sessionID, homeDir)

	// Get initial file size for offset
	var offset int64
	if info, err := sftpClient.Stat(remotePath); err == nil {
		offset = info.Size()
	}

	sftpClient.Close()
	sshClient.Close()

	rw := jsonlview.NewRemoteWatcher(connector, remotePath, offset)
	rw.Start()

	stopCh := make(chan struct{})
	go h.forwardEvents(sessionID, rw.Events(), stopCh)

	h.mu.Lock()
	h.watchers[sessionID] = &watcherEntry{
		stopFunc: func() {
			rw.Stop()
			close(stopCh)
		},
	}
	h.mu.Unlock()
}

// forwardEvents reads events from either watcher type and broadcasts them via WebSocket.
func (h *StructuredViewHandler) forwardEvents(sessionID string, eventsCh <-chan []*jsonlview.SessionEvent, stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case events, ok := <-eventsCh:
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
}
