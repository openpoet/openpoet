package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/session"
	"openpoet/internal/sessionprompt"
)

const (
	// initialPromptQuestionWait bounds how long an initial prompt waits for a
	// startup question (trust dialog, permission, ...) to be answered.
	initialPromptQuestionWait = 30 * time.Minute
	questionPollInterval      = 250 * time.Millisecond
	questionMonitorInterval   = time.Second
	maxHookToolInputChars     = 4000
	maxHookPlanChars          = 8000
)

// platformSessionQuestions merges the two places a session can be blocked on
// a question: a PermissionRequest hook parked in the HookHandler (tool
// permission, AskUserQuestion, plan approval) and a dialog painted on the
// session's terminal. The hook wins when both exist: while a hook is parked
// the agent is waiting on it, not on its terminal.
type platformSessionQuestions struct {
	hook *HookHandler
	mgr  *session.Manager
}

func (p platformSessionQuestions) PendingQuestion(_ context.Context, sessionID string) (*sessionprompt.Question, error) {
	if p.hook != nil {
		if q := p.hook.pendingHookQuestion(sessionID); q != nil {
			return q, nil
		}
	}
	if p.mgr != nil && p.mgr.IsSessionRunning(sessionID) {
		return p.mgr.TerminalQuestion(sessionID), nil
	}
	return nil, nil
}

func (p platformSessionQuestions) AnswerQuestion(ctx context.Context, sessionID string, q *sessionprompt.Question, answer sessionprompt.Answer) error {
	switch q.Source {
	case sessionprompt.SourceHook:
		if p.hook == nil {
			return errors.New("hook responder unavailable")
		}
		return mapQuestionError(p.hook.answerHookQuestion(ctx, sessionID, q, answer))
	case sessionprompt.SourceTerminal:
		if p.mgr == nil {
			return errors.New("session runtime unavailable")
		}
		if answer.Option == 0 && answer.Text != "" && q.Kind != sessionprompt.KindConfirm {
			// A bare text answer to a terminal menu goes through the option that
			// opens a text box ("No, and tell Claude what to do differently").
			for _, option := range q.Options {
				label := strings.ToLower(option.Label)
				if strings.Contains(label, "differently") || strings.Contains(label, "tell codex") {
					answer.Option = option.Index
				}
			}
		}
		return mapQuestionError(p.mgr.AnswerTerminalQuestion(sessionID, q.ID, answer))
	}
	return fmt.Errorf("unknown question source %q", q.Source)
}

func mapQuestionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrNoPendingQuestion), errors.Is(err, ErrNoPendingPermission):
		return &application.Error{Kind: application.ErrorConflict, Code: "session_question_not_pending", Message: "The session has no pending question"}
	case errors.Is(err, session.ErrQuestionChanged):
		return &application.Error{Kind: application.ErrorConflict, Code: "session_question_changed", Message: err.Error()}
	case errors.Is(err, session.ErrInvalidAnswer):
		return &application.Error{Kind: application.ErrorValidation, Code: "session_question_answer_invalid", Message: err.Error()}
	case errors.Is(err, session.ErrSessionNotRunning):
		return &application.Error{Kind: application.ErrorConflict, Code: "session_not_running", Message: "The session is not running"}
	}
	return err
}

// pendingHookQuestion describes the parked PermissionRequest hook, if any.
func (h *HookHandler) pendingHookQuestion(sessionID string) *sessionprompt.Question {
	h.mu.Lock()
	pending, ok := h.pending[sessionID]
	var event map[string]interface{}
	var msgType string
	var createdAt time.Time
	if ok {
		event, msgType, createdAt = pending.hookEvent, pending.msgType, pending.createdAt
	}
	h.mu.Unlock()
	if !ok {
		return nil
	}
	toolName, _ := event["tool_name"].(string)
	toolInput, _ := event["tool_input"].(map[string]interface{})
	q := &sessionprompt.Question{
		ID:         hookQuestionID(sessionID, createdAt),
		Source:     sessionprompt.SourceHook,
		ToolName:   toolName,
		DetectedAt: createdAt.UTC(),
	}
	switch msgType {
	case "askUser":
		q.Kind = sessionprompt.KindAskUserQuestion
		q.AcceptsText = true
		q.Questions = askUserSubQuestions(toolInput)
		if len(q.Questions) == 1 {
			q.Text = q.Questions[0].Text
			q.Options = q.Questions[0].Options
		} else {
			q.Text = strconv.Itoa(len(q.Questions)) + " questions; answer with answers {question text: option label or free text}"
		}
	case "exitPlan":
		q.Kind = sessionprompt.KindPlanApproval
		q.AcceptsText = true
		plan, _ := toolInput["plan"].(string)
		q.Text = sessionprompt.Truncate(strings.TrimSpace(plan), maxHookPlanChars)
		q.Options = []sessionprompt.Option{
			{Index: 1, Label: "Approve, clear context and auto-accept edits"},
			{Index: 2, Label: "Approve and auto-accept edits"},
			{Index: 3, Label: "Approve and review each edit"},
			{Index: 4, Label: "Keep planning", Description: "rejects the plan; text is sent as feedback"},
		}
	default:
		q.Kind = sessionprompt.KindToolPermission
		q.AcceptsText = true
		q.Text = hookPermissionText(toolName, toolInput)
		if encoded, err := json.Marshal(toolInput); err == nil && len(toolInput) > 0 {
			q.ToolInput = sessionprompt.Truncate(string(encoded), maxHookToolInputChars)
		}
		q.Options = []sessionprompt.Option{
			{Index: 1, Label: "Allow", GrantsPermission: true},
			{Index: 2, Label: "Allow always in this session", Description: "also allows later " + toolName + " calls", GrantsPermission: true},
			{Index: 3, Label: "Deny", Description: "text is sent to the agent as the reason"},
		}
	}
	return q
}

