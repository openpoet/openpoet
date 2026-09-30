package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/deployrecord"
)

const (
	// A deploy restart is only attributed to a record whose production stop
	// happened shortly before this boot: an old record must never label a
	// later crash or reboot as "deploy".
	deployRestartWindow = 15 * time.Minute
	// How long the boot waits for deploy.sh to finish (health check, maybe a
	// rollback) so the resume prompt can carry the result. deploy.sh itself
	// gives up on health after 60s per start.
	deployOutcomeWait = 3 * time.Minute
	// How long a deploy-result notice waits for the requesting session's turn
	// to end before typing anyway is not an option: it gives up instead.
	deployNoticeTurnWait  = 30 * time.Minute
	deployWatchInterval   = 5 * time.Second
	settingDeployRestart  = "deploy.last_restart_key"
	settingDeployReported = "deploy.last_reported_id"
)

// restartRecovery resumes the work a server restart interrupted. It keeps
// every live session's turn state in the database, and on the next boot:
//   - restores the sessions (claude --resume, as before);
//   - types a continuation prompt into each session that was mid-turn and not
//     waiting on a question — once per restart, never into an idle session;
//   - publishes platform.session.restored for each one;
//   - reads the deploy record (.run/deploy.record.json) and publishes
//     platform.deploy.completed / platform.deploy.failed, telling the session
//     that ran deploy.sh how it went (the deploy is its turn's last step).
//
// Its collaborators are functions so tests can drive it without a runtime.
type restartRecovery struct {
	db         *database.DB
	recordPath string
	bootAt     time.Time

	restore         func(ctx context.Context, sess *database.Session) error
	submit          func(ctx context.Context, sessionID, text string) error
	isRunning       func(sessionID string) bool
	turnState       func(sessionID string) (application.SessionTurnState, bool)
	pendingQuestion func(ctx context.Context, sessionID string) bool
	publish         func(ctx context.Context, domain, action, aggregateID string, fields map[string]any)

	// pollInterval is how often waits re-check; tests shorten it.
	pollInterval time.Duration

	mu     sync.Mutex // serializes persistence with the shutdown snapshot
	frozen bool

	deployMu sync.Mutex // one deploy report at a time (boot and watcher)
	// resumed holds sessions whose continuation prompt already carries the
	// deploy result, so they are not told a second time.
	resumed map[string]bool
	// pendingContinuation holds restored sessions whose continuation prompt
	// has not opened a turn yet. The resumed agent's SessionStart must not
	// mark them closed, or a second restart before the prompt lands would
	// resume nothing.
	pendingContinuation map[string]bool
}

func newRestartRecovery(db *database.DB, recordPath string) *restartRecovery {
	return &restartRecovery{
		db:           db,
		recordPath:   recordPath,
		bootAt:       time.Now(),
		pollInterval: time.Second,
		resumed:      make(map[string]bool),

		pendingContinuation: make(map[string]bool),
	}
}

// PersistTurn records a turn change as it happens, so even a crash (no
// graceful shutdown) leaves the last known state behind. After the shutdown
// snapshot it is a no-op: agents killed by the shutdown fire SessionEnd, and
// that must not overwrite the turn they were in.
func (r *restartRecovery) PersistTurn(sessionID string, turn application.SessionTurnState) {
	if r == nil || r.db == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	if r.pendingContinuation[sessionID] {
		if !turn.Open {
			return
		}
		delete(r.pendingContinuation, sessionID)
	}
	if err := r.db.SaveSessionTurnState(context.Background(), sessionID, turn.Open, turn.Since, turn.Reason); err != nil {
		log.Printf("[Restart] could not persist turn of %s: %v", shortSessionID(sessionID), err)
	}
}

