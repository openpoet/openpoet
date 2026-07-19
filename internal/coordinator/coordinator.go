package coordinator

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"openpoet/internal/database"
)

const (
	ingestBuffer     = 1024
	flushInterval    = 2 * time.Second
	resolveTimeout   = 5 * time.Second
	hysteresisWindow = 5 * time.Minute
	// sessionCacheTTL bounds how stale a cached session→project resolution may
	// get: liveness and task links change mid-session (user stop, link-task),
	// and a frozen snapshot turns into false conflicts.
	sessionCacheTTL = 60 * time.Second
	// resolveRetryBackoff: a failed resolution is NEVER cached as a permanent
	// verdict (the hook token already proved the session exists) — only backed
	// off, so a transient DB stall can't blind the radar to a session forever.
	resolveRetryBackoff = 10 * time.Second
	// maxPendingEvents bounds the event buffer during a persistent DB outage;
	// oldest events are dropped (counted) rather than exhausting process memory.
	maxPendingEvents = 4096
	// incidentMemoryTTL prunes quiet incidents from the in-memory maps; the DB
	// row remains the durable record.
	incidentMemoryTTL = 24 * time.Hour
)

// sessionInfo caches the session→project resolution so the ingest hot path
// never touches SQLite more than once per session (the single shared
// connection is the scarcest resource in the process).
type sessionInfo struct {
	id         string
	projectID  int64
	projectKey string
	rootRaw    string
	rootReal   string
	remote     bool
	taskID     int64
	live       bool
	fetchedAt  time.Time
}

type ingestMsg struct {
	kind      string // "touch" | "turn"
	sessionID string
	tool      string
	touches   []Touch
	ts        time.Time
}

type ledgerKey struct {
	sessionID string
	path      string
	kind      string
}

type ledgerDelta struct {
	projectID int64
	count     int64
	firstTs   time.Time
	lastTs    time.Time
	lastTool  string
}

type claimKey struct {
	projectKey string
	rel        string
}

// Coordinator is the observe-only conflict radar. A single indexer goroutine
// drains the ingest channel and owns the claim index; a flusher goroutine
// batches ledger upserts, incident rows and outbox events into one
// transaction every 2s. It never denies, pauses or answers anything.
type Coordinator struct {
	db *database.DB

	ch   chan ingestMsg
	quit chan struct{}
	wg   sync.WaitGroup

	mu             sync.Mutex
	sessions       map[string]*sessionInfo
	failedResolve  map[string]time.Time              // sessionID → do-not-retry-before
	claims         map[claimKey]map[string]TouchKind // (projectKey,rel) → sessionID → strongest kind
	turnTouched    map[string]map[string]struct{}    // sessionID → rel paths touched this turn
	incidents      map[string]*Incident              // rule|scope_key → incident
	dirtyIncidents map[string]struct{}
	dirtyLedger    map[ledgerKey]*ledgerDelta
	pendingEvents  []database.EventOutboxAppend
	lastEmit       map[string]time.Time // hysteresis per rule|scope_key
	lastEscalate   map[int64]time.Time  // projectID → last human escalation
	dropped        int64
	droppedEvents  int64

	resolveSession func(ctx context.Context, sessionID string) (*sessionInfo, error)
	now            func() time.Time

	// OnEscalate fires asynchronously once per newly-opened critical incident.
	// Wired in main.go to the proactive-notification machinery.
	OnEscalate func(inc Incident)

	// OnBrainConsult fires asynchronously ONCE per newly-opened critical
	// incident (no per-project rate limit — that would suppress consults for
	// concurrent incidents). Wired in main.go to the one-shot LLM brain, which
	// re-validates the proposed action against the project dial/budget/policy.
	OnBrainConsult func(inc Incident)
}

