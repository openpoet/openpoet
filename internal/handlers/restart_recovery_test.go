package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/deployrecord"
)

type recoveryHarness struct {
	t          *testing.T
	db         *database.DB
	recordPath string

	mu        sync.Mutex
	prompts   map[string][]string
	events    []string
	payloads  map[string][]map[string]any
	running   map[string]bool
	turns     map[string]application.SessionTurnState
	questions map[string]bool
}

func newRecoveryHarness(t *testing.T) *recoveryHarness {
	t.Helper()
	dir := t.TempDir()
	db, err := database.New(filepath.Join(dir, "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &recoveryHarness{
		t: t, db: db, recordPath: filepath.Join(dir, deployrecord.FileName),
		prompts: map[string][]string{}, payloads: map[string][]map[string]any{},
		running: map[string]bool{}, turns: map[string]application.SessionTurnState{}, questions: map[string]bool{},
	}
}

// process builds a recovery as one server process would see it.
func (h *recoveryHarness) process() *restartRecovery {
	r := newRestartRecovery(h.db, h.recordPath)
	r.pollInterval = 10 * time.Millisecond
	r.restore = func(_ context.Context, sess *database.Session) error {
		h.mu.Lock()
		h.running[sess.ID] = true
		h.mu.Unlock()
		return nil
	}
	r.submit = func(_ context.Context, sessionID, text string) error {
		h.mu.Lock()
		h.prompts[sessionID] = append(h.prompts[sessionID], text)
		h.mu.Unlock()
		return nil
	}
	r.isRunning = func(id string) bool { h.mu.Lock(); defer h.mu.Unlock(); return h.running[id] }
	r.turnState = func(id string) (application.SessionTurnState, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		turn, ok := h.turns[id]
		return turn, ok
	}
	r.pendingQuestion = func(_ context.Context, id string) bool { h.mu.Lock(); defer h.mu.Unlock(); return h.questions[id] }
	r.publish = func(_ context.Context, domain, action, aggregateID string, fields map[string]any) {
		h.mu.Lock()
		key := "platform." + domain + "." + action
		h.events = append(h.events, key+":"+aggregateID)
		h.payloads[key+":"+aggregateID] = append(h.payloads[key+":"+aggregateID], fields)
		h.mu.Unlock()
	}
	return r
}

func (h *recoveryHarness) turn(r *restartRecovery, id string, open bool, reason string) {
	state := application.SessionTurnState{Open: open, Since: time.Now(), Reason: reason}
	h.mu.Lock()
	h.turns[id] = state
	h.mu.Unlock()
	r.PersistTurn(id, state)
}

func (h *recoveryHarness) writeRecord(record deployrecord.Record) {
	h.t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.recordPath, data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *recoveryHarness) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = map[string][]string{}
	h.events = nil
	h.payloads = map[string][]map[string]any{}
	h.running = map[string]bool{}
	h.turns = map[string]application.SessionTurnState{}
	h.questions = map[string]bool{}
}

func (h *recoveryHarness) waitPrompts(want int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := 0
		for _, list := range h.prompts {
			n += len(list)
		}
		h.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("expected %d prompts, got %v", want, h.prompts)
}

func (h *recoveryHarness) eventCount(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, event := range h.events {
		if strings.HasPrefix(event, prefix) {
			n++
		}
	}
	return n
}

func sessions(ids ...string) []database.Session {
	out := make([]database.Session, 0, len(ids))
	for _, id := range ids {
		out = append(out, database.Session{ID: id, Status: "running"})
	}
	return out
}