// PersistAwaitingInput records that a session started or stopped waiting on a
// question, which the question monitor sees before any shutdown.
func (r *restartRecovery) PersistAwaitingInput(sessionID string, awaiting bool) {
	if r == nil || r.db == nil || r.turnState == nil {
		return
	}
	turn, known := r.turnState(sessionID)
	if !known {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	_ = r.db.SaveSessionRestartState(context.Background(), database.SessionRestartState{
		SessionID: sessionID, TurnOpen: turn.Open, TurnReason: turn.Reason, AwaitingInput: awaiting,
		TurnSince: sql.NullTime{Time: turn.Since, Valid: !turn.Since.IsZero()},
	})
}

// SnapshotForShutdown writes the final state of every running session and
// freezes persistence. Call it before the sessions are stopped.
func (r *restartRecovery) SnapshotForShutdown(sessionIDs []string) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	open := 0
	for _, id := range sessionIDs {
		if r.turnState == nil {
			break
		}
		turn, known := r.turnState(id)
		if !known {
			// Keep whatever the last boot left (e.g. a continuation still
			// pending): unknown is not "idle".
			continue
		}
		awaiting := r.pendingQuestion != nil && r.pendingQuestion(ctx, id)
		if turn.Open {
			open++
		}
		if err := r.db.SaveSessionRestartState(ctx, database.SessionRestartState{
			SessionID: id, TurnOpen: turn.Open, TurnReason: turn.Reason, AwaitingInput: awaiting,
			TurnSince: sql.NullTime{Time: turn.Since, Valid: !turn.Since.IsZero()},
		}); err != nil {
			log.Printf("[Restart] could not snapshot %s: %v", shortSessionID(id), err)
		}
	}
	log.Printf("[Restart] shutdown snapshot: %d session(s), %d mid-turn", len(sessionIDs), open)
}

// restartCause says why this process started: a deploy (with its record) or
// any other restart (crash, reboot, manual, update).
func (r *restartRecovery) restartCause(ctx context.Context) (string, *deployrecord.Record) {
	record, err := deployrecord.Read(r.recordPath)
	if err != nil {
		log.Printf("[Restart] deploy record unreadable: %v", err)
		return "restart", nil
	}
	if record == nil || record.RestartKey() == "" || record.StoppedAt == nil {
		return "restart", nil
	}
	if record.StoppedAt.After(r.bootAt) || r.bootAt.Sub(*record.StoppedAt) > deployRestartWindow {
		return "restart", nil
	}
	key := record.RestartKey()
	if last, _ := r.db.GetSetting(ctx, settingDeployRestart); last == key {
		return "restart", nil
	}
	_ = r.db.SetSetting(ctx, settingDeployRestart, key)
	return "deploy", record
}

type restoredSession struct {
	session     database.Session
	state       database.SessionRestartState
	known       bool
	interrupted bool
}

// RunBoot restores the sessions that were live before the restart and then,
// in the background, resumes the interrupted ones. It returns once every
// restore was attempted.
func (r *restartRecovery) RunBoot(ctx context.Context, sessions []database.Session) {
	if r == nil {
		return
	}
	states, err := r.db.ListSessionRestartStates(ctx)
	if err != nil {
		log.Printf("[Restart] turn states unavailable, nothing will be resumed: %v", err)
		states = map[string]database.SessionRestartState{}
	}
	cause, record := r.restartCause(ctx)
	deployID := ""
	if record != nil {
		deployID = record.ID
	}

	var restored []restoredSession
	if len(sessions) > 0 {
		log.Printf("[AutoRestore] Restoring %d active session(s) from before restart (cause: %s)...", len(sessions), cause)
	}
	for _, sess := range sessions {
		sess := sess
		state, known := states[sess.ID]
		delete(states, sess.ID)
		item := restoredSession{session: sess, state: state, known: known, interrupted: known && state.TurnOpen && !state.AwaitingInput}
		if item.interrupted {
			// Stays open until the continuation's own UserPromptSubmit lands, so
			// a second restart before that resumes it again. Marked before the
			// restore: the resumed agent's SessionStart can arrive at any time.
			r.mu.Lock()
			r.pendingContinuation[sess.ID] = true
			r.mu.Unlock()
			state.TurnReason = "continuation_pending"
			_ = r.db.SaveSessionRestartState(ctx, state)
		} else {
			_ = r.db.DeleteSessionRestartState(ctx, sess.ID)
		}
		if err := r.restore(ctx, &sess); err != nil {
			log.Printf("[AutoRestore] Failed to restore session %s: %v", sess.ID, err)
			r.mu.Lock()
			delete(r.pendingContinuation, sess.ID)
			r.mu.Unlock()
			_ = r.db.DeleteSessionRestartState(ctx, sess.ID)
			continue
		}
		log.Printf("[AutoRestore] Session %s restored successfully", sess.ID)
		restored = append(restored, item)
		var turnSince any
		if known && state.TurnSince.Valid {
			turnSince = state.TurnSince.Time.UTC().Format(time.RFC3339)
		}
		r.emit(ctx, "session", "restored", sess.ID, map[string]any{
			"action": "restored", "interrupted_turn": known && state.TurnOpen, "awaiting_input": known && state.AwaitingInput,
			"turn_known": known, "turn_since": turnSince, "will_resume": item.interrupted,
			"restart_cause": cause, "deploy_id": deployID, "task_id": nullInt(sess.TaskID),
		})
	}
	if len(sessions) > 0 {
		log.Printf("[AutoRestore] Done: %d/%d sessions restored", len(restored), len(sessions))
	}
	if cause == "deploy" {
		// Their continuation carries the deploy result; the deploy report
		// (boot or watcher, whichever runs first) must not repeat it.
		r.deployMu.Lock()
		for _, item := range restored {
			if item.interrupted {
				r.resumed[item.session.ID] = true
			}
		}
		r.deployMu.Unlock()
	}
	// Sessions that did not come back leave no state behind.
	for id := range states {
		_ = r.db.DeleteSessionRestartState(ctx, id)
	}

	go r.resumeInterrupted(context.WithoutCancel(ctx), cause, record, restored)
}

