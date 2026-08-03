package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/notifications"
	"openpoet/internal/session"
	"openpoet/internal/sessiontoken"
	"openpoet/internal/websocket"
)

// PermissionResponse is what the browser sends back when user clicks Allow/Deny/AllowAlways
type PermissionResponse struct {
	Behavior              string            `json:"behavior"` // "allow", "allowAlways", "deny", or "passthrough"
	Message               string            `json:"message,omitempty"`
	ToolName              string            `json:"tool_name,omitempty"`
	Answers               map[string]string `json:"answers,omitempty"`                // For AskUserQuestion responses
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
	OriginalBehavior   string                 `json:"originalBehavior,omitempty"`
	Message            string                 `json:"message,omitempty"`
	UpdatedInput       map[string]interface{} `json:"updatedInput,omitempty"`
	UpdatedPermissions []interface{}          `json:"updatedPermissions,omitempty"`
}

// preToolUseGateOutput is Claude Code's NATIVE PreToolUse hook contract — note
// this differs from the PermissionRequest shape above (hookSpecificOutput.decision
// object): PreToolUse uses a string permissionDecision + permissionDecisionReason,
// and it is honored even under dangerously_skip_permissions.
type preToolUseGateOutput struct {
	HookSpecificOutput preToolUseHookOutput `json:"hookSpecificOutput"`
}

type preToolUseHookOutput struct {
	HookEventName string `json:"hookEventName"`
	// PermissionDecision is set ONLY to "deny" (a conflict/substrate block). On
	// allow it is OMITTED so Claude Code's normal flow proceeds — emitting an
	// explicit "allow" would AUTO-APPROVE the tool and bypass OpenPoet's
	// permission dialog for every non-opted project (the default is observe), so
	// the gate must stay silent unless it is actively denying.
	PermissionDecision       string `json:"permissionDecision,omitempty"`       // "" (neutral) | "deny"
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"` // model reads this on deny
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
	api          *API

	// OnEvaluateSession is called asynchronously when a session evaluation is triggered.
	// Returns true if suggestions were generated, false otherwise.
	// Set by main.go to wire AI evaluation into hook events.
	OnEvaluateSession func(sessionID string, trigger string, outputSnapshot []byte) bool

	// OnActivityTouch is called (debounced) when a session has activity, to update last_activity_at.
	OnActivityTouch func(sessionID string)

	// OnPlanUpdated is called when ExitPlanMode is intercepted with plan content.
	OnPlanUpdated func(sessionID string, planContent string)

	// OnReconcileEffectiveModel reads the runtime transcript after a completed
	// Claude turn so mid-session /model changes are reflected in session metadata.
	OnReconcileEffectiveModel func(sessionID string)

	// OnToolEvent taps the hook firehose for the conflict radar: fired for
	// PreToolUse/PostToolUse/PostToolUseFailure (tool_input intact) and Stop.
	// Implementations must only enqueue — this runs on the hook response path.
	OnToolEvent func(sessionID, eventName string, hookEvent map[string]interface{})

	// ConsultConflict is called SYNCHRONOUSLY in HandlePermission before parking
	// a write permission. Returns (deny, reason) — a non-empty reason when
	// another live session holds a write claim on the same path. Must not block
	// on I/O beyond a bounded cache read. Set by main.go to the conflict radar.
	ConsultConflict func(sessionID, toolName string, toolInput map[string]interface{}) (bool, string)

	// ReleaseClaim drops a session's write claim for a path whose write was just
	// denied (the write never happens), so a denied attempt cannot leave a
	// residual claim that mutually locks out the legitimate claimant.
	ReleaseClaim func(sessionID, toolName string, toolInput map[string]interface{})

	// GatePreToolUse answers the SYNCHRONOUS PreToolUse gate (Phase 5, L2). Set by
	// main.go: it resolves the project's conflict_policy + the global
	// conflict_enforcement_enabled kill switch and, only for opted-in projects
	// with the switch on, consults the conflict radar's atomic gate. Returns
	// (deny, reason). A nil callback (or any non-opted path) means allow — the
	// gate is strictly opt-in and fail-open. This is the ONLY control point that
	// governs dangerously_skip_permissions sessions (they never fire
	// PermissionRequest, but always fire PreToolUse).
	GatePreToolUse func(ctx context.Context, sessionID, toolName string, toolInput map[string]interface{}) (bool, string)

	// HasLinkedTask checks if a session has a linked task. Set by main.go.
	HasLinkedTask func(sessionID string) bool

	// HasRecentSuggestions checks if a session has any AI suggestions (any status) within the last 3 minutes. Set by main.go.
	HasRecentSuggestions func(sessionID string) bool

	mu                sync.Mutex
	pending           map[string]*pendingPermission     // sessionID -> pending permission
	alwaysAllow       map[string]map[string]bool        // sessionID -> toolName -> true
	toolEvents        map[string][]toolEventEntry       // sessionID -> recent tool events buffer
	lastPushSent      map[string]time.Time              // sessionID -> last push timestamp (for rate-limiting)
	userStopped       map[string]bool                   // sessionID -> true if user explicitly stopped
	lastEvaluation    map[string]time.Time              // sessionID -> last evaluation timestamp (3-min cooldown)
	taskNotifs        map[string]map[string]interface{} // sessionID -> task notification data (pending until user responds)
	lastActivityTouch map[string]time.Time              // sessionID -> last DB touch for activity debounce
	sessionMode       map[string]string                 // sessionID -> "plan_mode" | "executing" | "idle"
	modeIdleTimers    map[string]*time.Timer            // sessionID -> inactivity timer that sets mode to idle
	imagePromptMeta   map[string]string                 // sessionID -> user's text prompt when images were included
	evalTimers        map[string]*time.Timer            // sessionID -> debounced evaluation timer
	acpUsage          map[string]*ACPUsageInfo          // sessionID -> ACP usage tracking (model, premium requests)
	promptWaiters     map[string][]chan struct{}        // sessionID -> awaiters woken by the next UserPromptSubmit (send ack)
}

