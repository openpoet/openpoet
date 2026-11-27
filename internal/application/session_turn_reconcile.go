package application

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/sessionprompt"
)

// A turn the hooks left open (an Esc sends no Stop, a hook POST was lost) keeps
// every guarded send refused with session_busy. TurnPreflight says whether the
// runtime has positive evidence that the agent is back at its prompt, and
// ReconcileTurn closes such a turn, changing only the turn state: it never
// types, presses a key or interrupts anything. Silence and the age of the turn
// are never evidence: the agent's own transcript must end the turn.

// SessionPendingTool is a tool the hooks saw start (PreToolUse) and not finish.
type SessionPendingTool struct {
	ID    string    `json:"tool_use_id,omitempty"`
	Name  string    `json:"tool_name,omitempty"`
	Since time.Time `json:"since"`
}

// SessionPermissionTimeout is a permission request nobody answered in time:
// the agent was told deny and carried on (or stopped) on its own.
type SessionPermissionTimeout struct {
	At   time.Time `json:"at"`
	Tool string    `json:"tool_name,omitempty"`
}

// SessionTurnActivity is the hook-derived detail behind a session's turn.
type SessionTurnActivity struct {
	Turn  SessionTurnState
	Known bool
	// Token changes with every hook event the session sends (and with a server
	// restart); a reconciliation only applies to the token it was checked at.
	Token        string
	LastEvent    string
	LastEventAt  time.Time
	LastPromptAt time.Time
	// LastPrompt is the prompt text of the last UserPromptSubmit (never exposed).
	LastPrompt        string
	PendingTools      []SessionPendingTool
	PendingPermission bool
	PermissionTimeout *SessionPermissionTimeout
	// Duplicates and Stale count hook deliveries dropped as a retry already
	// seen and as older than the event that set the current turn state.
	Duplicates int
	Stale      int
}

// SessionTurnControl is implemented by the hook handler, which owns the turn.
type SessionTurnControl interface {
	SessionTurnActivity(sessionID string) SessionTurnActivity
	// CloseTurnIfUnchanged closes the open turn only if no hook event arrived
	// since token was read and no permission request is parked.
	CloseTurnIfUnchanged(sessionID, token, reason string) (previous, current SessionTurnState, err error)
}

var (
	ErrTurnChanged          = errors.New("the session sent hook events since the turn was checked")
	ErrTurnNotOpen          = errors.New("the session's turn is not open")
	ErrTurnPermissionParked = errors.New("a permission request is waiting for an answer")
)

// TurnReconcileQuietPeriod is how long the session must have sent no hook
// event before a reconciliation: it lets the transcript settle after the last
// prompt. It is a stability condition, never evidence of idleness.
const TurnReconcileQuietPeriod = 30 * time.Second

// Preflight verdicts.
const (
	TurnVerdictReconcilable = "reconcilable"
	TurnVerdictBlocked      = "blocked"
	TurnVerdictNotNeeded    = "not_needed"
)

// Transcript evidence kinds.
const (
	TurnEvidenceInterrupted  = "interrupted"  // the user interrupted the turn (Esc)
	TurnEvidenceCompleted    = "completed"    // the reply ended (end_turn) but no Stop arrived
	TurnEvidenceToolRunning  = "tool_running" // a tool call has no result yet
	TurnEvidenceGenerating   = "generating"   // the agent still owes a reply
	TurnEvidenceInconclusive = "inconclusive" // the transcript does not tell
)

type TurnCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// Code is the blocker code when the check failed.
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type TurnEvidence struct {
	Source string     `json:"source"` // transcript
	Kind   string     `json:"kind"`
	At     *time.Time `json:"at,omitempty"`
	Detail string     `json:"detail,omitempty"`
	// UnresolvedTools are tool calls with no result in the transcript.
	UnresolvedTools []string `json:"unresolved_tools,omitempty"`
	// resolved holds every tool_use id the transcript shows a result for.
	resolved map[string]bool
}