func New(db *database.DB) *Coordinator {
	c := &Coordinator{
		db:             db,
		ch:             make(chan ingestMsg, ingestBuffer),
		quit:           make(chan struct{}),
		sessions:       make(map[string]*sessionInfo),
		failedResolve:  make(map[string]time.Time),
		claims:         make(map[claimKey]map[string]TouchKind),
		turnTouched:    make(map[string]map[string]struct{}),
		incidents:      make(map[string]*Incident),
		dirtyIncidents: make(map[string]struct{}),
		dirtyLedger:    make(map[ledgerKey]*ledgerDelta),
		lastEmit:       make(map[string]time.Time),
		lastEscalate:   make(map[int64]time.Time),
		now:            time.Now,
	}
	c.resolveSession = c.resolveFromDB
	return c
}

// Start hydrates open incidents from the DB (so incident identity survives a
// restart: no phantom ids in events, no duplicate escalation, no
// evidence_count regression), then spawns the indexer and flusher goroutines.
func (c *Coordinator) Start() {
	c.hydrateIncidents()
	c.wg.Add(2)
	go func() { defer c.wg.Done(); c.indexLoop() }()
	go func() { defer c.wg.Done(); c.flushLoop() }()
}

func (c *Coordinator) hydrateIncidents() {
	if c.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	rows, err := c.db.ListCoordinatorIncidents(ctx, 0, "open", 500)
	if err != nil {
		log.Printf("[Coordinator] incident hydration failed (continuing cold): %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range rows {
		inc, err := incidentFromRow(row)
		if err != nil {
			log.Printf("[Coordinator] skipping malformed incident %s: %v", row.ID, err)
			continue
		}
		key := inc.Rule + "|" + inc.ScopeKey
		c.incidents[key] = inc
		// Respect hysteresis across the restart: the last persisted evidence
		// stamp is the best available proxy for the last emission.
		c.lastEmit[key] = inc.LastEvidenceAt
	}
	if len(rows) > 0 {
		log.Printf("[Coordinator] hydrated %d open incident(s)", len(rows))
	}
}

// Stop drains the goroutines and performs a final flush.
func (c *Coordinator) Stop() {
	close(c.quit)
	c.wg.Wait()
}

// OnHookEvent is the HandleEvent tap (wired as hookHandler.OnToolEvent). It
// only classifies and enqueues — never blocks the hook response path. Claims
// ride a drop-oldest channel; under burst we prefer losing a touch to
// stalling every hooked session in the process.
func (c *Coordinator) OnHookEvent(sessionID, eventName string, hookEvent map[string]interface{}) {
	switch eventName {
	case "Stop":
		c.enqueue(ingestMsg{kind: "turn", sessionID: sessionID, ts: c.now()})
	case "PreToolUse":
		// Claims come from PreToolUse ONLY: it is the earliest signal and the
		// one phase every tool call emits exactly once. Counting PostToolUse
		// too would double every touch_count and evidence_count.
		tool, _ := hookEvent["tool_name"].(string)
		toolInput, _ := hookEvent["tool_input"].(map[string]interface{})
		touches := ExtractTouches(tool, toolInput)
		if len(touches) == 0 {
			return
		}
		c.enqueue(ingestMsg{kind: "touch", sessionID: sessionID, tool: tool, touches: touches, ts: c.now()})
	}
}

// RecordAttention is the sentinel sink (wired as sessionMgr.OnSessionAttention):
// a detected question in a session's PTY output becomes a durable
// session.awaiting_input event on the next flush.
func (c *Coordinator) RecordAttention(sessionID, kind, excerpt string) {
	// Resolve the project so the event carries project_id — event consumers with
	// a project_filter must be able to scope awaiting_input, not just turn events.
	var projectID int64
	if si := c.session(sessionID); si != nil {
		projectID = si.projectID
	}
	ev := awaitingInputEvent(sessionID, projectID, kind, excerpt, c.now())
	c.mu.Lock()
	c.queueEventLocked(ev)
	c.mu.Unlock()
}

// queueEventLocked appends a pending event under c.mu, bounded so a persistent
// DB outage cannot grow the buffer without limit; oldest events are dropped
// and counted (loudly) instead of exhausting host memory.
func (c *Coordinator) queueEventLocked(ev database.EventOutboxAppend) {
	if len(c.pendingEvents) >= maxPendingEvents {
		c.pendingEvents = c.pendingEvents[1:]
		c.droppedEvents++
		if c.droppedEvents&(c.droppedEvents-1) == 0 {
			log.Printf("[Coordinator] pending-event buffer full: %d event(s) dropped so far", c.droppedEvents)
		}
	}
	c.pendingEvents = append(c.pendingEvents, ev)
}

// ForgetSession drops a session's cache entry, live claims, and any not-yet-
// flushed ledger deltas (the session row may be gone — flushing them would
// poison the batch on the foreign key). Persisted ledger rows survive.
func (c *Coordinator) ForgetSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, sessionID)
	delete(c.failedResolve, sessionID)
	delete(c.turnTouched, sessionID)
	for key, set := range c.claims {
		delete(set, sessionID)
		if len(set) == 0 {
			delete(c.claims, key)
		}
	}
	for key := range c.dirtyLedger {
		if key.sessionID == sessionID {
			delete(c.dirtyLedger, key)
		}
	}
}