// A deploy restarts the server while one session is mid-turn (it is the one
// that ran deploy.sh), one is idle and one is mid-turn on a question. After
// the restart only the mid-turn session is resumed, with the deploy result
// that arrives after the boot, and the deploy is published once.
func TestRestartRecoveryResumesInterruptedTurnWithDeployResult(t *testing.T) {
	h := newRecoveryHarness(t)
	before := h.process()
	h.turn(before, "mid", true, "UserPromptSubmit")
	h.turn(before, "idle", true, "UserPromptSubmit")
	h.turn(before, "idle", false, "Stop")
	h.turn(before, "asking", true, "UserPromptSubmit")
	h.questions["asking"] = true
	before.SnapshotForShutdown([]string{"mid", "idle", "asking"})
	// Killed agents fire SessionEnd during the stop; it must not close the turn.
	h.turn(before, "mid", false, "SessionEnd")

	stopped := time.Now().Add(-2 * time.Second)
	record := deployrecord.Record{
		ID: "20260929T223432Z-1", Commit: "abc1234", RequestedBySession: "mid",
		StartedAt: stopped.Add(-30 * time.Second), StoppedAt: &stopped, Restarts: 1,
		State: deployrecord.StateRunning, Step: "verifying production", Health: "pending",
	}
	h.writeRecord(record)

	h.reset()
	after := h.process()
	after.RunBoot(context.Background(), sessions("mid", "idle", "asking"))
	if got := h.eventCount("platform.session.restored:"); got != 3 {
		t.Fatalf("restored events = %d, want 3: %v", got, h.events)
	}
	mid := h.payloads["platform.session.restored:mid"][0]
	if mid["interrupted_turn"] != true || mid["will_resume"] != true || mid["restart_cause"] != "deploy" {
		t.Fatalf("mid restored payload = %v", mid)
	}
	asking := h.payloads["platform.session.restored:asking"][0]
	if asking["interrupted_turn"] != true || asking["awaiting_input"] != true || asking["will_resume"] != false {
		t.Fatalf("asking restored payload = %v", asking)
	}

	// deploy.sh finishes the health check after the new server is up.
	time.Sleep(50 * time.Millisecond)
	finished := time.Now()
	record.State, record.Health, record.Version, record.FinishedAt = deployrecord.StateSucceeded, "ok", "v9", &finished
	h.writeRecord(record)

	h.waitPrompts(1)
	time.Sleep(100 * time.Millisecond)
	if len(h.prompts) != 1 || len(h.prompts["mid"]) != 1 {
		t.Fatalf("prompts = %v, want exactly one for mid", h.prompts)
	}
	text := h.prompts["mid"][0]
	for _, want := range []string{"reiniciou durante o seu turno", "abc1234 concluído com sucesso", "você mesmo disparou", "continue de onde parou"} {
		if !strings.Contains(text, want) {
			t.Fatalf("continuation %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "\n") {
		t.Fatalf("continuation must be one line: %q", text)
	}
	if got := h.eventCount("platform.deploy.completed:" + record.ID); got != 1 {
		t.Fatalf("deploy.completed = %d: %v", got, h.events)
	}
	if got := h.eventCount("platform.session.resume_prompt_delivered:mid"); got != 1 {
		t.Fatalf("resume_prompt_delivered = %d: %v", got, h.events)
	}

	// The resumed agent reports SessionStart before the continuation lands;
	// that must not read as the turn being over.
	h.turn(after, "mid", false, "SessionStart")

	// A crash later (the same deploy record, no new stop) is a plain restart:
	// the deploy is not reported again. mid's continuation never landed (no
	// UserPromptSubmit), so it is resumed again, once.
	h.reset()
	again := h.process()
	again.RunBoot(context.Background(), sessions("mid", "idle", "asking"))
	h.waitPrompts(1)
	time.Sleep(50 * time.Millisecond)
	if len(h.prompts) != 1 || !strings.Contains(h.prompts["mid"][0], "reinício do serviço") {
		t.Fatalf("second restart prompts = %v", h.prompts)
	}
	if got := h.eventCount("platform.deploy."); got != 0 {
		t.Fatalf("deploy reported again: %v", h.events)
	}

	// Once the continuation's turn ran and ended, the next restart resumes nothing.
	h.turn(again, "mid", true, "UserPromptSubmit")
	h.turn(again, "mid", false, "Stop")
	again.SnapshotForShutdown([]string{"mid", "idle", "asking"})
	h.reset()
	third := h.process()
	third.RunBoot(context.Background(), sessions("mid", "idle", "asking"))
	time.Sleep(100 * time.Millisecond)
	if len(h.prompts) != 0 {
		t.Fatalf("idle sessions were prompted: %v", h.prompts)
	}
}

// deploy as the last step: the requesting session ended its turn before the
// server stopped. It is not "interrupted", but it is told the deploy result.
func TestRestartRecoveryTellsIdleRequesterTheDeployResult(t *testing.T) {
	h := newRecoveryHarness(t)
	before := h.process()
	h.turn(before, "req", true, "UserPromptSubmit")
	h.turn(before, "req", false, "Stop")
	before.SnapshotForShutdown([]string{"req"})

	stopped := time.Now().Add(-time.Second)
	finished := time.Now()
	h.writeRecord(deployrecord.Record{
		ID: "d-rollback", Commit: "bad0001", RequestedBySession: "req", StartedAt: stopped, StoppedAt: &stopped,
		FinishedAt: &finished, Restarts: 2, State: deployrecord.StateRolledBack, Health: "failed", Rollback: "ok", Version: "v8",
	})
	h.reset()
	after := h.process()
	after.RunBoot(context.Background(), sessions("req"))
	h.waitPrompts(1)
	text := h.prompts["req"][0]
	if !strings.Contains(text, "O deploy que você disparou (d-rollback)") || !strings.Contains(text, "revertido") || !strings.Contains(text, "retomada com --resume") {
		t.Fatalf("notice = %q", text)
	}
	if got := h.eventCount("platform.deploy.failed:d-rollback"); got != 1 {
		t.Fatalf("deploy.failed = %d: %v", got, h.events)
	}
	if h.payloads["platform.session.restored:req"][0]["will_resume"] != false {
		t.Fatalf("idle requester must not be resumed as interrupted")
	}
}

// A build that fails never restarts the server: the running watcher reports
// it and tells the requester once its turn is over.
func TestRestartRecoveryReportsDeployWithoutRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	r := h.process()
	h.running["req"] = true
	h.turn(r, "req", true, "UserPromptSubmit")
	started := time.Now()
	h.writeRecord(deployrecord.Record{
		ID: "d-build", Commit: "c0ffee1", RequestedBySession: "req", StartedAt: started, FinishedAt: &started,
		State: deployrecord.StateFailed, Step: "building project", Detail: "build error",
	})
	record, _ := deployrecord.Read(h.recordPath)
	done := make(chan struct{})
	go func() { r.reportDeploy(context.Background(), record, false); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if len(h.prompts) != 0 {
		t.Fatalf("typed into a mid-turn session: %v", h.prompts)
	}
	h.turn(r, "req", false, "Stop")
	<-done
	if len(h.prompts["req"]) != 1 || !strings.Contains(h.prompts["req"][0], "antes de parar a produção (build error)") {
		t.Fatalf("prompts = %v", h.prompts)
	}
	r.reportDeploy(context.Background(), record, false)
	if got := h.eventCount("platform.deploy.failed:d-build"); got != 1 {
		t.Fatalf("deploy.failed published %d times", got)
	}
	if len(h.prompts["req"]) != 1 {
		t.Fatalf("requester told twice: %v", h.prompts)
	}
}

func TestRestartRecoveryIgnoresStaleDeployRecord(t *testing.T) {
	h := newRecoveryHarness(t)
	stopped := time.Now().Add(-2 * time.Hour)
	h.writeRecord(deployrecord.Record{ID: "old", StartedAt: stopped, StoppedAt: &stopped, Restarts: 1, State: deployrecord.StateSucceeded})
	cause, record := h.process().restartCause(context.Background())
	if cause != "restart" || record != nil {
		t.Fatalf("cause = %q record = %v, want a plain restart", cause, record)
	}
}

func TestHookTurnChangesReachTheObserver(t *testing.T) {
	hook := NewHookHandler(nil, nil, nil)
	var seen []bool
	hook.setTurnObserver(func(_ string, turn application.SessionTurnState) { seen = append(seen, turn.Open) })
	hook.trackTurnFromEvent("s1", "UserPromptSubmit", nil)
	hook.trackTurnFromEvent("s1", "PostToolUse", nil)
	hook.trackTurnFromEvent("s1", "Stop", nil)
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("observer saw %v, want [true false]", seen)
	}
}

// The events land in the outbox with the platform.* names the myLifeOS
// signal router maps.
func TestRestartRecoveryEventsReachTheOutbox(t *testing.T) {
	h := newRecoveryHarness(t)
	effects := &platformEffects{db: h.db}
	r := h.process()
	r.publish = func(ctx context.Context, domain, action, aggregateID string, fields map[string]any) {
		effects.auditPayload(ctx, domain, action, aggregateID, application.Actor{Type: "system", ID: "restart-recovery"}, fields)
	}
	now := time.Now()
	record := &deployrecord.Record{ID: "d-ok", Commit: "1234567", StartedAt: now, FinishedAt: &now, State: deployrecord.StateSucceeded, Restarts: 1}
	r.reportDeploy(context.Background(), record, true)
	events, err := h.db.ListEventOutboxAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != "platform.deploy.completed" || events[0].AggregateID != "d-ok" {
		t.Fatalf("outbox = %+v", events)
	}
	if !strings.Contains(events[0].PayloadJSON, `"commit":"1234567"`) {
		t.Fatalf("payload = %s", events[0].PayloadJSON)
	}
}
