package mcp

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPHandler serves the MCP Streamable HTTP transport at /mcp.
type HTTPHandler struct {
	apiURL    string
	getAPIKey func() string
	getPolicy func() ToolPolicy

	mu       sync.RWMutex
	sessions map[string]*httpSession
}

type httpSession struct {
	id       string
	handler  *RequestHandler
	lastUsed time.Time
}

// NewHTTPHandler creates a new MCP HTTP transport handler.
func NewHTTPHandler(apiURL string, getAPIKey func() string, getPolicy func() ToolPolicy) *HTTPHandler {
	h := &HTTPHandler{
		apiURL:    apiURL,
		getAPIKey: getAPIKey,
		getPolicy: getPolicy,
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

	// Handle notifications (no response needed)
	if req.ID == nil || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
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

		h.mu.Lock()
		h.sessions[sessID] = &httpSession{
			id:       sessID,
			handler:  handler,
			lastUsed: time.Now(),
		}
		h.mu.Unlock()

		resp := handler.Handle(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sessID)
		json.NewEncoder(w).Encode(resp)
		return
	}

	// For other methods, look up existing session or create ad-hoc handler
	sessID := r.Header.Get("Mcp-Session-Id")
	if sessID != "" {
		h.mu.RLock()
		sess, ok := h.sessions[sessID]
		h.mu.RUnlock()
		if ok {
			sess.lastUsed = time.Now()
			handler = sess.handler
		}
	}

	if handler == nil {
		// No session — create a stateless handler
		policy := h.getPolicy()
		client := NewAPIClient(h.apiURL)
		handler = NewRequestHandler(client, "", "http", "", policy)
	}

	resp := handler.Handle(&req)

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

	h.mu.Lock()
	delete(h.sessions, sessID)
	h.mu.Unlock()

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
		for id, sess := range h.sessions {
			if now.Sub(sess.lastUsed) > 30*time.Minute {
				delete(h.sessions, id)
				log.Printf("[mcp-http] cleaned up stale session %s", id[:8])
			}
		}
		h.mu.Unlock()
	}
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
