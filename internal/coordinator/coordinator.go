package coordinator

import (
	"context"
	"errors"
	"log"
	"path/filepath"
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
	claims         map[claimKey]map[string]TouchKind // (projectKey,rel) → sessionID → strongest kind
	incidents      map[string]*Incident              // rule|scope_key → incident
	dirtyIncidents map[string]struct{}
	dirtyLedger    map[ledgerKey]*ledgerDelta
	pendingEvents  []database.EventOutboxAppend
	lastEmit       map[string]time.Time // hysteresis per rule|scope_key
	dropped        int64

	resolveSession func(ctx context.Context, sessionID string) (*sessionInfo, error)
	now            func() time.Time

	// OnEscalate fires asynchronously once per newly-opened critical incident.
	// Wired in main.go to the proactive-notification machinery.
	OnEscalate func(inc Incident)
}

func New(db *database.DB) *Coordinator {
	c := &Coordinator{
		db:             db,
		ch:             make(chan ingestMsg, ingestBuffer),
		quit:           make(chan struct{}),
		sessions:       make(map[string]*sessionInfo),
		claims:         make(map[claimKey]map[string]TouchKind),
		incidents:      make(map[string]*Incident),
		dirtyIncidents: make(map[string]struct{}),
		dirtyLedger:    make(map[ledgerKey]*ledgerDelta),
		lastEmit:       make(map[string]time.Time),
		now:            time.Now,
	}
	c.resolveSession = c.resolveFromDB
	return c
}

// Start spawns the indexer and flusher goroutines.
func (c *Coordinator) Start() {
	c.wg.Add(2)
	go func() { defer c.wg.Done(); c.indexLoop() }()
	go func() { defer c.wg.Done(); c.flushLoop() }()
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
	case "PreToolUse", "PostToolUse", "PostToolUseFailure":
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
	ev := awaitingInputEvent(sessionID, kind, excerpt, c.now())
	c.mu.Lock()
	c.pendingEvents = append(c.pendingEvents, ev)
	c.mu.Unlock()
}

// ForgetSession drops a session's cache entry and live claims. Persisted
// ledger rows survive (they are the audit trail, not the live index).
func (c *Coordinator) ForgetSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, sessionID)
	for key, set := range c.claims {
		delete(set, sessionID)
		if len(set) == 0 {
			delete(c.claims, key)
		}
	}
}

func (c *Coordinator) enqueue(msg ingestMsg) {
	select {
	case c.ch <- msg:
		return
	default:
	}
	// Full: drop the oldest queued message, then retry once.
	select {
	case <-c.ch:
	default:
	}
	c.mu.Lock()
	c.dropped++
	c.mu.Unlock()
	select {
	case c.ch <- msg:
	default:
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
		c.pendingEvents = append(c.pendingEvents, turnCompletedEvent(si.id, msg.ts))
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
		if t.Kind == KindWrite && inProject {
			c.noteWriteClaim(si, rel, msg.tool, msg.ts)
		}
	}
	c.evalSameTask(si, msg.ts)
}

// session returns the cached resolution for sessionID, hitting the DB only on
// first sight. Resolution happens outside c.mu (it does I/O).
func (c *Coordinator) session(id string) *sessionInfo {
	c.mu.Lock()
	si, seen := c.sessions[id]
	c.mu.Unlock()
	if seen {
		return si
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	resolved, err := c.resolveSession(ctx, id)
	if err != nil {
		log.Printf("[Coordinator] cannot resolve session %s: %v", id, err)
		resolved = nil
	}
	c.mu.Lock()
	c.sessions[id] = resolved
	c.mu.Unlock()
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
