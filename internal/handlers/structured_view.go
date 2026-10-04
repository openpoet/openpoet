package handlers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/jsonlview"
	"openpoet/internal/websocket"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// StructuredViewHandler manages JSONL file watchers for the structured view.
type StructuredViewHandler struct {
	db                   *database.DB
	hub                  *websocket.Hub
	decryptFunc          func(string, string) (string, error)
	recordEffectiveModel func(context.Context, string, string) error

	mu                    sync.Mutex
	watchers              map[string]*watcherEntry // sessionID → watcher
	nextWatcherGeneration uint64

	// endedTranscripts caches the parsed transcripts of ended remote
	// sessions: they no longer change, and each read otherwise costs an SSH
	// connection plus the whole file.
	transcriptMu     sync.Mutex
	endedTranscripts []endedTranscript
}

type endedTranscript struct {
	key    string
	events []*jsonlview.SessionEvent
}

const (
	// remoteTranscriptTimeout bounds one remote transcript read, so a slow or
	// unreachable host fails the caller in time instead of holding it for minutes.
	remoteTranscriptTimeout = 20 * time.Second
	maxEndedTranscripts     = 4
)

func (h *StructuredViewHandler) SetEffectiveModelRecorder(recorder func(context.Context, string, string) error) {
	h.recordEffectiveModel = recorder
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
	// endedKey identifies the finished transcript of an ended session (empty
	// while the session can still write to it).
	endedKey string
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
		events, err = h.readRemoteEvents(r.Context(), source)
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
	h.recordLatestEffectiveModel(r.Context(), sessionID, events)

	respondJSON(w, http.StatusOK, events)
}

// ReconcileEffectiveModel reads the trusted transcript source for a session and
// records the latest main-agent assistant model. It is invoked after Stop hooks
// so /model changes are reflected even when Structured View is closed.
func (h *StructuredViewHandler) ReconcileEffectiveModel(ctx context.Context, sessionID string) {
	if h.recordEffectiveModel == nil {
		return
	}
	source, reason := h.resolveJSONLSourceContext(ctx, sessionID)
	if reason != "" {
		return
	}
	events, err := h.readRecentSessionEvents(source)
	if err != nil {
		log.Printf("[StructuredView] Failed to reconcile model for session %s: %v", sessionID, err)
		return
	}
	h.recordLatestEffectiveModel(ctx, sessionID, events)
}

const effectiveModelTranscriptTailBytes int64 = 4 * 1024 * 1024

