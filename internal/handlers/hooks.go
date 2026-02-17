package handlers

import (
	"context"
	"encoding/json"
	"fmt"
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

	// OnEvaluateSession is called asynchronously when a session evaluation is triggered.
	// Set by main.go to wire AI evaluation into hook events.
	OnEvaluateSession func(sessionID string, trigger string, outputSnapshot []byte)

	// OnActivityTouch is called (debounced) when a session has activity, to update last_activity_at.
	OnActivityTouch func(sessionID string)

	// OnPlanUpdated is called when ExitPlanMode is intercepted with plan content.
	OnPlanUpdated func(sessionID string, planContent string)

	// HasLinkedTask checks if a session has a linked task. Set by main.go.
	HasLinkedTask func(sessionID string) bool

	// HasRecentSuggestions checks if a session has any AI suggestions (any status) within the last 3 minutes. Set by main.go.
	HasRecentSuggestions func(sessionID string) bool

	mu                sync.Mutex
	pending           map[string]*pendingPermission  // sessionID -> pending permission
	alwaysAllow       map[string]map[string]bool     // sessionID -> toolName -> true
	toolEvents        map[string][]toolEventEntry    // sessionID -> recent tool events buffer
	lastPushSent      map[string]time.Time           // sessionID -> last push timestamp (for rate-limiting)
	userStopped       map[string]bool                // sessionID -> true if user explicitly stopped
	lastEvaluation    map[string]time.Time           // sessionID -> last evaluation timestamp (3-min cooldown)
	taskNotifs        map[string]map[string]interface{} // sessionID -> task notification data (pending until user responds)
	lastActivityTouch map[string]time.Time           // sessionID -> last DB touch for activity debounce
	sessionMode       map[string]string              // sessionID -> "plan_mode" | "executing" | "idle"
	modeIdleTimers    map[string]*time.Timer         // sessionID -> inactivity timer that sets mode to idle
	imagePromptMeta   map[string]string              // sessionID -> user's text prompt when images were included
	evalTimers        map[string]*time.Timer         // sessionID -> debounced evaluation timer
}

// NewHookHandler creates a new hook handler
func NewHookHandler(hub *websocket.Hub, notifService *notifications.Service, sessionMgr *session.Manager) *HookHandler {
	return &HookHandler{
		hub:               hub,
		notifService:      notifService,
		sessionMgr:        sessionMgr,
		pending:           make(map[string]*pendingPermission),
		alwaysAllow:       make(map[string]map[string]bool),
		toolEvents:        make(map[string][]toolEventEntry),
		lastPushSent:      make(map[string]time.Time),
		userStopped:       make(map[string]bool),
		lastEvaluation:    make(map[string]time.Time),
		taskNotifs:        make(map[string]map[string]interface{}),
		lastActivityTouch: make(map[string]time.Time),
		sessionMode:       make(map[string]string),
		modeIdleTimers:    make(map[string]*time.Timer),
		imagePromptMeta:   make(map[string]string),
		evalTimers:        make(map[string]*time.Timer),
	}
}

// modeIdleTimeout is how long to wait without events before assuming idle.
const modeIdleTimeout = 15 * time.Second

// setSessionMode updates the mode and broadcasts if changed. Must NOT hold h.mu.
func (h *HookHandler) setSessionMode(sessionID, newMode, reason string) {
	h.mu.Lock()
	oldMode := h.sessionMode[sessionID]
	if oldMode == newMode {
		h.mu.Unlock()
		return
	}
	h.sessionMode[sessionID] = newMode
	h.mu.Unlock()

	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	log.Printf("[hooks] Mode changed: session=%s old=%q new=%q (%s)", shortID, oldMode, newMode, reason)
	h.hub.BroadcastStateUpdate("session", map[string]interface{}{
		"action":     "mode_changed",
		"session_id": sessionID,
		"mode":       newMode,
	})
}

// resetIdleTimer resets (or starts) the inactivity timer for a session.
// When the timer fires, the session mode is set to idle.
// Must NOT hold h.mu when calling.
func (h *HookHandler) resetIdleTimer(sessionID string) {
	h.mu.Lock()
	if t, ok := h.modeIdleTimers[sessionID]; ok {
		t.Stop()
	}
	h.modeIdleTimers[sessionID] = time.AfterFunc(modeIdleTimeout, func() {
		h.setSessionMode(sessionID, "idle", "inactivity-timeout")
	})
	h.mu.Unlock()
}

