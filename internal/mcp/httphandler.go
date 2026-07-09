package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"openpoet/internal/database"
	"strings"
	"sync"
	"time"
)

// HTTPHandler serves the MCP Streamable HTTP transport at /mcp.
type HTTPHandler struct {
	apiURL    string
	getAPIKey func() string
	getPolicy func() ToolPolicy
	store     HTTPSessionStore

	mu       sync.RWMutex
	sessions map[string]*httpSession
}

type HTTPSessionStore interface {
	UpsertMCPHTTPSession(ctx context.Context, s *database.MCPHTTPSession) error
	GetMCPHTTPSession(ctx context.Context, id string) (*database.MCPHTTPSession, error)
	TouchMCPHTTPSession(ctx context.Context, id, method string) error
	CloseMCPHTTPSession(ctx context.Context, id, status string) error
	ExpireStaleMCPHTTPSessions(ctx context.Context, before time.Time) (int64, error)
	CleanupMCPHTTPSessions(ctx context.Context, closedBefore time.Time, eventBefore time.Time, maxEventsPerSession int) (database.MCPHTTPCleanupStats, error)
	CreateMCPHTTPSessionEvent(ctx context.Context, e *database.MCPHTTPSessionEvent) error
}

type httpSession struct {
	id                string
	openpoetSessionID string
	context           string
	handler           *RequestHandler
	lastUsed          time.Time
}

// NewHTTPHandler creates a new MCP HTTP transport handler.
func NewHTTPHandler(apiURL string, getAPIKey func() string, getPolicy func() ToolPolicy, store ...HTTPSessionStore) *HTTPHandler {
	var sessionStore HTTPSessionStore
	if len(store) > 0 {
		sessionStore = store[0]
	}
	h := &HTTPHandler{
		apiURL:    apiURL,
		getAPIKey: getAPIKey,
		getPolicy: getPolicy,
		store:     sessionStore,
		sessions:  make(map[string]*httpSession),
	}
	go h.cleanupLoop()
	return h
}

