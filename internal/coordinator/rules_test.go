package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testCoordinator builds a Coordinator with canned session resolutions and a
// controllable clock; process() is driven directly (no goroutines, no DB).
func testCoordinator(t *testing.T, sessions map[string]*sessionInfo) (*Coordinator, *time.Time) {
	t.Helper()
	c := New(nil)
	clock := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	for id, si := range sessions {
		si.fetchedAt = clock
		c.sessions[id] = si
	}
	c.resolveSession = func(_ context.Context, id string) (*sessionInfo, error) {
		if si, ok := sessions[id]; ok {
			return si, nil
		}
		return nil, errors.New("unknown session " + id)
	}
	return c, &clock
}

func localSession(id string, projectID int64, root string, taskID int64) *sessionInfo {
	return &sessionInfo{
		id:         id,
		projectID:  projectID,
		projectKey: "local|" + root,
		rootRaw:    root,
		taskID:     taskID,
		live:       true,
	}
}

func touchMsg(sessionID, tool, path string, kind TouchKind, ts time.Time) ingestMsg {
	return ingestMsg{
		kind:      "touch",
		sessionID: sessionID,
		tool:      tool,
		touches:   []Touch{{Kind: kind, Path: path}},
		ts:        ts,
	}
}

func conflictEvents(c *Coordinator) int {
	n := 0
	for _, ev := range c.pendingEvents {
		if ev.EventType == "conflict.detected" {
			n++
		}
	}
	return n
}

