package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openpoet/internal/websocket"
)

func postHookEvent(t *testing.T, h *HookHandler, sessionID, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/event", strings.NewReader(body))
	req.Header.Set("X-Session-ID", sessionID)
	rr := httptest.NewRecorder()
	h.HandleEvent(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("hook event status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// The d1f920bb sequence: a prompt, a Bash run that outlives the 15 s
// inactivity timer, then the end of the turn. Only the prompt and the agent
// coming back to its prompt move the turn; tool events, the inactivity timer
// and a compaction do not.
func TestHookTurnStaysOpenAcrossLongToolRun(t *testing.T) {
	hub := websocket.NewHub()
	go hub.Run()
	h := NewHookHandler(hub, nil, nil)
	if _, known := h.SessionTurnState("s1"); known {
		t.Fatal("turn known before any hook")
	}
	assertTurn := func(step string, open bool) {
		t.Helper()
		turn, known := h.SessionTurnState("s1")
		if !known || turn.Open != open {
			t.Fatalf("%s: turn=%+v known=%v, want open=%v", step, turn, known, open)
		}
	}

	postHookEvent(t, h, "s1", `{"hook_event_name":"UserPromptSubmit","permission_mode":"default","prompt":"deploy"}`)
	assertTurn("prompt accepted", true)
	postHookEvent(t, h, "s1", `{"hook_event_name":"PreToolUse","permission_mode":"default","tool_name":"Bash"}`)
	h.setSessionMode("s1", "idle", "inactivity-timeout")
	assertTurn("inactivity timer during Bash", true)
	postHookEvent(t, h, "s1", `{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`)
	postHookEvent(t, h, "s1", `{"hook_event_name":"SessionStart","source":"compact"}`)
	postHookEvent(t, h, "s1", `{"hook_event_name":"PostToolUse","permission_mode":"default","tool_name":"Bash"}`)
	assertTurn("tool done, reply still streaming", true)
	postHookEvent(t, h, "s1", `{"hook_event_name":"Stop","permission_mode":"default"}`)
	assertTurn("stop", false)

	// An interrupted turn sends no Stop; the idle notification closes it.
	postHookEvent(t, h, "s1", `{"hook_event_name":"UserPromptSubmit","permission_mode":"default"}`)
	postHookEvent(t, h, "s1", `{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`)
	assertTurn("idle notification", false)

	// A restarted process is back at its prompt.
	postHookEvent(t, h, "s1", `{"hook_event_name":"UserPromptSubmit","permission_mode":"default"}`)
	postHookEvent(t, h, "s1", `{"hook_event_name":"SessionStart","source":"resume"}`)
	assertTurn("resumed", false)

	h.ClearSession("s1")
	if _, known := h.SessionTurnState("s1"); known {
		t.Fatal("turn survived ClearSession")
	}
}
