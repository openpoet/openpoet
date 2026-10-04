package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCodexTurnLifecyclePostsHooks: a Codex turn reports its start
// (mode_changed=executing) and its end (Stop with the turn's last assistant
// message, not an earlier turn's) to the OpenPoet hook endpoint, in order.
func TestCodexTurnLifecyclePostsHooks(t *testing.T) {
	received := make(chan map[string]interface{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hooks/event" || r.Header.Get("X-Backend") != "codex" || r.Header.Get("X-Session-ID") != "session-1" {
			http.Error(w, "unexpected hook request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var event map[string]interface{}
		_ = json.Unmarshal(body, &event)
		received <- event
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	r := &CodexRunner{
		outputHandler:    func([]byte) {},
		done:             make(chan struct{}),
		interruptedTurns: make(map[string]bool),
		commandProcesses: make(map[string]map[string]codexCommandProcess),
		cfg:              &SessionConfig{SessionID: "session-1", ServerAddr: strings.TrimPrefix(server.URL, "http://")},
	}
	t.Cleanup(func() { close(r.done) })

	r.addCodexTranscriptBlock("assistant", "reply from an earlier turn", "", "", "")
	r.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-1"}}`))
	r.handleNotification("item/agentMessage/delta", json.RawMessage(`{"turnId":"turn-1","itemId":"a1","delta":"Vou ler "}`))
	r.handleNotification("item/started", json.RawMessage(`{"turnId":"turn-1","item":{"type":"commandExecution","id":"c1","command":"ls"}}`))
	r.handleNotification("item/agentMessage/delta", json.RawMessage(`{"turnId":"turn-1","itemId":"a2","delta":"Pronto: "}`))
	r.handleNotification("item/agentMessage/delta", json.RawMessage(`{"turnId":"turn-1","itemId":"a2","delta":"três arquivos."}`))
	r.handleNotification("turn/completed", json.RawMessage(`{"turn":{"id":"turn-1","status":"completed"}}`))

	started := waitHook(t, received)
	if started["hook_event_name"] != "mode_changed" || started["mode"] != "executing" || started["turn_id"] != "turn-1" {
		t.Fatalf("first hook = %v, want mode_changed executing for turn-1", started)
	}
	stopped := waitHook(t, received)
	if stopped["hook_event_name"] != "Stop" || stopped["turn_status"] != "completed" {
		t.Fatalf("second hook = %v, want Stop completed", stopped)
	}
	if stopped["last_assistant_message"] != "Pronto: três arquivos." {
		t.Fatalf("last_assistant_message = %q", stopped["last_assistant_message"])
	}

	r.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-2"}}`))
	r.handleNotification("turn/completed", json.RawMessage(`{"turn":{"id":"turn-2","status":"failed","error":{"message":"boom"}}}`))
	waitHook(t, received)
	failed := waitHook(t, received)
	if failed["hook_event_name"] != "Stop" || failed["turn_status"] != "failed" {
		t.Fatalf("failed turn hook = %v", failed)
	}
	if _, present := failed["last_assistant_message"]; present {
		t.Fatalf("a turn without a reply must not repeat the previous one: %v", failed)
	}
}

func waitHook(t *testing.T, received <-chan map[string]interface{}) map[string]interface{} {
	t.Helper()
	select {
	case event := <-received:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("hook not posted")
		return nil
	}
}