func TestR1FileOverlapCriticalAcrossSessions(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	if len(c.incidents) != 0 {
		t.Fatalf("single writer already produced %d incidents", len(c.incidents))
	}
	// Same session again: never a self-conflict.
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	if len(c.incidents) != 0 {
		t.Fatalf("self re-touch produced %d incidents", len(c.incidents))
	}
	// Second session, same file: the marquee critical conflict.
	c.process(touchMsg("sb", "Edit", root+"/shared.go", KindWrite, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(c.incidents))
	}
	for _, inc := range c.incidents {
		if inc.Rule != RuleFileOverlap || inc.Severity != SeverityCritical {
			t.Fatalf("incident = %s/%s, want file_overlap/critical", inc.Rule, inc.Severity)
		}
		if len(inc.Sessions) != 2 || inc.Sessions[0] > inc.Sessions[1] {
			t.Fatalf("sessions not a sorted pair: %v", inc.Sessions)
		}
	}
	if got := conflictEvents(c); got != 1 {
		t.Fatalf("conflict.detected events = %d, want 1", got)
	}
	// Different files never overlap (the fake-conflict negative control).
	c.process(touchMsg("sa", "Edit", root+"/independent_a.go", KindWrite, *clock))
	c.process(touchMsg("sb", "Edit", root+"/independent_b.go", KindWrite, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("independent files created incidents: %d", len(c.incidents))
	}
	// Read touches never conflict.
	c.process(touchMsg("sa", "Read", root+"/other.go", KindRead, *clock))
	c.process(touchMsg("sb", "Read", root+"/other.go", KindRead, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("read-read created incidents: %d", len(c.incidents))
	}
	// A dead peer's claim never fires a live conflict.
	c.sessions["sa"].live = false
	c.process(touchMsg("sb", "Edit", root+"/independent_a.go", KindWrite, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("dead-session claim fired a conflict: %d incidents", len(c.incidents))
	}
}

func TestR5ClaudeDirPrecedenceOverR1(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Write", root+"/.claude/settings.json", KindWrite, *clock))
	c.process(touchMsg("sb", "Write", root+"/.claude/settings.json", KindWrite, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(c.incidents))
	}
	for _, inc := range c.incidents {
		if inc.Rule != RuleSharedClaudeDir {
			t.Fatalf("rule = %s, want shared_claude_dir (R5 must beat R1 on .claude/**)", inc.Rule)
		}
		if inc.Severity != SeverityWarn {
			t.Fatalf("severity = %s, want warn — .claude contention must never be critical", inc.Severity)
		}
	}
}

func TestR3SameTaskWarn(t *testing.T) {
	root := "/lib"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sc": localSession("sc", 2, root, 77),
		"sd": localSession("sd", 2, root, 77),
		"se": localSession("se", 2, root, 0), // no task: never in an R3 pair
	})
	// Different files — R3 is task-keyed, not path-keyed.
	c.process(touchMsg("sc", "Edit", root+"/calc.go", KindWrite, *clock))
	c.process(touchMsg("sd", "Edit", root+"/calc_test.go", KindWrite, *clock))
	c.process(touchMsg("se", "Edit", root+"/other.go", KindWrite, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("incidents = %d, want exactly 1 same_task pair", len(c.incidents))
	}
	for _, inc := range c.incidents {
		if inc.Rule != RuleSameTask || inc.Severity != SeverityWarn {
			t.Fatalf("incident = %s/%s, want same_task/warn", inc.Rule, inc.Severity)
		}
		for _, sid := range inc.Sessions {
			if sid == "se" {
				t.Fatalf("task-less session ended up in a same_task pair: %v", inc.Sessions)
			}
		}
	}
}

func TestHysteresisSuppressesReEmission(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	c.process(touchMsg("sb", "Edit", root+"/shared.go", KindWrite, *clock))
	for i := 0; i < 5; i++ {
		*clock = clock.Add(10 * time.Second)
		c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	}
	if got := conflictEvents(c); got != 1 {
		t.Fatalf("events inside hysteresis window = %d, want 1", got)
	}
	var evidence int64
	for _, inc := range c.incidents {
		evidence = inc.EvidenceCount
	}
	if evidence < 6 {
		t.Fatalf("evidence_count = %d, want >= 6 (re-evidence must still be counted)", evidence)
	}
	// Past the window, one (and only one) re-emission.
	*clock = clock.Add(hysteresisWindow + time.Minute)
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	if got := conflictEvents(c); got != 2 {
		t.Fatalf("events after window = %d, want 2", got)
	}
}

func TestExternalPathsNeverFireRules(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	// Shared auto-memory outside the project root: ledger only, no incident.
	memory := "/Users/x/.claude/projects/enc/memory/MEMORY.md"
	c.process(touchMsg("sa", "Edit", memory, KindWrite, *clock))
	c.process(touchMsg("sb", "Edit", memory, KindWrite, *clock))
	if len(c.incidents) != 0 {
		t.Fatalf("outside-root path fired %d incidents — exclusion list broken", len(c.incidents))
	}
	if len(c.dirtyLedger) != 2 {
		t.Fatalf("ledger deltas = %d, want 2 (activity still recorded)", len(c.dirtyLedger))
	}
}

func TestRemoteClaimsKeyedByHost(t *testing.T) {
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sl": localSession("sl", 3, "/fake-lib", 0),
		"sr": {
			id: "sr", projectID: 4, remote: true,
			rootRaw: "/remote-src", projectKey: "ssh:localhost|/remote-src", live: true,
		},
	})
	c.process(touchMsg("sl", "Edit", "/fake-lib/main.go", KindWrite, *clock))
	c.process(touchMsg("sr", "Edit", "/remote-src/main.go", KindWrite, *clock))
	if len(c.incidents) != 0 {
		t.Fatalf("cross-host identically-named rel paths collided: %d incidents", len(c.incidents))
	}
}

// TestTransientResolutionFailureIsRetried pins the fix for the review's major
// finding: a failed session resolution must be a backoff, never a permanent
// negative cache — otherwise one DB stall blinds the radar to a session for
// the whole process lifetime.
func TestTransientResolutionFailureIsRetried(t *testing.T) {
	c, clock := testCoordinator(t, map[string]*sessionInfo{})
	root := "/proj"
	healthy := localSession("sa", 1, root, 0)
	calls := 0
	c.resolveSession = func(_ context.Context, id string) (*sessionInfo, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient db stall")
		}
		return healthy, nil
	}
	c.process(touchMsg("sa", "Edit", root+"/a.go", KindWrite, *clock))
	if len(c.dirtyLedger) != 0 {
		t.Fatal("failed resolution still produced ledger entries")
	}
	// Within the backoff window nothing is retried (no DB hammering)…
	c.process(touchMsg("sa", "Edit", root+"/a.go", KindWrite, *clock))
	if calls != 1 {
		t.Fatalf("resolution retried inside backoff window (calls=%d)", calls)
	}
	// …and after it, the session becomes visible to the radar again.
	*clock = clock.Add(resolveRetryBackoff + time.Second)
	c.process(touchMsg("sa", "Edit", root+"/a.go", KindWrite, *clock))
	if calls != 2 {
		t.Fatalf("resolution not retried after backoff (calls=%d)", calls)
	}
	if len(c.dirtyLedger) != 1 {
		t.Fatalf("recovered session produced %d ledger entries, want 1", len(c.dirtyLedger))
	}
}

