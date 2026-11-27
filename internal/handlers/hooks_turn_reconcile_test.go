package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/sessionprompt"
	"openpoet/internal/websocket"
)

// These tests drive the real HookHandler over HTTP, the way the bridge does,
// against a SessionService wired like production, with a fake transcript.
// The event sequences mirror what Claude Code 2.1.332 was observed to send:
// an Esc during a tool or during a reply sends no Stop, no PostToolUse and
// no idle notification; the transcript records the interruption.

const turnTestSession = "sess-turn-1"

type turnTestStore struct{ session database.Session }

func (s *turnTestStore) GetProject(context.Context, int64) (*database.Project, error) {
	return nil, sql.ErrNoRows
}
func (s *turnTestStore) GetSession(_ context.Context, id string) (*database.Session, error) {
	if id != s.session.ID {
		return nil, sql.ErrNoRows
	}
	copy := s.session
	return &copy, nil
}
func (s *turnTestStore) GetTask(context.Context, int64) (*database.ProjectTask, error) {
	return nil, sql.ErrNoRows
}
func (s *turnTestStore) GetTaskForSession(context.Context, string) (*database.ProjectTask, error) {
	return nil, sql.ErrNoRows
}
func (s *turnTestStore) EndSession(context.Context, string, string) error { return nil }
func (s *turnTestStore) UpdateSessionLineage(context.Context, string, string, string) error {
	return nil
}
func (s *turnTestStore) UpdateSessionWorkspace(context.Context, string, string, string) error {
	return nil
}
func (s *turnTestStore) SessionWriteFootprint(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s *turnTestStore) ProjectIDsForTags(context.Context, []int64) ([]int64, error) {
	return nil, nil
}

type turnTestManager struct{}

func (turnTestManager) StartSession(context.Context, *database.Project, map[string]string) (*database.Session, error) {
	return nil, errors.New("unused")
}
func (turnTestManager) StartRemoteSession(context.Context, *database.Project, map[string]string, func(string, string) (string, error)) (*database.Session, error) {
	return nil, errors.New("unused")
}
func (turnTestManager) ReopenSession(context.Context, *database.Session, *database.Project, map[string]string, func(string, string) (string, error)) error {
	return errors.New("unused")
}
func (turnTestManager) StopSession(context.Context, string) error { return errors.New("unused") }
func (turnTestManager) IsSessionRunning(string) bool              { return true }
func (turnTestManager) WriteToSession(string, []byte) error       { return errors.New("unused") }
func (turnTestManager) GetSessionOutput(string) ([]byte, error)   { return nil, nil }

// turnTestInput records typed lines; block makes a send hang until released.
type turnTestInput struct {
	mu    sync.Mutex
	lines []string
	block chan struct{}
	typed chan struct{}
}

func (i *turnTestInput) SubmitSessionLine(_ context.Context, _ string, text string) error {
	i.mu.Lock()
	block, typed := i.block, i.typed
	i.lines = append(i.lines, text)
	i.mu.Unlock()
	if typed != nil {
		typed <- struct{}{}
	}
	if block != nil {
		<-block
	}
	return nil
}

func (i *turnTestInput) typedLines() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.lines...)
}

type turnTestTranscripts struct {
	mu     sync.Mutex
	events []*jsonlview.SessionEvent
	reason string
	err    error
	// onRead runs while the transcript is being read (concurrency tests).
	onRead func()
}

func (t *turnTestTranscripts) set(events ...*jsonlview.SessionEvent) {
	t.mu.Lock()
	t.events = events
	t.mu.Unlock()
}

func (t *turnTestTranscripts) SessionTranscript(context.Context, string) ([]*jsonlview.SessionEvent, string, error) {
	t.mu.Lock()
	events, reason, err, onRead := t.events, t.reason, t.err, t.onRead
	t.mu.Unlock()
	if onRead != nil {
		onRead()
	}
	return events, reason, err
}

type turnTestEffects struct {
	mu      sync.Mutex
	records []application.SessionTurnReconciliation
	changes []string
}

func (e *turnTestEffects) PublishSessionChange(_ context.Context, change application.SessionChange) {
	e.mu.Lock()
	e.changes = append(e.changes, change.Action)
	e.mu.Unlock()
}

