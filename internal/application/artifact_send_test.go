package application

import (
	"context"
	"strings"
	"testing"
)

// recordingAgentInput stands in for the session service: it records every
// line typed into a session and can refuse, as a session that is not running
// does.
type recordingAgentInput struct {
	commands []SendSessionInputCommand
	err      error
}

func (r *recordingAgentInput) SendInput(_ context.Context, command SendSessionInputCommand) error {
	if r.err != nil {
		return r.err
	}
	r.commands = append(r.commands, command)
	return nil
}

// "Send to agent" types one line into the session that published the artifact
// and records the send on every annotation that was open: the viewer shows
// what is stored, so a send that failed must leave nothing marked.
func TestSendAnnotationsToAgentRecordsWhatReachedTheSession(t *testing.T) {
	ctx := context.Background()
	service, _, effects := artifactTestService(t)
	notifier := &recordingArtifactNotifier{}
	service.SetNotifier(notifier)
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "local-ui"}}
	agent := ActionAuthorization{Actor: Actor{Type: "session", ID: "sess-pub"}}

	published, err := service.Create(ctx, CreateArtifactCommand{
		Title: "Plan\n\"v1\"", Content: artifactWithQuestions(artifactQuestionsBlock), SessionID: "sess-pub", Authorization: agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := published.Artifact.ID
	send := func(authorization ActionAuthorization) (ArtifactSendResult, error) {
		return service.SendAnnotationsToAgent(ctx, SendArtifactAnnotationsCommand{ArtifactID: id, Authorization: authorization})
	}
	annotations := func(status string) map[int64]ArtifactAnnotationView {
		t.Helper()
		views, err := service.ListAnnotations(ctx, id, status)
		if err != nil {
			t.Fatal(err)
		}
		byID := make(map[int64]ArtifactAnnotationView, len(views))
		for _, view := range views {
			byID[view.ID] = view
		}
		return byID
	}
	note := func(body string) ArtifactAnnotationView {
		t.Helper()
		view, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{
			ArtifactID: id, Anchor: ArtifactAnchor{Type: "text", Selector: "h1", Exact: "Plan"}, Body: body, Authorization: user,
		})
		if err != nil {
			t.Fatal(err)
		}
		return view
	}

	// Without a way to reach sessions nothing is sent and nothing is marked.
	first := note("first-note-marker")
	if _, err := send(user); ErrorCode(err) != "session_input_unavailable" {
		t.Fatalf("send without an input port err = %v", err)
	}
	input := &recordingAgentInput{}
	service.SetAgentInput(input)

	second := note("second-note-marker")
	answers, err := service.AnswerQuestions(ctx, AnswerArtifactQuestionsCommand{
		ArtifactID: id, Answers: []ArtifactAnswerInput{{Question: "D1", Options: []string{"a"}, Comment: "answer-comment-marker"}}, Authorization: user,
	})
	if err != nil || len(answers) != 1 {
		t.Fatalf("answers = %+v err=%v", answers, err)
	}
	answer := answers[0]

	// The button is the user's: a session cannot press it.
	if _, err := send(agent); ErrorCode(err) != "annotation_author_not_allowed" || len(input.commands) != 0 {
		t.Fatalf("send by a session err = %v commands=%d", err, len(input.commands))
	}
	if _, err := send(ActionAuthorization{}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("send without an actor err = %v", err)
	}

	// A session that does not take the line: the error reaches the caller and
	// no annotation is marked as sent.
	changesBefore, notifiedBefore := len(effects.changes), len(notifier.annotationChanges)
	input.err = conflictError("session_not_running", "Session is not running")
	if _, err := send(user); ErrorCode(err) != "session_not_running" {
		t.Fatalf("send to a stopped session err = %v", err)
	}
	for _, view := range annotations("open") {
		if view.SentAt != nil || view.SentSessionID != "" {
			t.Fatalf("a failed send marked annotation %d as sent: %+v", view.ID, view)
		}
	}
	if len(effects.changes) != changesBefore || len(notifier.annotationChanges) != notifiedBefore {
		t.Fatalf("a failed send announced a change")
	}

	// The send that works: one line, to the publishing session, as the user,
	// with counts and none of what the user wrote.
	input.err = nil
	result, err := send(user)
	if err != nil {
		t.Fatal(err)
	}
	if result.ArtifactID != id || result.SessionID != "sess-pub" || result.Sent != 3 || result.SentAt.IsZero() || len(result.Annotations) != 3 {
		t.Fatalf("send result = %+v", result)
	}
	if len(input.commands) != 1 {
		t.Fatalf("lines typed = %d, want 1", len(input.commands))
	}
	command := input.commands[0]
	if command.SessionID != "sess-pub" || command.Authorization.Actor != user.Actor || command.RejectIfBusy || command.AwaitAck {
		t.Fatalf("command = %+v", command)
	}
	wantPrefix := `[OpenPoet] The user pressed "Send to agent" on the HTML artifact "Plan "v1"" (artifact_id ` + id + `, current version 1): 1 answer to your questions and 2 notes are open. Read them with openpoet_list_artifact_annotations.`
	if !strings.HasPrefix(command.Text, wantPrefix) || !strings.HasSuffix(command.Text, "or leave it open and tell the user why.") || strings.ContainsAny(command.Text, "\n\r") {
		t.Fatalf("line typed = %q", command.Text)
	}
	for _, marker := range []string{"first-note-marker", "second-note-marker", "answer-comment-marker"} {
		if strings.Contains(command.Text, marker) {
			t.Fatalf("the line carries what the user wrote (%s): %q", marker, command.Text)
		}
	}
	for _, view := range annotations("open") {
		if view.SentAt == nil || view.SentAt.IsZero() || view.SentSessionID != "sess-pub" {
			t.Fatalf("annotation %d is not marked as sent: %+v", view.ID, view)
		}
	}
	if last := effects.changes[len(effects.changes)-1]; last.Domain != "artifacts" || last.Action != "annotations_sent" || last.Meta["artifact_id"] != id {
		t.Fatalf("change = %+v", last)
	}
	if len(notifier.annotationChanges) != notifiedBefore+1 {
		t.Fatalf("open viewers were not told about the send")
	}

	// What is written after the send has not been sent.
	third := note("third-note-marker")
	if view := annotations("open")[third.ID]; view.SentAt != nil {
		t.Fatalf("a note written after the send is marked as sent: %+v", view)
	}
	// An answer replaced after the send keeps its id and loses the mark: the
	// agent was told about the previous answer.
	replaced, err := service.AnswerQuestions(ctx, AnswerArtifactQuestionsCommand{
		ArtifactID: id, Answers: []ArtifactAnswerInput{{Question: "D1", Options: []string{"b"}}}, Authorization: user,
	})
	if err != nil || len(replaced) != 1 || replaced[0].ID != answer.ID || replaced[0].SentAt != nil || replaced[0].SentSessionID != "" {
		t.Fatalf("replaced answer = %+v err=%v", replaced, err)
	}
	// A resolved annotation keeps the record of the send; reopening it clears
	// the record, because the agent has not been told it is open again.
	if _, err := service.ResolveAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: first.ID, ResolutionNote: "done", Authorization: agent}); err != nil {
		t.Fatal(err)
	}
	if view := annotations("resolved")[first.ID]; view.SentAt == nil || view.SentSessionID != "sess-pub" {
		t.Fatalf("resolved annotation lost its send record: %+v", view)
	}
	reopened, err := service.ReopenAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: first.ID, Authorization: user})
	if err != nil || reopened.SentAt != nil || reopened.SentSessionID != "" {
		t.Fatalf("reopened annotation = %+v err=%v", reopened, err)
	}
	open := annotations("open")
	if len(open) != 4 || open[second.ID].SentAt == nil || open[first.ID].SentAt != nil || open[third.ID].SentAt != nil || open[answer.ID].SentAt != nil {
		t.Fatalf("open annotations after changes = %+v", open)
	}

	// Sending again announces everything that is open and marks it all; a
	// resolved annotation is not touched.
	if _, err := service.ResolveAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: third.ID, Authorization: agent}); err != nil {
		t.Fatal(err)
	}
	again, err := send(user)
	if err != nil || again.Sent != 3 || len(input.commands) != 2 {
		t.Fatalf("second send = %+v err=%v lines=%d", again, err, len(input.commands))
	}
	if !strings.Contains(input.commands[1].Text, "1 answer to your questions and 2 notes are open") {
		t.Fatalf("second line = %q", input.commands[1].Text)
	}
	for _, view := range annotations("open") {
		if view.SentAt == nil {
			t.Fatalf("annotation %d is not marked after the second send", view.ID)
		}
	}
	if view := annotations("resolved")[third.ID]; view.SentAt != nil {
		t.Fatalf("a resolved annotation that was never sent got marked: %+v", view)
	}
}