// TestForgetSessionDropsLiveStateEverywhere pins the fix for the critical
// finding: ForgetSession must clear claims AND pending ledger deltas (a
// deleted session row would poison the flush batch on the foreign key).
func TestForgetSessionDropsLiveStateEverywhere(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	if len(c.dirtyLedger) != 1 || len(c.claims) != 1 {
		t.Fatalf("setup: ledger=%d claims=%d", len(c.dirtyLedger), len(c.claims))
	}
	c.ForgetSession("sa")
	if len(c.dirtyLedger) != 0 {
		t.Fatal("ForgetSession left pending ledger deltas (FK poison risk)")
	}
	if len(c.claims) != 0 {
		t.Fatal("ForgetSession left live claims (ghost-conflict risk)")
	}
	// A later writer must NOT conflict with the forgotten session.
	c.process(touchMsg("sb", "Edit", root+"/shared.go", KindWrite, *clock))
	if len(c.incidents) != 0 {
		t.Fatalf("ghost claim fired %d incident(s) after ForgetSession", len(c.incidents))
	}
}

// TestSessionCacheTTLRefreshesLiveness: a stopped peer stops firing conflicts
// once its cache entry expires, even if ForgetSession was missed.
func TestSessionCacheTTLRefreshesLiveness(t *testing.T) {
	root := "/proj"
	sa := localSession("sa", 1, root, 0)
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": sa,
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	// sa "stops" out-of-band; its next resolution reports live=false.
	stopped := *sa
	stopped.live = false
	*clock = clock.Add(sessionCacheTTL + time.Second)
	c.resolveSession = func(_ context.Context, id string) (*sessionInfo, error) {
		if id == "sa" {
			return &stopped, nil
		}
		return localSession(id, 1, root, 0), nil
	}
	// sa's OWN next touch refreshes its cache entry to live=false…
	c.process(touchMsg("sa", "Read", root+"/other.go", KindRead, *clock))
	// …so sb writing the contested file no longer sees a live peer.
	c.process(touchMsg("sb", "Edit", root+"/shared.go", KindWrite, *clock))
	if len(c.incidents) != 0 {
		t.Fatalf("stale-cached liveness fired %d incident(s) after TTL refresh", len(c.incidents))
	}
}

func TestEscalationRateLimitedPerProject(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	escalations := make(chan Incident, 64)
	c.OnEscalate = func(inc Incident) { escalations <- inc }
	for i := 0; i < 10; i++ {
		file := root + "/f" + string(rune('a'+i)) + ".go"
		c.process(touchMsg("sa", "Edit", file, KindWrite, *clock))
		c.process(touchMsg("sb", "Edit", file, KindWrite, *clock))
	}
	time.Sleep(50 * time.Millisecond) // escalations fire on goroutines
	if got := len(escalations); got != 1 {
		t.Fatalf("escalations = %d, want 1 (10 fresh criticals inside the cooldown are ONE situation)", got)
	}
	if len(c.incidents) != 10 {
		t.Fatalf("incidents = %d, want 10 (rate limit must not suppress incident records)", len(c.incidents))
	}
}

func TestStopEventQueuesTurnCompleted(t *testing.T) {
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, "/proj", 0),
	})
	c.process(ingestMsg{kind: "turn", sessionID: "sa", ts: *clock})
	found := false
	for _, ev := range c.pendingEvents {
		if ev.EventType == "session.turn_completed" && ev.AggregateID == "sa" {
			found = true
		}
	}
	if !found {
		t.Fatal("Stop did not queue a session.turn_completed event")
	}
}