func (e *turnTestEffects) RecordSessionTurnReconciliation(_ context.Context, record application.SessionTurnReconciliation) {
	e.mu.Lock()
	e.records = append(e.records, record)
	e.mu.Unlock()
}

func (e *turnTestEffects) outcomes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, record := range e.records {
		out = append(out, record.Outcome)
	}
	return out
}

// turnTestQuestions shows the hook's parked question, else a terminal one.
type turnTestQuestions struct {
	hook     *HookHandler
	mu       sync.Mutex
	terminal *sessionprompt.Question
}

func (q *turnTestQuestions) PendingQuestion(_ context.Context, sessionID string) (*sessionprompt.Question, error) {
	if question := q.hook.pendingHookQuestion(sessionID); question != nil {
		return question, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.terminal, nil
}

func (q *turnTestQuestions) AnswerQuestion(context.Context, string, *sessionprompt.Question, sessionprompt.Answer) error {
	return errors.New("unused")
}

type turnHarness struct {
	hook        *HookHandler
	service     *application.SessionService
	transcripts *turnTestTranscripts
	effects     *turnTestEffects
	input       *turnTestInput
	questions   *turnTestQuestions
	// skew moves the service's clock forward, standing in for hook silence.
	skew  atomic.Int64
	seq   atomic.Int64
	clock atomic.Int64
}

func newTurnHarness(t *testing.T) *turnHarness {
	t.Helper()
	hub := websocket.NewHub()
	go hub.Run()
	h := &turnHarness{
		hook: NewHookHandler(hub, nil, nil), transcripts: &turnTestTranscripts{},
		effects: &turnTestEffects{}, input: &turnTestInput{},
	}
	h.clock.Store(time.Now().UnixNano())
	h.questions = &turnTestQuestions{hook: h.hook}
	store := &turnTestStore{session: database.Session{
		ID: turnTestSession, ProjectID: 20, Status: "running", Backend: "claude_code",
		TaskID: sql.NullInt64{Int64: 42, Valid: true},
	}}
	h.service = application.NewSessionService(store, turnTestManager{}, nil, nil, nil, nil, nil, h.effects,
		application.SessionCreationCollaborators{
			Input: h.input, Signals: h.hook,
			Now: func() time.Time { return time.Now().Add(time.Duration(h.skew.Load())) },
		})
	questions := application.NewSessionQuestionService(store, h.questions, h.effects)
	h.service.SetTurnReconciliation(h.transcripts, questions)
	return h
}

// later makes the service see the session as silent for d more.
func (h *turnHarness) later(d time.Duration) { h.skew.Add(int64(d)) }

// post delivers a hook event like the new bridge: a fresh id and an emission
// time later than every earlier event.
func (h *turnHarness) post(t *testing.T, event map[string]any) {
	t.Helper()
	h.postWith(t, event, "ev-"+strconv.FormatInt(h.seq.Add(1), 10), h.clock.Add(int64(time.Millisecond)))
}

func (h *turnHarness) postWith(t *testing.T, event map[string]any, id string, emittedAt int64) {
	t.Helper()
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/event", strings.NewReader(string(body)))
	req.Header.Set("X-Session-ID", turnTestSession)
	if id != "" {
		req.Header.Set(hookEventIDHeader, id)
	}
	if emittedAt > 0 {
		req.Header.Set(hookEventAtHeader, strconv.FormatInt(emittedAt, 10))
	}
	rr := httptest.NewRecorder()
	h.hook.HandleEvent(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("hook event status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func (h *turnHarness) prompt(t *testing.T, text string) {
	h.post(t, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": text})
}

func (h *turnHarness) preTool(t *testing.T, name, id string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": name, "tool_use_id": id, "tool_input": map[string]any{}})
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/pretooluse", strings.NewReader(string(body)))
	req.Header.Set("X-Session-ID", turnTestSession)
	rr := httptest.NewRecorder()
	h.hook.HandlePreToolUseGate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pretooluse status=%d", rr.Code)
	}
}

func (h *turnHarness) turnOpen(t *testing.T) bool {
	t.Helper()
	turn, known := h.hook.SessionTurnState(turnTestSession)
	if !known {
		t.Fatal("turn state unknown")
	}
	return turn.Open
}

func (h *turnHarness) preflight(t *testing.T) *application.TurnPreflightView {
	t.Helper()
	view, err := h.service.TurnPreflight(context.Background(), turnTestSession, application.SessionTurnProbe{})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	return view
}

func turnOperator() application.ActionAuthorization {
	return application.ActionAuthorization{
		Actor:  application.Actor{Type: "automation_client", ID: "operator"},
		Reason: "turn left open after an interrupt", Approved: true, ApprovedBy: "operator",
	}
}

func (h *turnHarness) reconcile(token string) (*application.TurnReconcileResult, error) {
	return h.service.ReconcileTurn(context.Background(), application.ReconcileSessionTurnCommand{
		SessionID: turnTestSession, TurnToken: token, AuthorizationRef: "ain:example-7", Authorization: turnOperator(),
	})
}

func (h *turnHarness) send(force bool) error {
	_, err := h.service.SendInputWithAck(context.Background(), application.SendSessionInputCommand{
		SessionID: turnTestSession, Text: "next step", Authorization: turnOperator(),
		RejectIfBusy: !force, RejectIfAwaitingInput: true,
	})
	return err
}

func applicationCode(err error) string {
	var appErr *application.Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	if err == nil {
		return ""
	}
	return "untyped:" + err.Error()
}

func hasBlocker(view *application.TurnPreflightView, code string) bool {
	for _, blocker := range view.Blockers {
		if blocker == code {
			return true
		}
	}
	return false
}

// Transcript builders, shaped like jsonlview's parse of Claude Code's JSONL.
var transcriptClock = time.Date(2026, 11, 27, 12, 44, 0, 0, time.UTC)

func tEntry(kind string, blocks []jsonlview.ContentBlock, stop, model string) *jsonlview.SessionEvent {
	transcriptClock = transcriptClock.Add(time.Second)
	return &jsonlview.SessionEvent{Type: kind, UUID: "u" + strconv.FormatInt(transcriptClock.Unix(), 10), Timestamp: transcriptClock,
		Message: &jsonlview.EventMessage{Role: kind, ContentBlocks: blocks, StopReason: stop, Model: model}}
}
func tPrompt(text string) *jsonlview.SessionEvent {
	return tEntry("user", []jsonlview.ContentBlock{{Type: "text", Text: text}}, "", "")
}
func tReply(text, stop string) *jsonlview.SessionEvent {
	return tEntry("assistant", []jsonlview.ContentBlock{{Type: "text", Text: text}}, stop, "claude-haiku-5-5")
}
func tToolUse(name, id string) *jsonlview.SessionEvent {
	return tEntry("assistant", []jsonlview.ContentBlock{{Type: "tool_use", ToolName: name, ToolID: id}}, "tool_use", "claude-haiku-5-5")
}
func tToolResult(id string, isError bool, content string) *jsonlview.SessionEvent {
	return tEntry("user", []jsonlview.ContentBlock{{Type: "tool_result", ToolID: id, IsError: isError, Content: content}}, "", "")
}
func tInterrupt(forTool bool) *jsonlview.SessionEvent {
	text := "[Request interrupted by user]"
	if forTool {
		text = "[Request interrupted by user for tool use]"
	}
	return tEntry("user", []jsonlview.ContentBlock{{Type: "text", Text: text}}, "", "")
}

const turnPrompt = "Run the checksum script: python3 work.py (it takes about 45 seconds) and tell me the checksum."

// The observed failure: an Esc while a tool runs leaves the turn open for
// good; the preflight finds the interruption in the transcript, the
// reconciliation closes the turn, and guarded sends are accepted again.
func TestTurnReconcileAfterInterruptDuringTool(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, turnPrompt)
	h.preTool(t, "Bash", "toolu_A")
	h.post(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "toolu_A"})
	h.preTool(t, "Bash", "toolu_B")
	// Esc: Claude Code sends nothing more.
	h.transcripts.set(
		tPrompt("older prompt"), tReply("done", "end_turn"),
		tPrompt(turnPrompt), tToolUse("Bash", "toolu_A"), tToolResult("toolu_A", false, "work.py"),
		tToolUse("Bash", "toolu_B"),
		tToolResult("toolu_B", true, "The user doesn't want to proceed with this tool use. The tool use was rejected"),
		tInterrupt(true),
	)
	if !h.turnOpen(t) {
		t.Fatal("turn should be open after the interrupt (no Stop was sent)")
	}
	if code := applicationCode(h.send(false)); code != "session_busy" {
		t.Fatalf("guarded send while stuck = %q, want session_busy", code)
	}

	// Right after the interrupt the hooks are not quiet yet.
	if view := h.preflight(t); view.Verdict != application.TurnVerdictBlocked || !hasBlocker(view, "hooks_recent") {
		t.Fatalf("immediate preflight = %s %v, want blocked by hooks_recent", view.Verdict, view.Blockers)
	}
	h.later(31 * time.Second)
	view := h.preflight(t)
	if view.Verdict != application.TurnVerdictReconcilable || view.Evidence == nil || view.Evidence.Kind != application.TurnEvidenceInterrupted {
		t.Fatalf("preflight = %+v, want reconcilable on interrupted evidence", view)
	}
	if view.Turn == nil || len(view.Turn.PendingTools) != 1 || view.Turn.PendingTools[0].ID != "toolu_B" {
		t.Fatalf("turn detail = %+v, want toolu_B pending in the hooks (resolved by the transcript)", view.Turn)
	}

	result, err := h.reconcile(view.TurnToken)
	if err != nil || result.Outcome != "reconciled" || !result.Changed {
		t.Fatalf("reconcile = %+v, %v", result, err)
	}
	if h.turnOpen(t) {
		t.Fatal("turn still open after reconcile")
	}
	turn, _ := h.hook.SessionTurnState(turnTestSession)
	if turn.Reason != "reconciled" {
		t.Fatalf("turn reason = %q", turn.Reason)
	}
	if lines := h.input.typedLines(); len(lines) != 0 {
		t.Fatalf("reconcile typed into the session: %q", lines)
	}
	if err := h.send(false); err != nil {
		t.Fatalf("guarded send after reconcile: %v", err)
	}

	// Repeating the call is safe: nothing to do.
	again, err := h.reconcile(view.TurnToken)
	if err != nil || again.Outcome != "already_closed" || again.Changed {
		t.Fatalf("second reconcile = %+v, %v", again, err)
	}
	h.effects.mu.Lock()
	first := h.effects.records[0]
	h.effects.mu.Unlock()
	if first.Outcome != "reconciled" || first.AuthorizationRef != "ain:example-7" || first.Reason == "" ||
		first.Evidence != application.TurnEvidenceInterrupted || first.TaskID != 42 || first.PreviousReason != "UserPromptSubmit" {
		t.Fatalf("audit record = %+v", first)
	}
	if got := h.effects.outcomes(); strings.Join(got, ",") != "reconciled,already_closed" {
		t.Fatalf("audit outcomes = %v", got)
	}
}

func TestTurnReconcileAfterInterruptedReply(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "Write a 600-word essay about the history of tea.")
	h.transcripts.set(tPrompt("Write a 600-word essay about the history of tea."), tReply("A Leaf That Changed", ""), tInterrupt(false))
	h.later(time.Minute)
	view := h.preflight(t)
	if view.Verdict != application.TurnVerdictReconcilable || view.Evidence.Kind != application.TurnEvidenceInterrupted {
		t.Fatalf("preflight = %s %+v %v", view.Verdict, view.Evidence, view.Blockers)
	}
}

