package coordinator

import (
	"strings"
	"testing"
)

// TestGateDeniesContestedWrite: a live peer's write claim makes the gate deny a
// contested write, with a reason naming the peer. Reads and uncontested writes
// pass; a dead peer never denies.
func TestGateDeniesContestedWrite(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	// SA claims shared.go through the normal async ingest.
	c.process(touchMsg("sa", "Edit", root+"/shared.go", KindWrite, *clock))

	deny, reason := c.Gate("sb", "Write", map[string]interface{}{"file_path": root + "/shared.go"})
	if !deny {
		t.Fatal("contested write not denied by gate")
	}
	if reason == "" || !strings.Contains(reason, "sa") {
		t.Fatalf("deny reason should name the peer sa: %q", reason)
	}
	// Read of the contested file: never denied (write-class only).
	if d, _ := c.Gate("sb", "Read", map[string]interface{}{"file_path": root + "/shared.go"}); d {
		t.Fatal("read denied by gate")
	}
	// A dead peer's claim does not deny.
	c.sessions["sa"].live = false
	if d, _ := c.Gate("sb", "Edit", map[string]interface{}{"file_path": root + "/shared.go"}); d {
		t.Fatal("dead peer's claim denied a write")
	}
}

// TestGateAllowsCleanWrite: an uncontested write is allowed AND atomically
// claimed — a concurrent second session gating the SAME clean path then loses
// (closes the two-simultaneous-first-writers TOCTOU ConsultWrite leaves open).
func TestGateAllowsCleanWrite(t *testing.T) {
	root := "/proj"
	c, _ := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
		"sb": localSession("sb", 1, root, 0),
	})
	// SA gates a clean path: allowed, and the gate records SA's claim atomically.
	if deny, _ := c.Gate("sa", "Edit", map[string]interface{}{"file_path": root + "/fresh.go"}); deny {
		t.Fatal("clean write denied")
	}
	// SB now gates the same path → the gate's own atomic claim makes SB lose.
	deny, reason := c.Gate("sb", "Write", map[string]interface{}{"file_path": root + "/fresh.go"})
	if !deny {
		t.Fatal("atomic check-and-claim did not close the TOCTOU: second writer allowed")
	}
	if !strings.Contains(reason, "sa") {
		t.Fatalf("TOCTOU deny should name the first claimant sa: %q", reason)
	}
}

// TestSubstrateProtectionDenies: writes into the .openpoet/ orchestration
// substrate are denied with a substrate reason (distinct from a conflict), while
// a session's own worktree files under .openpoet/worktrees/ are NOT substrate.
func TestSubstrateProtectionDenies(t *testing.T) {
	root := "/proj"
	c, _ := testCoordinator(t, map[string]*sessionInfo{
		"sa": localSession("sa", 1, root, 0),
	})
	deny, reason := c.Gate("sa", "Write", map[string]interface{}{"file_path": root + "/.openpoet/environment.yaml"})
	if !deny {
		t.Fatal(".openpoet substrate write not denied")
	}
	if !strings.Contains(reason, "ubstrate") {
		t.Fatalf("substrate deny reason should say 'substrate': %q", reason)
	}
	// worktree working area is exempt (governed by normal conflict rules).
	if d, _ := c.Gate("sa", "Edit", map[string]interface{}{"file_path": root + "/.openpoet/worktrees/ws1/foo.go"}); d {
		t.Fatal("worktree file wrongly treated as protected substrate")
	}
	// pure string helper coverage
	if !IsSubstratePath(".openpoet/tokens") || IsSubstratePath(".openpoet/worktrees/x/a.go") || IsSubstratePath("src/a.go") {
		t.Fatal("IsSubstratePath classification wrong")
	}
}
