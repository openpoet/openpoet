package application

import (
	"context"
	"strings"
	"unicode/utf8"

	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
)

const (
	maxQuestionAnswerRunes = 4000
	maxQuestionAnswers     = 20
)

// SessionQuestionPort reads and answers whatever a session is blocked on: a
// parked PermissionRequest hook (tool permission, AskUserQuestion, plan
// approval) or a dialog painted on its terminal.
type SessionQuestionPort interface {
	PendingQuestion(ctx context.Context, sessionID string) (*sessionprompt.Question, error)
	AnswerQuestion(ctx context.Context, sessionID string, question *sessionprompt.Question, answer sessionprompt.Answer) error
}

// SessionQuestionStore is the session lookup the answer policy needs.
type SessionQuestionStore interface {
	GetSession(context.Context, string) (*database.Session, error)
}

// SessionQuestionService lets any actor see a session's pending question and
// lets an authorized actor answer it, with the risk of the chosen answer
// deciding what authorization is required.
type SessionQuestionService struct {
	store   SessionQuestionStore
	port    SessionQuestionPort
	effects SessionEffects
}

func NewSessionQuestionService(store SessionQuestionStore, port SessionQuestionPort, effects SessionEffects) *SessionQuestionService {
	return &SessionQuestionService{store: store, port: port, effects: effects}
}

// Pending returns the session's pending question, or nil.
func (s *SessionQuestionService) Pending(ctx context.Context, sessionID string) (*sessionprompt.Question, error) {
	if s == nil || s.port == nil {
		return nil, nil
	}
	return s.port.PendingQuestion(ctx, sessionID)
}

type AnswerSessionQuestionCommand struct {
	SessionID     string
	QuestionID    string
	Answer        sessionprompt.Answer
	Authorization ActionAuthorization
}

type AnswerSessionQuestionResult struct {
	SessionID  string                `json:"session_id"`
	QuestionID string                `json:"question_id"`
	Kind       string                `json:"kind"`
	Source     string                `json:"source"`
	Answered   bool                  `json:"answered"`
	Option     *sessionprompt.Option `json:"option,omitempty"`
}

// Answer validates an answer against the question currently pending and
// delivers it.
//
// Risk policy: an answer that grants the agent a permission (allowing a tool,
// accepting bypass-permissions mode) or that makes the agent exit requires an
// explicit approval with a reason, the same bar as hooks.respond_permission.
// Accepting bypass-permissions mode additionally requires the session to have
// been created with dangerously_skip_permissions, so an answer can never grant
// more than the create call was authorized for. Everything else (trusting the
// project's own workspace, answering the agent's questions, approving a plan)
// only requires an authenticated actor.
func (s *SessionQuestionService) Answer(ctx context.Context, command AnswerSessionQuestionCommand) (*AnswerSessionQuestionResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	sessionID, err := validateActionSessionID(command.SessionID)
	if err != nil {
		return nil, err
	}
	questionID := strings.TrimSpace(command.QuestionID)
	if questionID == "" || len(questionID) > 100 {
		return nil, validationError("question_id_required", "question_id is required (read it from sessions.get awaiting_input)")
	}
	if s.port == nil || s.store == nil {
		return nil, validationError("session_questions_unavailable", "Session question answering is unavailable")
	}
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return nil, notFoundError("session_not_found", "Session not found", err)
	}
	question, err := s.port.PendingQuestion(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if question == nil {
		return nil, conflictError("session_question_not_pending", "The session has no pending question")
	}
	if question.ID != questionID {
		return nil, conflictError("session_question_changed", "The pending question is now "+question.ID+"; re-read sessions.get and answer that one")
	}
	answer, chosen, err := normalizeQuestionAnswer(question, command.Answer)
	if err != nil {
		return nil, err
	}
	if chosen != nil && (chosen.GrantsPermission || chosen.EndsSession) {
		if err := requireExplicitActionApproval(command.Authorization); err != nil {
			return nil, validationError("session_question_reason_required",
				"This answer grants a permission or ends the session: send a reason with the command")
		}
		if question.Kind == sessionprompt.KindBypassPermissions && chosen.GrantsPermission && !session.SkipPermissions {
			return nil, validationError("unsafe_permissions_forbidden",
				"Bypass permissions can only be accepted for a session created with dangerously_skip_permissions")
		}
	}
	if err := s.port.AnswerQuestion(ctx, sessionID, question, answer); err != nil {
		return nil, err
	}
	if s.effects != nil {
		s.effects.PublishSessionChange(ctx, SessionChange{Action: "input_answered", ID: sessionID, Actor: command.Authorization.Actor})
	}
	return &AnswerSessionQuestionResult{
		SessionID: sessionID, QuestionID: question.ID, Kind: question.Kind, Source: question.Source,
		Answered: true, Option: chosen,
	}, nil
}

