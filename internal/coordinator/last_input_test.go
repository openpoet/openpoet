package coordinator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"openpoet/internal/sessioninput"
)

func eventPayload(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// session.turn_completed and session.awaiting_input carry last_input when the
// origin of the session's last prompt is known, and omit it otherwise
// (additive: SchemaVersion stays 1).
func TestSessionEventsCarryLastInput(t *testing.T) {
	sentAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	last := &sessioninput.LastInput{
		Source: sessioninput.SourceAutomation, Actor: "automation_client:orq", CommandID: "orq-9",
		Via: sessioninput.ViaSendInput, Basis: sessioninput.BasisPromptText, At: sentAt.Add(time.Second), SentAt: &sentAt,
	}
	for name, ev := range map[string]struct {
		with, without string
		schema        int
	}{
		"turn_completed": {
			turnCompletedEvent("s1", 7, nil, "", "", last, time.Now()).PayloadJSON,
			turnCompletedEvent("s1", 7, nil, "", "", nil, time.Now()).PayloadJSON,
			turnCompletedEvent("s1", 7, nil, "", "", last, time.Now()).SchemaVersion,
		},
		"awaiting_input": {
			awaitingInputEvent("s1", 7, "permission", "Allow?", last, time.Now()).PayloadJSON,
			awaitingInputEvent("s1", 7, "permission", "Allow?", nil, time.Now()).PayloadJSON,
			awaitingInputEvent("s1", 7, "permission", "Allow?", last, time.Now()).SchemaVersion,
		},
	} {
		got, ok := eventPayload(t, ev.with)["last_input"].(map[string]any)
		if !ok || got["source"] != "automation" || got["actor"] != "automation_client:orq" || got["command_id"] != "orq-9" ||
			got["basis"] != "prompt_text" || got["at"] == nil || got["sent_at"] == nil {
			t.Fatalf("%s last_input = %v", name, got)
		}
		if _, present := eventPayload(t, ev.without)["last_input"]; present {
			t.Fatalf("%s carries last_input with no known origin", name)
		}
		if ev.schema != 1 {
			t.Fatalf("%s schema version = %d", name, ev.schema)
		}
	}
}

// The origin is read when Stop arrives: a prompt accepted before the turn
// event is processed must not be credited to the turn that just ended.
func TestTurnCompletedTakesLastInputAtStop(t *testing.T) {
	c := New(nil)
	origin := sessioninput.LastInput{Source: sessioninput.SourceHuman, Basis: sessioninput.BasisPromptText}
	c.LastInput = func(string) (sessioninput.LastInput, bool) { return origin, true }
	c.OnHookEvent("s1", "Stop", map[string]interface{}{})
	origin = sessioninput.LastInput{Source: sessioninput.SourceAutomation}
	msg := <-c.ch
	if msg.lastInput == nil || msg.lastInput.Source != sessioninput.SourceHuman {
		t.Fatalf("turn message last input = %+v", msg.lastInput)
	}
}

func TestRecordAttentionCarriesLastInput(t *testing.T) {
	c := New(nil)
	c.resolveSession = func(_ context.Context, id string) (*sessionInfo, error) {
		return &sessionInfo{id: id, projectID: 3}, nil
	}
	c.LastInput = func(string) (sessioninput.LastInput, bool) {
		return sessioninput.LastInput{Source: sessioninput.SourceAutomation, CommandID: "orq-3"}, true
	}
	c.RecordAttention("s1", "permission", "Allow?")
	if len(c.pendingEvents) != 1 {
		t.Fatalf("pending events = %d", len(c.pendingEvents))
	}
	got, _ := eventPayload(t, c.pendingEvents[0].PayloadJSON)["last_input"].(map[string]any)
	if got["command_id"] != "orq-3" {
		t.Fatalf("awaiting_input last_input = %v", got)
	}
}
