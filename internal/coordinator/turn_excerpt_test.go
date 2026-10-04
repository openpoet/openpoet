package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStopHookLastMessageBecomesTurnExcerpt: the agent's final reply carried
// by a Stop hook lands in session.turn_completed as a one-line excerpt.
func TestStopHookLastMessageBecomesTurnExcerpt(t *testing.T) {
	c, _ := testCoordinator(t, map[string]*sessionInfo{"s1": localSession("s1", 42, "/home/dev/project", 0)})
	c.OnHookEvent("s1", "Stop", map[string]interface{}{"last_assistant_message": "Done.\n\nAll   three files read."})
	c.process(<-c.ch)

	if len(c.pendingEvents) != 1 || c.pendingEvents[0].EventType != "session.turn_completed" {
		t.Fatalf("pending events = %+v, want one session.turn_completed", c.pendingEvents)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(c.pendingEvents[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["last_message"] != "Done. All three files read." {
		t.Fatalf("last_message = %q", payload["last_message"])
	}

	c.OnHookEvent("s1", "Stop", map[string]interface{}{})
	c.process(<-c.ch)
	payload = map[string]any{}
	if err := json.Unmarshal([]byte(c.pendingEvents[1].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["last_message"]; present {
		t.Fatalf("a Stop without a reply must omit last_message: %v", payload)
	}
}

func TestTurnExcerptIsBounded(t *testing.T) {
	got := turnExcerpt(strings.Repeat("palavra ", 200))
	if runes := []rune(got); len(runes) != turnExcerptMaxRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("excerpt of %d runes (want %d + …): %q", len(runes), turnExcerptMaxRunes, got)
	}
	if got := turnExcerpt("ação concluída"); got != "ação concluída" {
		t.Fatalf("short excerpt changed: %q", got)
	}
}
