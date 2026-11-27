package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"openpoet/internal/application"
)

// Hook delivery headers the bridge sends with every event. A retried POST
// keeps its event id (the server drops the copy) and its emission time, in
// nanoseconds on the agent's host clock, so a late event cannot undo a newer
// turn change. Older bridges send neither and keep the arrival-order behavior.
const (
	hookEventIDHeader = "X-Hook-Event-ID"
	hookEventAtHeader = "X-Hook-Event-At"
	// hookRecentEventIDs is how many delivered event ids per session are
	// remembered for deduplication.
	hookRecentEventIDs = 128
)

// hookTurnActivity is the per-session hook detail behind the turn: what the
// operator needs to diagnose a stuck turn, and the epoch a reconciliation is
// checked against.
type hookTurnActivity struct {
	epoch        uint64
	lastEvent    string
	lastEventAt  time.Time
	lastPromptAt time.Time
	pendingTools map[string]application.SessionPendingTool
	permTimeout  *application.SessionPermissionTimeout
	// stateAt is the emission time of the newest event that set or confirmed
	// the turn state (0 when no event carried one).
	stateAt    int64
	recentIDs  []string
	duplicates int
	stale      int
}

func newHookBootID() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(raw[:])
}

// activityLocked returns the session's activity record. Callers hold h.mu.
func (h *HookHandler) activityLocked(sessionID string) *hookTurnActivity {
	if h.turnActivity == nil {
		h.turnActivity = make(map[string]*hookTurnActivity)
	}
	activity, ok := h.turnActivity[sessionID]
	if !ok {
		activity = &hookTurnActivity{pendingTools: map[string]application.SessionPendingTool{}}
		h.turnActivity[sessionID] = activity
	}
	return activity
}

// hookEventEmittedAt parses the bridge's emission time (0 when absent).
func hookEventEmittedAt(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get(hookEventAtHeader))
	if raw == "" || len(raw) > 24 {
		return 0
	}
	at, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || at <= 0 {
		return 0
	}
	return at
}

// duplicateHookDelivery remembers the event id and reports whether it was
// already delivered (a bridge retry after a response it never received).
func (h *HookHandler) duplicateHookDelivery(sessionID, eventID string) bool {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" || len(eventID) > 200 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	activity := h.activityLocked(sessionID)
	for _, seen := range activity.recentIDs {
		if seen == eventID {
			activity.duplicates++
			return true
		}
	}
	activity.recentIDs = append(activity.recentIDs, eventID)
	if len(activity.recentIDs) > hookRecentEventIDs {
		activity.recentIDs = activity.recentIDs[len(activity.recentIDs)-hookRecentEventIDs:]
	}
	return false
}

// noteHookEvent records that the session sent a hook event. Every event moves
// the epoch, so a reconciliation checked before it no longer applies.
func (h *HookHandler) noteHookEvent(sessionID, eventName string, hookEvent map[string]interface{}) {
	now := time.Now()
	toolUseID, _ := hookEvent["tool_use_id"].(string)
	toolName, _ := hookEvent["tool_name"].(string)
	h.mu.Lock()
	defer h.mu.Unlock()
	activity := h.activityLocked(sessionID)
	activity.epoch++
	activity.lastEvent = eventName
	activity.lastEventAt = now
	switch eventName {
	case "UserPromptSubmit":
		activity.lastPromptAt = now
	case "PreToolUse":
		key := toolUseID
		if key == "" {
			key = "?" + toolName + "#" + strconv.FormatUint(activity.epoch, 10)
		}
		activity.pendingTools[key] = application.SessionPendingTool{ID: toolUseID, Name: toolName, Since: now}
	case "PostToolUse", "PostToolUseFailure":
		if toolUseID != "" {
			delete(activity.pendingTools, toolUseID)
			break
		}
		// No id: retire the oldest anonymous call of that tool.
		var oldest string
		for key, tool := range activity.pendingTools {
			if tool.ID == "" && tool.Name == toolName && (oldest == "" || tool.Since.Before(activity.pendingTools[oldest].Since)) {
				oldest = key
			}
		}
		if oldest != "" {
			delete(activity.pendingTools, oldest)
		}
	}
}

// notePermissionTimeout records a permission request nobody answered.
func (h *HookHandler) notePermissionTimeout(sessionID, toolName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activityLocked(sessionID).permTimeout = &application.SessionPermissionTimeout{At: time.Now(), Tool: toolName}
}

func (h *HookHandler) turnTokenLocked(activity *hookTurnActivity) string {
	var epoch uint64
	if activity != nil {
		epoch = activity.epoch
	}
	return h.bootID + "." + strconv.FormatUint(epoch, 10)
}

// SessionTurnActivity implements application.SessionTurnControl.
func (h *HookHandler) SessionTurnActivity(sessionID string) application.SessionTurnActivity {
	h.mu.Lock()
	defer h.mu.Unlock()
	turn, known := h.turns[sessionID]
	_, parked := h.pending[sessionID]
	out := application.SessionTurnActivity{
		Turn: turn, Known: known, PendingPermission: parked,
		LastPrompt: h.submittedPrompts[sessionID],
	}
	activity := h.turnActivity[sessionID]
	out.Token = h.turnTokenLocked(activity)
	if activity == nil {
		return out
	}
	out.LastEvent, out.LastEventAt, out.LastPromptAt = activity.lastEvent, activity.lastEventAt, activity.lastPromptAt
	out.Duplicates, out.Stale = activity.duplicates, activity.stale
	if activity.permTimeout != nil {
		timeout := *activity.permTimeout
		out.PermissionTimeout = &timeout
	}
	for _, tool := range activity.pendingTools {
		out.PendingTools = append(out.PendingTools, tool)
	}
	sort.Slice(out.PendingTools, func(i, j int) bool { return out.PendingTools[i].Since.Before(out.PendingTools[j].Since) })
	return out
}

// CloseTurnIfUnchanged implements application.SessionTurnControl: it closes
// the open turn only when no hook event arrived since token was issued and no
// permission request is parked. It changes the turn state and nothing else.
func (h *HookHandler) CloseTurnIfUnchanged(sessionID, token, reason string) (application.SessionTurnState, application.SessionTurnState, error) {
	h.mu.Lock()
	activity := h.turnActivity[sessionID]
	if h.turnTokenLocked(activity) != token {
		h.mu.Unlock()
		return application.SessionTurnState{}, application.SessionTurnState{}, application.ErrTurnChanged
	}
	previous, known := h.turns[sessionID]
	if !known || !previous.Open {
		h.mu.Unlock()
		return previous, previous, application.ErrTurnNotOpen
	}
	if _, parked := h.pending[sessionID]; parked {
		h.mu.Unlock()
		return previous, previous, application.ErrTurnPermissionParked
	}
	turn := application.SessionTurnState{Open: false, Since: time.Now(), Reason: reason}
	h.turns[sessionID] = turn
	if activity != nil {
		activity.epoch++ // a token is good for one reconciliation
		activity.pendingTools = map[string]application.SessionPendingTool{}
	}
	observer := h.turnObserver
	h.mu.Unlock()
	if observer != nil {
		observer(sessionID, turn)
	}
	log.Printf("[hooks] Turn closed: session=%s (%s; was open since %s by %s)", shortSessionID(sessionID), reason, previous.Since.UTC().Format(time.RFC3339), previous.Reason)
	return previous, turn, nil
}