// A lost Stop POST: the reply finished (end_turn) but the turn stayed open.
func TestTurnReconcileWhenStopWasLost(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "summarize")
	h.transcripts.set(tPrompt("summarize"), tReply("Here is the summary.", "end_turn"))
	h.later(time.Minute)
	view := h.preflight(t)
	if view.Verdict != application.TurnVerdictReconcilable || view.Evidence.Kind != application.TurnEvidenceCompleted {
		t.Fatalf("preflight = %s %+v %v", view.Verdict, view.Evidence, view.Blockers)
	}
	if result, err := h.reconcile(view.TurnToken); err != nil || result.Outcome != "reconciled" {
		t.Fatalf("reconcile = %+v %v", result, err)
	}
}

// A long legitimate tool run: the turn is hours old and the hooks silent,
// which proves nothing; the transcript shows the call without a result.
func TestTurnPreflightRefusesLongToolRun(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "run the migration")
	h.preTool(t, "Bash", "toolu_long")
	h.transcripts.set(tPrompt("run the migration"), tToolUse("Bash", "toolu_long"))
	h.later(3 * time.Hour)
	view := h.preflight(t)
	if view.Verdict != application.TurnVerdictBlocked || !hasBlocker(view, "tool_pending") || view.Evidence.Kind != application.TurnEvidenceToolRunning {
		t.Fatalf("preflight = %s %v %+v", view.Verdict, view.Blockers, view.Evidence)
	}
	_, err := h.reconcile(view.TurnToken)
	if code := applicationCode(err); code != "turn_reconcile_refused" || !strings.Contains(err.Error(), "tool_pending") {
		t.Fatalf("reconcile err = %v (%s)", err, code)
	}
	if !h.turnOpen(t) {
		t.Fatal("a refused reconciliation closed the turn")
	}
	if got := h.effects.outcomes(); len(got) != 1 || got[0] != "refused" {
		t.Fatalf("audit outcomes = %v, want the refusal recorded", got)
	}

	// The hooks saw a tool start that the transcript does not show at all
	// (transcript lagging): still pending.
	h.transcripts.set(tPrompt("run the migration"), tReply("Let me check.", "end_turn"))
	if view := h.preflight(t); !hasBlocker(view, "tool_pending") {
		t.Fatalf("hook-pending tool not blocking: %v", view.Blockers)
	}
}

