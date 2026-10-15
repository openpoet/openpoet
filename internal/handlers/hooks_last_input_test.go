package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessioninput"
	"openpoet/internal/websocket"
)

func postBackendHookEvent(t *testing.T, h *HookHandler, sessionID, backend string, event map[string]any) {
	t.Helper()
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/event", strings.NewReader(string(body)))
	req.Header.Set("X-Session-ID", sessionID)
	if backend != "" {
		req.Header.Set("X-Backend", backend)
	}
	rr := httptest.NewRecorder()
	h.HandleEvent(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("hook event status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func newLastInputHookHandler() *HookHandler {
	hub := websocket.NewHub()
	go hub.Run()
	return NewHookHandler(hub, nil, nil)
}

var orqInput = sessioninput.Expected{
	Text: "Revise o PR 12 e responda no canal", Actor: "automation_client:orq", ActorType: "automation_client",
	CommandID: "orq-77", Via: sessioninput.ViaSendInput,
}

// Claude Code: UserPromptSubmit carries the prompt; an announced input takes
// its sender, anything else was typed by a human.
func TestHookAttributesClaudeCodePrompts(t *testing.T) {
	h := newLastInputHookHandler()
	if _, ok := h.LastSessionInput("s1"); ok {
		t.Fatal("last input known before any prompt")
	}
	h.ExpectSessionInput("s1", orqInput)
	postBackendHookEvent(t, h, "s1", "", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": orqInput.Text})
	if got, _ := h.LastSessionInput("s1"); got.Source != sessioninput.SourceAutomation || got.CommandID != "orq-77" {
		t.Fatalf("automation prompt = %+v", got)
	}
	postBackendHookEvent(t, h, "s1", "", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "e o teste do login?"})
	if got, _ := h.LastSessionInput("s1"); got.Source != sessioninput.SourceHuman || got.Via != sessioninput.ViaTerminal {
		t.Fatalf("typed prompt = %+v", got)
	}
	// Tool events and Stop do not touch the attribution.
	postBackendHookEvent(t, h, "s1", "", map[string]any{"hook_event_name": "Stop"})
	if got, _ := h.LastSessionInput("s1"); got.Source != sessioninput.SourceHuman {
		t.Fatalf("after Stop = %+v", got)
	}
}

// Codex app-server reports the turn's prompt on mode_changed=executing.
func TestHookAttributesCodexTurnPrompt(t *testing.T) {
	h := newLastInputHookHandler()
	h.ExpectSessionInput("s1", orqInput)
	postBackendHookEvent(t, h, "s1", "", map[string]any{"hook_event_name": "mode_changed", "mode": "executing", "prompt": orqInput.Text})
	if got, _ := h.LastSessionInput("s1"); got.Source != sessioninput.SourceAutomation || got.CommandID != "orq-77" {
		t.Fatalf("codex turn prompt = %+v", got)
	}
}

// ACP sends mode_changed=executing (no prompt) and then a textless
// userPromptSubmitted: the prompt is observed once, by timing.
func TestHookAttributesTextlessACPPromptOnce(t *testing.T) {
	h := newLastInputHookHandler()
	h.ExpectSessionInput("s1", orqInput)
	postBackendHookEvent(t, h, "s1", "acp", map[string]any{"hook_event_name": "mode_changed", "mode": "executing"})
	if _, ok := h.LastSessionInput("s1"); ok {
		t.Fatal("mode_changed without a prompt was observed")
	}
	postBackendHookEvent(t, h, "s1", "acp", map[string]any{"hook_event_name": "userPromptSubmitted"})
	if got, _ := h.LastSessionInput("s1"); got.Source != sessioninput.SourceAutomation || got.Basis != sessioninput.BasisTiming {
		t.Fatalf("acp prompt = %+v", got)
	}
}

// platform.session.* events (stopped, input_sent, awaiting_input, ...) carry
// last_input when it is known and keep their bare payload otherwise.
func TestSessionChangeEventsCarryLastInput(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "last-input.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := newLastInputHookHandler()
	effects := &platformEffects{api: &API{hookHandler: h}, db: db}

	effects.PublishSessionChange(ctx, application.SessionChange{Action: "stopped", ID: "s-none"})
	h.ExpectSessionInput("s1", orqInput)
	postBackendHookEvent(t, h, "s1", "", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": orqInput.Text})
	effects.PublishSessionChange(ctx, application.SessionChange{Action: "stopped", ID: "s1"})

	payload := func(id string) map[string]any {
		var raw string
		if err := db.GetContext(ctx, &raw, `SELECT payload_json FROM event_outbox WHERE event_type = 'platform.session.stopped' AND aggregate_id = ?`, id); err != nil {
			t.Fatalf("outbox stopped event for %s: %v", id, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	if got := payload("s-none"); len(got) != 1 || got["action"] != "stopped" {
		t.Fatalf("payload without a known input = %v", got)
	}
	got, _ := payload("s1")["last_input"].(map[string]any)
	if got["source"] != "automation" || got["actor"] != "automation_client:orq" || got["command_id"] != "orq-77" {
		t.Fatalf("stopped last_input = %v", got)
	}
}
