package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"devmanager/internal/notifications"
	"devmanager/internal/session"
	"devmanager/internal/websocket"
)

// PermissionResponse is what the browser sends back when user clicks Allow/Deny/AllowAlways
type PermissionResponse struct {
	Behavior              string            `json:"behavior"` // "allow", "allowAlways", "deny", or "passthrough"
	Message               string            `json:"message,omitempty"`
	ToolName              string            `json:"tool_name,omitempty"`
	Answers               map[string]string `json:"answers,omitempty"`               // For AskUserQuestion responses
	PermissionSuggestions []interface{}     `json:"permission_suggestions,omitempty"` // From hook input, for allowAlways
}

// hookPermissionOutput is the envelope Claude Code expects from PermissionRequest hooks
type hookPermissionOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName string             `json:"hookEventName"`
	Decision      permissionDecision `json:"decision"`
}

type permissionDecision struct {
	Behavior           string                 `json:"behavior"`
	Message            string                 `json:"message,omitempty"`
	UpdatedInput       map[string]interface{} `json:"updatedInput,omitempty"`
	UpdatedPermissions []interface{}          `json:"updatedPermissions,omitempty"`
}

// pendingPermission tracks a blocking permission request
type pendingPermission struct {
	responseCh chan PermissionResponse
	cancel     context.CancelFunc
	hookEvent  map[string]interface{} // original event data for GET endpoint
	msgType    string                 // "permission", "askUser", "exitPlan"
	createdAt  time.Time
}

// toolEventEntry stores a buffered tool event for the GET endpoint
type toolEventEntry struct {
	Event     map[string]interface{} `json:"event"`
	MsgType   string                 `json:"msg_type"`
	Timestamp time.Time              `json:"timestamp"`
}

// HookHandler manages hook API endpoints
type HookHandler struct {
	hub          *websocket.Hub
	notifService *notifications.Service
	sessionMgr   *session.Manager

	mu            sync.Mutex
	pending       map[string]*pendingPermission  // sessionID -> pending permission
	alwaysAllow   map[string]map[string]bool     // sessionID -> toolName -> true
	toolEvents    map[string][]toolEventEntry    // sessionID -> recent tool events buffer
	lastPushSent  map[string]time.Time           // sessionID -> last push timestamp (for rate-limiting)
}

// NewHookHandler creates a new hook handler
func NewHookHandler(hub *websocket.Hub, notifService *notifications.Service, sessionMgr *session.Manager) *HookHandler {
	return &HookHandler{
		hub:          hub,
		notifService: notifService,
		sessionMgr:   sessionMgr,
		pending:      make(map[string]*pendingPermission),
		alwaysAllow:  make(map[string]map[string]bool),
		toolEvents:   make(map[string][]toolEventEntry),
		lastPushSent: make(map[string]time.Time),
	}
}

