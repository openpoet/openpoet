package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendToSessionForwardsGuardAndReportsAck(t *testing.T) {
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/s1/input" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		sent = nil
		_ = json.Unmarshal(body, &sent)
		fmt.Fprint(w, `{"status":"sent","acknowledged":true}`)
	}))
	t.Cleanup(server.Close)

	result, err := executeTool(NewAPIClient(server.URL), "openpoet_send_to_session",
		json.RawMessage(`{"session_id":"s1","text":"leia o doc","if_idle":true,"await_ack":true}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if sent["if_idle"] != true || sent["await_ack"] != true || sent["text"] != "leia o doc" {
		t.Fatalf("forwarded body = %v", sent)
	}
	if !strings.Contains(result, "Acknowledged: the agent accepted the prompt.") {
		t.Fatalf("result = %q", result)
	}

	// Plain sends keep their old body and output.
	result, err = executeTool(NewAPIClient(server.URL), "openpoet_send_to_session", json.RawMessage(`{"session_id":"s1","text":"oi"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, guarded := sent["if_idle"]; guarded || strings.Contains(result, "Acknowledged") {
		t.Fatalf("plain send body=%v result=%q", sent, result)
	}
}

func TestGetSessionShowsTurn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sessions/s1":
			fmt.Fprint(w, `{"id":"s1","project_id":0,"status":"running","name":"worker",
				"turn":{"open":true,"since":"2026-09-29T23:52:25Z","reason":"UserPromptSubmit"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	result, err := executeTool(NewAPIClient(server.URL), "openpoet_get_session", json.RawMessage(`{"session_id":"s1"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Turn: open (mid-turn) since ") || !strings.Contains(result, "(UserPromptSubmit)") {
		t.Fatalf("result =\n%s", result)
	}
}