func (h *StructuredViewHandler) readRecentSessionEvents(source *jsonlSource) ([]*jsonlview.SessionEvent, error) {
	if !source.isRemote {
		file, err := os.Open(source.localPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		return parseRecentSessionEvents(file, info.Size())
	}

	fm := files.NewRemoteFileManager(source.project, h.decryptFunc)
	connector := fm.NewSFTPConnector()
	sshClient, sftpClient, err := connector.Connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()
	remotePath, err := remoteJSONLPath(sshClient, sftpClient, source)
	if err != nil {
		return nil, err
	}
	file, err := sftpClient.Open(remotePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return parseRecentSessionEvents(file, info.Size())
}

func parseRecentSessionEvents(reader io.ReadSeeker, size int64) ([]*jsonlview.SessionEvent, error) {
	offset := size - effectiveModelTranscriptTailBytes
	if offset < 0 {
		offset = 0
	}
	events, _, err := jsonlview.ParseReaderFromOffset(reader, size, offset)
	return events, err
}

func (h *StructuredViewHandler) recordLatestEffectiveModel(ctx context.Context, sessionID string, events []*jsonlview.SessionEvent) {
	if h.recordEffectiveModel == nil {
		return
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event == nil || event.IsSidechain || event.Type != "assistant" || event.Message == nil || strings.TrimSpace(event.Message.Model) == "" {
			continue
		}
		if err := h.recordEffectiveModel(ctx, sessionID, event.Message.Model); err != nil {
			log.Printf("[StructuredView] Failed to persist effective model for session %s: %v", sessionID, err)
		}
		return
	}
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
		// Workspace sessions run in a lane, and Claude Code keys transcript
		// dirs by cwd — resolve against the dir the session actually ran in.
		transcriptRoot := project.Path
		if sess.WorkDir != "" {
			transcriptRoot = sess.WorkDir
		}
		return &jsonlSource{
			isRemote:  false,
			localPath: jsonlview.ResolveJSONLPath(transcriptRoot, providerSessionID),
			sessionID: providerSessionID,
		}, ""
	}

	// Remote project
	source := &jsonlSource{
		isRemote:  true,
		project:   project,
		sessionID: providerSessionID,
	}
	if sess.EndTime.Valid && sess.Status != "running" && sess.Status != "starting" {
		source.endedKey = sessionID + "|" + providerSessionID + "|" + strconv.FormatInt(sess.EndTime.Time.UnixNano(), 10)
	}
	return source, ""
}

// ReadSessionTranscript parses a session's complete transcript, locally or
// over SFTP. reason is set (not_found, unsupported_backend) when the session
// has no transcript to read; a transcript not written yet yields no events.
func (h *StructuredViewHandler) ReadSessionTranscript(ctx context.Context, sessionID string) ([]*jsonlview.SessionEvent, string, error) {
	source, reason := h.resolveJSONLSourceContext(ctx, sessionID)
	if reason == "unsupported_backend" {
		if sess, err := h.db.GetSession(ctx, sessionID); err == nil && sess.Backend == "codex" {
			return h.readCodexTranscript(ctx, sess)
		}
	}
	if reason != "" {
		return nil, reason, nil
	}
	if !source.isRemote {
		events, err := jsonlview.ParseFile(source.localPath)
		return events, "", err
	}
	if events, ok := h.cachedEndedTranscript(source.endedKey); ok {
		return events, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, remoteTranscriptTimeout)
	defer cancel()
	type read struct {
		events []*jsonlview.SessionEvent
		err    error
	}
	done := make(chan read, 1)
	go func() {
		events, err := h.readRemoteEvents(ctx, source)
		done <- read{events, err}
	}()
	select {
	case result := <-done:
		if result.err == nil && result.events != nil {
			h.cacheEndedTranscript(source.endedKey, result.events)
		}
		return result.events, "", result.err
	case <-ctx.Done():
		// The SSH dial cannot be interrupted; readRemoteEvents closes the
		// connection as soon as it is made.
		return nil, "", fmt.Errorf("reading the remote session transcript: %w", ctx.Err())
	}
}

func (h *StructuredViewHandler) cachedEndedTranscript(key string) ([]*jsonlview.SessionEvent, bool) {
	if key == "" {
		return nil, false
	}
	h.transcriptMu.Lock()
	defer h.transcriptMu.Unlock()
	for _, entry := range h.endedTranscripts {
		if entry.key == key {
			return entry.events, true
		}
	}
	return nil, false
}

func (h *StructuredViewHandler) cacheEndedTranscript(key string, events []*jsonlview.SessionEvent) {
	if key == "" {
		return
	}
	h.transcriptMu.Lock()
	defer h.transcriptMu.Unlock()
	for _, entry := range h.endedTranscripts {
		if entry.key == key {
			return
		}
	}
	if len(h.endedTranscripts) >= maxEndedTranscripts {
		h.endedTranscripts = h.endedTranscripts[1:]
	}
	h.endedTranscripts = append(h.endedTranscripts, endedTranscript{key: key, events: events})
}

// readRemoteEvents reads and parses the full JSONL file from a remote host via
// SFTP. Canceling ctx closes the connection, aborting the transfer.
func (h *StructuredViewHandler) readRemoteEvents(ctx context.Context, source *jsonlSource) ([]*jsonlview.SessionEvent, error) {
	fm := files.NewRemoteFileManager(source.project, h.decryptFunc)
	connector := fm.NewSFTPConnector()

	sshClient, sftpClient, err := connector.Connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()
	stop := context.AfterFunc(ctx, func() { _ = sshClient.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	remotePath, err := remoteJSONLPath(sshClient, sftpClient, source)
	if err != nil {
		return nil, err
	}

	file, err := sftpClient.Open(remotePath)
	if err != nil {
		log.Printf("[StructuredView] No transcript yet for session %s at %s: %v", source.sessionID, remotePath, err)
		return nil, nil // File doesn't exist yet
	}
	defer file.Close()

	events, err := jsonlview.ParseReader(file)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr // a cut-off transfer is never a complete transcript
	}
	return events, err
}

// remoteJSONLPath resolves the session transcript path on the remote host,
// under the Claude config root that SSH sessions there actually use.
func remoteJSONLPath(sshClient *ssh.Client, sftpClient *sftp.Client, source *jsonlSource) (string, error) {
	homeDir, err := sftpClient.Getwd()
	if err != nil {
		return "", err
	}
	configDir := jsonlview.ClaudeConfigDir(homeDir, remoteClaudeConfigDirEnv(sshClient))
	return jsonlview.ResolveRemoteJSONLPath(source.project.Path, source.sessionID, configDir), nil
}

const remoteConfigDirMarker = "__OPENPOET_CLAUDE_CONFIG_DIR__"

// remoteClaudeConfigDirEnv reads CLAUDE_CONFIG_DIR as a login shell on the
// remote host sees it (/etc/environment and /etc/profile.d both apply), since
// that is the environment the Claude session was launched with. Returns ""
// when unset or when the host has no POSIX shell (e.g. Windows).
func remoteClaudeConfigDirEnv(sshClient *ssh.Client) string {
	sess, err := sshClient.NewSession()
	if err != nil {
		return ""
	}
	defer sess.Close()
	out, err := sess.Output(`sh -lc 'printf "\n` + remoteConfigDirMarker + `%s" "$CLAUDE_CONFIG_DIR"'`)
	if err != nil {
		return ""
	}
	// Profile scripts may print noise; the value follows the last marker.
	idx := strings.LastIndex(string(out), remoteConfigDirMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(string(out[idx+len(remoteConfigDirMarker):]))
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

	remotePath, err := remoteJSONLPath(sshClient, sftpClient, source)
	if err != nil {
		sftpClient.Close()
		sshClient.Close()
		log.Printf("[StructuredView] Failed to resolve remote transcript for session %s: %v", sessionID, err)
		return false
	}

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
				h.recordLatestEffectiveModel(context.Background(), sessionID, []*jsonlview.SessionEvent{event})
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