// HandlePermission handles POST /api/hooks/permission
// This blocks until the browser user responds or timeout
func (h *HookHandler) HandlePermission(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing X-Session-ID header")
		return
	}

	// Parse the hook event JSON
	var hookEvent map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&hookEvent); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	// Check tool name for auto-allow logic
	toolName, _ := hookEvent["tool_name"].(string)

	// Auto-allow safe tools that should never show a permission dialog
	safeTools := map[string]bool{
		"TodoRead":      true,
		"TodoWrite":     true,
		"TaskList":      true,
		"TaskGet":       true,
		"TaskCreate":    true,
		"TaskUpdate":    true,
		"EnterPlanMode": true,
	}
	if safeTools[toolName] {
		log.Printf("[hooks] Auto-allowing safe tool %s for session %s", toolName, sessionID)
		output := hookPermissionOutput{
			HookSpecificOutput: hookSpecificOutput{
				HookEventName: "PermissionRequest",
				Decision:      permissionDecision{Behavior: "allow"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(output)
		return
	}

	// Special tools with custom UI modals
	isAskUser := toolName == "AskUserQuestion"
	isExitPlan := toolName == "ExitPlanMode"

	// Check if this tool is "always allowed" for this session
	h.mu.Lock()
	if tools, ok := h.alwaysAllow[sessionID]; ok && toolName != "" && tools[toolName] {
		h.mu.Unlock()
		log.Printf("[hooks] Auto-allowing %s for session %s (always allow)", toolName, sessionID)
		output := hookPermissionOutput{
			HookSpecificOutput: hookSpecificOutput{
				HookEventName: "PermissionRequest",
				Decision:      permissionDecision{Behavior: "allow"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(output)
		return
	}
	h.mu.Unlock()

	// Create a context with timeout (590s, just under Claude Code's 600s default)
	ctx, cancel := context.WithTimeout(r.Context(), 590*time.Second)

	// Create response channel
	responseCh := make(chan PermissionResponse, 1)

	// Determine message type for the pending entry
	var pendingMsgType string
	if isAskUser {
		pendingMsgType = "askUser"
	} else if isExitPlan {
		pendingMsgType = "exitPlan"
	} else {
		pendingMsgType = "permission"
	}

	h.mu.Lock()
	// Cancel any existing pending permission for this session
	if existing, ok := h.pending[sessionID]; ok {
		existing.cancel()
	}
	h.pending[sessionID] = &pendingPermission{
		responseCh: responseCh,
		cancel:     cancel,
		hookEvent:  hookEvent,
		msgType:    pendingMsgType,
		createdAt:  time.Now(),
	}
	h.mu.Unlock()

	// Broadcast to browser via WebSocket
	if isAskUser {
		// Send questions UI instead of permission dialog
		h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeHookAskUser,
			Data: map[string]interface{}{
				"session_id": sessionID,
				"event":      hookEvent,
			},
		})

		// Send push notification for AskUser question
		if h.notifService != nil && h.shouldSendPush(sessionID, true) {
			log.Printf("[hooks] Sending push for AskUserQuestion on session %s", sessionID)
			go func() {
				if err := h.notifService.Send(context.Background(), sessionID, "question",
					"Question from Claude", "Claude needs your input"); err != nil {
					log.Printf("[hooks] Push failed for AskUserQuestion: %v", err)
				}
			}()
		}
	} else if isExitPlan {
		// Send plan approval UI
		h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeHookExitPlan,
			Data: map[string]interface{}{
				"session_id": sessionID,
				"event":      hookEvent,
			},
		})

		// Send push notification for plan approval
		if h.notifService != nil && h.shouldSendPush(sessionID, true) {
			log.Printf("[hooks] Sending push for ExitPlanMode on session %s", sessionID)
			go func() {
				if err := h.notifService.Send(context.Background(), sessionID, "permission",
					"Plan Ready", "Claude's plan needs your approval"); err != nil {
					log.Printf("[hooks] Push failed for ExitPlanMode: %v", err)
				}
			}()
		}
	} else {
		// Standard permission request dialog
		h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeHookPermission,
			Data: map[string]interface{}{
				"session_id": sessionID,
				"event":      hookEvent,
			},
		})

		// Send push notification for permission request
		if h.notifService != nil && h.shouldSendPush(sessionID, true) {
			notifToolName := "Tool"
			if tn, ok := hookEvent["tool_name"].(string); ok {
				notifToolName = tn
			}
			log.Printf("[hooks] Sending push for PermissionRequest (%s) on session %s", notifToolName, sessionID)
			go func() {
				if err := h.notifService.Send(context.Background(), sessionID, "permission",
					"Permission Request", notifToolName+" needs approval"); err != nil {
					log.Printf("[hooks] Push failed for PermissionRequest: %v", err)
				}
			}()
		}
	}

	// Block until response or timeout
	select {
	case resp := <-responseCh:
		// User responded
		cancel()
		h.mu.Lock()
		delete(h.pending, sessionID)
		h.mu.Unlock()

		// Cancel push notifications for this session
		if h.notifService != nil {
			go h.notifService.SendCancel(context.Background(), sessionID)
		}

		// Build decision based on user response
		decision := permissionDecision{}
		denyMessage := resp.Message

		if resp.Behavior == "passthrough" {
			// User dismissed the dialog — return empty so hook exits without output
			// and Claude Code falls through to its terminal prompt
			log.Printf("[hooks] Permission passthrough for session %s (user dismissed)", sessionID)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Handle ExitPlanMode: allow tool, then select plan approval option via terminal
		if isExitPlan && resp.Behavior == "allow" && resp.Message != "" {
			decision.Behavior = "allow"
			planChoice := resp.Message
			// Option 3 (manual): just approve, nothing else (default behavior).
			// Option 2 (auto-accept): approve, wait, Shift+Tab.
			// Option 1 (clear context + auto-accept): approve, dismiss dialog,
			//   /clear, paste plan content from event, Shift+Tab.
			if h.sessionMgr != nil && planChoice == "1" {
				// Get plan content from the hook event (tool_input.plan)
				planContent := ""
				if ti, ok := hookEvent["tool_input"].(map[string]interface{}); ok {
					if p, ok := ti["plan"].(string); ok {
						planContent = strings.TrimSpace(p)
					}
				}
				if planContent == "" {
					log.Printf("[hooks] Option 1: no plan content in hook event, falling back to option 2 (Shift+Tab only)")
					go func() {
						time.Sleep(3 * time.Second)
						h.sessionMgr.WriteToSession(sessionID, []byte("\x1b[Z"))
					}()
				} else {
					go func() {
						log.Printf("[hooks] Option 1: using plan content from event (%d bytes)", len(planContent))

						// Hook "allow" already approved the plan.
						// Wait for Claude to process.
						time.Sleep(5 * time.Second)

						// Escape to interrupt Claude if generating
						h.sessionMgr.WriteToSession(sessionID, []byte("\x1b"))
						time.Sleep(1 * time.Second)

						// /clear command
						h.sessionMgr.WriteToSession(sessionID, []byte("/clear"))
						time.Sleep(50 * time.Millisecond)
						h.sessionMgr.WriteToSession(sessionID, []byte("\r"))
						time.Sleep(3 * time.Second)

						// Paste plan content (bracketed paste preserves newlines)
						paste := "\x1b[200~" + planContent + "\x1b[201~"
						h.sessionMgr.WriteToSession(sessionID, []byte(paste))
						time.Sleep(1 * time.Second)
						// Submit with Enter (separate write, like voice auto-submit)
						h.sessionMgr.WriteToSession(sessionID, []byte("\r"))
						time.Sleep(3 * time.Second)

						// Shift+Tab for auto-accept mode
						h.sessionMgr.WriteToSession(sessionID, []byte("\x1b[Z"))
						log.Printf("[hooks] Option 1 complete: cleared context, sent plan, auto-accept for session %s", sessionID)
					}()
				}
			} else if h.sessionMgr != nil && planChoice == "2" {
				go func() {
					time.Sleep(3 * time.Second)
					h.sessionMgr.WriteToSession(sessionID, []byte("\x1b[Z"))
					log.Printf("[hooks] Sent Shift+Tab for plan option 2 to session %s", sessionID)
				}()
			}
		} else if isExitPlan && resp.Behavior == "allow" {
			// ExitPlanMode allow without specific choice
			decision.Behavior = "allow"
		} else if isAskUser && len(resp.Answers) > 0 {
			decision.Behavior = "allow"
			// Build updatedInput with original tool_input + answers
			toolInput, _ := hookEvent["tool_input"].(map[string]interface{})
			updatedInput := make(map[string]interface{})
			for k, v := range toolInput {
				updatedInput[k] = v
			}
			// Convert answers to interface{} map for JSON
			answersMap := make(map[string]interface{})
			for k, v := range resp.Answers {
				answersMap[k] = v
			}
			updatedInput["answers"] = answersMap
			decision.UpdatedInput = updatedInput
			log.Printf("[hooks] AskUserQuestion answered for session %s: %v", sessionID, resp.Answers)
		} else if resp.Behavior == "allowAlways" {
			decision.Behavior = "allow"
			// Pass permission suggestions as updatedPermissions to Claude Code
			if len(resp.PermissionSuggestions) > 0 {
				decision.UpdatedPermissions = resp.PermissionSuggestions
				log.Printf("[hooks] Including %d permission suggestions in updatedPermissions for session %s", len(resp.PermissionSuggestions), sessionID)
			}
			// Also store locally as fast-path for subsequent requests
			if resp.ToolName != "" {
				h.mu.Lock()
				if h.alwaysAllow[sessionID] == nil {
					h.alwaysAllow[sessionID] = make(map[string]bool)
				}
				h.alwaysAllow[sessionID][resp.ToolName] = true
				h.mu.Unlock()
				log.Printf("[hooks] Tool %s set to always-allow for session %s", resp.ToolName, sessionID)
			}
		} else {
			decision.Behavior = resp.Behavior
			// For deny, include the message so Claude knows why
			if resp.Behavior == "deny" && denyMessage != "" {
				decision.Message = denyMessage
				log.Printf("[hooks] Deny with message for session %s: %s", sessionID, denyMessage)
			}
		}

		// Return the response in Claude Code's expected envelope format
		output := hookPermissionOutput{
			HookSpecificOutput: hookSpecificOutput{
				HookEventName: "PermissionRequest",
				Decision:      decision,
			},
		}
		outputJSON, _ := json.Marshal(output)
		log.Printf("[hooks] Permission response for session %s: %s", sessionID, string(outputJSON))
		w.Header().Set("Content-Type", "application/json")
		w.Write(outputJSON)

	case <-ctx.Done():
		// Timeout or context cancelled
		h.mu.Lock()
		delete(h.pending, sessionID)
		h.mu.Unlock()
		cancel()

		// Broadcast resolved to browser so UI cleans up
		h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeHookPermissionResolved,
			Data: map[string]interface{}{
				"session_id": sessionID,
				"behavior":   "deny",
				"reason":     "timeout",
			},
		})

		// Cancel push notifications for this session
		if h.notifService != nil {
			go h.notifService.SendCancel(context.Background(), sessionID)
		}

		// Return deny on timeout in Claude Code's expected envelope format
		output := hookPermissionOutput{
			HookSpecificOutput: hookSpecificOutput{
				HookEventName: "PermissionRequest",
				Decision: permissionDecision{
					Behavior: "deny",
					Message:  "Permission request timed out",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(output)
	}
}

// HandlePermissionRespond handles POST /api/hooks/permission/{sessionId}/respond
// Called by the browser when user clicks Allow/Deny
func (h *HookHandler) HandlePermissionRespond(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing session ID")
		return
	}

	var resp PermissionResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	h.mu.Lock()
	pending, ok := h.pending[sessionID]
	h.mu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending permission request for this session")
		return
	}

	// Send the response to the blocking handler
	select {
	case pending.responseCh <- resp:
		log.Printf("Permission response sent for session %s: %s", sessionID, resp.Behavior)
	default:
		log.Printf("Permission response channel full for session %s", sessionID)
	}

	// Broadcast permission resolved to browser
	h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
		Type: websocket.MsgTypeHookPermissionResolved,
		Data: map[string]interface{}{
			"session_id": sessionID,
			"behavior":   resp.Behavior,
		},
	})

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleEvent handles POST /api/hooks/event
// Non-blocking: broadcasts hook events to the browser
func (h *HookHandler) HandleEvent(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing X-Session-ID header")
		return
	}

	var hookEvent map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&hookEvent); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	eventName, _ := hookEvent["hook_event_name"].(string)

	// Map hook event to the appropriate WebSocket message type
	var msgType websocket.MessageType
	switch eventName {
	case "PreToolUse":
		msgType = websocket.MsgTypeHookToolStart
	case "PostToolUse":
		msgType = websocket.MsgTypeHookToolDone
	case "PostToolUseFailure":
		msgType = websocket.MsgTypeHookToolFailed
	case "Notification":
		msgType = websocket.MsgTypeHookNotify
	case "Stop":
		msgType = websocket.MsgTypeHookStop
	default:
		msgType = websocket.MsgTypeHookNotify
	}

	// Broadcast to browser
	h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
		Type: msgType,
		Data: map[string]interface{}{
			"session_id": sessionID,
			"event":      hookEvent,
		},
	})

	// Buffer tool events for the GET endpoint (max 50 per session)
	if eventName == "PreToolUse" || eventName == "PostToolUse" || eventName == "PostToolUseFailure" {
		h.mu.Lock()
		entry := toolEventEntry{
			Event:     hookEvent,
			MsgType:   string(msgType),
			Timestamp: time.Now(),
		}
		h.toolEvents[sessionID] = append([]toolEventEntry{entry}, h.toolEvents[sessionID]...)
		if len(h.toolEvents[sessionID]) > 50 {
			h.toolEvents[sessionID] = h.toolEvents[sessionID][:50]
		}
		h.mu.Unlock()
	}

	// For Stop events, send push notification (rate-limited)
	if eventName == "Stop" && h.notifService != nil && h.shouldSendPush(sessionID, false) {
		log.Printf("[hooks] Sending push for Stop event on session %s", sessionID)
		go func() {
			if err := h.notifService.Send(context.Background(), sessionID, "info",
				"Claude Stopped", "Claude Code finished execution"); err != nil {
				log.Printf("[hooks] Push failed for Stop: %v", err)
			}
		}()
	}

	// For Notification events, also persist and push (rate-limited)
	if eventName == "Notification" && h.notifService != nil && h.shouldSendPush(sessionID, false) {
		title := "Claude Code"
		body := "Notification"
		if msg, ok := hookEvent["message"].(string); ok {
			body = msg
		}
		log.Printf("[hooks] Sending push for Notification event on session %s: %s", sessionID, body)
		go func() {
			if err := h.notifService.Send(context.Background(), sessionID, "hook_notification", title, body); err != nil {
				log.Printf("[hooks] Push failed for Notification: %v", err)
			}
		}()
	}

	w.WriteHeader(http.StatusOK)
}

