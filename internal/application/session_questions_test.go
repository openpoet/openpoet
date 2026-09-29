package application

import (
	"context"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
)

type questionStoreFake struct{ session *database.Session }

func (s questionStoreFake) GetSession(context.Context, string) (*database.Session, error) {
	return s.session, nil
}

type questionPortFake struct {
	question *sessionprompt.Question
	answered []sessionprompt.Answer
}

func (p *questionPortFake) PendingQuestion(context.Context, string) (*sessionprompt.Question, error) {
	return p.question, nil
}

func (p *questionPortFake) AnswerQuestion(_ context.Context, _ string, _ *sessionprompt.Question, answer sessionprompt.Answer) error {
	p.answered = append(p.answered, answer)
	return nil
}

type questionEffectsFake struct{ actions []string }

func (e *questionEffectsFake) PublishSessionChange(_ context.Context, change SessionChange) {
	e.actions = append(e.actions, change.Action)
}

var helena = Actor{Type: "automation_client", ID: "helena"}

func trustQuestion() *sessionprompt.Question {
	return &sessionprompt.Question{ID: "t_trust", Source: sessionprompt.SourceTerminal, Kind: sessionprompt.KindWorkspaceTrust,
		Options: []sessionprompt.Option{{Index: 1, Label: "No, exit", EndsSession: true}, {Index: 2, Label: "Yes, I trust this folder"}}}
}

func permissionQuestion(kind string) *sessionprompt.Question {
	return &sessionprompt.Question{ID: "h_perm", Source: sessionprompt.SourceHook, Kind: kind, AcceptsText: true,
		Options: []sessionprompt.Option{{Index: 1, Label: "Allow", GrantsPermission: true}, {Index: 2, Label: "Deny"}}}
}

func newQuestionService(q *sessionprompt.Question, session *database.Session) (*SessionQuestionService, *questionPortFake, *questionEffectsFake) {
	port := &questionPortFake{question: q}
	effects := &questionEffectsFake{}
	return NewSessionQuestionService(questionStoreFake{session: session}, port, effects), port, effects
}

func TestAnswerTrustNeedsOnlyAnActor(t *testing.T) {
	service, port, effects := newQuestionService(trustQuestion(), &database.Session{ID: "s1"})
	result, err := service.Answer(context.Background(), AnswerSessionQuestionCommand{
		SessionID: "s1", QuestionID: "t_trust", Answer: sessionprompt.Answer{Option: 2},
		Authorization: ActionAuthorization{Actor: helena},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Answered || len(port.answered) != 1 || port.answered[0].Option != 2 {
		t.Fatalf("result=%+v answered=%+v", result, port.answered)
	}
	if len(effects.actions) != 1 || effects.actions[0] != "input_answered" {
		t.Fatalf("effects = %v", effects.actions)
	}
}

func TestAnswerThatGrantsPermissionOrEndsSessionRequiresReason(t *testing.T) {
	cases := map[string]struct {
		question *sessionprompt.Question
		option   int
	}{
		"allow tool":   {permissionQuestion(sessionprompt.KindToolPermission), 1},
		"exit session": {trustQuestion(), 1},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			service, port, _ := newQuestionService(test.question, &database.Session{ID: "s1"})
			command := AnswerSessionQuestionCommand{
				SessionID: "s1", QuestionID: test.question.ID, Answer: sessionprompt.Answer{Option: test.option},
				Authorization: ActionAuthorization{Actor: helena, Approved: true, ApprovedBy: "helena"},
			}
			if _, err := service.Answer(context.Background(), command); ErrorCode(err) != "session_question_reason_required" {
				t.Fatalf("without reason err = %v", err)
			}
			command.Authorization.Reason = "task #848 needs the build to run"
			if _, err := service.Answer(context.Background(), command); err != nil {
				t.Fatalf("with reason err = %v", err)
			}
			if len(port.answered) != 1 {
				t.Fatalf("answered = %d", len(port.answered))
			}
		})
	}
}

