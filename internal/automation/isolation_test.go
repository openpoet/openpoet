package automation

import (
	"encoding/json"
	"testing"
)

// TestProjectScopeTagAndRestricted covers the group-tag and restricted helpers
// added so blackboard/group-scoped surfaces can be confined.
func TestProjectScopeTagAndRestricted(t *testing.T) {
	unr := &ProjectScopeSet{Unrestricted: true}
	if !unr.AllowsTag(9) || unr.Restricted() {
		t.Fatal("unrestricted scope must allow any tag and not be restricted")
	}
	s := &ProjectScopeSet{Allowed: map[int64]bool{1: true}, AllowedTags: map[int64]bool{7: true}}
	if !s.Restricted() {
		t.Fatal("a real filter must report Restricted()")
	}
	if !s.AllowsTag(7) || s.AllowsTag(8) || s.AllowsTag(0) {
		t.Fatal("AllowsTag must gate on the filter's tag_ids")
	}
	if !s.Allows(1) || s.Allows(2) {
		t.Fatal("Allows must gate on the filter's project_ids")
	}
}

// TestFilterEventsByScope: the event surfaces (poll + push) drop out-of-scope
// events; unrestricted passes everything.
func TestFilterEventsByScope(t *testing.T) {
	ev := func(pid int64) automationEvent {
		return automationEvent{EventType: "session.turn_completed", Payload: map[string]any{"project_id": float64(pid)}}
	}
	events := []automationEvent{ev(1), ev(2), ev(3)}
	scope := &ProjectScopeSet{Allowed: map[int64]bool{2: true}}
	got := filterEventsByScope(events, scope)
	if len(got) != 1 || eventProjectID(got[0]) != 2 {
		t.Fatalf("scoped filter kept %d events (want only project 2)", len(got))
	}
	// unrestricted → passthrough
	if len(filterEventsByScope(events, &ProjectScopeSet{Unrestricted: true})) != 3 {
		t.Fatal("unrestricted scope must pass all events")
	}
	// an event without project_id (project 0) is allowed (genuinely global events).
	none := []automationEvent{{EventType: "x", Payload: map[string]any{}}}
	if len(filterEventsByScope(none, scope)) != 1 {
		t.Fatal("project-less event must not be dropped")
	}
}

// TestBlackboardScopeEnforcement: a scoped client may only touch its own
// project/group boards; the global board stays shared.
func TestBlackboardScopeEnforcement(t *testing.T) {
	scope := &ProjectScopeSet{Allowed: map[int64]bool{1: true}, AllowedTags: map[int64]bool{7: true}}
	if err := validBlackboardScope("global", 0, scope); err != nil {
		t.Fatalf("global board must be allowed: %v", err)
	}
	if err := validBlackboardScope("project", 1, scope); err != nil {
		t.Fatalf("in-scope project board blocked: %v", err)
	}
	if err := validBlackboardScope("project", 2, scope); err == nil {
		t.Fatal("out-of-scope project board must be blocked")
	}
	if err := validBlackboardScope("group", 7, scope); err != nil {
		t.Fatalf("in-scope group board blocked: %v", err)
	}
	if err := validBlackboardScope("group", 8, scope); err == nil {
		t.Fatal("out-of-scope group board must be blocked")
	}
	// unrestricted client may touch any board
	unr := &ProjectScopeSet{Unrestricted: true}
	if validBlackboardScope("project", 99, unr) != nil || validBlackboardScope("group", 99, unr) != nil {
		t.Fatal("unrestricted client must reach any board")
	}
}

// TestEffectiveCommandProjectID: the scope gate resolves the project from the
// target AND the payload, so a payload-carried project cannot escape the filter.
func TestEffectiveCommandProjectID(t *testing.T) {
	// target names the project
	c1 := &commandEnvelope{Target: commandTarget{Type: "project", ProjectID: 5}}
	if effectiveCommandProjectID(c1) != 5 {
		t.Fatal("target project not resolved")
	}
	// project only in the payload (the legacy tasks.create bypass)
	c2 := &commandEnvelope{Payload: json.RawMessage(`{"project_id":9,"title":"x"}`)}
	if effectiveCommandProjectID(c2) != 9 {
		t.Fatal("payload project must be resolved so it cannot bypass the filter")
	}
	// neither → 0 (project-less)
	c3 := &commandEnvelope{Payload: json.RawMessage(`{}`)}
	if effectiveCommandProjectID(c3) != 0 {
		t.Fatal("project-less command must resolve to 0")
	}
}
