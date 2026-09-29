package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"openpoet/internal/session"
	"openpoet/internal/sessionprompt"
)

func parkHook(h *HookHandler, sessionID, msgType string, event map[string]interface{}) chan PermissionResponse {
	responses := make(chan PermissionResponse, 1)
	h.mu.Lock()
	h.pending[sessionID] = &pendingPermission{responseCh: responses, cancel: func() {}, hookEvent: event, msgType: msgType, createdAt: time.Now()}
	h.mu.Unlock()
	return responses
}

func TestHookQuestionToolPermissionRoundTrip(t *testing.T) {
	h := NewHookHandler(nil, nil, nil)
	suggestions := []interface{}{map[string]interface{}{"type": "addRules"}}
	responses := parkHook(h, "s1", "permission", map[string]interface{}{
		"tool_name": "Bash", "tool_input": map[string]interface{}{"command": "make build", "description": "Build"},
		"permission_suggestions": suggestions,
	})
	questions := platformSessionQuestions{hook: h}
	q, err := questions.PendingQuestion(context.Background(), "s1")
	if err != nil || q == nil {
		t.Fatalf("q=%v err=%v", q, err)
	}
	if q.Kind != sessionprompt.KindToolPermission || q.Source != sessionprompt.SourceHook || q.ToolName != "Bash" || len(q.Options) != 3 {
		t.Fatalf("question = %+v", q)
	}
	if !q.Options[0].GrantsPermission || !q.Options[1].GrantsPermission || q.Options[2].GrantsPermission {
		t.Fatalf("grants flags = %+v", q.Options)
	}
	if err := questions.AnswerQuestion(context.Background(), "s1", q, sessionprompt.Answer{Option: 2}); err != nil {
		t.Fatal(err)
	}
	response := <-responses
	if response.Behavior != "allowAlways" || response.ToolName != "Bash" || len(response.PermissionSuggestions) != 1 {
		t.Fatalf("response = %+v", response)
	}
}

func TestHookQuestionDenyCarriesTextAndStaleIDIsRejected(t *testing.T) {
	h := NewHookHandler(nil, nil, nil)
	responses := parkHook(h, "s1", "permission", map[string]interface{}{"tool_name": "Write"})
	questions := platformSessionQuestions{hook: h}
	q, _ := questions.PendingQuestion(context.Background(), "s1")
	stale := *q
	stale.ID = "h_old"
	if err := questions.AnswerQuestion(context.Background(), "s1", &stale, sessionprompt.Answer{Option: 1}); err == nil {
		t.Fatal("stale question id was answered")
	}
	if err := questions.AnswerQuestion(context.Background(), "s1", q, sessionprompt.Answer{Text: "outside the task"}); err != nil {
		t.Fatal(err)
	}
	if response := <-responses; response.Behavior != "deny" || response.Message != "outside the task" {
		t.Fatalf("response = %+v", response)
	}
}

func TestHookQuestionAskUserAndPlanApproval(t *testing.T) {
	h := NewHookHandler(nil, nil, nil)
	responses := parkHook(h, "s1", "askUser", map[string]interface{}{
		"tool_name": "AskUserQuestion",
		"tool_input": map[string]interface{}{"questions": []interface{}{map[string]interface{}{
			"question": "Which DB?", "header": "DB",
			"options": []interface{}{map[string]interface{}{"label": "Postgres"}, map[string]interface{}{"label": "SQLite", "description": "embedded"}},
		}}},
	})
	questions := platformSessionQuestions{hook: h}
	q, _ := questions.PendingQuestion(context.Background(), "s1")
	if q.Kind != sessionprompt.KindAskUserQuestion || q.Text != "Which DB?" || len(q.Options) != 2 || q.Options[1].Description != "embedded" {
		t.Fatalf("question = %+v", q)
	}
	if err := questions.AnswerQuestion(context.Background(), "s1", q, sessionprompt.Answer{Option: 2}); err != nil {
		t.Fatal(err)
	}
	if response := <-responses; response.Behavior != "allow" || response.Answers["Which DB?"] != "SQLite" {
		t.Fatalf("response = %+v", response)
	}

	responses = parkHook(h, "s2", "exitPlan", map[string]interface{}{"tool_name": "ExitPlanMode", "tool_input": map[string]interface{}{"plan": "1. do it"}})
	q, _ = questions.PendingQuestion(context.Background(), "s2")
	if q.Kind != sessionprompt.KindPlanApproval || q.Text != "1. do it" || len(q.Options) != 4 {
		t.Fatalf("plan question = %+v", q)
	}
	if err := questions.AnswerQuestion(context.Background(), "s2", q, sessionprompt.Answer{Option: 2}); err != nil {
		t.Fatal(err)
	}
	if response := <-responses; response.Behavior != "allow" || response.Message != "2" {
		t.Fatalf("plan response = %+v", response)
	}
}

func TestMapQuestionErrorGivesStableCodes(t *testing.T) {
	if err := mapQuestionError(session.ErrNoPendingQuestion); err == nil || err.Error() == session.ErrNoPendingQuestion.Error() {
		t.Fatalf("not mapped: %v", err)
	}
	if err := mapQuestionError(errors.New("boom")); err == nil || err.Error() != "boom" {
		t.Fatalf("unexpected: %v", err)
	}
}