// ServeHTTP implements http.Handler for the MCP Streamable HTTP transport.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Auth check
	if !h.authenticate(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *HTTPHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":"invalid JSON-RPC"}`, http.StatusBadRequest)
		return
	}

	var handler *RequestHandler

	if req.Method == "initialize" {
		// Create new session
		sessID := generateSessionID()
		policy := h.getPolicy()
		client := NewAPIClient(h.apiURL)

		// If session_id is provided via query param, this is a remote Claude Code
		// session connecting through the reverse tunnel. Use "session" context so
		// the correct tool policy (mcp_tool_policy_session) is applied.
		openpoetSessionID := r.URL.Query().Get("session_id")
		mcpContext := "http"
		if openpoetSessionID != "" {
			mcpContext = "session"
		}
		handler = NewRequestHandler(client, openpoetSessionID, mcpContext, "", policy)

		now := time.Now()
		h.mu.Lock()
		h.sessions[sessID] = &httpSession{
			id:                sessID,
			openpoetSessionID: openpoetSessionID,
			context:           mcpContext,
			handler:           handler,
			lastUsed:          now,
		}
		h.mu.Unlock()
		h.persistSession(r.Context(), sessID, openpoetSessionID, mcpContext, "active", "initialize")
		h.touchSession(r.Context(), sessID, "initialize")
		h.recordEvent(r.Context(), sessID, openpoetSessionID, "initialize", "initialize", "ok", "")

		resp := handler.Handle(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sessID)
		json.NewEncoder(w).Encode(resp)
		return
	}

	// For other methods, look up existing session or create ad-hoc handler
	sessID := r.Header.Get("Mcp-Session-Id")
	openpoetSessionID := r.URL.Query().Get("session_id")
	mcpContext := "http"
	if openpoetSessionID != "" {
		mcpContext = "session"
	}
	if sessID != "" {
		h.mu.Lock()
		sess, ok := h.sessions[sessID]
		if ok {
			sess.lastUsed = time.Now()
			openpoetSessionID = sess.openpoetSessionID
			mcpContext = sess.context
			handler = sess.handler
		}
		h.mu.Unlock()
	}

	if handler == nil && sessID != "" {
		if restored := h.restoreSession(r.Context(), sessID, openpoetSessionID, mcpContext); restored != nil {
			openpoetSessionID = restored.openpoetSessionID
			mcpContext = restored.context
			handler = restored.handler
		}
	}

	if handler == nil {
		// Session expired or unknown — recover session_id from query parameter
		// (always present in the URL for remote Claude Code sessions via tunnel).
		policy := h.getPolicy()
		client := NewAPIClient(h.apiURL)
		handler = NewRequestHandler(client, openpoetSessionID, mcpContext, "", policy)
		if sessID != "" {
			now := time.Now()
			h.mu.Lock()
			h.sessions[sessID] = &httpSession{
				id:                sessID,
				openpoetSessionID: openpoetSessionID,
				context:           mcpContext,
				handler:           handler,
				lastUsed:          now,
			}
			h.mu.Unlock()
			h.persistSession(r.Context(), sessID, openpoetSessionID, mcpContext, "active", req.Method)
		}
	}

	// Handle notifications after session recovery so their activity is durable.
	if req.ID == nil || string(req.ID) == "null" {
		if sessID != "" {
			h.touchSession(r.Context(), sessID, req.Method)
			h.recordEvent(r.Context(), sessID, openpoetSessionID, req.Method, "notification", "accepted", "")
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := handler.Handle(&req)
	if sessID != "" {
		status := "ok"
		errMsg := ""
		if resp != nil && resp.Error != nil {
			status = "error"
			errMsg = resp.Error.Message
		}
		h.touchSession(r.Context(), sessID, req.Method)
		h.recordEvent(r.Context(), sessID, openpoetSessionID, req.Method, "request", status, errMsg)
	}

	w.Header().Set("Content-Type", "application/json")
	if sessID != "" {
		w.Header().Set("Mcp-Session-Id", sessID)
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *HTTPHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessID := r.Header.Get("Mcp-Session-Id")
	if sessID == "" {
		http.Error(w, `{"error":"Mcp-Session-Id header required"}`, http.StatusBadRequest)
		return
	}

	openpoetSessionID := h.openpoetSessionIDFor(r.Context(), sessID)
	h.mu.Lock()
	delete(h.sessions, sessID)
	h.mu.Unlock()
	h.closeSession(r.Context(), sessID, "closed")
	h.recordEvent(r.Context(), sessID, openpoetSessionID, "DELETE", "delete", "closed", "")

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"session_closed"}`))
}

func (h *HTTPHandler) authenticate(r *http.Request) bool {
	// Allow unauthenticated access from localhost
	if isLocalhost(r.RemoteAddr) {
		return true
	}

	key := h.getAPIKey()
	if key == "" {
		// No API key configured — reject all non-local requests
		return false
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1
}

func (h *HTTPHandler) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		now := time.Now()
		cutoff := now.Add(-4 * time.Hour)
		for id, sess := range h.sessions {
			if sess.lastUsed.Before(cutoff) {
				delete(h.sessions, id)
				h.closeSession(context.Background(), id, "expired")
				log.Printf("[mcp-http] cleaned up stale session %s", id[:8])
			}
		}
		h.mu.Unlock()
		if h.store != nil {
			if n, err := h.store.ExpireStaleMCPHTTPSessions(context.Background(), cutoff); err != nil {
				log.Printf("[mcp-http] failed to expire stale sessions: %v", err)
			} else if n > 0 {
				log.Printf("[mcp-http] marked %d stale persisted sessions as expired", n)
			}
			closedBefore := now.AddDate(0, 0, -database.DefaultMCPHTTPSessionRetentionDays)
			eventBefore := now.AddDate(0, 0, -database.DefaultMCPHTTPEventRetentionDays)
			stats, err := h.store.CleanupMCPHTTPSessions(context.Background(), closedBefore, eventBefore, database.DefaultMCPHTTPMaxEventsPerSession)
			if err != nil {
				log.Printf("[mcp-http] failed to clean up persisted sessions: %v", err)
			} else if stats.SessionsDeleted > 0 || stats.ExpiredDeleted > 0 || stats.OverflowDeleted > 0 {
				log.Printf("[mcp-http] cleanup removed sessions=%d expired_events=%d overflow_events=%d",
					stats.SessionsDeleted, stats.ExpiredDeleted, stats.OverflowDeleted)
			}
		}
	}
}

func (h *HTTPHandler) restoreSession(ctx context.Context, sessID, fallbackOpenPoetSessionID, fallbackContext string) *httpSession {
	if h.store == nil {
		return nil
	}
	persisted, err := h.store.GetMCPHTTPSession(ctx, sessID)
	if err != nil || persisted == nil || persisted.Status != "active" {
		return nil
	}
	openpoetSessionID := persisted.OpenPoetSessionID
	if openpoetSessionID == "" {
		openpoetSessionID = fallbackOpenPoetSessionID
	}
	mcpContext := persisted.Context
	if mcpContext == "" {
		mcpContext = fallbackContext
	}
	policy := h.getPolicy()
	client := NewAPIClient(h.apiURL)
	restored := &httpSession{
		id:                sessID,
		openpoetSessionID: openpoetSessionID,
		context:           mcpContext,
		handler:           NewRequestHandler(client, openpoetSessionID, mcpContext, "", policy),
		lastUsed:          time.Now(),
	}
	h.mu.Lock()
	h.sessions[sessID] = restored
	h.mu.Unlock()
	h.recordEvent(ctx, sessID, openpoetSessionID, "", "restore", "ok", "")
	return restored
}

func (h *HTTPHandler) openpoetSessionIDFor(ctx context.Context, sessID string) string {
	h.mu.RLock()
	if sess := h.sessions[sessID]; sess != nil {
		openpoetSessionID := sess.openpoetSessionID
		h.mu.RUnlock()
		return openpoetSessionID
	}
	h.mu.RUnlock()
	if h.store == nil {
		return ""
	}
	persisted, err := h.store.GetMCPHTTPSession(ctx, sessID)
	if err != nil || persisted == nil {
		return ""
	}
	return persisted.OpenPoetSessionID
}

func (h *HTTPHandler) persistSession(ctx context.Context, sessID, openpoetSessionID, mcpContext, status, method string) {
	if h.store == nil || sessID == "" {
		return
	}
	if err := h.store.UpsertMCPHTTPSession(ctx, &database.MCPHTTPSession{
		ID:                sessID,
		OpenPoetSessionID: openpoetSessionID,
		Context:           mcpContext,
		Status:            status,
		LastMethod:        method,
	}); err != nil {
		log.Printf("[mcp-http] failed to persist session %s: %v", shortSessionID(sessID), err)
	}
}

func (h *HTTPHandler) touchSession(ctx context.Context, sessID, method string) {
	if h.store == nil || sessID == "" {
		return
	}
	if err := h.store.TouchMCPHTTPSession(ctx, sessID, method); err != nil {
		log.Printf("[mcp-http] failed to touch session %s: %v", shortSessionID(sessID), err)
	}
}

func (h *HTTPHandler) closeSession(ctx context.Context, sessID, status string) {
	if h.store == nil || sessID == "" {
		return
	}
	if err := h.store.CloseMCPHTTPSession(ctx, sessID, status); err != nil {
		log.Printf("[mcp-http] failed to close session %s: %v", shortSessionID(sessID), err)
	}
}

func (h *HTTPHandler) recordEvent(ctx context.Context, sessID, openpoetSessionID, method, eventType, status, errMsg string) {
	if h.store == nil || sessID == "" {
		return
	}
	if err := h.store.CreateMCPHTTPSessionEvent(ctx, &database.MCPHTTPSessionEvent{
		MCPSessionID:      truncateMCPEventField(sessID, 128),
		OpenPoetSessionID: truncateMCPEventField(openpoetSessionID, 128),
		Method:            truncateMCPEventField(method, 128),
		EventType:         truncateMCPEventField(eventType, 64),
		Status:            truncateMCPEventField(status, 64),
		Error:             truncateMCPEventField(errMsg, 500),
	}); err != nil {
		log.Printf("[mcp-http] failed to record event for session %s: %v", shortSessionID(sessID), err)
	}
}

func truncateMCPEventField(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max]
}

func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func isLocalhost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