func (c *Coordinator) enqueue(msg ingestMsg) {
	select {
	case c.ch <- msg:
		return
	default:
	}
	// Full: drop the oldest queued message, then retry once. Only actual
	// discards are counted, and overload is surfaced in the log (throttled by
	// the power-of-two check) instead of being silently invisible.
	discarded := 0
	select {
	case <-c.ch:
		discarded++
	default:
	}
	select {
	case c.ch <- msg:
	default:
		discarded++
	}
	if discarded == 0 {
		return
	}
	c.mu.Lock()
	c.dropped += int64(discarded)
	total := c.dropped
	c.mu.Unlock()
	if total&(total-1) == 0 { // log at 1, 2, 4, 8, ... to avoid log storms
		log.Printf("[Coordinator] ingest overload: %d touch message(s) dropped so far", total)
	}
}

func (c *Coordinator) indexLoop() {
	for {
		select {
		case <-c.quit:
			return
		case msg := <-c.ch:
			c.process(msg)
		}
	}
}

func (c *Coordinator) process(msg ingestMsg) {
	si := c.session(msg.sessionID)
	if si == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg.kind == "turn" {
		// Drain the paths this session touched since its last Stop into the
		// turn_completed payload, then reset the accumulator atomically.
		files := c.drainTurnTouchesLocked(si.id)
		c.queueEventLocked(turnCompletedEvent(si.id, si.projectID, files, msg.ts))
		return
	}
	for _, t := range msg.touches {
		if t.Scope || t.Kind == KindOpaque || t.Path == "" {
			// Directory footprints and opaque commands never become claims —
			// guessing paths out of shell commands is how false denials start.
			continue
		}
		rel, inProject := si.relativize(t.Path)
		c.bumpLedger(si, rel, t.Kind, msg.tool, msg.ts)
		if inProject {
			c.recordTurnTouchLocked(si.id, rel)
		}
		if t.Kind == KindWrite && inProject {
			c.noteWriteClaim(si, rel, msg.tool, msg.ts)
		}
	}
	c.evalSameTask(si, msg.ts)
}

func (c *Coordinator) recordTurnTouchLocked(sessionID, rel string) {
	set := c.turnTouched[sessionID]
	if set == nil {
		set = make(map[string]struct{})
		c.turnTouched[sessionID] = set
	}
	set[rel] = struct{}{}
}

func (c *Coordinator) drainTurnTouchesLocked(sessionID string) []string {
	set := c.turnTouched[sessionID]
	delete(c.turnTouched, sessionID)
	files := make([]string, 0, len(set))
	for rel := range set {
		files = append(files, rel)
	}
	sort.Strings(files)
	return files
}