func (r *restartRecovery) resumeInterrupted(ctx context.Context, cause string, record *deployrecord.Record, restored []restoredSession) {
	if cause == "deploy" && record != nil && !record.Final() {
		record = r.waitDeployOutcome(ctx, record)
	}
	var interrupted []restoredSession
	for _, item := range restored {
		if item.interrupted {
			interrupted = append(interrupted, item)
		}
	}
	var wg sync.WaitGroup
	for _, item := range interrupted {
		wg.Add(1)
		go func(item restoredSession) {
			defer wg.Done()
			text := continuationPrompt(cause, record, r.bootAt, record != nil && record.RequestedBySession == item.session.ID)
			r.deliver(ctx, item.session.ID, "interrupted_turn", text, record)
		}(item)
	}
	// The deploy report (event + notice to an idle requester) follows the
	// continuations' bookkeeping so a requester that was mid-turn is told once.
	if record != nil && record.Final() {
		r.reportDeploy(ctx, record, cause == "deploy")
	}
	wg.Wait()
}

func (r *restartRecovery) waitDeployOutcome(ctx context.Context, record *deployrecord.Record) *deployrecord.Record {
	deadline := time.Now().Add(deployOutcomeWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return record
		case <-time.After(r.pollInterval):
		}
		latest, err := deployrecord.Read(r.recordPath)
		if err == nil && latest != nil && latest.ID == record.ID {
			record = latest
			if record.Final() {
				return record
			}
		}
	}
	log.Printf("[Restart] deploy %s still %s after %s; resuming without its result", record.ID, record.State, deployOutcomeWait)
	return record
}

// WatchDeploys reports deploys that finish while this process runs — a
// build that failed before stopping production, for instance — and any the
// boot did not report. It returns when ctx ends.
func (r *restartRecovery) WatchDeploys(ctx context.Context) {
	if r == nil || r.recordPath == "" {
		return
	}
	ticker := time.NewTicker(deployWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		record, err := deployrecord.Read(r.recordPath)
		if err != nil || record == nil || !record.Final() {
			continue
		}
		r.reportDeploy(ctx, record, false)
	}
}

// reportDeploy publishes a finished deploy once (across processes: the id is
// kept in settings) and tells the session that ran deploy.sh the result,
// unless its continuation prompt already did.
func (r *restartRecovery) reportDeploy(ctx context.Context, record *deployrecord.Record, restartedThisBoot bool) {
	r.deployMu.Lock()
	if last, _ := r.db.GetSetting(ctx, settingDeployReported); last == record.ID {
		r.deployMu.Unlock()
		return
	}
	if err := r.db.SetSetting(ctx, settingDeployReported, record.ID); err != nil {
		r.deployMu.Unlock()
		log.Printf("[Restart] could not mark deploy %s reported: %v", record.ID, err)
		return
	}
	alreadyTold := r.resumed[record.RequestedBySession]
	r.deployMu.Unlock()

	action := "completed"
	if !record.Succeeded() {
		action = "failed"
	}
	r.emit(ctx, "deploy", action, record.ID, deployEventFields(record, action))
	log.Printf("[Restart] deploy %s %s: %s", record.ID, action, record.Outcome())

	requester := strings.TrimSpace(record.RequestedBySession)
	if requester == "" || alreadyTold || r.isRunning == nil || !r.isRunning(requester) {
		return
	}
	if !r.waitTurnClosed(ctx, requester) {
		log.Printf("[Restart] session %s stayed mid-turn; deploy %s result not typed into it", shortSessionID(requester), record.ID)
		return
	}
	r.deliver(ctx, requester, "deploy_result", deployNoticePrompt(record, restartedThisBoot), record)
}