func TestTurnPreflightRefusesAgentStillWorking(t *testing.T) {
	cases := map[string][]*jsonlview.SessionEvent{
		"reply streaming":        {tPrompt("go"), tReply("Working on", "")},
		"tool result unanswered": {tPrompt("go"), tToolUse("Read", "toolu_r"), tToolResult("toolu_r", false, "file")},
		"prompt just written":    {tPrompt("go")},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			h := newTurnHarness(t)
			h.prompt(t, "go")
			h.transcripts.set(events...)
			h.later(time.Hour)
			view := h.preflight(t)
			if view.Verdict != application.TurnVerdictBlocked || !hasBlocker(view, "agent_working") {
				t.Fatalf("preflight = %s %v %+v", view.Verdict, view.Blockers, view.Evidence)
			}
		})
	}
}

func TestTurnPreflightInconclusiveRefuses(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		setup  func(*turnTestTranscripts)
	}{
		{"no transcript", "go", func(tr *turnTestTranscripts) { tr.reason = "not_found" }},
		{"transcript unreadable", "go", func(tr *turnTestTranscripts) { tr.err = errors.New("ssh timeout") }},
		{"prompt not in transcript", "the newest prompt", func(tr *turnTestTranscripts) {
			tr.set(tPrompt("an older prompt"), tReply("done", "end_turn"))
		}},
		{"no prompt text from hooks", "", func(tr *turnTestTranscripts) { tr.set(tPrompt("x"), tInterrupt(false)) }},
		{"harness error message", "go", func(tr *turnTestTranscripts) {
			tr.set(tPrompt("go"), tEntry("assistant", []jsonlview.ContentBlock{{Type: "text", Text: "API Error: overloaded"}}, "stop_sequence", "<synthetic>"))
		}},
		{"max tokens", "go", func(tr *turnTestTranscripts) { tr.set(tPrompt("go"), tReply("cut", "max_tokens")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTurnHarness(t)
			h.prompt(t, tc.prompt)
			tc.setup(h.transcripts)
			h.later(time.Hour)
			view := h.preflight(t)
			if view.Verdict != application.TurnVerdictBlocked || !hasBlocker(view, "evidence_inconclusive") {
				t.Fatalf("preflight = %s %v %+v", view.Verdict, view.Blockers, view.Evidence)
			}
			if _, err := h.reconcile(view.TurnToken); applicationCode(err) != "turn_reconcile_refused" {
				t.Fatalf("reconcile err = %v", err)
			}
			if !h.turnOpen(t) {
				t.Fatal("turn closed on inconclusive evidence")
			}
		})
	}
}