func hookQuestionID(sessionID string, createdAt time.Time) string {
	return sessionprompt.HashID("h_", sessionID, strconv.FormatInt(createdAt.UnixNano(), 10))
}

func hookPermissionText(toolName string, toolInput map[string]interface{}) string {
	text := "The agent wants to use " + toolName
	for _, key := range []string{"description", "command", "file_path", "url", "pattern"} {
		if value, ok := toolInput[key].(string); ok && strings.TrimSpace(value) != "" {
			text += "\n" + key + ": " + sessionprompt.Truncate(strings.TrimSpace(value), 500)
		}
	}
	return text
}

func askUserSubQuestions(toolInput map[string]interface{}) []sessionprompt.SubQuestion {
	raw, _ := toolInput["questions"].([]interface{})
	out := make([]sessionprompt.SubQuestion, 0, len(raw))
	for _, item := range raw {
		entry, _ := item.(map[string]interface{})
		sub := sessionprompt.SubQuestion{}
		sub.Text, _ = entry["question"].(string)
		sub.Header, _ = entry["header"].(string)
		sub.MultiSelect, _ = entry["multiSelect"].(bool)
		options, _ := entry["options"].([]interface{})
		for _, rawOption := range options {
			option, _ := rawOption.(map[string]interface{})
			label, _ := option["label"].(string)
			if label == "" {
				continue
			}
			description, _ := option["description"].(string)
			sub.Options = append(sub.Options, sessionprompt.Option{Index: len(sub.Options) + 1, Label: label, Description: description})
		}
		out = append(out, sub)
	}
	return out
}

// answerHookQuestion converts a generic answer into the hook response the
// parked PermissionRequest expects.
func (h *HookHandler) answerHookQuestion(ctx context.Context, sessionID string, q *sessionprompt.Question, answer sessionprompt.Answer) error {
	h.mu.Lock()
	pending, ok := h.pending[sessionID]
	var event map[string]interface{}
	if ok {
		event = pending.hookEvent
		ok = hookQuestionID(sessionID, pending.createdAt) == q.ID
	}
	h.mu.Unlock()
	if !ok {
		return session.ErrQuestionChanged
	}
	response := application.HookPermissionResponse{}
	switch q.Kind {
	case sessionprompt.KindToolPermission:
		switch answer.Option {
		case 1:
			response.Behavior = "allow"
		case 2:
			response.Behavior = "allowAlways"
			response.ToolName = q.ToolName
			if suggestions, ok := event["permission_suggestions"].([]interface{}); ok {
				response.PermissionSuggestions = suggestions
			}
		default:
			response.Behavior = "deny"
			response.Message = answer.Text
			if response.Message == "" {
				response.Message = "Denied by the automation operator."
			}
		}
	case sessionprompt.KindPlanApproval:
		switch answer.Option {
		case 1, 2, 3:
			response.Behavior = "allow"
			response.Message = strconv.Itoa(answer.Option)
		default:
			response.Behavior = "deny"
			response.Message = answer.Text
			if response.Message == "" {
				response.Message = "Keep planning."
			}
		}
	case sessionprompt.KindAskUserQuestion:
		answers, err := askUserAnswers(q, answer)
		if err != nil {
			return err
		}
		response.Behavior = "allow"
		response.Answers = answers
	default:
		return fmt.Errorf("%w: unsupported hook question kind %s", session.ErrInvalidAnswer, q.Kind)
	}
	return h.RespondPermission(ctx, sessionID, response)
}