func (r *restartRecovery) waitTurnClosed(ctx context.Context, sessionID string) bool {
	deadline := time.Now().Add(deployNoticeTurnWait)
	for {
		turn, known := application.SessionTurnState{}, false
		if r.turnState != nil {
			turn, known = r.turnState(sessionID)
		}
		if !known || !turn.Open {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.pollInterval):
		}
	}
}

func (r *restartRecovery) deliver(ctx context.Context, sessionID, kind, text string, record *deployrecord.Record) {
	fields := map[string]any{"kind": kind}
	if record != nil {
		fields["deploy_id"] = record.ID
	}
	err := r.submit(ctx, sessionID, text)
	if err != nil {
		log.Printf("[Restart] %s prompt not delivered to %s: %v", kind, shortSessionID(sessionID), err)
		fields["action"], fields["error"] = "resume_prompt_failed", err.Error()
		r.emit(ctx, "session", "resume_prompt_failed", sessionID, fields)
		return
	}
	log.Printf("[Restart] %s prompt delivered to %s", kind, shortSessionID(sessionID))
	fields["action"] = "resume_prompt_delivered"
	r.emit(ctx, "session", "resume_prompt_delivered", sessionID, fields)
}

func (r *restartRecovery) emit(ctx context.Context, domain, action, aggregateID string, fields map[string]any) {
	if r.publish != nil {
		r.publish(ctx, domain, action, aggregateID, fields)
	}
}

func deployEventFields(record *deployrecord.Record, action string) map[string]any {
	fields := map[string]any{
		"action": action, "deploy_id": record.ID, "commit": record.Commit, "state": record.State,
		"step": record.Step, "health": record.Health, "rollback": record.Rollback, "detail": record.Detail,
		"version": record.Version, "previous_version": record.PreviousVersion,
		"requested_by_session": record.RequestedBySession, "restarts": record.Restarts,
		"started_at": record.StartedAt.UTC().Format(time.RFC3339), "outcome": record.Outcome(),
	}
	if record.FinishedAt != nil {
		fields["finished_at"] = record.FinishedAt.UTC().Format(time.RFC3339)
	}
	return fields
}

// continuationPrompt is typed into a session whose turn the restart cut.
// It is one line: the terminal submits on newline.
func continuationPrompt(cause string, record *deployrecord.Record, bootAt time.Time, requester bool) string {
	reason := "reinício do serviço (crash, reboot ou restart manual)"
	if cause == "deploy" && record != nil {
		reason = "deploy; resultado: " + record.Outcome()
		if requester {
			reason += " — foi o deploy que você mesmo disparou"
		}
	}
	return fmt.Sprintf("[OpenPoet] O servidor do OpenPoet reiniciou durante o seu turno (%s; motivo: %s). "+
		"Esta sessão foi retomada com --resume e qualquer comando que estava rodando foi interrompido. "+
		"Confira o estado atual (git, arquivos, processos, deploy) antes de repetir qualquer efeito e continue de onde parou.",
		bootAt.Format("02/01 15:04"), reason)
}

// deployNoticePrompt tells an idle session the result of the deploy it ran.
func deployNoticePrompt(record *deployrecord.Record, restarted bool) string {
	prefix := ""
	if restarted {
		prefix = "O servidor reiniciou por causa desse deploy e esta sessão foi retomada com --resume. "
	}
	return fmt.Sprintf("[OpenPoet] O deploy que você disparou (%s) terminou: %s. %s"+
		"Faça a verificação pós-deploy (./.scripts/deploy.sh --status e o que mais a task pedir) e registre o resultado na task/relatório.",
		record.ID, record.Outcome(), prefix)
}

func nullInt(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func shortSessionID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