// parkPermission parks a PermissionRequest the way the bridge does and
// returns a channel with the hook's HTTP response once it is answered or
// times out.
func (h *turnHarness) parkPermission(t *testing.T, tool string, toolInput map[string]any) <-chan string {
	t.Helper()
	done := make(chan string, 1)
	body, _ := json.Marshal(map[string]any{"hook_event_name": "PermissionRequest", "tool_name": tool, "tool_input": toolInput})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/hooks/permission", strings.NewReader(string(body)))
		req.Header.Set("X-Session-ID", turnTestSession)
		rr := httptest.NewRecorder()
		h.hook.HandlePermission(rr, req)
		done <- rr.Body.String()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !h.hook.HasPendingPermission(turnTestSession) {
		if time.Now().After(deadline) {
			t.Fatal("permission request never parked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return done
}

// An open AskUserQuestion: preflight and reconcile refuse, and force cannot
// type into the dialog. Its timeout answers deny, is recorded, and does not
// close the turn.
func TestTurnPendingQuestionBlocksAndPermissionTimeoutKeepsTurn(t *testing.T) {
	h := newTurnHarness(t)
	h.hook.permissionTimeout = 400 * time.Millisecond
	h.prompt(t, "pick one")
	h.transcripts.set(tPrompt("pick one"), tReply("Which one?", "end_turn"))
	done := h.parkPermission(t, "AskUserQuestion", map[string]any{"questions": []any{map[string]any{"question": "A or B?", "options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B"}}}}})
	h.later(time.Hour)

	view := h.preflight(t)
	if !hasBlocker(view, "session_awaiting_input") || !hasBlocker(view, "permission_pending") {
		t.Fatalf("preflight blockers = %v", view.Blockers)
	}
	if _, err := h.reconcile(view.TurnToken); applicationCode(err) != "turn_reconcile_refused" {
		t.Fatalf("reconcile err = %v", err)
	}
	if code := applicationCode(h.send(true)); code != "session_awaiting_input" {
		t.Fatalf("force send into a dialog = %q, want session_awaiting_input", code)
	}
	if lines := h.input.typedLines(); len(lines) != 0 {
		t.Fatalf("typed into the dialog: %q", lines)
	}

	select {
	case response := <-done:
		if !strings.Contains(response, `"behavior":"deny"`) || !strings.Contains(response, "timed out") {
			t.Fatalf("timeout response = %s", response)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("permission request never timed out")
	}
	activity := h.hook.SessionTurnActivity(turnTestSession)
	if activity.PermissionTimeout == nil || activity.PermissionTimeout.Tool != "AskUserQuestion" || activity.PendingPermission {
		t.Fatalf("activity after timeout = %+v", activity)
	}
	if !h.turnOpen(t) {
		t.Fatal("a permission timeout closed the turn")
	}
}

// A dialog painted on the terminal (no hook, no open turn) is protected too:
// neither a guarded nor a forced send types into it. The interactive UI path
// (no RejectIfAwaitingInput) is unchanged.
func TestTerminalDialogRefusesAgentSends(t *testing.T) {
	h := newTurnHarness(t)
	h.questions.mu.Lock()
	h.questions.terminal = &sessionprompt.Question{ID: "t_1", Kind: "workspace_trust", Source: sessionprompt.SourceTerminal}
	h.questions.mu.Unlock()
	for _, force := range []bool{false, true} {
		if code := applicationCode(h.send(force)); code != "session_awaiting_input" {
			t.Fatalf("send force=%v = %q, want session_awaiting_input", force, code)
		}
	}
	if _, err := h.service.SendInputWithAck(context.Background(), application.SendSessionInputCommand{
		SessionID: turnTestSession, Text: "1", Authorization: turnOperator(),
	}); err != nil {
		t.Fatalf("UI send: %v", err)
	}
	if lines := h.input.typedLines(); len(lines) != 1 || lines[0] != "1" {
		t.Fatalf("typed = %q, want only the UI line", lines)
	}
}

// A bridge retry (same event id) is applied once.
func TestRepeatedHookDeliveryIsAppliedOnce(t *testing.T) {
	h := newTurnHarness(t)
	h.postWith(t, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "a"}, "ev-a", 100)
	h.postWith(t, map[string]any{"hook_event_name": "Stop"}, "ev-stop", 200)
	token := h.hook.SessionTurnActivity(turnTestSession).Token
	h.postWith(t, map[string]any{"hook_event_name": "Stop"}, "ev-stop", 200)
	h.postWith(t, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "a"}, "ev-a", 100)
	activity := h.hook.SessionTurnActivity(turnTestSession)
	if activity.Duplicates != 2 || activity.Token != token {
		t.Fatalf("duplicates=%d token %s -> %s, want 2 drops and no epoch change", activity.Duplicates, token, activity.Token)
	}
	if h.turnOpen(t) {
		t.Fatal("a replayed UserPromptSubmit reopened the finished turn")
	}
}

