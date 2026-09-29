package automation

import (
	"context"
	"encoding/json"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
)

type questionSessionQueries struct{ session *database.Session }

func (q questionSessionQueries) ListSessions(context.Context) ([]database.Session, error) {
	return []database.Session{*q.session}, nil
}

func (q questionSessionQueries) GetSession(context.Context, string) (*database.Session, error) {
	return q.session, nil
}

type questionRuntime struct{ running bool }

func (r questionRuntime) IsSessionRunning(string) bool { return r.running }
func (r questionRuntime) GetSessionOutput(string) ([]byte, error) {
	if !r.running {
		return nil, errNoRuntimeForTest
	}
	return []byte("live"), nil
}
func (r questionRuntime) SessionStartupState(string) (string, string) {
	return "awaiting_input", "workspace_trust t_trust"
}

var errNoRuntimeForTest = platformFailure("session_not_found", "session not found", false)

type questionPort struct {
	question *sessionprompt.Question
	answers  []sessionprompt.Answer
}

func (p *questionPort) PendingQuestion(context.Context, string) (*sessionprompt.Question, error) {
	return p.question, nil
}

func (p *questionPort) AnswerQuestion(_ context.Context, _ string, _ *sessionprompt.Question, answer sessionprompt.Answer) error {
	p.answers = append(p.answers, answer)
	p.question = nil
	return nil
}

func runSessionCommand(t *testing.T, executor *sessionPlatformExecutor, handler, target, payload string, scope *ProjectScopeSet) (any, error) {
	t.Helper()
	command, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: application.CapabilityHandler(handler), Target: json.RawMessage(target), Payload: json.RawMessage(payload), ProjectScope: scope,
	})
	if err != nil {
		return nil, err
	}
	return command.Execute(context.Background(), application.ActionAuthorization{Actor: application.Actor{Type: "automation_client", ID: "helena"}})
}

func TestSessionsGetExposesAwaitingInputAndAnswerPromptResolvesIt(t *testing.T) {
	session := &database.Session{ID: "s1", ProjectID: 12, Status: "running"}
	port := &questionPort{question: &sessionprompt.Question{
		ID: "t_trust", Source: sessionprompt.SourceTerminal, Kind: sessionprompt.KindWorkspaceTrust, Text: "Is this a project you trust?",
		Options:       []sessionprompt.Option{{Index: 1, Label: "No, exit", EndsSession: true}, {Index: 2, Label: "Yes, I trust this folder"}},
		SelectedIndex: 1,
	}}
	queries := questionSessionQueries{session: session}
	executor := &sessionPlatformExecutor{
		questions: application.NewSessionQuestionService(queries, port, nil),
		queries:   queries, runtime: questionRuntime{running: true},
	}
	result, err := runSessionCommand(t, executor, "sessions.get", `{"id":"s1"}`, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	view := result.(SessionAutomationView)
	if view.InteractionState != "awaiting_input" || view.AwaitingInput == nil || view.AwaitingInput.ID != "t_trust" || view.StartupState != "awaiting_input" {
		t.Fatalf("view = %+v", view)
	}

	if _, err := runSessionCommand(t, executor, "sessions.answer_prompt", `{"id":"s1"}`, `{"question_id":"t_trust","option":2}`, &ProjectScopeSet{Allowed: map[int64]bool{47: true}}); err == nil {
		t.Fatal("answer outside the client's project_filter was accepted")
	}
	result, err = runSessionCommand(t, executor, "sessions.answer_prompt", `{"id":"s1"}`, `{"question_id":"t_trust","option":2}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answered := result.(*application.AnswerSessionQuestionResult); !answered.Answered || answered.Option == nil || answered.Option.Index != 2 {
		t.Fatalf("result = %+v", answered)
	}
	if len(port.answers) != 1 {
		t.Fatalf("answers = %+v", port.answers)
	}
	result, _ = runSessionCommand(t, executor, "sessions.get", `{"id":"s1"}`, `{}`, nil)
	if view := result.(SessionAutomationView); view.AwaitingInput != nil || view.InteractionState == "awaiting_input" {
		t.Fatalf("question still shown after answer: %+v", view)
	}
}

func TestSessionsHistoryAndGetExplainAnErroredSession(t *testing.T) {
	session := &database.Session{ID: "s1", ProjectID: 12, Status: "error",
		ErrorReason: "agent process exited: exit status 1 while an interactive workspace_trust question was on screen",
		LastOutput:  "Accessing workspace:\n❯ No, exit\n  Yes, I trust this folder"}
	queries := questionSessionQueries{session: session}
	executor := &sessionPlatformExecutor{queries: queries, runtime: questionRuntime{running: false}}
	result, err := runSessionCommand(t, executor, "sessions.history", `{"id":"s1"}`, `{}`, nil)
	if err != nil {
		t.Fatalf("history of an ended session failed: %v", err)
	}
	history := result.(sessionHistoryAutomationView)
	if history.Source != "persisted" || history.ErrorReason == "" || history.Content == "" || history.Status != "error" {
		t.Fatalf("history = %+v", history)
	}
	result, _ = runSessionCommand(t, executor, "sessions.get", `{"id":"s1"}`, `{}`, nil)
	view := result.(SessionAutomationView)
	if view.ErrorReason == "" || view.LastOutput == "" || view.InteractionState != "ended" {
		t.Fatalf("view = %+v", view)
	}
}

func TestAnswerPromptIsDescribedInDiscovery(t *testing.T) {
	for _, definition := range sessionPlatformDefinitions() {
		if definition.Name != "sessions.answer_prompt" {
			continue
		}
		if definition.Payload == nil || definition.Payload.Notes == "" || len(definition.Payload.Example) == 0 {
			t.Fatalf("answer_prompt is not self-describing: %+v", definition.Payload)
		}
		for _, field := range definition.Payload.Fields {
			if field.Name == "question_id" && field.Required {
				return
			}
		}
		t.Fatalf("question_id is not a required field: %+v", definition.Payload.Fields)
	}
	t.Fatal("sessions.answer_prompt is not registered")
}
