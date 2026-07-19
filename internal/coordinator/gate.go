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
// LogicalRel maps a lane-relative path back to its logical project path:
// ".openpoet/worktrees/<ws>/src/a.go" → "src/a.go". Claims are keyed by the
// logical path so parallel lanes of one project share a conflict namespace.
func LogicalRel(rel string) string {
	return laneRelPattern.ReplaceAllString(rel, "")
}

var laneRelPattern = regexp.MustCompile(`^\.openpoet/worktrees/[^/]+/`)

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

	// Relativize (symlink syscalls) OUTSIDE the lock; collect write-class paths.
	var writeRels []string
	for _, t := range touches {
		if t.Kind != KindWrite || t.Path == "" || t.Scope {
			continue
		}
		rel, inProject := si.relativize(t.Path)
		if !inProject {
			continue
		}
		if IsSubstratePath(rel) {
			return true, fmt.Sprintf(
				"Substrate protected: %s is OpenPoet orchestration substrate (.openpoet metadata) "+
					"and must not be written by a session.", rel)
		}
		// Lane normalization (Phase 7.4): a write inside a workspace lane
		// (.openpoet/worktrees/<ws>/src/foo.go) claims the LOGICAL project
		// path (src/foo.go) — main-tree and lane edits of the same file must
		// collide, not slide past each other on different rel strings.
		// Substrate protection above ran on the RAW rel on purpose.
		rel = LogicalRel(rel)
		// .claude/** contention is the shared-config hazard (R5 warn), never a
		// hard code-collision deny — configsync manages those writes.
		if IsClaudeDirPath(rel) {
			continue
		}
		writeRels = append(writeRels, rel)
	}
	if len(writeRels) == 0 {
		return false, ""
	}

	// Atomic check-and-claim: scan for a live peer AND record our claim under one
	// lock hold so a concurrent gate call for the same path serializes and loses.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rel := range writeRels {
		key := claimKey{projectKey: si.projectKey, rel: rel}
		set := c.claims[key]
		for otherID, kind := range set {
			if otherID == sessionID || kind != KindWrite {
				continue
			}
			if other := c.sessions[otherID]; other != nil && other.live {
				return true, fmt.Sprintf(
					"Conflict: %s is currently being edited by another live session (%s). "+
						"Coordinate before writing, or work on a different file.", rel, otherID)
			}
		}
		if set == nil {
			set = map[string]TouchKind{}
			c.claims[key] = set
		}
		set[sessionID] = KindWrite
	}
	return false, ""
}