// A late delivery cannot undo a newer turn change, in either direction.
func TestLateHookDeliveryDoesNotUndoNewerTurn(t *testing.T) {
	h := newTurnHarness(t)
	// The prompt's POST is delayed (retried) past the turn's own Stop.
	h.postWith(t, map[string]any{"hook_event_name": "Stop"}, "stop-1", 200)
	h.postWith(t, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "a"}, "ups-1", 100)
	if h.turnOpen(t) {
		t.Fatal("a late UserPromptSubmit reopened the finished turn")
	}
	// A new prompt opens the next turn; a late Stop from before it is ignored.
	h.postWith(t, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "b"}, "ups-2", 300)
	h.postWith(t, map[string]any{"hook_event_name": "Stop"}, "stop-0", 250)
	if !h.turnOpen(t) {
		t.Fatal("a late Stop closed the newer turn")
	}
	if activity := h.hook.SessionTurnActivity(turnTestSession); activity.Stale != 2 {
		t.Fatalf("stale drops = %d, want 2", activity.Stale)
	}
	// An old bridge (no headers) keeps arrival order.
	h.postWith(t, map[string]any{"hook_event_name": "Stop"}, "", 0)
	if h.turnOpen(t) {
		t.Fatal("a headerless Stop did not close the turn")
	}
}

