package coordinator

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
		physical, inProject := si.relativize(t.Path) // syscall I/O only, no DB
		if !inProject {
			continue
		}
		// (tree, logical path): every tree of the project shares one comparison
		// namespace, and only a peer in the SAME tree is an immediate hazard.
		tree, rel := SplitLane(physical)
		// .claude/** contention is the shared-config hazard (R5 warn), never a
		// hard code-collision VETO — do not deny configsync-managed writes.
		if IsClaudeDirPath(rel) {
			continue
		}
		key := claimKey{projectKey: si.projectKey, rel: rel}
		now := c.now()
		c.mu.Lock()
		var peer string
		for otherID, held := range c.claims[key] {
			if otherID == sessionID || held.kind != KindWrite {
				continue
			}
			if held.tree != tree {
				continue // divergence across trees: git arbitrates at merge time
			}
			if now.Sub(held.at) >= claimTTL {
				continue // stale claim, not live contention
			}
			if other := c.sessions[otherID]; other != nil && other.live {
				peer = otherID
				break
			}
		}
		c.mu.Unlock()
		if peer != "" {
			return true, conflictDenyReason(rel, tree, peer)
		}
	}
	return false, ""
}

// ReleaseClaim drops sessionID's write claim on the path a denied write tool
// call targeted. Called after a veto deny (the write never happens), so a
// denied attempt does not leave a residual claim that would mutually lock out
// the session that legitimately holds the file. Also useful on
// PostToolUseFailure. No-op for non-write / out-of-project / cold sessions.
func (c *Coordinator) ReleaseClaim(sessionID, tool string, toolInput map[string]interface{}) {
	touches := ExtractTouches(tool, toolInput)
	c.mu.Lock()
	si := c.sessions[sessionID]
	c.mu.Unlock()
	if si == nil {
		return
	}
	for _, t := range touches {
		if t.Kind != KindWrite || t.Path == "" || t.Scope {
			continue
		}
		rel, inProject := si.relativize(t.Path)
		if !inProject {
			continue
		}
		rel = LogicalRel(rel)
		key := claimKey{projectKey: si.projectKey, rel: rel}
		c.mu.Lock()
		if set := c.claims[key]; set != nil {
			delete(set, sessionID)
			if len(set) == 0 {
				delete(c.claims, key)
			}
		}
		c.mu.Unlock()
	}
}
