package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"openpoet/internal/websocket"
)

// TestHookRecordsSubmittedPromptBeforeAck pins that a send-with-ack, once
// woken by UserPromptSubmit, can read the prompt the agent received.
func TestHookRecordsSubmittedPromptBeforeAck(t *testing.T) {
	hub := websocket.NewHub()
	go hub.Run()
	h := NewHookHandler(hub, nil, nil)
	if _, ok := h.SubmittedPrompt("s1"); ok {
		t.Fatal("prompt known before any hook")
	}
	ack, cancel := h.RegisterPromptWaiter("s1")
	defer cancel()

	prompt := strings.Repeat("confira a configuração ", 60) + "\nfim"
	body, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "permission_mode": "default", "prompt": prompt})
	postHookEvent(t, h, "s1", string(body))

	select {
	case <-ack:
	default:
		t.Fatal("waiter not woken")
	}
	if got, ok := h.SubmittedPrompt("s1"); !ok || got != prompt {
		t.Fatalf("submitted prompt = %d chars (known %v), want the %d-char prompt", len(got), ok, len(prompt))
	}
}