// A hook event between preflight and reconcile voids the token.
func TestReconcileRefusesAfterHookSincePreflight(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "go")
	h.transcripts.set(tPrompt("go"), tInterrupt(false))
	h.later(time.Minute)
	view := h.preflight(t)
	if view.Verdict != application.TurnVerdictReconcilable {
		t.Fatalf("preflight = %s %v", view.Verdict, view.Blockers)
	}
	h.post(t, map[string]any{"hook_event_name": "Notification", "message": "Claude needs your permission"})
	if _, err := h.reconcile(view.TurnToken); applicationCode(err) != "turn_changed" {
		t.Fatalf("reconcile err = %v, want turn_changed", err)
	}
	if !h.turnOpen(t) {
		t.Fatal("stale token closed the turn")
	}
}

// A hook arriving while the reconciliation is checking (during its
// transcript read) makes the compare-and-swap fail.
func TestReconcileLosesRaceWithHookDuringCheck(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "go")
	h.transcripts.set(tPrompt("go"), tInterrupt(false))
	h.later(time.Minute)
	token := h.preflight(t).TurnToken
	var once sync.Once
	h.transcripts.mu.Lock()
	h.transcripts.onRead = func() {
		once.Do(func() {
			h.post(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "tool_use_id": "x"})
		})
	}
	h.transcripts.mu.Unlock()
	_, err := h.reconcile(token)
	if code := applicationCode(err); code != "turn_changed" {
		t.Fatalf("reconcile err = %v, want turn_changed", err)
	}
	if !h.turnOpen(t) {
		t.Fatal("turn closed despite a concurrent hook")
	}
}