// normalizeQuestionAnswer checks the answer's shape against the question and
// returns the chosen option (nil for text-only and multi-question answers).
func normalizeQuestionAnswer(question *sessionprompt.Question, answer sessionprompt.Answer) (sessionprompt.Answer, *sessionprompt.Option, error) {
	answer.Text = strings.TrimSpace(answer.Text)
	if utf8.RuneCountInString(answer.Text) > maxQuestionAnswerRunes || strings.IndexByte(answer.Text, 0) >= 0 {
		return answer, nil, validationError("session_question_answer_invalid", "text must not exceed 4000 characters or contain NUL")
	}
	if len(answer.Answers) > maxQuestionAnswers || len(answer.Options) > maxQuestionAnswers {
		return answer, nil, validationError("session_question_answer_invalid", "at most 20 answers or options are accepted")
	}
	for key, value := range answer.Answers {
		if strings.TrimSpace(key) == "" || utf8.RuneCountInString(key) > maxQuestionAnswerRunes || utf8.RuneCountInString(value) > maxQuestionAnswerRunes {
			return answer, nil, validationError("session_question_answer_invalid", "answers keys and values must be 1-4000 characters")
		}
	}
	if len(answer.Answers) > 0 && question.Kind != sessionprompt.KindAskUserQuestion {
		return answer, nil, validationError("session_question_answer_invalid", "answers is only accepted for ask_user_question")
	}
	if len(answer.Options) > 0 {
		if question.Kind != sessionprompt.KindAskUserQuestion || len(question.Questions) != 1 || !question.Questions[0].MultiSelect {
			return answer, nil, validationError("session_question_answer_invalid", "options is only accepted for a single multi-select ask_user_question")
		}
		for _, index := range answer.Options {
			if _, ok := question.OptionByIndex(index); !ok {
				return answer, nil, validationError("session_question_answer_invalid", "options contains an index that is not offered")
			}
		}
		return answer, nil, nil
	}
	if answer.Text != "" && !question.AcceptsText {
		return answer, nil, validationError("session_question_answer_invalid", "this question does not accept text; answer with option")
	}
	if answer.Option == 0 {
		if answer.Text == "" && len(answer.Answers) == 0 {
			return answer, nil, validationError("session_question_answer_invalid", "send option (the 1-based index from awaiting_input.options), text, or answers")
		}
		if answer.Text != "" && question.Kind != sessionprompt.KindConfirm && question.Kind != sessionprompt.KindAskUserQuestion && question.Kind != sessionprompt.KindPlanApproval && question.Kind != sessionprompt.KindToolPermission {
			return answer, nil, validationError("session_question_answer_invalid", "this question needs an option")
		}
		return answer, nil, nil
	}
	option, ok := question.OptionByIndex(answer.Option)
	if !ok {
		return answer, nil, validationError("session_question_answer_invalid", "option is not one of the offered indexes")
	}
	return answer, &option, nil
}
