package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
}

// HookHandler manages hook API endpoints
type HookHandler struct {
	hub          *websocket.Hub
	notifService *notifications.Service
	sessionMgr   *session.Manager

	mu           sync.Mutex
	pending      map[string]*pendingPermission  // sessionID -> pending permission
	alwaysAllow  map[string]map[string]bool     // sessionID -> toolName -> true
}

// NewHookHandler creates a new hook handler
func NewHookHandler(hub *websocket.Hub, notifService *notifications.Service, sessionMgr *session.Manager) *HookHandler {
	return &HookHandler{
		hub:          hub,
		notifService: notifService,
		sessionMgr:   sessionMgr,
		pending:      make(map[string]*pendingPermission),
		alwaysAllow:  make(map[string]map[string]bool),
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

	h.mu.Lock()
	// Cancel any existing pending permission for this session
	if existing, ok := h.pending[sessionID]; ok {
		existing.cancel()
	}
	h.pending[sessionID] = &pendingPermission{
		responseCh: responseCh,
		cancel:     cancel,
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
	} else if isExitPlan {
		// Send plan approval UI
		h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeHookExitPlan,
			Data: map[string]interface{}{
				"session_id": sessionID,
				"event":      hookEvent,
			},
		})
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
		if h.notifService != nil {
			notifToolName := "Tool"
			if tn, ok := hookEvent["tool_name"].(string); ok {
				notifToolName = tn
			}
			go h.notifService.Send(context.Background(), sessionID, "permission",
				"Permission Request", notifToolName+" needs approval")
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
			//   /clear, paste plan content, Shift+Tab.
			if h.sessionMgr != nil && planChoice == "1" {
				go func() {
					// Hook "allow" already approved the plan.
					// Wait for Claude to process and write the plan file.
					time.Sleep(5 * time.Second)
					planContent, planFile, err := findLatestPlanFile()
					if err != nil {
						log.Printf("[hooks] Option 1: could not find plan file: %v — falling back to option 2 (Shift+Tab only)", err)
						h.sessionMgr.WriteToSession(sessionID, []byte("\x1b[Z"))
						return
					}
					log.Printf("[hooks] Option 1: found plan file: %s (%d bytes)", planFile, len(planContent))

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

	// For Notification events, also persist and push
	if eventName == "Notification" && h.notifService != nil {
		title := "Claude Code"
		body := "Notification"
		if msg, ok := hookEvent["message"].(string); ok {
			body = msg
		}
		go h.notifService.Send(context.Background(), sessionID, "hook_notification", title, body)
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

// ClearSession removes all state for a session (call when session ends)
func (h *HookHandler) ClearSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.alwaysAllow, sessionID)
	if pending, ok := h.pending[sessionID]; ok {
		pending.cancel()
		delete(h.pending, sessionID)
	}
}

// findLatestPlanFile finds and reads the most recently modified plan file in ~/.claude/plans/
func findLatestPlanFile() (content string, filePath string, err error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	plansDir := filepath.Join(homeDir, ".claude", "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		return "", "", err
	}

	var latestFile string
	var latestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latestFile = filepath.Join(plansDir, entry.Name())
		}
	}

	if latestFile == "" {
		return "", "", os.ErrNotExist
	}

	data, err := os.ReadFile(latestFile)
	if err != nil {
		return "", "", err
	}

	return strings.TrimSpace(string(data)), latestFile, nil
}