// The input gate serializes reconciliation and guarded sends.
func TestReconcileAndGuardedSendExcludeEachOther(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "go")
	h.post(t, map[string]any{"hook_event_name": "Stop"})

	// A guarded send in flight blocks a reconciliation.
	h.input.mu.Lock()
	h.input.block, h.input.typed = make(chan struct{}), make(chan struct{}, 1)
	h.input.mu.Unlock()
	sendDone := make(chan error, 1)
	go func() { sendDone <- h.send(false) }()
	<-h.input.typed
	if view := h.preflight(t); view.Verdict != application.TurnVerdictNotNeeded {
		t.Fatalf("preflight on a closed turn = %s", view.Verdict)
	}
	if _, err := h.reconcile("whatever"); applicationCode(err) != "session_input_in_flight" {
		t.Fatalf("reconcile during send = %v, want session_input_in_flight", err)
	}
	close(h.input.block)
	if err := <-sendDone; err != nil {
		t.Fatalf("send: %v", err)
	}
	h.input.mu.Lock()
	h.input.block, h.input.typed = nil, nil
	h.input.mu.Unlock()

	// A reconciliation in progress blocks a guarded send.
	h.prompt(t, "go")
	h.transcripts.set(tPrompt("go"), tInterrupt(false))
	h.later(time.Minute)
	token := h.preflight(t).TurnToken
	reading, release := make(chan struct{}), make(chan struct{})
	h.transcripts.mu.Lock()
	h.transcripts.onRead = func() { close(reading); <-release }
	h.transcripts.mu.Unlock()
	reconcileDone := make(chan error, 1)
	go func() { _, err := h.reconcile(token); reconcileDone <- err }()
	<-reading
	if code := applicationCode(h.send(false)); code != "session_busy" {
		t.Fatalf("send during reconcile = %q, want session_busy", code)
	}
	close(release)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestReconcileRequiresAuthorizationAndToken(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "go")
	base := application.ReconcileSessionTurnCommand{SessionID: turnTestSession, TurnToken: "x.1", AuthorizationRef: "ain:1", Authorization: turnOperator()}
	cases := map[string]func(*application.ReconcileSessionTurnCommand){
		"reason_required":            func(c *application.ReconcileSessionTurnCommand) { c.Authorization.Reason = " " },
		"action_approval_required":   func(c *application.ReconcileSessionTurnCommand) { c.Authorization.Approved = false },
		"authorization_ref_required": func(c *application.ReconcileSessionTurnCommand) { c.AuthorizationRef = "no-ref" },
		"turn_token_required":        func(c *application.ReconcileSessionTurnCommand) { c.TurnToken = "" },
		"action_actor_required":      func(c *application.ReconcileSessionTurnCommand) { c.Authorization.Actor = application.Actor{} },
	}
	for want, mutate := range cases {
		command := base
		mutate(&command)
		if _, err := h.service.ReconcileTurn(context.Background(), command); applicationCode(err) != want {
			t.Fatalf("%s: got %v", want, err)
		}
	}
	if got := h.effects.outcomes(); len(got) != 0 {
		t.Fatalf("unauthorized attempts recorded: %v", got)
	}
}

// Hooks, sends and reconciliations racing each other (run with -race): the
// turn is never closed while a hook event the reconciliation did not check
// has arrived, and the invariant holds that a reconciled close is followed
// only by states hooks set.
func TestTurnConcurrencyStress(t *testing.T) {
	h := newTurnHarness(t)
	h.prompt(t, "go")
	h.transcripts.set(tPrompt("go"), tInterrupt(false))
	h.later(time.Minute)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.post(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "tool_use_id": "r" + strconv.Itoa(i)})
		}
	}()
	var reconciled, changed atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				view, err := h.service.TurnPreflight(context.Background(), turnTestSession, application.SessionTurnProbe{})
				if err != nil {
					t.Error(err)
					return
				}
				_, err = h.reconcile(view.TurnToken)
				switch applicationCode(err) {
				case "":
					reconciled.Add(1)
				case "turn_changed", "turn_reconcile_refused", "session_input_in_flight":
					changed.Add(1)
				default:
					t.Errorf("unexpected reconcile error %v", err)
				}
				_ = h.send(false)
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if reconciled.Load()+changed.Load() == 0 {
		t.Fatal("no reconciliation attempts completed")
	}
}