// TurnPreflightView is the full diagnosis of a session's turn.
type TurnPreflightView struct {
	SessionID string `json:"session_id"`
	Verdict   string `json:"verdict"`
	// TurnToken is what sessions.reconcile_turn must be sent with.
	TurnToken string         `json:"turn_token,omitempty"`
	Turn      *TurnStateView `json:"turn,omitempty"`
	Checks    []TurnCheck    `json:"checks"`
	Blockers  []string       `json:"blockers,omitempty"`
	Evidence  *TurnEvidence  `json:"evidence,omitempty"`
	CheckedAt time.Time      `json:"checked_at"`
}

// TurnStateView is the operator's view of a turn and the hooks behind it.
type TurnStateView struct {
	Known             bool                      `json:"known"`
	Open              bool                      `json:"open"`
	Since             *time.Time                `json:"since,omitempty"`
	Reason            string                    `json:"reason,omitempty"`
	LastEvent         string                    `json:"last_event,omitempty"`
	LastEventAt       *time.Time                `json:"last_event_at,omitempty"`
	LastPromptAt      *time.Time                `json:"last_prompt_at,omitempty"`
	PendingTools      []SessionPendingTool      `json:"pending_tools,omitempty"`
	PendingPermission bool                      `json:"pending_permission"`
	PermissionTimeout *SessionPermissionTimeout `json:"last_permission_timeout,omitempty"`
	DuplicateEvents   int                       `json:"duplicate_events_dropped,omitempty"`
	StaleEvents       int                       `json:"stale_events_dropped,omitempty"`
}