func askUserAnswers(q *sessionprompt.Question, answer sessionprompt.Answer) (map[string]string, error) {
	if len(answer.Answers) > 0 {
		return answer.Answers, nil
	}
	if len(q.Questions) != 1 {
		return nil, fmt.Errorf("%w: this prompt has %d questions; send answers keyed by question text", session.ErrInvalidAnswer, len(q.Questions))
	}
	sub := q.Questions[0]
	var value string
	switch {
	case len(answer.Options) > 0:
		labels := make([]string, 0, len(answer.Options))
		for _, index := range answer.Options {
			if index >= 1 && index <= len(sub.Options) {
				labels = append(labels, sub.Options[index-1].Label)
			}
		}
		value = strings.Join(labels, ", ")
	case answer.Option >= 1 && answer.Option <= len(sub.Options):
		value = sub.Options[answer.Option-1].Label
	default:
		value = answer.Text
	}
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%w: send option, options or text", session.ErrInvalidAnswer)
	}
	return map[string]string{sub.Text: value}, nil
}

// waitForInitialPromptSlot waits until the new session can take its first
// prompt: its TUI finished painting and nothing interactive is open. Typing
// into an open dialog is what used to kill sessions (Enter confirmed the
// trust dialog's default "No, exit"), so while a question is pending the
// prompt is held back and the startup state says so.
func (a *API) waitForInitialPromptSlot(ctx context.Context, sessionID string, questions platformSessionQuestions) error {
	sess, err := a.db.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return fmt.Errorf("load session: %w", err)
	}
	claude := sess.Backend == string(session.BackendClaudeCode)
	questionDeadline := time.Now().Add(initialPromptQuestionWait)
	readySince := time.Now() // readiness timeout restarts after each answered question
	lastOutput := -1
	var quietSince time.Time
	ticker := time.NewTicker(questionPollInterval)
	defer ticker.Stop()
	for {
		if !a.sessionMgr.IsSessionRunning(sessionID) {
			return session.ErrSessionNotRunning
		}
		if q, _ := questions.PendingQuestion(ctx, sessionID); q != nil {
			a.sessionMgr.SetStartupState(sessionID, session.StartupAwaitingInput, q.Kind+" "+q.ID)
			if time.Now().After(questionDeadline) {
				return fmt.Errorf("question %s (%s) stayed unanswered for %s", q.ID, q.Kind, initialPromptQuestionWait)
			}
			readySince, lastOutput, quietSince = time.Now(), -1, time.Time{}
		} else {
			a.sessionMgr.SetStartupState(sessionID, session.StartupStarting, "")
			output, outputErr := a.sessionMgr.GetSessionOutput(sessionID)
			if outputErr != nil {
				return outputErr
			}
			painted := !claude || strings.Contains(string(output), claudeAlternateScreenSequence)
			if painted && len(output) != lastOutput {
				lastOutput, quietSince = len(output), time.Now()
			}
			minQuiet := taskSessionReadyQuietPeriod
			if !claude {
				minQuiet = taskSessionInitialDelay
			}
			if painted && !quietSince.IsZero() && time.Since(quietSince) >= minQuiet {
				return nil
			}
			if time.Since(readySince) >= taskSessionReadyTimeout {
				log.Printf("[Session] initial prompt readiness timed out for %s; submitting anyway", sessionID)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// startSessionQuestionMonitor publishes platform.session.awaiting_input when
// a session starts waiting on a question and platform.session.input_resolved
// when it stops, so automation clients can react without polling every
// session. Consumers read the question itself with sessions.get.
func (a *API) startSessionQuestionMonitor(questions platformSessionQuestions, effects application.SessionEffects) {
	if a.sessionMgr == nil || effects == nil {
		return
	}
	a.questionMonitorOnce.Do(func() {
		go func() {
			actor := application.Actor{Type: "system", ID: "session-question-monitor"}
			last := make(map[string]string)
			ticker := time.NewTicker(questionMonitorInterval)
			defer ticker.Stop()
			for range ticker.C {
				ctx := context.Background()
				running := make(map[string]bool)
				for _, id := range a.sessionMgr.ListRunningSessions() {
					running[id] = true
					current := ""
					if q, _ := questions.PendingQuestion(ctx, id); q != nil {
						current = q.ID
					}
					previous := last[id]
					if current == previous {
						continue
					}
					if current != "" {
						effects.PublishSessionChange(ctx, application.SessionChange{Action: "awaiting_input", ID: id, Actor: actor})
						last[id] = current
					} else {
						effects.PublishSessionChange(ctx, application.SessionChange{Action: "input_resolved", ID: id, Actor: actor})
						delete(last, id)
					}
				}
				for id := range last {
					if !running[id] {
						delete(last, id)
					}
				}
			}
		}()
	})
}