// ACPUsageInfo holds Copilot ACP usage tracking data for a session.
type ACPUsageInfo struct {
	CurrentModel    string              `json:"current_model"`
	ModelMultiplier string              `json:"model_multiplier"`
	PremiumRequests int                 `json:"premium_requests"`
	AvailableModels []ACPAvailableModel `json:"available_models,omitempty"`
}

// ACPAvailableModel describes a model available in the Copilot ACP session.
type ACPAvailableModel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Multiplier string `json:"multiplier"`
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
		acpUsage:          make(map[string]*ACPUsageInfo),
		promptWaiters:     make(map[string][]chan struct{}),
	}
}

// BindPlatformAPI gives HTTP-only hook routes access to the immutable
// application bundle. Hook ingestion and the hook response port remain owned
// by HookHandler; only the UI boundary traverses back through the composition
// root. ConfigurePlatformServices calls this once during startup.
func (h *HookHandler) BindPlatformAPI(api *API) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.api = api
	h.mu.Unlock()
}

func (h *HookHandler) platformApplicationServices() (*PlatformApplicationServices, bool) {
	if h == nil {
		return nil, false
	}
	h.mu.Lock()
	api := h.api
	h.mu.Unlock()
	if api == nil {
		return nil, false
	}
	return api.platformApplicationServices()
}

// modeIdleTimeout is how long to wait without events before assuming idle.
const modeIdleTimeout = 15 * time.Second

// normalizeCopilotEventName maps Copilot CLI hook event names to Claude Code equivalents.
// This allows the rest of the hook handler to use a single code path.
func normalizeCopilotEventName(name string) string {
	switch name {
	case "preToolUse":
		return "PreToolUse"
	case "postToolUse":
		return "PostToolUse"
	case "userPromptSubmitted":
		return "UserPromptSubmit"
	case "sessionStart":
		return "Notification"
	case "sessionEnd":
		return "Stop"
	default:
		return name
	}
}

func hookBackendDisplayName(backend string) string {
	switch backend {
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "copilot", "acp":
		return "Copilot"
	default:
		return "Claude"
	}
}

