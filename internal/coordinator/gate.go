package coordinator

import (
	"fmt"
	"regexp"
	"strings"
)

// IsSubstratePath reports whether rel targets the orchestration SUBSTRATE — the
// `.openpoet/` metadata tree (environment manifests, coordinator/lease state,
// per-session tokens). A session must never tamper with the machinery that
// orchestrates it. Worktree working areas (`.openpoet/worktrees/**`) are EXEMPT:
// those are managed code checkouts governed by the normal conflict rules, not
// protected metadata — denying them would block every workspace session.
func IsSubstratePath(relPath string) bool {
	p := strings.TrimPrefix(filepathToSlash(relPath), "./")
	inSubstrate := p == ".openpoet" || strings.HasPrefix(p, ".openpoet/") || strings.Contains(p, "/.openpoet/")
	if !inSubstrate {
		return false
	}
	// A worktree's own files live under .openpoet/worktrees/<ws>/ — legitimate work.
	if strings.Contains(p, ".openpoet/worktrees/") {
		return false
	}
	return true
}

// Gate is the Phase 5 SYNCHRONOUS PreToolUse gate (L2). Unlike ConsultWrite
// (observe-only, called from the PermissionRequest path which
// dangerously_skip_permissions sessions NEVER fire), Gate answers the PreToolUse
// path — which fires in every permission mode — so it is the only mechanism that
// governs orchestrator-spawned skip-permissions fleets.
//
// It returns (deny, reason). Two deny classes, distinct reasons:
//   - substrate: a write into `.openpoet/` orchestration metadata.
//   - conflict:  another LIVE session already holds a write claim on the path.
//
// For non-substrate write-class touches with no live peer, Gate performs an
// ATOMIC check-and-claim under a single c.mu hold: it records this session's
// write claim in the same critical section it scanned, so two simultaneous
// first-writers cannot both pass (closing the TOCTOU ConsultWrite documents but
// leaves open, veto.go). The durable ledger is still written only by the async
// indexer; Gate touches the in-memory index idempotently. A cold (uncached)
// session fails OPEN — the caller already verified the hook token, and blocking
// a tool call on a cold radar is unacceptable.
// SplitLane splits a project-relative path into the working TREE that physically
// holds it and the path within that tree:
//
//	".openpoet/worktrees/ws-a/src/a.go" → ("ws-a", "src/a.go")
//	"src/a.go"                          → ("",     "src/a.go")
//
// The tree comes from the PATH, never from the writing session: a session can
// write an absolute path into a different tree, and the thing being contended is
// the file on disk — not the writer's cwd.
func SplitLane(rel string) (tree string, inTree string) {
	m := laneRelPattern.FindStringSubmatch(rel)
	if m == nil {
		return "", rel
	}
	return m[1], m[2]
}

// LogicalRel maps a lane-relative path back to its logical project path:
// ".openpoet/worktrees/<ws>/src/a.go" → "src/a.go". Claims are keyed by the
// logical path so every tree of one project shares ONE comparison namespace —
// which is what lets the radar notice that two trees hold the same file and
// classify that as divergence rather than collision.
func LogicalRel(rel string) string {
	_, inTree := SplitLane(rel)
	return inTree
}

var laneRelPattern = regexp.MustCompile(`^\.openpoet/worktrees/([^/]+)/(.*)$`)

// conflictDenyReason is the text the model reads back on a veto. It names the
// peer AND the shared tree, and points at the remedy the platform can actually
// apply — running the work in its own worktree lane — so a denied session (or an
// orchestrator reading its transcript) knows there is a way forward besides
// waiting for the peer to finish.
func conflictDenyReason(rel, tree, peer string) string {
	return fmt.Sprintf(
		"Conflict: %s is being written right now by another live session (%s) in the same working tree (%s). "+
			"Do not write it. Options: work on a different file, coordinate with %s, or have the orchestrator "+
			"re-run this work in an isolated worktree lane (a session started with isolation:\"always\" gets its "+
			"own tree and merges back afterwards).",
		rel, peer, treeLabel(tree), peer)
}

func (c *Coordinator) Gate(sessionID, tool string, toolInput map[string]interface{}) (bool, string) {
	touches := ExtractTouches(tool, toolInput)

	// Resolve the requesting session with the cache+DB-fallback path (NOT the raw
	// cache): an orchestrator-spawned skip-permissions session must be gated on
	// its very FIRST write, before any prior hook warmed the cache. c.session()
	// manages its own locking, so call it before taking c.mu.
	si := c.session(sessionID)
	if si == nil {
		return false, "" // unresolvable session: fail open (hook token already verified it)
	}

	// Relativize (symlink syscalls) OUTSIDE the lock; collect write-class targets
	// together with the tree that physically holds each one.
	type writeTarget struct{ tree, rel string }
	var writes []writeTarget
	for _, t := range touches {
		if t.Kind != KindWrite || t.Path == "" || t.Scope {
			continue
		}
		physical, inProject := si.relativize(t.Path)
		if !inProject {
			continue
		}
		if IsSubstratePath(physical) {
			return true, fmt.Sprintf(
				"Substrate protected: %s is OpenPoet orchestration substrate (.openpoet metadata) "+
					"and must not be written by a session.", physical)
		}
		// Split the write into (tree, logical path): the logical path is the
		// comparison namespace shared by every tree of the project, and the tree
		// decides whether a peer holding it is an immediate hazard or mere
		// divergence. Substrate protection above ran on the RAW path on purpose.
		tree, rel := SplitLane(physical)
		// .claude/** contention is the shared-config hazard (R5 warn), never a
		// hard code-collision deny — configsync manages those writes.
		if IsClaudeDirPath(rel) {
			continue
		}
		writes = append(writes, writeTarget{tree: tree, rel: rel})
	}
	if len(writes) == 0 {
		return false, ""
	}

	// Atomic check-and-claim: scan for a live peer AND record our claim under one
	// lock hold so a concurrent gate call for the same path serializes and loses.
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, wr := range writes {
		key := claimKey{projectKey: si.projectKey, rel: wr.rel}
		set := c.claims[key]
		for otherID, held := range set {
			if otherID == sessionID || held.kind != KindWrite {
				continue
			}
			if held.tree != wr.tree {
				// DIVERGENCE, not collision: the peer is editing its own checkout
				// of this file in another tree. Nothing can be lost by letting this
				// write proceed — git arbitrates at merge time, and the async
				// indexer records the merge risk. Vetoing here is precisely what
				// used to make isolating a session pointless.
				continue
			}
			if now.Sub(held.at) >= claimTTL {
				// Stale claim: the peer has not written this path in claimTTL, so it
				// is no longer evidence of live contention.
				continue
			}
			if other := c.sessions[otherID]; other != nil && other.live {
				return true, conflictDenyReason(wr.rel, wr.tree, otherID)
			}
		}
		if set == nil {
			set = map[string]claimHolder{}
			c.claims[key] = set
		}
		set[sessionID] = claimHolder{kind: KindWrite, tree: wr.tree, at: now}
	}
	return false, ""
}
