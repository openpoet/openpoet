package coordinator

import (
	"strings"
	"testing"
)

// TestGateLaneCollidesWithMain (Phase 7.4): a write inside a workspace lane
// claims the LOGICAL project path — main-tree and lane edits of the same file
// collide; different logical files do not; substrate protection still fires on
// the raw lane path.
func TestGateLaneCollidesWithMain(t *testing.T) {
	root := "/proj"
	c, clock := testCoordinator(t, map[string]*sessionInfo{
		"main-s": localSession("main-s", 1, root, 0),
		"lane-s": localSession("lane-s", 1, root, 0),
	})
	// main session claims shared.go on the MAIN tree.
	c.process(touchMsg("main-s", "Edit", root+"/shared.go", KindWrite, *clock))

	lanePath := root + "/.openpoet/worktrees/ws1/shared.go"
	deny, reason := c.Gate("lane-s", "Write", map[string]interface{}{"file_path": lanePath})
	if !deny {
		t.Fatal("lane write of the same LOGICAL file slid past the main-tree claim")
	}
	if !strings.Contains(reason, "main-s") {
		t.Fatalf("deny reason should name the main-tree peer: %q", reason)
	}

	// A DIFFERENT logical file via the lane stays clean.
	if d, r := c.Gate("lane-s", "Write", map[string]interface{}{"file_path": root + "/.openpoet/worktrees/ws1/other.go"}); d {
		t.Fatalf("different logical file falsely denied: %q", r)
	}

	// The reverse direction: a lane claim blocks a later main-tree write.
	c.process(touchMsg("lane-s", "Edit", root+"/.openpoet/worktrees/ws2/rev.go", KindWrite, *clock))
	if d, r := c.Gate("main-s", "Write", map[string]interface{}{"file_path": root + "/rev.go"}); !d || !strings.Contains(r, "lane-s") {
		t.Fatalf("main-tree write of a lane-claimed logical file not denied: deny=%v reason=%q", d, r)
	}

	// Substrate still protected on the RAW path (never rewritten away).
	if d, r := c.Gate("lane-s", "Write", map[string]interface{}{"file_path": root + "/.openpoet/environment.yaml"}); !d || !strings.Contains(strings.ToLower(r), "substrate") {
		t.Fatalf("substrate protection lost: deny=%v reason=%q", d, r)
	}

	// LogicalRel itself.
	if got := LogicalRel(".openpoet/worktrees/x-1/src/a.go"); got != "src/a.go" {
		t.Fatalf("LogicalRel = %q", got)
	}
	if got := LogicalRel("src/a.go"); got != "src/a.go" {
		t.Fatalf("non-lane rel must be untouched, got %q", got)
	}
}