// trackClaudeProviderSessionID keeps the OpenPoet session attached to the
// transcript Claude Code is actually writing. The two ids diverge when a user
// starts a fresh terminal and selects an older conversation with /resume.
func (h *HookHandler) trackClaudeProviderSessionID(openPoetSessionID string, hookEvent map[string]interface{}, backend string) {
	if h.sessionMgr == nil {
		return
	}
	// Claude's bridge historically sends no X-Backend header. Do not interpret
	// session_id fields from the other backend hook formats as Claude ids.
	if backend != "" && backend != "claude" && backend != "claude_code" {
		return
	}
	providerSessionID, _ := hookEvent["session_id"].(string)
	providerSessionID = strings.TrimSpace(providerSessionID)
	if providerSessionID == "" {
		return
	}
	h.sessionMgr.PersistProviderSessionID(context.Background(), openPoetSessionID, providerSessionID, "Claude")
}

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
	if !h.hookTokenAllowed(r.Context(), sessionID, r.Header.Get("X-Hook-Token")) {
		respondError(w, http.StatusUnauthorized, "invalid or missing hook token")
		return
	}

	// Detect backend from header (both "copilot" and "acp" use non-Claude hook format)
	backend := r.Header.Get("X-Backend")
	isCopilot := backend == "copilot" || backend == "acp"
	agentName := hookBackendDisplayName(backend)

	// Parse the hook event JSON. Windows claude.exe pipes hook input using the
	// host's ANSI codepage; normalize to UTF-8 so JSON strings round-trip cleanly.
	rawBody, _ := io.ReadAll(r.Body)
	var hookEvent map[string]interface{}
	if err := json.Unmarshal(normalizeUTF8Body(rawBody), &hookEvent); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if backend != "" {
		hookEvent["backend"] = backend
	}
	h.trackClaudeProviderSessionID(sessionID, hookEvent, backend)

	// Normalize Copilot event names
	if isCopilot {
		if en, ok := hookEvent["hook_event_name"].(string); ok {
			hookEvent["hook_event_name"] = normalizeCopilotEventName(en)
		}
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

	// Synchronous conflict veto (Phase 3, L1): consult the radar BEFORE the
	// always-allow fast-path — a contested write must be denied even when the
	// user clicked "always allow Edit", because the conflict is cross-session,
	// orthogonal to the per-session allow. Runs only for write-class tools (the
	// consult filters), never touches the DB, fails open when the radar is cold.
	// On deny it releases the requesting session's just-registered claim so a
	// denied write cannot mutually lock out the legitimate claimant.
	if h.ConsultConflict != nil {
		toolInput, _ := hookEvent["tool_input"].(map[string]interface{})
		if deny, reason := h.ConsultConflict(sessionID, toolName, toolInput); deny {
			log.Printf("[hooks] Conflict veto for session %s tool %s: %s", sessionID, toolName, reason)
			if h.ReleaseClaim != nil {
				h.ReleaseClaim(sessionID, toolName, toolInput)
			}
			output := hookPermissionOutput{
				HookSpecificOutput: hookSpecificOutput{
					HookEventName: "PermissionRequest",
					Decision:      permissionDecision{Behavior: "deny", Message: reason},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(output)
			return
		}
	}

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
					"Question from "+agentName, agentName+" needs your input", ""); err != nil {
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
					"Plan Ready", agentName+"'s plan needs your approval", ""); err != nil {
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
		decision := permissionDecision{OriginalBehavior: resp.Behavior}
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
		} else if isAskUser && resp.Message == askUserClarifySentinel {
			// "Chat about this" — deny carrying Claude Code's own clarification
			// request, so the agent asks what the user wants to discuss instead of
			// treating any option as chosen.
			toolInput, _ := hookEvent["tool_input"].(map[string]interface{})
			questions := extractAskUserQuestions(toolInput)
			answers, _ := reconcileAskUserAnswers(questions, resp.Answers, sessionID)
			flat := make(map[string]string, len(answers))
			for k, v := range answers {
				if s, ok := v.(string); ok {
					flat[k] = s
				}
			}
			decision.Behavior = "deny"
			decision.Message = askUserClarifyMessage(questions, flat)
			log.Printf("[hooks] AskUserQuestion clarification requested for session %s", sessionID)
		} else if isAskUser && resp.Behavior == "allow" && len(resp.Answers) > 0 {
			decision.Behavior = "allow"
			toolInput, _ := hookEvent["tool_input"].(map[string]interface{})
			questions := extractAskUserQuestions(toolInput)
			if len(questions) == 0 {
				// No questions to key the answers against. Claude Code hard-validates
				// updatedInput against the tool's schema right before the call, and a
				// payload without a valid `questions` array kills the tool with an
				// InputValidationError — strictly worse than a plain allow.
				log.Printf("[hooks] AskUserQuestion for session %s has no parsable questions; allowing without answers", sessionID)
			} else {
				// Build updatedInput with original tool_input + answers
				updatedInput := make(map[string]interface{})
				for k, v := range toolInput {
					updatedInput[k] = v
				}
				// Claude Code looks answers up by the exact question text and checks
				// the value against the exact option labels; anything else is dropped
				// and the agent is told the user did not answer. reconcileAskUserAnswers
				// repairs the ways a client can mangle those strings.
				answersMap, annotations := reconcileAskUserAnswers(questions, resp.Answers, sessionID)
				updatedInput["answers"] = answersMap
				if len(annotations) > 0 {
					updatedInput["annotations"] = annotations
				}
				decision.UpdatedInput = updatedInput
				log.Printf("[hooks] AskUserQuestion answered for session %s: %v", sessionID, answersMap)
			}
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

		// Trigger plan_accepted evaluation when ExitPlanMode is approved (fire-and-forget)
		if isExitPlan && decision.Behavior == "allow" {
			h.triggerSessionEvaluation(sessionID, "plan_accepted") //nolint:errcheck
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

// askUserClarifySentinel is the deny message the web client sends when the user
// picks "Chat about this" in the AskUserQuestion modal. The server swaps it for
// Claude Code's own clarification wording (see askUserClarifyMessage) so the
// text stays correct no matter which client version produced the request.
const askUserClarifySentinel = "__openpoet_ask_user_clarify__"

// askUserQuestion is one entry of an AskUserQuestion tool_input, reduced to what
// answer reconciliation needs.
type askUserQuestion struct {
	text     string
	labels   []string
	previews map[string]string // option label -> preview, for annotations
}

// extractAskUserQuestions pulls the ordered questions out of an AskUserQuestion
// tool_input (shape: {"questions": [{"question": "...", "options": [{"label": …,
// "preview": …}]}]}). Returns nil when the shape doesn't match.
func extractAskUserQuestions(toolInput map[string]interface{}) []askUserQuestion {
	raw, ok := toolInput["questions"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]askUserQuestion, len(raw))
	for i, q := range raw {
		qm, ok := q.(map[string]interface{})
		if !ok {
			continue
		}
		if qt, ok := qm["question"].(string); ok {
			out[i].text = qt
		}
		opts, ok := qm["options"].([]interface{})
		if !ok {
			continue
		}
		for _, o := range opts {
			om, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			label, ok := om["label"].(string)
			if !ok || label == "" {
				continue
			}
			out[i].labels = append(out[i].labels, label)
			if preview, ok := om["preview"].(string); ok && preview != "" {
				if out[i].previews == nil {
					out[i].previews = make(map[string]string)
				}
				out[i].previews[label] = preview
			}
		}
	}
	return out
}

// repairQuoteTruncation recovers a string that was cut short at a double quote.
//
// A web client that interpolates a question text or an option label into an HTML
// attribute loses everything from the first `"` onward, because the attribute
// ends there. The damage is recognisable: the surviving text is a proper prefix
// of exactly one candidate, and the very next character of that candidate is the
// `"` that ended the attribute. Only that exact shape is repaired, so a genuine
// free-text ("Other") answer — which has no reason to stop right before a quote
// in some option label — is never rewritten.
func repairQuoteTruncation(value string, candidates []string) (string, bool) {
	if value == "" {
		return value, false
	}
	for _, c := range candidates {
		if c == value {
			return value, false // exact match — nothing to repair
		}
	}
	repaired := ""
	matches := 0
	for _, c := range candidates {
		if len(c) > len(value) && strings.HasPrefix(c, value) && c[len(value)] == '"' {
			repaired = c
			matches++
		}
	}
	if matches != 1 {
		return value, false
	}
	return repaired, true
}

// reconcileAskUserAnswers maps the browser's answers onto the exact question
// texts and option labels Claude Code will look them up by.
//
// Claude Code matches answers to questions by the question TEXT and checks the
// answer against the option LABELS, both by exact string equality. A key that
// matches no question is silently dropped, and if nothing matches the tool
// result becomes "The user did not answer the questions." — the agent then
// proceeds with its own pick, which reads as it having ignored the user.
//
// Three client-side mistakes are corrected here so answers survive regardless of
// which client version sent them (a PWA tab can run weeks-old JS):
//   - numeric-index keys ("0", "1", …) from clients before 2899f17;
//   - keys and values truncated at a double quote by HTML attribute
//     interpolation (see repairQuoteTruncation);
//   - a single unmatched answer to a single-question prompt.
//
// It also rebuilds the `annotations` map the terminal UI sends, so a chosen
// option's preview is echoed back to the model the same way.
func reconcileAskUserAnswers(questions []askUserQuestion, answers map[string]string, sessionID string) (map[string]interface{}, map[string]interface{}) {
	texts := make([]string, len(questions))
	for i, q := range questions {
		texts[i] = q.text
	}

	resolved := make(map[string]interface{}, len(answers))
	annotations := make(map[string]interface{})

	// Exact matches claim their question first, and the remaining keys are
	// walked in sorted order: two keys can resolve to the same question (say an
	// index key alongside the literal text), and Go's randomized map iteration
	// would otherwise make the survivor a coin flip.
	claimed := make([]bool, len(texts))
	var fuzzy []string
	for key := range answers {
		matched := false
		for i, t := range texts {
			if t != "" && t == key {
				claimed[i] = true
				matched = true
				break
			}
		}
		if !matched {
			fuzzy = append(fuzzy, key)
		}
	}
	sort.Strings(fuzzy)

	keys := make([]string, 0, len(answers))
	for i, t := range texts {
		if claimed[i] {
			keys = append(keys, t)
		}
	}
	keys = append(keys, fuzzy...)

	for _, key := range keys {
		value := answers[key]
		idx := -1

		// 1. Exact question text.
		for i, t := range texts {
			if t != "" && t == key {
				idx = i
				break
			}
		}
		// 2. Numeric index key from a pre-2899f17 client.
		if idx < 0 {
			if n, err := strconv.Atoi(key); err == nil && n >= 0 && n < len(texts) && texts[n] != "" && !claimed[n] {
				idx = n
				log.Printf("[hooks] AskUserQuestion answer key remapped from index %d for session %s", n, sessionID)
			}
		}
		// 3. Key truncated at a double quote by HTML attribute interpolation.
		if idx < 0 {
			if repaired, ok := repairQuoteTruncation(key, texts); ok {
				for i, t := range texts {
					if t == repaired && !claimed[i] {
						idx = i
						break
					}
				}
				if idx >= 0 {
					log.Printf("[hooks] AskUserQuestion answer key repaired (quote-truncated) for session %s: %q -> %q", sessionID, key, repaired)
				}
			}
		}
		// 4. Single question, single unmatched answer — it can only belong there.
		if idx < 0 && len(texts) == 1 && len(answers) == 1 && texts[0] != "" {
			idx = 0
			log.Printf("[hooks] AskUserQuestion answer key %q matched the only question for session %s", key, sessionID)
		}

		if idx < 0 {
			log.Printf("[hooks] AskUserQuestion answer key %q matches no question for session %s — Claude Code will drop it", key, sessionID)
			resolved[key] = value
			continue
		}
		claimed[idx] = true

		question := questions[idx]
		repairedValue, repaired := repairQuoteTruncation(value, question.labels)
		if !repaired && strings.Contains(value, ", ") {
			// Multi-select answers arrive joined with ", "; repair each part.
			parts := strings.Split(value, ", ")
			changed := false
			for i, part := range parts {
				if fixed, ok := repairQuoteTruncation(part, question.labels); ok {
					parts[i] = fixed
					changed = true
				}
			}
			if changed {
				repairedValue = strings.Join(parts, ", ")
				repaired = true
			}
		}
		if repaired {
			log.Printf("[hooks] AskUserQuestion answer value repaired (quote-truncated) for session %s: %q -> %q", sessionID, value, repairedValue)
		}

		resolved[question.text] = repairedValue

		// Echo the selected option's preview, matching the terminal UI.
		if preview, ok := question.previews[repairedValue]; ok {
			annotations[question.text] = map[string]interface{}{"preview": preview}
		}
	}

	// Mark the questions the user left blank. Claude Code treats a missing key
	// as "nothing to report" and filters it out, so a partly answered prompt
	// would otherwise be summarised to the agent as "Your questions have been
	// answered" with the skipped ones invisible. A note flips it to the careful
	// wording and renders the question as "(no option selected)".
	if len(resolved) > 0 {
		for _, q := range questions {
			if q.text == "" {
				continue
			}
			if _, answered := resolved[q.text]; answered {
				continue
			}
			annotations[q.text] = map[string]interface{}{
				"notes": "The user did not answer this question.",
			}
			log.Printf("[hooks] AskUserQuestion question left unanswered for session %s: %q", sessionID, q.text)
		}
	}

	return resolved, annotations
}

// askUserClarifyMessage reproduces Claude Code's "Chat about this" text: a
// denial that asks the agent to find out what the user wants to clarify instead
// of treating any option as chosen. Any options already selected ride along.
func askUserClarifyMessage(questions []askUserQuestion, answers map[string]string) string {
	var b strings.Builder
	b.WriteString("The user wants to clarify these questions.\n")
	b.WriteString("    This means they may have additional information, context or questions for you.\n")
	b.WriteString("    Take their response into account and then reformulate the questions if appropriate.\n")
	b.WriteString("    Start by asking them what they would like to clarify.\n")
	b.WriteString("    Questions asked:\n")
	for i, q := range questions {
		if i > 0 {
			b.WriteString("\n")
		}
		// Raw quotes, matching Claude Code's own rendering of this list.
		fmt.Fprintf(&b, "- \"%s\"\n", q.text)
		if answer, ok := answers[q.text]; ok && answer != "" {
			fmt.Fprintf(&b, "  Answer: %s", answer)
		} else {
			b.WriteString("  (No answer provided)")
		}
	}
	return b.String()
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

	services, ready := h.platformApplicationServices()
	if !ready || services.Execution.HookResponses == nil {
		respondError(w, http.StatusServiceUnavailable, "platform hook response service is unavailable")
		return
	}

	err := services.Execution.HookResponses.RespondPermission(platformUIContext(r), application.RespondPermissionCommand{
		SessionID: sessionID,
		Response: application.HookPermissionResponse{
			Behavior: resp.Behavior, Message: resp.Message, ToolName: resp.ToolName,
			Answers: resp.Answers, PermissionSuggestions: resp.PermissionSuggestions,
		},
		Authorization: platformUIAuthorization(r),
	})
	if errors.Is(err, ErrNoPendingPermission) {
		respondError(w, http.StatusNotFound, "No pending permission request for this session")
		return
	}
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	log.Printf("Permission response sent for session %s: %s", sessionID, resp.Behavior)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// hookTokenAllowed verifies the X-Hook-Token against the session's stored hook
// token hash. Enforcement is keyed on token presence: a session with no stored
// hash (legacy/pre-Phase-0) fails open so running sessions never go dark; an
// unknown session id is rejected. This closes the spoofable-X-Session-ID hole
// without a bespoke auth path.
func (h *HookHandler) hookTokenAllowed(ctx context.Context, sessionID, token string) bool {
	if h.sessionMgr == nil {
		return true // no session store wired (isolated handler tests)
	}
	_, hookHash, err := h.sessionMgr.SessionTokenHashes(ctx, sessionID)
	if err != nil {
		return false // unknown session id — reject spoofed identities
	}
	if hookHash == "" {
		return true // legacy session without a minted token: fail open
	}
	return sessiontoken.EqualHash(token, hookHash)
}

// HandlePreToolUseGate handles POST /api/hooks/pretooluse — the SYNCHRONOUS
// conflict gate (Phase 5, L2). The bridge calls this for write-class tools and
// echoes the verdict to Claude Code. Because it answers the PreToolUse hook
// (which fires in every permission mode), it is the only gate that governs
// dangerously_skip_permissions sessions. Memory-only + fail-open: any error or
// non-opted path returns allow so the gate can never brick a session; a missing
// hook token is rejected 401 (a blocking gate over spoofable input would be a
// denial-of-work weapon).
func (h *HookHandler) HandlePreToolUseGate(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing X-Session-ID header")
		return
	}
	if !h.hookTokenAllowed(r.Context(), sessionID, r.Header.Get("X-Hook-Token")) {
		respondError(w, http.StatusUnauthorized, "invalid or missing hook token")
		return
	}
	rawBody, _ := io.ReadAll(r.Body)
	var hookEvent map[string]interface{}
	_ = json.Unmarshal(normalizeUTF8Body(rawBody), &hookEvent)
	toolName, _ := hookEvent["tool_name"].(string)
	toolInput, _ := hookEvent["tool_input"].(map[string]interface{})

	// The bridge routes PreToolUse HERE (synchronously) instead of the
	// fire-and-forget /api/hooks/event, so feed the async conflict index / turn
	// tracking exactly as HandleEvent's PreToolUse case would — the radar must
	// still see every touch even though the verdict is now synchronous.
	if h.OnToolEvent != nil {
		h.OnToolEvent(sessionID, "PreToolUse", hookEvent)
	}

	deny, reason := false, ""
	if h.GatePreToolUse != nil {
		deny, reason = h.GatePreToolUse(r.Context(), sessionID, toolName, toolInput)
	}
	out := preToolUseHookOutput{HookEventName: "PreToolUse"}
	if deny {
		out.PermissionDecision = "deny" // allow stays neutral (omitted) — see the type
		out.PermissionDecisionReason = reason
	}
	respondJSON(w, http.StatusOK, preToolUseGateOutput{HookSpecificOutput: out})
}

// HandleEvent handles POST /api/hooks/event
// Non-blocking: broadcasts hook events to the browser
func (h *HookHandler) HandleEvent(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "Missing X-Session-ID header")
		return
	}
	if !h.hookTokenAllowed(r.Context(), sessionID, r.Header.Get("X-Hook-Token")) {
		respondError(w, http.StatusUnauthorized, "invalid or missing hook token")
		return
	}

	rawBody, _ := io.ReadAll(r.Body)
	var hookEvent map[string]interface{}
	if err := json.Unmarshal(normalizeUTF8Body(rawBody), &hookEvent); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	backend := r.Header.Get("X-Backend")
	if backend != "" {
		hookEvent["backend"] = backend
	}
	h.trackClaudeProviderSessionID(sessionID, hookEvent, backend)

	eventName, _ := hookEvent["hook_event_name"].(string)
	if eventName == "SessionStart" && h.sessionMgr != nil {
		if model, ok := hookEvent["model"].(string); ok && strings.TrimSpace(model) != "" {
			go func() {
				if err := h.sessionMgr.RecordEffectiveModel(context.Background(), sessionID, model); err != nil {
					log.Printf("[hooks] Failed to persist effective model for session %s: %v", sessionID, err)
				}
			}()
		}
	}

	// Normalize Copilot/ACP event names to Claude Code equivalents
	if r.Header.Get("X-Backend") == "copilot" || r.Header.Get("X-Backend") == "acp" {
		eventName = normalizeCopilotEventName(eventName)
		hookEvent["hook_event_name"] = eventName
	}

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

	// Conflict-radar tap: tool phases carry tool_input (file paths), Stop
	// marks a completed turn. The callback only enqueues — no I/O here.
	if h.OnToolEvent != nil {
		switch eventName {
		case "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop":
			h.OnToolEvent(sessionID, eventName, hookEvent)
		}
	}

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
		if h.OnReconcileEffectiveModel != nil {
			go h.OnReconcileEffectiveModel(sessionID)
		}
	case "opencode_session_info":
		if h.sessionMgr != nil {
			if providerID, ok := hookEvent["provider_session_id"].(string); ok && strings.TrimSpace(providerID) != "" {
				h.sessionMgr.PersistProviderSessionID(context.Background(), sessionID, providerID, "OpenCode")
			}
		}
	case "opencode_plan_updated":
		if h.OnPlanUpdated != nil {
			if plan, ok := hookEvent["plan"].(string); ok && strings.TrimSpace(plan) != "" {
				go h.OnPlanUpdated(sessionID, strings.TrimSpace(plan))
			}
		}
	case "Notification":
		if msg, ok := hookEvent["message"].(string); ok {
			if strings.Contains(msg, "waiting for your input") || strings.Contains(msg, "awaiting") {
				h.cancelIdleTimer(sessionID)
				h.setSessionMode(sessionID, "idle", "event=Notification")
			}
		}
	case "acp_session_info", "acp_usage_update":
		// Track ACP usage info (model, premium requests)
		h.updateACPUsage(sessionID, hookEvent)
	case "mode_changed":
		// Direct mode update from ACP agent (idle/executing)
		if mode, ok := hookEvent["mode"].(string); ok {
			if mode == "idle" {
				h.cancelIdleTimer(sessionID)
			} else {
				h.resetIdleTimer(sessionID)
			}
			h.setSessionMode(sessionID, mode, "event=mode_changed")
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
		h.wakePromptWaiters(sessionID)
	}

	w.WriteHeader(http.StatusOK)
}

// RegisterPromptWaiter returns a channel that fires when the session's next
// UserPromptSubmit hook arrives — the reliable "the agent accepted the input"
// signal for claude_code PTY sessions. The cancel func must be called to avoid
// leaking the waiter if it is abandoned before firing.
func (h *HookHandler) RegisterPromptWaiter(sessionID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.promptWaiters[sessionID] = append(h.promptWaiters[sessionID], ch)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		waiters := h.promptWaiters[sessionID]
		for i, w := range waiters {
			if w == ch {
				h.promptWaiters[sessionID] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		if len(h.promptWaiters[sessionID]) == 0 {
			delete(h.promptWaiters, sessionID)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

func (h *HookHandler) wakePromptWaiters(sessionID string) {
	h.mu.Lock()
	waiters := h.promptWaiters[sessionID]
	delete(h.promptWaiters, sessionID)
	h.mu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WaitForPromptSubmit blocks until the session's next UserPromptSubmit hook or
// the deadline. Used by send-with-ack.
func (h *HookHandler) WaitForPromptSubmit(ctx context.Context, sessionID string, timeout time.Duration) bool {
	ch, cancel := h.RegisterPromptWaiter(sessionID)
	defer cancel()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	case <-ctx.Done():
		return false
	}
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
// Returns (triggered, resultCh) where triggered indicates if evaluation was started,
// and resultCh receives true if suggestions were generated.
func (h *HookHandler) triggerSessionEvaluation(sessionID, trigger string) (bool, <-chan bool) {
	if h.OnEvaluateSession == nil {
		return false, nil
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
		return false, nil
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
	h.mu.Lock()
	imagePrompt := h.imagePromptMeta[sessionID]
	delete(h.imagePromptMeta, sessionID)
	h.mu.Unlock()

	if imagePrompt != "" {
		hint := fmt.Sprintf("\n[OpenPoet context: User submitted input with image(s). User's text prompt: \"%s\"]\n", imagePrompt)
		output = append(output, []byte(hint)...)
	}

	log.Printf("[hooks] Triggering session evaluation: session=%s trigger=%s outputLen=%d hasImageMeta=%v", sessionID, trigger, len(output), imagePrompt != "")
	resultCh := make(chan bool, 1)
	go func() {
		resultCh <- h.OnEvaluateSession(sessionID, trigger, output)
	}()
	return true, resultCh
}

// updateACPUsage extracts ACP model/usage data from a hook event and stores it.
func (h *HookHandler) updateACPUsage(sessionID string, hookEvent map[string]interface{}) {
	h.mu.Lock()
	info := h.acpUsage[sessionID]
	if info == nil {
		info = &ACPUsageInfo{}
		h.acpUsage[sessionID] = info
	}

	if model, ok := hookEvent["current_model"].(string); ok && model != "" {
		info.CurrentModel = model
	}
	if mult, ok := hookEvent["model_multiplier"].(string); ok && mult != "" {
		info.ModelMultiplier = mult
	}
	if pr, ok := hookEvent["premium_requests"].(float64); ok {
		info.PremiumRequests = int(pr)
	}
	if models, ok := hookEvent["available_models"].([]interface{}); ok && len(models) > 0 {
		info.AvailableModels = make([]ACPAvailableModel, 0, len(models))
		for _, m := range models {
			if mm, ok := m.(map[string]interface{}); ok {
				am := ACPAvailableModel{}
				if id, ok := mm["id"].(string); ok {
					am.ID = id
				}
				if name, ok := mm["name"].(string); ok {
					am.Name = name
				}
				if mult, ok := mm["multiplier"].(string); ok {
					am.Multiplier = mult
				}
				info.AvailableModels = append(info.AvailableModels, am)
			}
		}
	}
	h.mu.Unlock()

	// Broadcast the updated usage info to the frontend
	h.hub.BroadcastStateUpdate("session", map[string]interface{}{
		"action":     "acp_usage",
		"session_id": sessionID,
		"acp_usage":  info,
	})
}

// GetACPUsage returns the current ACP usage info for a session.
func (h *HookHandler) GetACPUsage(sessionID string) *ACPUsageInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acpUsage[sessionID]
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
	delete(h.acpUsage, sessionID)
	// Drop any prompt waiters WITHOUT signaling them: a torn-down session never
	// accepted the pending input, so the ack must time out to acknowledged=false
	// rather than fabricate a true ack from teardown.
	delete(h.promptWaiters, sessionID)
	if t, ok := h.evalTimers[sessionID]; ok {
		t.Stop()
		delete(h.evalTimers, sessionID)
	}
	if pending, ok := h.pending[sessionID]; ok {
		pending.cancel()
		delete(h.pending, sessionID)
	}
}

// ScheduleSessionEvaluation mirrors Claude Code's UserPromptSubmit hook for
// backends whose prompts enter OpenPoet directly, such as Codex.
func (h *HookHandler) ScheduleSessionEvaluation(sessionID string) {
	h.scheduleDebouncedEval(sessionID)
}

// evalRetryDelay is how long to wait before retrying evaluation when the first
// immediate evaluation produced no suggestions. By then, Claude Code has had time
// to process the prompt and produce output, giving the evaluator richer context.
const evalRetryDelay = 40 * time.Second

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

	services, ready := h.platformApplicationServices()
	if !ready || services.Execution.Sessions == nil {
		respondError(w, http.StatusServiceUnavailable, "platform session service is unavailable")
		return
	}
	if err := services.Execution.Sessions.StoreImagePromptHint(platformUIContext(r), application.StoreImagePromptHintCommand{
		SessionID:     sessionID,
		UserPrompt:    input.UserPrompt,
		ImageCount:    input.ImageCount,
		Authorization: platformUIAuthorization(r),
	}); err != nil {
		respondApplicationError(w, err)
		return
	}

	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	log.Printf("[hooks] Image prompt hint stored: session=%s imageCount=%d promptLen=%d",
		shortID, input.ImageCount, len(input.UserPrompt))

	w.WriteHeader(http.StatusOK)
}

// scheduleDebouncedEval triggers an immediate session evaluation on user prompt,
// with a single retry after evalRetryDelay if no suggestions are generated.
// Each new UserPromptSubmit cancels any pending retry and starts fresh.
func (h *HookHandler) scheduleDebouncedEval(sessionID string) {
	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	// Cancel any pending retry timer from a previous prompt
	h.mu.Lock()
	if t, ok := h.evalTimers[sessionID]; ok {
		t.Stop()
		delete(h.evalTimers, sessionID)
	}
	h.mu.Unlock()

	// Guard 1: session still running?
	if h.sessionMgr == nil || !h.sessionMgr.IsSessionRunning(sessionID) {
		log.Printf("[hooks] Immediate eval skipped: session %s not running", shortID)
		return
	}

	// Guard 2: session already has linked task?
	if h.HasLinkedTask != nil && h.HasLinkedTask(sessionID) {
		log.Printf("[hooks] Immediate eval skipped: session %s already has linked task", shortID)
		return
	}

	// Guard 3: recent suggestions exist?
	if h.HasRecentSuggestions != nil && h.HasRecentSuggestions(sessionID) {
		log.Printf("[hooks] Immediate eval skipped: session %s has recent suggestions", shortID)
		return
	}

	// Fire evaluation IMMEDIATELY
	log.Printf("[hooks] Immediate eval firing for session %s", shortID)
	triggered, resultCh := h.triggerSessionEvaluation(sessionID, "user_prompt")
	if !triggered {
		return // rate-limited
	}

	// Wait for result in background; schedule retry if no suggestions
	go func() {
		gotSuggestions := <-resultCh
		if gotSuggestions {
			log.Printf("[hooks] Immediate eval produced suggestions for session %s, no retry needed", shortID)
			return
		}

		log.Printf("[hooks] Immediate eval produced no suggestions for session %s, scheduling retry in %v", shortID, evalRetryDelay)
		h.mu.Lock()
		h.evalTimers[sessionID] = time.AfterFunc(evalRetryDelay, func() {
			h.mu.Lock()
			delete(h.evalTimers, sessionID)
			h.mu.Unlock()

			// Re-check guards before retrying
			if h.sessionMgr == nil || !h.sessionMgr.IsSessionRunning(sessionID) {
				log.Printf("[hooks] Retry eval skipped: session %s not running", shortID)
				return
			}
			if h.HasLinkedTask != nil && h.HasLinkedTask(sessionID) {
				log.Printf("[hooks] Retry eval skipped: session %s already has linked task", shortID)
				return
			}
			if h.HasRecentSuggestions != nil && h.HasRecentSuggestions(sessionID) {
				log.Printf("[hooks] Retry eval skipped: session %s has recent suggestions", shortID)
				return
			}

			log.Printf("[hooks] Retry eval firing for session %s", shortID)
			h.triggerSessionEvaluation(sessionID, "user_prompt_retry")
		})
		h.mu.Unlock()
	}()
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

	services, ready := h.platformApplicationServices()
	if !ready || services.Execution.HookResponses == nil {
		respondError(w, http.StatusServiceUnavailable, "platform hook response service is unavailable")
		return
	}
	if err := services.Execution.HookResponses.RespondTaskNotification(platformUIContext(r), application.RespondTaskNotificationCommand{
		SessionID:     sessionID,
		Authorization: platformUIAuthorization(r),
	}); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
