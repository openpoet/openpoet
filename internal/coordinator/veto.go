package coordinator

import "fmt"

// ConsultWrite is the conflict radar's first synchronous "hand" (Phase 3, L1):
// called INLINE from HandlePermission before a write permission parks, it
// returns a deny + explanatory reason when another LIVE session already holds a
// write claim on the same (projectKey, rel). The message is what the model
// reads back, so it names the peer session and the contested path.
//
// It is read-only against the async-populated claims index (no synchronous
// claim insertion), so it inherits the observe-only radar's semantics: it sees
// a peer's claim only after that peer's PreToolUse has been ingested. The
// residual TOCTOU for two simultaneous first-writers is closed by the Phase 5
// atomic check-and-claim gate, not here. It never touches the DB on this hot
// path — an uncached session fails open (allow), because the hook token already
// proved the session exists and blocking a tool on a cold radar is unacceptable.
func (c *Coordinator) ConsultWrite(sessionID, tool string, toolInput map[string]interface{}) (bool, string) {
	touches := ExtractTouches(tool, toolInput)
	// Resolve the requesting session WITHOUT a DB fallback (cache-only): the
	// permission path must never serialize on the single SQLite connection.
	c.mu.Lock()
	si := c.sessions[sessionID]
	c.mu.Unlock()
	if si == nil {
		return false, ""
	}
	for _, t := range touches {
		if t.Kind != KindWrite || t.Path == "" || t.Scope {
			continue
		}
		rel, inProject := si.relativize(t.Path) // syscall I/O only, no DB
		if !inProject {
			continue
		}
		// .claude/** contention is the shared-config hazard (R5 warn), never a
		// hard code-collision VETO — do not deny configsync-managed writes.
		if IsClaudeDirPath(rel) {
			continue
		}
		key := claimKey{projectKey: si.projectKey, rel: rel}
		c.mu.Lock()
		var peer string
		for otherID, kind := range c.claims[key] {
			if otherID == sessionID || kind != KindWrite {
				continue
			}
			if other := c.sessions[otherID]; other != nil && other.live {
				peer = otherID
				break
			}
		}
		c.mu.Unlock()
		if peer != "" {
			return true, fmt.Sprintf(
				"Conflict: %s is currently being edited by another live session (%s). "+
					"Coordinate before writing, or work on a different file.", rel, peer)
		}
	}
	return false, ""
}