func turnTimePointer(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

// TurnActivityView renders the activity for sessions.get and the preflight.
func TurnActivityView(activity SessionTurnActivity) *TurnStateView {
	view := &TurnStateView{
		Known: activity.Known, Open: activity.Known && activity.Turn.Open,
		LastEvent: activity.LastEvent, LastEventAt: turnTimePointer(activity.LastEventAt),
		LastPromptAt: turnTimePointer(activity.LastPromptAt), PendingTools: activity.PendingTools,
		PendingPermission: activity.PendingPermission, PermissionTimeout: activity.PermissionTimeout,
		DuplicateEvents: activity.Duplicates, StaleEvents: activity.Stale,
	}
	if activity.Known {
		view.Since = turnTimePointer(activity.Turn.Since)
		view.Reason = activity.Turn.Reason
	}
	return view
}

// SessionTurnProbe is what the caller knows about the session's screen
// lifecycle: the startup state of the initial prompt.
type SessionTurnProbe struct {
	StartupState func(sessionID string) string
}

// SetTurnReconciliation gives the service the agent transcripts and the
// question reader the turn preflight needs. Without them every preflight is
// inconclusive and nothing is ever reconciled.
func (s *SessionService) SetTurnReconciliation(transcripts SessionTranscriptPort, questions *SessionQuestionService) {
	s.turnTranscripts = transcripts
	s.turnQuestions = questions
}

func (s *SessionService) turnControl() (SessionTurnControl, bool) {
	control, ok := s.creation.Signals.(SessionTurnControl)
	return control, ok && control != nil
}

// TurnActivity reports the hook-derived turn detail, when the hook handler
// provides it.
func (s *SessionService) TurnActivity(sessionID string) (SessionTurnActivity, bool) {
	if s == nil {
		return SessionTurnActivity{}, false
	}
	control, ok := s.turnControl()
	if !ok {
		return SessionTurnActivity{}, false
	}
	return control.SessionTurnActivity(sessionID), true
}

// TurnPreflight diagnoses the session's turn without changing anything.
func (s *SessionService) TurnPreflight(ctx context.Context, sessionID string, probe SessionTurnProbe) (*TurnPreflightView, error) {
	session, err := s.getSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	view, _ := s.turnPreflight(ctx, session, probe, false)
	return view, nil
}

type turnCheckList struct {
	checks   []TurnCheck
	blockers []string
}

func (l *turnCheckList) pass(name, detail string) {
	l.checks = append(l.checks, TurnCheck{Name: name, Passed: true, Detail: detail})
}

func (l *turnCheckList) fail(name, code, detail string) {
	l.checks = append(l.checks, TurnCheck{Name: name, Code: code, Detail: detail})
	l.blockers = append(l.blockers, code)
}

// turnPreflight runs every check, even after one fails, so the operator sees
// the whole picture. gateHeld says the caller holds the session's input gate.
func (s *SessionService) turnPreflight(ctx context.Context, session *database.Session, probe SessionTurnProbe, gateHeld bool) (*TurnPreflightView, SessionTurnActivity) {
	now := s.creation.Now
	if now == nil {
		now = time.Now
	}
	view := &TurnPreflightView{SessionID: session.ID, CheckedAt: now().UTC()}
	var list turnCheckList

	running := session.Status == "running" && s.manager != nil && s.manager.IsSessionRunning(session.ID)
	if running {
		list.pass("session_running", "")
	} else {
		list.fail("session_running", "session_not_running", "status "+session.Status+"; only a running session with a live runner has a turn to reconcile")
	}
	if session.Backend == "" || session.Backend == "claude_code" {
		list.pass("backend_supported", "claude_code")
	} else {
		list.fail("backend_supported", "backend_unsupported", "turn reconciliation reads Claude Code transcripts; backend "+session.Backend+" is not supported")
	}

	activity, ok := s.TurnActivity(session.ID)
	if !ok {
		list.fail("turn_state", "turn_state_unavailable", "the hook handler does not report turn detail")
		return finishPreflight(view, list, nil), activity
	}
	view.Turn = TurnActivityView(activity)
	view.TurnToken = activity.Token
	if !activity.Known {
		list.fail("turn_state", "turn_state_unknown", "no turn signal was seen since the server started; there is no open turn to close, and an unknown state already lets guarded sends through")
		return finishPreflight(view, list, nil), activity
	}
	if !activity.Turn.Open {
		view.Verdict = TurnVerdictNotNeeded
		list.pass("turn_open", "the turn is already closed ("+activity.Turn.Reason+")")
		view.Checks, view.Blockers = list.checks, list.blockers
		if len(list.blockers) > 0 {
			view.Verdict = TurnVerdictBlocked
		}
		return view, activity
	}
	list.pass("turn_open", "open since "+activity.Turn.Since.UTC().Format(time.RFC3339)+" ("+activity.Turn.Reason+")")

	if probe.StartupState != nil && probe.StartupState(session.ID) == "starting" {
		list.fail("startup_complete", "session_starting", "the initial prompt is still being delivered")
	} else {
		list.pass("startup_complete", "")
	}
	if s.turnQuestions == nil {
		list.fail("no_open_question", "question_state_unavailable", "the question reader is unavailable")
	} else if question, err := s.turnQuestions.Pending(ctx, session.ID); err != nil {
		list.fail("no_open_question", "question_state_unavailable", "the open question could not be read")
	} else if question != nil {
		list.fail("no_open_question", "session_awaiting_input", "the session is waiting on a "+questionKindLabel(question)+"; answer it with sessions.answer_prompt")
	} else {
		list.pass("no_open_question", "")
	}
	if activity.PendingPermission {
		list.fail("no_pending_permission", "permission_pending", "a permission request is parked waiting for an answer")
	} else {
		list.pass("no_pending_permission", "")
	}
	switch {
	case gateHeld:
		list.pass("no_input_in_flight", "")
	case s.inputs.busy(session.ID):
		list.fail("no_input_in_flight", "session_input_in_flight", "a guarded send to this session is still being delivered")
	default:
		list.pass("no_input_in_flight", "")
	}
	if quiet := now().Sub(activity.LastEventAt); activity.LastEventAt.IsZero() || quiet >= TurnReconcileQuietPeriod {
		list.pass("hooks_quiet", "last hook event "+activity.LastEvent+" "+quiet.Truncate(time.Second).String()+" ago")
	} else {
		list.fail("hooks_quiet", "hooks_recent", "the session sent "+activity.LastEvent+" "+quiet.Truncate(time.Second).String()+" ago; wait "+TurnReconcileQuietPeriod.String()+" of hook silence (this is a settling time, not evidence)")
	}

	evidence := s.turnTranscriptEvidence(ctx, session.ID, activity)
	view.Evidence = evidence
	switch evidence.Kind {
	case TurnEvidenceInterrupted, TurnEvidenceCompleted:
		list.pass("transcript_turn_ended", evidence.Detail)
	case TurnEvidenceToolRunning:
		list.fail("transcript_turn_ended", "tool_pending", evidence.Detail)
	case TurnEvidenceGenerating:
		list.fail("transcript_turn_ended", "agent_working", evidence.Detail)
	default:
		list.fail("transcript_turn_ended", "evidence_inconclusive", evidence.Detail)
	}
	if unresolved := unresolvedHookTools(activity.PendingTools, evidence); len(unresolved) > 0 {
		list.fail("tools_resolved", "tool_pending", "tools started without a result in the transcript: "+strings.Join(unresolved, ", "))
	} else {
		list.pass("tools_resolved", "")
	}
	return finishPreflight(view, list, evidence), activity
}

func finishPreflight(view *TurnPreflightView, list turnCheckList, evidence *TurnEvidence) *TurnPreflightView {
	view.Checks, view.Blockers = list.checks, dedupeStrings(list.blockers)
	if evidence != nil {
		view.Evidence = evidence
	}
	view.Verdict = TurnVerdictReconcilable
	if len(view.Blockers) > 0 {
		view.Verdict = TurnVerdictBlocked
	}
	return view
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func questionKindLabel(question *sessionprompt.Question) string {
	if question.Kind == "" {
		return "question"
	}
	return question.Kind + " question"
}

// unresolvedHookTools lists the tools the hooks saw start that the
// transcript does not show finished.
func unresolvedHookTools(pending []SessionPendingTool, evidence *TurnEvidence) []string {
	var unresolved []string
	for _, tool := range pending {
		label := tool.Name
		if tool.ID == "" {
			unresolved = append(unresolved, label+" (no tool_use_id)")
			continue
		}
		if evidence == nil || evidence.resolved == nil || !evidence.resolved[tool.ID] {
			unresolved = append(unresolved, label+" "+tool.ID)
		}
	}
	sort.Strings(unresolved)
	return unresolved
}

func (s *SessionService) turnTranscriptEvidence(ctx context.Context, sessionID string, activity SessionTurnActivity) *TurnEvidence {
	if s.turnTranscripts == nil {
		return &TurnEvidence{Source: "transcript", Kind: TurnEvidenceInconclusive, Detail: "the transcript reader is unavailable"}
	}
	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	events, reason, err := s.turnTranscripts.SessionTranscript(readCtx, sessionID)
	switch {
	case reason != "":
		return &TurnEvidence{Source: "transcript", Kind: TurnEvidenceInconclusive, Detail: "the session has no readable transcript (" + reason + ")"}
	case err != nil:
		return &TurnEvidence{Source: "transcript", Kind: TurnEvidenceInconclusive, Detail: "the transcript could not be read"}
	}
	return ClassifyTurnTranscript(events, activity.LastPrompt)
}

const (
	transcriptInterruptMarker = "[Request interrupted by user"
	transcriptSyntheticModel  = "<synthetic>"
)

// ClassifyTurnTranscript reads how the agent's transcript left the turn that
// lastPrompt opened. It anchors on the last user entry holding that prompt
// (no clocks are compared, so a remote host's skew does not matter) and
// looks at what the agent wrote after it.
func ClassifyTurnTranscript(events []*jsonlview.SessionEvent, lastPrompt string) *TurnEvidence {
	evidence := &TurnEvidence{Source: "transcript", Kind: TurnEvidenceInconclusive}
	evidence.resolved = map[string]bool{}
	var entries []*jsonlview.SessionEvent
	for _, event := range events {
		if event == nil || event.Message == nil || event.IsSidechain {
			continue
		}
		if event.Type != "user" && event.Type != "assistant" {
			continue
		}
		entries = append(entries, event)
		if event.Type == "user" {
			for _, block := range event.Message.ContentBlocks {
				if block.Type == "tool_result" && block.ToolID != "" {
					evidence.resolved[block.ToolID] = true
				}
			}
		}
	}
	if strings.TrimSpace(lastPrompt) == "" {
		evidence.Detail = "the hooks reported no prompt text to anchor the turn in the transcript"
		return evidence
	}
	anchor := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if text, ok := transcriptPromptText(entries[i]); ok && transcriptPromptMatches(text, lastPrompt) {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		evidence.Detail = "the last prompt the agent accepted is not in its transcript"
		return evidence
	}

	issued := map[string]string{} // tool_use id -> tool name, for calls after the anchor
	var order []string
	last := -1
	for i := anchor + 1; i < len(entries); i++ {
		entry := entries[i]
		if entry.Type == "assistant" {
			if entry.Message.Model == transcriptSyntheticModel && transcriptBlocksText(entry.Message.ContentBlocks, false) == "No response requested." {
				continue // Claude Code's filler after an interruption
			}
			for _, block := range entry.Message.ContentBlocks {
				if block.Type == "tool_use" && block.ToolID != "" {
					if _, seen := issued[block.ToolID]; !seen {
						order = append(order, block.ToolID)
					}
					issued[block.ToolID] = block.ToolName
				}
			}
		}
		last = i
	}
	for _, id := range order {
		if !evidence.resolved[id] {
			evidence.UnresolvedTools = append(evidence.UnresolvedTools, issued[id]+" "+id)
		}
	}
	if last < 0 {
		evidence.Kind = TurnEvidenceGenerating
		evidence.At = turnTimePointer(entries[anchor].Timestamp)
		evidence.Detail = "the agent wrote nothing after the prompt yet"
		return evidence
	}
	entry := entries[last]
	evidence.At = turnTimePointer(entry.Timestamp)
	switch {
	case len(evidence.UnresolvedTools) > 0:
		evidence.Kind = TurnEvidenceToolRunning
		evidence.Detail = "tool calls without a result: " + strings.Join(evidence.UnresolvedTools, ", ")
	case entry.Type == "user" && transcriptIsInterruptMarker(entry):
		evidence.Kind = TurnEvidenceInterrupted
		evidence.Detail = "the transcript ends with the user's interruption of the turn"
	case entry.Type == "user":
		evidence.Kind = TurnEvidenceGenerating
		evidence.Detail = "the transcript ends with input the agent has not answered yet"
	case entry.Message.Model == transcriptSyntheticModel:
		evidence.Detail = "the transcript ends with a message the harness wrote (" + transcriptExcerpt(transcriptBlocksText(entry.Message.ContentBlocks, false)) + ")"
	case entry.Message.StopReason == "end_turn" && !transcriptHasToolUse(entry):
		evidence.Kind = TurnEvidenceCompleted
		evidence.Detail = "the agent's last reply ended its turn (end_turn) but no Stop hook arrived"
	case entry.Message.StopReason == "" || entry.Message.StopReason == "tool_use":
		evidence.Kind = TurnEvidenceGenerating
		evidence.Detail = "the agent's last reply is not finished (stop_reason " + strconv.Quote(entry.Message.StopReason) + ")"
	default:
		evidence.Detail = "the agent's last reply stopped with " + entry.Message.StopReason
	}
	return evidence
}

func transcriptHasToolUse(entry *jsonlview.SessionEvent) bool {
	for _, block := range entry.Message.ContentBlocks {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func transcriptIsInterruptMarker(entry *jsonlview.SessionEvent) bool {
	for _, block := range entry.Message.ContentBlocks {
		if block.Type == "text" && strings.HasPrefix(strings.TrimSpace(block.Text), transcriptInterruptMarker) {
			return true
		}
	}
	return false
}

// transcriptPromptText returns the text of a user entry that is a prompt
// (not a tool result, harness line or interruption marker).
func transcriptPromptText(entry *jsonlview.SessionEvent) (string, bool) {
	if entry.Type != "user" || entry.IsMeta || entry.IsSummary {
		return "", false
	}
	var parts []string
	for _, block := range entry.Message.ContentBlocks {
		switch block.Type {
		case "tool_result":
			return "", false
		case "text":
			parts = append(parts, block.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" || strings.HasPrefix(text, transcriptInterruptMarker) {
		return "", false
	}
	return text, true
}

func transcriptPromptMatches(text, prompt string) bool {
	if strings.TrimSpace(prompt) == "" {
		return false
	}
	return PromptContainsInput(text, prompt) || PromptContainsInput(prompt, text)
}

func transcriptExcerpt(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return string(runes)
}

// ReconcileSessionTurnCommand closes a turn the preflight found ended.
type ReconcileSessionTurnCommand struct {
	SessionID string
	// TurnToken is the preflight's turn_token: the reconciliation only applies
	// to the state the operator checked.
	TurnToken        string
	AuthorizationRef string
	Probe            SessionTurnProbe
	Authorization    ActionAuthorization
}

// TurnReconcileResult is what a reconciliation did.
type TurnReconcileResult struct {
	SessionID string `json:"session_id"`
	// Outcome is reconciled (the turn was closed) or already_closed.
	Outcome  string         `json:"outcome"`
	Changed  bool           `json:"changed"`
	Previous *TurnStateView `json:"previous_turn,omitempty"`
	Turn     *TurnStateView `json:"turn,omitempty"`
	Evidence *TurnEvidence  `json:"evidence,omitempty"`
	Checks   []TurnCheck    `json:"checks"`
}

// SessionTurnReconciliation is the audit record of a reconciliation attempt.
type SessionTurnReconciliation struct {
	SessionID        string
	ProjectID        int64
	TaskID           int64
	Outcome          string // reconciled | already_closed | refused
	Blockers         []string
	Evidence         string
	PreviousSince    time.Time
	PreviousReason   string
	TurnToken        string
	Reason           string
	AuthorizationRef string
	Actor            Actor
	ApprovedBy       string
}

// SessionTurnReconciliationRecorder is optionally implemented by
// SessionEffects to keep who reconciled a turn, on what evidence and why.
type SessionTurnReconciliationRecorder interface {
	RecordSessionTurnReconciliation(context.Context, SessionTurnReconciliation)
}

// ReconcileTurn closes a session's open turn when the preflight finds
// positive evidence that it ended, holding the session's input gate so no
// guarded send can start meanwhile. Every attempt past authorization is
// recorded, refused ones included.
func (s *SessionService) ReconcileTurn(ctx context.Context, command ReconcileSessionTurnCommand) (*TurnReconcileResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(command.Authorization.Reason)
	if reason == "" {
		return nil, validationError("reason_required", "A reason is required to reconcile a session's turn")
	}
	if !command.Authorization.Approved || strings.TrimSpace(command.Authorization.ApprovedBy) == "" {
		return nil, validationError("action_approval_required", "Reconciling a session's turn needs an owner-tier caller (UI, paired device or automation client); session agents cannot do it")
	}
	ref := strings.TrimSpace(command.AuthorizationRef)
	if !validPermissionModeAuthorizationRef(ref) {
		return nil, validationError("authorization_ref_required", "An authorization_ref (e.g. ain:example-1) naming the owner's authorization is required to reconcile a session's turn")
	}
	token := strings.TrimSpace(command.TurnToken)
	if token == "" || len(token) > 200 {
		return nil, validationError("turn_token_required", "turn_token from sessions.turn_preflight is required")
	}
	session, err := s.getSession(ctx, command.SessionID)
	if err != nil {
		return nil, err
	}
	control, ok := s.turnControl()
	if !ok {
		return nil, conflictError("turn_state_unavailable", "The hook handler does not report turn detail")
	}
	// Once past this point the attempt is recorded whatever the caller does.
	ctx = context.WithoutCancel(ctx)
	record := SessionTurnReconciliation{
		SessionID: session.ID, ProjectID: session.ProjectID, TurnToken: token,
		Reason: reason, AuthorizationRef: ref, Actor: command.Authorization.Actor, ApprovedBy: command.Authorization.ApprovedBy,
	}
	if session.TaskID.Valid {
		record.TaskID = session.TaskID.Int64
	}
	refuse := func(code, message string, blockers []string) (*TurnReconcileResult, error) {
		record.Outcome = "refused"
		record.Blockers = blockers
		s.recordTurnReconciliation(ctx, record)
		return nil, conflictError(code, message)
	}

	if !s.inputs.acquire(session.ID) {
		return refuse("session_input_in_flight", "A guarded send to this session is still being delivered; re-run sessions.turn_preflight after it is acknowledged", []string{"session_input_in_flight"})
	}
	defer s.inputs.release(session.ID)

	view, activity := s.turnPreflight(ctx, session, command.Probe, true)
	if view.Turn != nil {
		record.PreviousSince, record.PreviousReason = activity.Turn.Since, activity.Turn.Reason
	}
	if view.Evidence != nil {
		record.Evidence = view.Evidence.Kind
	}
	if view.Verdict == TurnVerdictNotNeeded {
		record.Outcome = "already_closed"
		s.recordTurnReconciliation(ctx, record)
		return &TurnReconcileResult{SessionID: session.ID, Outcome: "already_closed", Turn: view.Turn, Checks: view.Checks}, nil
	}
	if view.TurnToken != token {
		return refuse("turn_changed", "The session sent hook events (or the server restarted) since this turn_token was issued; re-run sessions.turn_preflight", []string{"turn_changed"})
	}
	if view.Verdict != TurnVerdictReconcilable {
		return refuse("turn_reconcile_refused", "The turn was not reconciled: "+strings.Join(view.Blockers, ", ")+"; sessions.turn_preflight details each check", view.Blockers)
	}
	previous, _, err := control.CloseTurnIfUnchanged(session.ID, token, "reconciled")
	switch {
	case errors.Is(err, ErrTurnChanged):
		return refuse("turn_changed", "A hook event arrived while the turn was being reconciled; re-run sessions.turn_preflight", []string{"turn_changed"})
	case errors.Is(err, ErrTurnPermissionParked):
		return refuse("turn_reconcile_refused", "A permission request arrived while the turn was being reconciled", []string{"permission_pending"})
	case errors.Is(err, ErrTurnNotOpen):
		record.Outcome = "already_closed"
		s.recordTurnReconciliation(ctx, record)
		return &TurnReconcileResult{SessionID: session.ID, Outcome: "already_closed", Turn: TurnActivityView(control.SessionTurnActivity(session.ID)), Checks: view.Checks}, nil
	case err != nil:
		return refuse("turn_reconcile_failed", "The turn could not be reconciled", []string{"turn_reconcile_failed"})
	}
	record.Outcome = "reconciled"
	record.PreviousSince, record.PreviousReason = previous.Since, previous.Reason
	s.recordTurnReconciliation(ctx, record)
	s.publish(ctx, SessionChange{Action: "turn_reconciled", Session: session, ID: session.ID, Actor: command.Authorization.Actor})
	return &TurnReconcileResult{
		SessionID: session.ID, Outcome: "reconciled", Changed: true,
		Previous: view.Turn, Turn: TurnActivityView(control.SessionTurnActivity(session.ID)),
		Evidence: view.Evidence, Checks: view.Checks,
	}, nil
}

func (s *SessionService) recordTurnReconciliation(ctx context.Context, record SessionTurnReconciliation) {
	if recorder, ok := s.effects.(SessionTurnReconciliationRecorder); ok {
		recorder.RecordSessionTurnReconciliation(ctx, record)
	}
}