// session returns the cached resolution for sessionID, hitting the DB on
// first sight and again once the TTL lapses (liveness and task links change
// mid-session). A failed resolution is never a permanent verdict — the hook
// token already proved the session exists — only a short retry backoff, so a
// transient DB stall cannot blind the radar to a session for the process
// lifetime. Resolution happens outside c.mu (it does I/O).
func (c *Coordinator) session(id string) *sessionInfo {
	nowTs := c.now()
	c.mu.Lock()
	si, seen := c.sessions[id]
	retryAt, failing := c.failedResolve[id]
	c.mu.Unlock()
	if seen && nowTs.Sub(si.fetchedAt) < sessionCacheTTL {
		return si
	}
	if failing && nowTs.Before(retryAt) {
		return si // nil on first sight; stale-but-usable during a blip
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	resolved, err := c.resolveSession(ctx, id)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		log.Printf("[Coordinator] cannot resolve session %s (retrying in %s): %v", id, resolveRetryBackoff, err)
		c.failedResolve[id] = nowTs.Add(resolveRetryBackoff)
		return si // keep any stale entry rather than going blind
	}
	resolved.fetchedAt = nowTs
	c.sessions[id] = resolved
	delete(c.failedResolve, id)
	return resolved
}

func (c *Coordinator) resolveFromDB(ctx context.Context, sessionID string) (*sessionInfo, error) {
	if c.db == nil {
		return nil, errors.New("coordinator has no database")
	}
	s, err := c.db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	p, err := c.db.GetProject(ctx, s.ProjectID)
	if err != nil {
		return nil, err
	}
	si := &sessionInfo{
		id:        s.ID,
		projectID: p.ID,
		live:      s.Status == "starting" || s.Status == "running",
	}
	if s.TaskID.Valid {
		si.taskID = s.TaskID.Int64
	}
	root := filepath.Clean(p.Path)
	si.rootRaw = root
	if p.Type == "remote" {
		// Remote claims are keyed by (ssh_host, path) so a local checkout and
		// a remote checkout of an identically-named tree never falsely collide.
		si.remote = true
		si.projectKey = "ssh:" + p.SSHHost.String + "|" + root
		return si, nil
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		si.rootReal = resolved
	}
	keyRoot := si.rootReal
	if keyRoot == "" {
		keyRoot = root
	}
	si.projectKey = "local|" + keyRoot
	return si, nil
}

// relativize normalizes an extractor path against the session's project root.
// Paths outside the root (auto-memory under ~/.claude, /tmp scratch, …) are
// recorded in the ledger by absolute path but excluded from conflict rules —
// the day-one exclusion that keeps shared-state paths from becoming false
// critical conflicts.
func (si *sessionInfo) relativize(p string) (string, bool) {
	clean := filepath.Clean(strings.TrimSpace(p))
	if si.remote {
		if rel, ok := underRoot(clean, si.rootRaw); ok {
			return rel, true
		}
		return clean, false
	}
	resolved := clean
	if r, err := filepath.EvalSymlinks(clean); err == nil {
		resolved = r
	} else if d, err := filepath.EvalSymlinks(filepath.Dir(clean)); err == nil {
		// The file may not exist yet (Write of a new file): resolve the parent.
		resolved = filepath.Join(d, filepath.Base(clean))
	}
	for _, root := range []string{si.rootReal, si.rootRaw} {
		if root == "" {
			continue
		}
		if rel, ok := underRoot(resolved, root); ok {
			return rel, true
		}
		if rel, ok := underRoot(clean, root); ok {
			return rel, true
		}
	}
	return clean, false
}

func underRoot(p, root string) (string, bool) {
	if p == root {
		return ".", true
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if strings.HasPrefix(p, prefix) {
		return p[len(prefix):], true
	}
	return "", false
}

func (c *Coordinator) bumpLedger(si *sessionInfo, path string, kind TouchKind, tool string, ts time.Time) {
	key := ledgerKey{sessionID: si.id, path: path, kind: string(kind)}
	if d, ok := c.dirtyLedger[key]; ok {
		d.count++
		d.lastTs = ts
		d.lastTool = tool
		return
	}
	c.dirtyLedger[key] = &ledgerDelta{projectID: si.projectID, count: 1, firstTs: ts, lastTs: ts, lastTool: tool}
}
