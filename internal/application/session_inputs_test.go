package application

import (
	"context"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/sessioninput"
	"openpoet/internal/sessionprompt"
)

type inputRecorderFake struct{ tracker *sessioninput.Tracker }

func (r inputRecorderFake) ExpectSessionInput(id string, in sessioninput.Expected) {
	r.tracker.Expect(id, in)
}

func (r inputRecorderFake) LastSessionInput(id string) (sessioninput.LastInput, bool) {
	return r.tracker.Last(id)
}

// A text answer is announced with its sender and command, so if the agent
// takes it as a prompt the prompt is credited to the automation; an option
// pick announces nothing.
func TestAnswerWithTextIsAttributedToItsSender(t *testing.T) {
	recorder := inputRecorderFake{tracker: sessioninput.NewTracker()}
	service, _, _ := newQuestionService(permissionQuestion(sessionprompt.KindToolPermission), &database.Session{ID: "s1"})
	service.SetInputRecorder(recorder)
	ctx := WithEventMetadata(context.Background(), EventMetadata{Actor: helena, CommandID: "orq-answer-1"})

	if _, err := service.Answer(ctx, AnswerSessionQuestionCommand{
		SessionID: "s1", QuestionID: "h_perm", Answer: sessionprompt.Answer{Option: 2},
		Authorization: ActionAuthorization{Actor: helena},
	}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.tracker.Observe("s1", "Deny", true); got.Source != sessioninput.SourceHuman {
		t.Fatalf("an option pick was announced: %+v", got)
	}

	text := "não, use a migration aditiva"
	if _, err := service.Answer(ctx, AnswerSessionQuestionCommand{
		SessionID: "s1", QuestionID: "h_perm", Answer: sessionprompt.Answer{Text: text},
		Authorization: ActionAuthorization{Actor: helena},
	}); err != nil {
		t.Fatal(err)
	}
	got := recorder.tracker.Observe("s1", text, true)
	if got.Source != sessioninput.SourceAutomation || got.Actor != "automation_client:helena" ||
		got.CommandID != "orq-answer-1" || got.Via != sessioninput.ViaAnswerPrompt {
		t.Fatalf("text answer prompt = %+v", got)
	}
}