// cancelIdleTimer stops the inactivity timer for a session (e.g. on explicit idle).
func (h *HookHandler) cancelIdleTimer(sessionID string) {
	h.mu.Lock()
	if t, ok := h.modeIdleTimers[sessionID]; ok {
		t.Stop()
		delete(h.modeIdleTimers, sessionID)
	}
	h.mu.Unlock()
}

// GetSessionMode returns the current execution mode for a session.
func (h *HookHandler) GetSessionMode(sessionID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionMode[sessionID]
}

// trackModeFromEvent reads the permission_mode field from a hook event,
// updates the session mode, and resets the inactivity timer.
func (h *HookHandler) trackModeFromEvent(sessionID string, hookEvent map[string]interface{}) {
	permMode, _ := hookEvent["permission_mode"].(string)
	var newMode string
	if permMode == "plan" {
		newMode = "plan_mode"
	} else if permMode != "" {
		newMode = "executing"
	}
	if newMode == "" {
		return
	}
	h.setSessionMode(sessionID, newMode, "permission_mode="+permMode)
	h.resetIdleTimer(sessionID)
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

	// Log every incoming permission request
	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	log.Printf("[hooks] Permission received: session=%s tool=%s", shortID, toolName)

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

		// Track mode from permission_mode field (source of truth)
		h.trackModeFromEvent(sessionID, hookEvent)

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
					"Question from Claude", "Claude needs your input", ""); err != nil {
					log.Printf("[hooks] Push failed for AskUserQuestion: %v", err)
				}
			}()
		}
	} else if isExitPlan {
		// Persist plan content from hook event
		if h.OnPlanUpdated != nil {
			if ti, ok := hookEvent["tool_input"].(map[string]interface{}); ok {
				if p, ok := ti["plan"].(string); ok {
					if pc := strings.TrimSpace(p); pc != "" {
						go h.OnPlanUpdated(sessionID, pc)
					}
				}
			}
		}

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
					"Plan Ready", "Claude's plan needs your approval", ""); err != nil {
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
					"Permission Request", notifToolName+" needs approval", ""); err != nil {
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

		// Mark session notifications as read and cancel push
		if h.notifService != nil {
			go h.notifService.MarkSessionRead(context.Background(), sessionID)
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

		// Trigger plan_accepted evaluation when ExitPlanMode is approved
		if isExitPlan && decision.Behavior == "allow" {
			h.triggerSessionEvaluation(sessionID, "plan_accepted")
			// Transition out of plan mode
			h.mu.Lock()
			h.sessionMode[sessionID] = "executing"
			h.mu.Unlock()
			h.hub.BroadcastStateUpdate("session", map[string]interface{}{
				"action":     "mode_changed",
				"session_id": sessionID,
				"mode":       "executing",
			})
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

		// Mark session notifications as read and cancel push
		if h.notifService != nil {
			go h.notifService.MarkSessionRead(context.Background(), sessionID)
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

	// Log every incoming event for debugging
	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	toolName, _ := hookEvent["tool_name"].(string)
	log.Printf("[hooks] Event received: session=%s event=%s tool=%s", shortID, eventName, toolName)

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
	case "UserPromptSubmit":
		msgType = websocket.MsgTypeHookNotify
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

	// Debounced activity tracking (max once per 30 seconds per session)
	if h.OnActivityTouch != nil {
		h.mu.Lock()
		if last, ok := h.lastActivityTouch[sessionID]; !ok || time.Since(last) > 30*time.Second {
			h.lastActivityTouch[sessionID] = time.Now()
			h.mu.Unlock()
			go h.OnActivityTouch(sessionID)
		} else {
			h.mu.Unlock()
		}
	}

	// Track execution mode changes
	switch eventName {
	case "Stop":
		h.cancelIdleTimer(sessionID)
		h.setSessionMode(sessionID, "idle", "event=Stop")
	case "Notification":
		if msg, ok := hookEvent["message"].(string); ok {
			if strings.Contains(msg, "waiting for your input") || strings.Contains(msg, "awaiting") {
				h.cancelIdleTimer(sessionID)
				h.setSessionMode(sessionID, "idle", "event=Notification")
			}
		}
	default:
		// Use permission_mode from the event + reset inactivity timer
		h.trackModeFromEvent(sessionID, hookEvent)
	}

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
	// Skip if the user explicitly stopped the session (no need to notify)
	h.mu.Lock()
	stoppedByUser := h.userStopped[sessionID]
	h.mu.Unlock()
	if eventName == "Stop" && h.notifService != nil && !stoppedByUser && h.shouldSendPush(sessionID, false) {
		log.Printf("[hooks] Sending push for Stop event on session %s", sessionID)
		go func() {
			if err := h.notifService.Send(context.Background(), sessionID, "info",
				"Claude Stopped", "Claude Code finished execution", ""); err != nil {
				log.Printf("[hooks] Push failed for Stop: %v", err)
			}
		}()
	} else if eventName == "Stop" && stoppedByUser {
		log.Printf("[hooks] Skipping push for Stop event on session %s (user-initiated stop)", sessionID)
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
			if err := h.notifService.Send(context.Background(), sessionID, "hook_notification", title, body, ""); err != nil {
				log.Printf("[hooks] Push failed for Notification: %v", err)
			}
		}()
	}

	// For UserPromptSubmit events, schedule debounced evaluation.
	// Waits 20s after the last prompt for Claude Code to produce output,
	// giving the evaluator richer context (especially for image inputs).
	if eventName == "UserPromptSubmit" {
		h.scheduleDebouncedEval(sessionID)
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

// MarkUserStopped marks a session as explicitly stopped by the user,
// so that the Stop hook does not send a push notification.
func (h *HookHandler) MarkUserStopped(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userStopped[sessionID] = true
}

// triggerSessionEvaluation triggers an async AI evaluation for a session with rate limiting.
// Returns true if the evaluation was triggered, false if rate-limited or unavailable.
func (h *HookHandler) triggerSessionEvaluation(sessionID, trigger string) bool {
	if h.OnEvaluateSession == nil {
		return false
	}

	// Shorter cooldown for plan_accepted (strong signal of scope change)
	cooldown := 3 * time.Minute
	if trigger == "plan_accepted" {
		cooldown = 30 * time.Second
	}

	h.mu.Lock()
	if last, ok := h.lastEvaluation[sessionID]; ok && time.Since(last) < cooldown {
		h.mu.Unlock()
		log.Printf("[hooks] Session evaluation rate-limited for %s (trigger=%s, last=%v ago, cooldown=%v)", sessionID, trigger, time.Since(last).Round(time.Second), cooldown)
		return false
	}
	h.lastEvaluation[sessionID] = time.Now()
	h.mu.Unlock()

	// Get session output snapshot
	var output []byte
	if h.sessionMgr != nil {
		if o, err := h.sessionMgr.GetSessionOutput(sessionID); err == nil {
			output = o
		}
	}

	// Consume image prompt metadata (if any) and append as context hint.
	// This helps the AI evaluator understand the user's intent when the terminal
	// output only shows file path references for images.
	h.mu.Lock()
	imagePrompt := h.imagePromptMeta[sessionID]
	delete(h.imagePromptMeta, sessionID)
	h.mu.Unlock()

	if imagePrompt != "" {
		hint := fmt.Sprintf("\n[DevManager context: User submitted input with image(s). User's text prompt: \"%s\"]\n", imagePrompt)
		output = append(output, []byte(hint)...)
	}

	log.Printf("[hooks] Triggering session evaluation: session=%s trigger=%s outputLen=%d hasImageMeta=%v", sessionID, trigger, len(output), imagePrompt != "")
	go h.OnEvaluateSession(sessionID, trigger, output)
	return true
}

// ClearSession removes all state for a session (call when session ends)
func (h *HookHandler) ClearSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.alwaysAllow, sessionID)
	delete(h.toolEvents, sessionID)
	delete(h.lastPushSent, sessionID)
	delete(h.userStopped, sessionID)
	delete(h.lastEvaluation, sessionID)
	delete(h.lastActivityTouch, sessionID)
	delete(h.sessionMode, sessionID)
	delete(h.imagePromptMeta, sessionID)
	if t, ok := h.evalTimers[sessionID]; ok {
		t.Stop()
		delete(h.evalTimers, sessionID)
	}
	if pending, ok := h.pending[sessionID]; ok {
		pending.cancel()
		delete(h.pending, sessionID)
	}
}

// evalDebounceDelay is how long to wait after the last UserPromptSubmit before evaluating.
// This gives Claude Code time to process the prompt and produce output, providing the
// AI evaluator with richer context (especially important for image-based inputs).
const evalDebounceDelay = 20 * time.Second

// HandleImagePromptHint handles POST /api/sessions/{id}/image-prompt-hint
// Called by the frontend when images are uploaded with a text prompt.
// Stores the user's text prompt as metadata for the next session evaluation.
func (h *HookHandler) HandleImagePromptHint(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing session ID")
		return
	}

	var input struct {
		UserPrompt string `json:"user_prompt"`
		ImageCount int    `json:"image_count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	h.mu.Lock()
	h.imagePromptMeta[sessionID] = input.UserPrompt
	h.mu.Unlock()

	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	log.Printf("[hooks] Image prompt hint stored: session=%s imageCount=%d promptLen=%d",
		shortID, input.ImageCount, len(input.UserPrompt))

	w.WriteHeader(http.StatusOK)
}

// scheduleDebouncedEval schedules (or reschedules) a debounced session evaluation.
// Each UserPromptSubmit resets the timer. The evaluation fires once activity settles,
// capturing Claude Code's response for richer context.
func (h *HookHandler) scheduleDebouncedEval(sessionID string) {
	h.mu.Lock()
	// Cancel previous timer (debounce: each new prompt resets the clock)
	if t, ok := h.evalTimers[sessionID]; ok {
		t.Stop()
	}
	h.evalTimers[sessionID] = time.AfterFunc(evalDebounceDelay, func() {
		h.mu.Lock()
		delete(h.evalTimers, sessionID)
		h.mu.Unlock()

		shortID := sessionID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		// Guard 1: session still running?
		if h.sessionMgr == nil || !h.sessionMgr.IsSessionRunning(sessionID) {
			log.Printf("[hooks] Debounced eval skipped: session %s not running", shortID)
			return
		}

		// Guard 2: no linked task?
		if h.HasLinkedTask != nil && h.HasLinkedTask(sessionID) {
			log.Printf("[hooks] Debounced eval skipped: session %s already has linked task", shortID)
			return
		}

		// Guard 3: no recent suggestions (any status) in last 3 min?
		if h.HasRecentSuggestions != nil && h.HasRecentSuggestions(sessionID) {
			log.Printf("[hooks] Debounced eval skipped: session %s has recent suggestions", shortID)
			return
		}

		log.Printf("[hooks] Debounced eval firing for session %s", shortID)
		h.triggerSessionEvaluation(sessionID, "user_prompt")
	})
	h.mu.Unlock()
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

	// Include pending task notification
	if notif, ok := h.taskNotifs[sessionID]; ok {
		result["task_notification"] = notif
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// SetTaskNotification stores and broadcasts a task-loaded notification for a session.
// Called by the API handler when a session is created from a task.
func (h *HookHandler) SetTaskNotification(sessionID string, taskID int64, taskTitle, taskDescription string) {
	data := map[string]interface{}{
		"session_id":  sessionID,
		"task_id":     taskID,
		"task_title":  taskTitle,
		"description": taskDescription,
	}

	h.mu.Lock()
	h.taskNotifs[sessionID] = data
	h.mu.Unlock()

	// Broadcast to connected clients
	h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
		Type: websocket.MsgTypeHookTaskLoaded,
		Data: data,
	})
}

// ClearTaskNotification removes a pending task notification for a session.
func (h *HookHandler) ClearTaskNotification(sessionID string) {
	h.mu.Lock()
	delete(h.taskNotifs, sessionID)
	h.mu.Unlock()
}

// HandleTaskNotificationRespond handles POST /api/hooks/task-notification/{sessionId}/respond
// Clears the pending task notification. The actual terminal input is sent by the frontend.
func (h *HookHandler) HandleTaskNotificationRespond(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing session ID")
		return
	}

	h.ClearTaskNotification(sessionID)
	w.WriteHeader(http.StatusOK)
}