// HasPendingPermission checks if a session has a pending permission request
func (h *HookHandler) HasPendingPermission(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.pending[sessionID]
	return ok
}

// shouldSendPush checks rate-limit for push notifications per session.
// isPermission=true (PermissionRequest, AskUser, ExitPlan) always sends and updates the timestamp.
// isPermission=false (Stop, Notification) skips if a push was sent in the last 5 seconds.
func (h *HookHandler) shouldSendPush(sessionID string, isPermission bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if isPermission {
		h.lastPushSent[sessionID] = time.Now()
		return true
	}

	if last, ok := h.lastPushSent[sessionID]; ok && time.Since(last) < 5*time.Second {
		return false
	}
	h.lastPushSent[sessionID] = time.Now()
	return true
}

// ClearSession removes all state for a session (call when session ends)
func (h *HookHandler) ClearSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.alwaysAllow, sessionID)
	delete(h.toolEvents, sessionID)
	delete(h.lastPushSent, sessionID)
	if pending, ok := h.pending[sessionID]; ok {
		pending.cancel()
		delete(h.pending, sessionID)
	}
}

// HandleGetPending handles GET /api/hooks/pending/{sessionId}
// Returns current pending permission and recent tool events for reconnection recovery
func (h *HookHandler) HandleGetPending(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing session ID")
		return
	}

	h.mu.Lock()
	result := map[string]interface{}{}

	// Include pending permission if exists
	if pending, ok := h.pending[sessionID]; ok {
		result["pending_permission"] = map[string]interface{}{
			"event":      pending.hookEvent,
			"msg_type":   pending.msgType,
			"created_at": pending.createdAt,
		}
	}

	// Include buffered tool events
	if events, ok := h.toolEvents[sessionID]; ok {
		result["tool_events"] = events
	} else {
		result["tool_events"] = []toolEventEntry{}
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
