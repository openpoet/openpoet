package coordinator

import (
	"strings"
	"testing"
	"time"
)

// laneSession builds a session whose runner started inside a workspace lane.
func laneSession(id string, projectID int64, root, lane string) *sessionInfo {
	si := localSession(id, projectID, root, 0)
	si.workDir = root + "/.openpoet/worktrees/" + lane
	si.tree = lane
	return si
}

func divergenceEvents(c *Coordinator) int {
	n := 0
	for _, ev := range c.pendingEvents {
		if ev.EventType == "conflict.divergence" {
			n++
		}
	}
	return n
}

func lanePath(root, lane, rel string) string {
	return root + "/.openpoet/worktrees/" + lane + "/" + rel
}

// TestLaneDivergenceIsNotACollision is the corrected semantics that makes
// isolation a real remedy.
//
// It deliberately REVERSES the Phase 7.4 rule ("main-tree and lane edits of the
// same file must collide"): under that rule, moving a session into a worktree
// changed nothing, because both trees normalized to one logical path and the gate
// kept denying. Two trees each holding their own checkout cannot lose each
// other's work — git arbitrates at merge time — so this is DIVERGENCE: allowed,
// reported for merge sequencing, and never an incident.
func TestLaneDivergenceIsNotACollision(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"main-s": localSession("main-s", 1, root, 0),
		"lane-a": laneSession("lane-a", 1, root, "ws-a"),
		"lane-b": laneSession("lane-b", 1, root, "ws-b"),
	})
	// The main checkout claims shared.go.
	c.process(touchMsg("main-s", "Edit", root+"/shared.go", KindWrite, *clock))

	// A lane writing ITS OWN copy of the same logical file must proceed.
	if deny, reason := c.Gate("lane-a", "Write", map[string]interface{}{"file_path": lanePath(root, "ws-a", "shared.go")}); deny {
		t.Fatalf("lane write vetoed by a main-tree claim — isolation would be pointless: %q", reason)
	}

	// The async indexer turns that overlap into merge-risk INFORMATION.
	c.process(touchMsg("lane-a", "Edit", lanePath(root, "ws-a", "shared.go"), KindWrite, *clock))
	if got := divergenceEvents(c); got != 1 {
		t.Fatalf("expected 1 conflict.divergence event, got %d", got)
	}
	if got := conflictEvents(c); got != 0 {
		t.Fatalf("divergence must not raise conflict.detected, got %d", got)
	}
	if len(c.incidents) != 0 {
		t.Fatalf("divergence must not open an incident, got %d", len(c.incidents))
	}

	// A THIRD tree diverges from both peers: one event per pair.
	if deny, reason := c.Gate("lane-b", "Write", map[string]interface{}{"file_path": lanePath(root, "ws-b", "shared.go")}); deny {
		t.Fatalf("second lane vetoed: %q", reason)
	}
	c.process(touchMsg("lane-b", "Edit", lanePath(root, "ws-b", "shared.go"), KindWrite, *clock))
	if got := divergenceEvents(c); got != 3 {
		t.Fatalf("expected 3 divergence events (a+main, b+main, b+a), got %d", got)
	}
	if len(c.incidents) != 0 {
		t.Fatalf("still no incidents expected, got %d", len(c.incidents))
	}

	// The payload must name the path, both sessions and both trees, so a
	// coordinator can act on it without re-deriving anything.
	var payload string
	for _, ev := range c.pendingEvents {
		if ev.EventType == "conflict.divergence" {
			payload = ev.PayloadJSON
			break
		}
	}
	for _, want := range []string{`"path":"shared.go"`, "main-s", "lane-a", `"main"`, `"ws-a"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("divergence payload missing %s: %s", want, payload)
		}
	}
}

// TestSameTreeStillCollides: the hard veto survives untouched WITHIN one tree —
// two sessions sharing a lane are exactly as dangerous as two sharing the main
// checkout, and the incident records which tree it happened in.
func TestSameTreeStillCollides(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"a": laneSession("a", 1, root, "ws-a"),
		"b": laneSession("b", 1, root, "ws-a"),
	})
	c.process(touchMsg("a", "Edit", lanePath(root, "ws-a", "shared.go"), KindWrite, *clock))

	deny, reason := c.Gate("b", "Write", map[string]interface{}{"file_path": lanePath(root, "ws-a", "shared.go")})
	if !deny {
		t.Fatal("two sessions in the SAME lane must still collide")
	}
	if !strings.Contains(reason, "a") || !strings.Contains(reason, "ws-a") {
		t.Fatalf("deny reason should name the peer and the shared tree: %q", reason)
	}
	// The deny must point at the remedy, not just refuse.
	if !strings.Contains(reason, "isolation") {
		t.Fatalf("deny reason should offer the isolation remedy: %q", reason)
	}

	// And the incident is filed against the lane, with the tree recorded.
	c.process(touchMsg("b", "Edit", lanePath(root, "ws-a", "shared.go"), KindWrite, *clock))
	if len(c.incidents) != 1 {
		t.Fatalf("expected 1 file_overlap incident, got %d", len(c.incidents))
	}
	for key, inc := range c.incidents {
		if inc.Rule != RuleFileOverlap || inc.Severity != SeverityCritical {
			t.Fatalf("expected critical file_overlap, got %s/%s", inc.Rule, inc.Severity)
		}
		if !strings.Contains(key, "lane:ws-a") {
			t.Fatalf("lane collision scope key should carry the lane: %q", key)
		}
		if inc.Details["tree"] != "ws-a" {
			t.Fatalf("incident should record the tree, got %v", inc.Details["tree"])
		}
	}
}

// TestMainTreeCollisionScopeKeyUnchanged: incident identity for the ordinary
// main-checkout collision must survive this upgrade byte-for-byte, or every open
// incident in a live DB would be re-filed under a new scope_key on restart.
func TestMainTreeCollisionScopeKeyUnchanged(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))
	c.process(touchMsg("sb", "Edit", root+"/shared.go", KindWrite, *clock))

	want := RuleFileOverlap + "|local|/proj|shared.go|sa+sb"
	if _, ok := c.incidents[want]; !ok {
		keys := make([]string, 0, len(c.incidents))
		for k := range c.incidents {
			keys = append(keys, k)
		}
		t.Fatalf("main-tree incident key changed shape; want %q, have %v", want, keys)
	}
}

// TestClaimTTLStopsVetoing: a claim is evidence of LIVE contention, not a
// permanent lock. Before claimTTL existed, a file written once at 09:00 still
// denied a peer at 17:00 — and an isolation trigger built on that would fire on
// sessions that merely coincided historically.
func TestClaimTTLStopsVetoing(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))

	// Fresh claim: denied.
	if deny, _ := c.Gate("sb", "Write", map[string]interface{}{"file_path": root + "/shared.go"}); !deny {
		t.Fatal("fresh claim should still veto")
	}
	// Past the TTL: the same claim is no longer contention.
	future := clock.Add(claimTTL + time.Minute)
	c.now = func() time.Time { return future }
	if deny, reason := c.Gate("sb", "Write", map[string]interface{}{"file_path": root + "/shared.go"}); deny {
		t.Fatalf("stale claim still vetoing after %s: %q", claimTTL, reason)
	}
	// ConsultWrite (the permission-path hand) must agree.
	if deny, reason := c.ConsultWrite("sb", "Write", map[string]interface{}{"file_path": root + "/shared.go"}); deny {
		t.Fatalf("ConsultWrite still vetoing on a stale claim: %q", reason)
	}
	// And the sweep reclaims the memory, keeping the fresh claim.
	c.mu.Lock()
	c.pruneMemoryLocked()
	set := c.claims[claimKey{projectKey: "local|" + root, rel: "shared.go"}]
	c.mu.Unlock()
	if _, stale := set["sa"]; stale {
		t.Fatal("expired claim survived the sweep")
	}
	if _, fresh := set["sb"]; !fresh {
		t.Fatal("sweep dropped a FRESH claim")
	}
}

// TestClaimRefreshKeepsVetoing: the TTL must not evict a file someone is
// actively working on — each write restamps the claim.
func TestClaimRefreshKeepsVetoing(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	now := *clock
	// SA keeps editing across a span far longer than the TTL.
	for i := 0; i < 5; i++ {
		now = now.Add(claimTTL - time.Minute)
		c.now = func() time.Time { return now }
		c.process(touchMsg("sa", "Edit", root+"/hot.go", KindWrite, now))
	}
	if deny, _ := c.Gate("sb", "Write", map[string]interface{}{"file_path": root + "/hot.go"}); !deny {
		t.Fatal("a continuously-edited file lost its claim to the TTL")
	}
}

// TestRelativePathResolvesAgainstWorkDir: tool paths are not always absolute
// (ACP-synthesized events, non-Claude backends). A relative path belongs to the
// RUNNER's cwd — resolving it against the project root instead attributed a
// lane's file to the main checkout. Before workDir existed these paths were
// dropped from the index altogether.
func TestRelativePathResolvesAgainstWorkDir(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"main-s":  localSession("main-s", 1, root, 0),
		"main-s2": localSession("main-s2", 1, root, 0),
		"lane-a":  laneSession("lane-a", 1, root, "ws-a"),
		"lane-a2": laneSession("lane-a2", 1, root, "ws-a"),
	})
	// A RELATIVE write from a main-tree session is indexed (not dropped).
	c.process(touchMsg("main-s", "Edit", "src/a.go", KindWrite, *clock))
	if deny, _ := c.Gate("main-s2", "Write", map[string]interface{}{"file_path": root + "/src/a.go"}); !deny {
		t.Fatal("relative write from a main-tree session was not indexed as a claim")
	}

	// The same relative path from a LANE session is that lane's own file, so it
	// diverges from the main tree instead of colliding with it.
	if deny, reason := c.Gate("lane-a", "Write", map[string]interface{}{"file_path": "src/a.go"}); deny {
		t.Fatalf("lane-relative path was attributed to the main tree: %q", reason)
	}
	// …but it does collide with a peer sharing that lane.
	if deny, reason := c.Gate("lane-a2", "Write", map[string]interface{}{"file_path": "src/a.go"}); !deny {
		t.Fatalf("two sessions in one lane must collide on a relative path: deny=%v %q", deny, reason)
	}
}

// TestDivergenceHysteresis: parallel lanes touch shared files (go.mod, a common
// header) on nearly every turn. Divergence is a standing fact, so it must be
// throttled like an incident, not streamed.
func TestDivergenceHysteresis(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"lane-a": laneSession("lane-a", 1, root, "ws-a"),
		"lane-b": laneSession("lane-b", 1, root, "ws-b"),
	})
	c.process(touchMsg("lane-a", "Edit", lanePath(root, "ws-a", "go.mod"), KindWrite, *clock))
	for i := 0; i < 5; i++ {
		c.process(touchMsg("lane-b", "Edit", lanePath(root, "ws-b", "go.mod"), KindWrite, *clock))
	}
	if got := divergenceEvents(c); got != 1 {
		t.Fatalf("expected divergence throttled to 1 event inside the window, got %d", got)
	}
	// Past the window, the standing fact is re-reported.
	future := clock.Add(hysteresisWindow + time.Minute)
	c.now = func() time.Time { return future }
	c.process(touchMsg("lane-b", "Edit", lanePath(root, "ws-b", "go.mod"), KindWrite, future))
	if got := divergenceEvents(c); got != 2 {
		t.Fatalf("expected a re-report after the hysteresis window, got %d", got)
	}
}

// TestCrossTreeAbsoluteWriteCollides: the tree is derived from the PATH, never
// from the writer's cwd. A lane session reaching into the main checkout by
// absolute path is a real same-tree collision with the main session.
func TestCrossTreeAbsoluteWriteCollides(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"main-s": localSession("main-s", 1, root, 0),
		"lane-a": laneSession("lane-a", 1, root, "ws-a"),
	})
	c.process(touchMsg("main-s", "Edit", root+"/shared.go", KindWrite, *clock))
	deny, reason := c.Gate("lane-a", "Write", map[string]interface{}{"file_path": root + "/shared.go"})
	if !deny {
		t.Fatal("a lane session writing INTO the main checkout must collide with the main session")
	}
	if !strings.Contains(reason, "main") {
		t.Fatalf("deny should name the contested tree (main): %q", reason)
	}
}

// TestSplitLane covers the pure path classifier both readers depend on.
func TestSplitLane(t *testing.T) {
	cases := []struct{ in, tree, rel string }{
		{".openpoet/worktrees/ws-a/src/a.go", "ws-a", "src/a.go"},
		{".openpoet/worktrees/x-1/a.go", "x-1", "a.go"},
		{"src/a.go", "", "src/a.go"},
		{".openpoet/environment.yaml", "", ".openpoet/environment.yaml"},
		// No trailing path inside the lane: not a file in a tree.
		{".openpoet/worktrees/ws-a", "", ".openpoet/worktrees/ws-a"},
		// A nested lane dir name must not be mistaken for the lane itself.
		{".openpoet/worktrees/ws-a/nested/.openpoet/x", "ws-a", "nested/.openpoet/x"},
	}
	for _, tc := range cases {
		tree, rel := SplitLane(tc.in)
		if tree != tc.tree || rel != tc.rel {
			t.Errorf("SplitLane(%q) = (%q, %q), want (%q, %q)", tc.in, tree, rel, tc.tree, tc.rel)
		}
		if got := LogicalRel(tc.in); got != tc.rel {
			t.Errorf("LogicalRel(%q) = %q, want %q", tc.in, got, tc.rel)
		}
	}
	if treeLabel("") != "main" || treeLabel("ws-a") != "ws-a" {
		t.Fatal("treeLabel classification wrong")
	}
}