func TestDenyingAPermissionNeedsNoReason(t *testing.T) {
	service, port, _ := newQuestionService(permissionQuestion(sessionprompt.KindToolPermission), &database.Session{ID: "s1"})
	_, err := service.Answer(context.Background(), AnswerSessionQuestionCommand{
		SessionID: "s1", QuestionID: "h_perm", Answer: sessionprompt.Answer{Text: "not in this task"},
		Authorization: ActionAuthorization{Actor: helena},
	})
	if err != nil || len(port.answered) != 1 {
		t.Fatalf("err=%v answered=%d", err, len(port.answered))
	}
}

func TestAcceptingBypassNeedsASkipPermissionsSession(t *testing.T) {
	q := &sessionprompt.Question{ID: "t_bypass", Source: sessionprompt.SourceTerminal, Kind: sessionprompt.KindBypassPermissions,
		Options: []sessionprompt.Option{{Index: 1, Label: "No, exit", EndsSession: true}, {Index: 2, Label: "Yes, I accept", GrantsPermission: true}}}
	authorization := ActionAuthorization{Actor: helena, Approved: true, ApprovedBy: "helena", Reason: "operator approved"}
	service, _, _ := newQuestionService(q, &database.Session{ID: "s1", SkipPermissions: false})
	_, err := service.Answer(context.Background(), AnswerSessionQuestionCommand{SessionID: "s1", QuestionID: "t_bypass", Answer: sessionprompt.Answer{Option: 2}, Authorization: authorization})
	if ErrorCode(err) != "unsafe_permissions_forbidden" {
		t.Fatalf("err = %v", err)
	}
	service, port, _ := newQuestionService(q, &database.Session{ID: "s1", SkipPermissions: true})
	if _, err := service.Answer(context.Background(), AnswerSessionQuestionCommand{SessionID: "s1", QuestionID: "t_bypass", Answer: sessionprompt.Answer{Option: 2}, Authorization: authorization}); err != nil || len(port.answered) != 1 {
		t.Fatalf("authorized bypass: err=%v answered=%d", err, len(port.answered))
	}
}

func TestAnswerRejectsStaleQuestionAndMalformedAnswers(t *testing.T) {
	service, port, _ := newQuestionService(trustQuestion(), &database.Session{ID: "s1"})
	authorization := ActionAuthorization{Actor: helena}
	cases := map[string]struct {
		id     string
		answer sessionprompt.Answer
		code   string
	}{
		"stale id":        {"t_old", sessionprompt.Answer{Option: 2}, "session_question_changed"},
		"unknown option":  {"t_trust", sessionprompt.Answer{Option: 9}, "session_question_answer_invalid"},
		"text not taken":  {"t_trust", sessionprompt.Answer{Option: 2, Text: "x"}, "session_question_answer_invalid"},
		"answers not ask": {"t_trust", sessionprompt.Answer{Answers: map[string]string{"q": "a"}}, "session_question_answer_invalid"},
		"empty":           {"t_trust", sessionprompt.Answer{}, "session_question_answer_invalid"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := service.Answer(context.Background(), AnswerSessionQuestionCommand{SessionID: "s1", QuestionID: test.id, Answer: test.answer, Authorization: authorization})
			if ErrorCode(err) != test.code {
				t.Fatalf("err = %v (code %q), want %s", err, ErrorCode(err), test.code)
			}
		})
	}
	if len(port.answered) != 0 {
		t.Fatalf("invalid answers reached the port: %+v", port.answered)
	}
	service, _, _ = newQuestionService(nil, &database.Session{ID: "s1"})
	if _, err := service.Answer(context.Background(), AnswerSessionQuestionCommand{SessionID: "s1", QuestionID: "t_trust", Answer: sessionprompt.Answer{Option: 1}, Authorization: authorization}); ErrorCode(err) != "session_question_not_pending" {
		t.Fatalf("no question err = %v", err)
	}
}