func TestSendAnnotationsToAgentNeedsSomethingToSendAndSomeoneToSendTo(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	input := &recordingAgentInput{}
	service.SetAgentInput(input)
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "local-ui"}}

	withSession, err := service.Create(ctx, CreateArtifactCommand{Title: "Doc", Content: artifactTestHTML, SessionID: "sess-pub", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendAnnotationsToAgent(ctx, SendArtifactAnnotationsCommand{ArtifactID: withSession.Artifact.ID, Authorization: user}); ErrorCode(err) != "no_open_annotations" {
		t.Fatalf("send with nothing open err = %v", err)
	}

	noSession, err := service.Create(ctx, CreateArtifactCommand{Title: "Loose", Content: artifactTestHTML + "<!-- other -->", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{
		ArtifactID: noSession.Artifact.ID, Anchor: ArtifactAnchor{Type: "text", Exact: "Plan"}, Body: "note", Authorization: user,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendAnnotationsToAgent(ctx, SendArtifactAnnotationsCommand{ArtifactID: noSession.Artifact.ID, Authorization: user}); ErrorCode(err) != "artifact_has_no_session" {
		t.Fatalf("send on an artifact without a session err = %v", err)
	}
	if _, err := service.SendAnnotationsToAgent(ctx, SendArtifactAnnotationsCommand{ArtifactID: "missing0", Authorization: user}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("send on a missing artifact err = %v", err)
	}
	if len(input.commands) != 0 {
		t.Fatalf("lines typed = %d, want none", len(input.commands))
	}
}

// A single open annotation reads "is open", and a single note is not "notes".
func TestArtifactSendMessageCountsAnswersAndNotes(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	input := &recordingAgentInput{}
	service.SetAgentInput(input)
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "local-ui"}}
	published, err := service.Create(ctx, CreateArtifactCommand{Title: "Doc", Content: artifactTestHTML, SessionID: "sess-pub", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{
		ArtifactID: published.Artifact.ID, Anchor: ArtifactAnchor{Type: "text", Exact: "Plan"}, Body: "note", Authorization: user,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendAnnotationsToAgent(ctx, SendArtifactAnnotationsCommand{ArtifactID: published.Artifact.ID, Authorization: user}); err != nil {
		t.Fatal(err)
	}
	if len(input.commands) != 1 || !strings.Contains(input.commands[0].Text, "current version 1): 1 note is open. Read them") {
		t.Fatalf("line = %+v", input.commands)
	}
}
