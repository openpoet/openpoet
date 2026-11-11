package application

import (
	"context"
	"strings"
	"testing"

	"openpoet/internal/database"
)

const artifactQuestionsBlock = `<script type="application/json" id="openpoet-questions">
{"questions":[
 {"id":"D1","prompt":"What do we call it?","type":"single","options":[{"id":"a","label":"Routine"},{"id":"b","label":"Automation"}]},
 {"id":"D2","prompt":"Which triggers ship first?","type":"multi","comment":false,"options":[{"id":"form","label":"Form"},{"id":"chat","label":"Chat"},{"id":"cron","label":"Schedule"}]},
 {"id":"NOTES","prompt":"Anything else?","type":"text"}
]}
</script>`

func artifactWithQuestions(block string) string {
	return `<!doctype html><html><body><h1>Plan</h1>` + block + `<script>document.title='x'</script></body></html>`
}

func TestParseArtifactQuestions(t *testing.T) {
	questions, err := ParseArtifactQuestions(artifactWithQuestions(artifactQuestionsBlock))
	if err != nil || len(questions) != 3 {
		t.Fatalf("questions = %+v err=%v", questions, err)
	}
	if q := questions[0]; q.ID != "D1" || q.Type != "single" || !q.Comment || len(q.Options) != 2 || q.Options[1] != (ArtifactQuestionOption{ID: "b", Label: "Automation"}) {
		t.Fatalf("first question = %+v", q)
	}
	if q := questions[1]; q.Type != "multi" || q.Comment || len(q.Options) != 3 {
		t.Fatalf("second question = %+v", q)
	}
	if q := questions[2]; q.Type != "text" || q.Comment || len(q.Options) != 0 {
		t.Fatalf("text question = %+v", q)
	}

	// No block, or only a mention of it, declares nothing.
	for _, html := range []string{
		artifactTestHTML,
		`<html><body><p>The id openpoet-questions is reserved.</p></body></html>`,
		// Markup that looks like the block inside another script's text is that
		// script's text, as it is for the browser.
		`<html><body><script>var s = '<script type="application/json" id="openpoet-questions">[]';</script></body></html>`,
	} {
		if questions, err := ParseArtifactQuestions(html); err != nil || len(questions) != 0 {
			t.Fatalf("ParseArtifactQuestions(%q) = %+v err=%v", html, questions, err)
		}
	}

	// Forms the block may take.
	for _, block := range []string{
		`<script id='openpoet-questions' type='application/json'>[{"id":"q","prompt":"P","options":[{"id":"y","label":"Yes"},{"id":"n","label":"No"}]}]</script>`,
		`<SCRIPT TYPE=application/json ID=openpoet-questions >{"questions":[{"id":"q","prompt":"P"}]}</SCRIPT >`,
		`<script type="application/json" id="openpoet-questions">{"questions":[]}</script>`,
	} {
		if _, err := ParseArtifactQuestions(artifactWithQuestions(block)); err != nil {
			t.Fatalf("block %s: %v", block, err)
		}
	}
	// The type is inferred: options make a single choice, none makes a text question.
	inferred, err := ParseArtifactQuestions(artifactWithQuestions(`<script type="application/json" id="openpoet-questions">[{"id":"c","prompt":"P","options":[{"id":"y","label":"Yes"},{"id":"n","label":"No"}]},{"id":"t","prompt":"T"}]</script>`))
	if err != nil || inferred[0].Type != "single" || inferred[1].Type != "text" {
		t.Fatalf("inferred = %+v err=%v", inferred, err)
	}

	invalid := map[string]string{
		"not a data block":   `<script id="openpoet-questions">[]</script>`,
		"two blocks":         artifactQuestionsBlock + artifactQuestionsBlock,
		"not JSON":           `<script type="application/json" id="openpoet-questions">{questions:</script>`,
		"bad id":             `<script type="application/json" id="openpoet-questions">[{"id":"has space","prompt":"P"}]</script>`,
		"empty id":           `<script type="application/json" id="openpoet-questions">[{"prompt":"P"}]</script>`,
		"duplicate id":       `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P"},{"id":"q","prompt":"P2"}]</script>`,
		"no prompt":          `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"  "}]</script>`,
		"unknown type":       `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","type":"radio"}]</script>`,
		"single one option":  `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","type":"single","options":[{"id":"a","label":"A"}]}]</script>`,
		"multi no option":    `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","type":"multi"}]</script>`,
		"text with options":  `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","type":"text","options":[{"id":"a","label":"A"}]}]</script>`,
		"duplicate option":   `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","options":[{"id":"a","label":"A"},{"id":"a","label":"B"}]}]</script>`,
		"option no label":    `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","options":[{"id":"a","label":""},{"id":"b","label":"B"}]}]</script>`,
		"option bad id":      `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"P","options":[{"id":"<a>","label":"A"},{"id":"b","label":"B"}]}]</script>`,
		"prompt too long":    `<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"` + strings.Repeat("p", 501) + `"}]</script>`,
		"too many questions": `<script type="application/json" id="openpoet-questions">[` + strings.TrimSuffix(strings.Repeat(`{"id":"q","prompt":"P"},`, 201), ",") + `]</script>`,
	}
	for name, block := range invalid {
		_, err := ParseArtifactQuestions(artifactWithQuestions(block))
		if !ErrorIsKind(err, ErrorValidation) || !strings.Contains(err.Error(), "openpoet-questions") {
			t.Errorf("%s: err = %v, want a validation error naming the block", name, err)
		}
	}
	// A multi-line prompt or label is stored as one line.
	folded, err := ParseArtifactQuestions(artifactWithQuestions(`<script type="application/json" id="openpoet-questions">[{"id":"q","prompt":"Line one\nline two","options":[{"id":"a","label":" A\tlabel "},{"id":"b","label":"B"}]}]</script>`))
	if err != nil || folded[0].Prompt != "Line one line two" || folded[0].Options[0].Label != "A label" {
		t.Fatalf("folded = %+v err=%v", folded, err)
	}
}

func TestArtifactQuestionsAreStoredPerVersionAndAnswered(t *testing.T) {
	ctx := context.Background()
	service, db, effects := artifactTestService(t)
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "local-ui"}, Approved: true, ApprovedBy: "local-ui", Reason: "test"}

	// A malformed block refuses the publish: nothing is stored.
	if _, err := service.Create(ctx, CreateArtifactCommand{Title: "Bad", Content: artifactWithQuestions(`<script type="application/json" id="openpoet-questions">nope</script>`), Authorization: phase4Authorization()}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("publish with a malformed block err = %v", err)
	}
	if _, err := CheckArtifactHTML(artifactWithQuestions(`<script type="application/json" id="openpoet-questions">nope</script>`)); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("CheckArtifactHTML err = %v", err)
	}

	published, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: artifactWithQuestions(artifactQuestionsBlock), Authorization: phase4Authorization()})
	if err != nil || published.Questions != 3 {
		t.Fatalf("publish = %+v err=%v", published, err)
	}
	id := published.Artifact.ID
	questions, err := service.Questions(ctx, id, 0)
	if err != nil || questions.Version != 1 || len(questions.Questions) != 3 || questions.Questions[0].ID != "D1" || questions.Questions[2].ID != "NOTES" {
		t.Fatalf("questions = %+v err=%v", questions, err)
	}

	save := func(version int, answers ...ArtifactAnswerInput) ([]ArtifactAnnotationView, error) {
		return service.AnswerQuestions(ctx, AnswerArtifactQuestionsCommand{ArtifactID: id, Version: version, Answers: answers, Authorization: user})
	}

	// Several answers are saved together.
	saved, err := save(0,
		ArtifactAnswerInput{Question: "D1", Options: []string{"b"}, Comment: "  Fewer collisions  "},
		ArtifactAnswerInput{Question: "D2", Options: []string{"cron", "form"}},
		ArtifactAnswerInput{Question: "NOTES", Text: "Ship it locally first"},
	)
	if err != nil || len(saved) != 3 {
		t.Fatalf("save = %+v err=%v", saved, err)
	}
	d1 := saved[0]
	if d1.Kind != "answer" || d1.QuestionID != "D1" || d1.Status != "open" || d1.Version != 1 || d1.Quote != "What do we call it?" ||
		d1.Body != "Fewer collisions" || d1.Anchor.Type != "question" || d1.Anchor.Question != "D1" || d1.CreatedBy != "user:local-ui" ||
		d1.Answer == nil || d1.Answer.Type != "single" || len(d1.Answer.Options) != 1 || d1.Answer.Options[0] != (ArtifactQuestionOption{ID: "b", Label: "Automation"}) {
		t.Fatalf("D1 answer = %+v (answer %+v)", d1, d1.Answer)
	}
	// Options are stored in the declared order, whatever order they came in.
	if d2 := saved[1]; len(d2.Answer.Options) != 2 || d2.Answer.Options[0].ID != "form" || d2.Answer.Options[1].ID != "cron" || d2.Body != "" {
		t.Fatalf("D2 answer = %+v (answer %+v)", d2, d2.Answer)
	}
	if notes := saved[2]; notes.Answer.Type != "text" || len(notes.Answer.Options) != 0 || notes.Body != "Ship it locally first" {
		t.Fatalf("NOTES answer = %+v", notes)
	}
	if count, _ := db.CountOpenArtifactAnnotations(ctx, id); count != 3 {
		t.Fatalf("open annotations = %d, want the 3 answers", count)
	}
	if last := effects.changes[len(effects.changes)-1]; last.Domain != "artifacts" || last.Action != "answers_saved" {
		t.Fatalf("last effect = %+v", last)
	}

	// Answering again replaces the open answer: same annotation, new choice.
	replaced, err := save(1, ArtifactAnswerInput{Question: "D1", Options: []string{"a"}})
	if err != nil || replaced[0].ID != d1.ID || replaced[0].Answer.Options[0].ID != "a" || replaced[0].Body != "" || replaced[0].UpdatedAt == nil {
		t.Fatalf("replaced = %+v err=%v", replaced, err)
	}
	if count, _ := db.CountOpenArtifactAnnotations(ctx, id); count != 3 {
		t.Fatalf("open annotations after replacing = %d", count)
	}

	// What an answer may not be.
	refused := map[string][]ArtifactAnswerInput{
		"unknown_artifact_question": {{Question: "D9", Options: []string{"a"}}},
		"unknown_artifact_option":   {{Question: "D1", Options: []string{"z"}}},
		"invalid_artifact_answer":   {{Question: "D1", Options: []string{"a", "b"}}},
		"duplicate_artifact_answer": {{Question: "D1", Options: []string{"a"}}, {Question: "D1", Options: []string{"b"}}},
	}
	for code, answers := range refused {
		if _, err := save(0, answers...); !ErrorIsKind(err, ErrorValidation) || !strings.Contains(ErrorCode(err), code) {
			t.Errorf("%s: err = %v (code %q)", code, err, ErrorCode(err))
		}
	}
	for name, answer := range map[string]ArtifactAnswerInput{
		"choice without an option":  {Question: "D1"},
		"choice with free text":     {Question: "D1", Options: []string{"a"}, Text: "free text smuggled into a choice"},
		"text question with option": {Question: "NOTES", Options: []string{"a"}, Text: "x"},
		"empty text answer":         {Question: "NOTES", Text: "   "},
	} {
		if _, err := save(0, answer); !ErrorIsKind(err, ErrorValidation) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	// A refused request saves none of its answers.
	if _, err := save(0, ArtifactAnswerInput{Question: "D2", Options: []string{"chat"}}, ArtifactAnswerInput{Question: "D9", Options: []string{"a"}}); err == nil {
		t.Fatal("a request with an unknown question was accepted")
	}
	open, _ := service.ListAnnotations(ctx, id, "open")
	for _, annotation := range open {
		if annotation.QuestionID == "D2" && len(annotation.Answer.Options) != 2 {
			t.Fatalf("D2 was changed by a refused request: %+v", annotation.Answer)
		}
	}
	if _, err := save(0); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("empty request err = %v", err)
	}
	if _, err := save(7, ArtifactAnswerInput{Question: "D1", Options: []string{"a"}}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("unknown version err = %v", err)
	}

	// An agent session cannot answer for the user, nor withdraw an answer, and
	// cannot write an answer through the notes route either.
	session := ActionAuthorization{Actor: Actor{Type: "session", ID: "sess-1"}, Approved: true, ApprovedBy: "sess-1", Reason: "test"}
	if _, err := service.AnswerQuestions(ctx, AnswerArtifactQuestionsCommand{ArtifactID: id, Answers: []ArtifactAnswerInput{{Question: "D1", Options: []string{"b"}}}, Authorization: session}); !ErrorIsKind(err, ErrorValidation) || ErrorCode(err) != "annotation_author_not_allowed" {
		t.Fatalf("session answer err = %v", err)
	}
	if err := service.WithdrawAnswer(ctx, WithdrawArtifactAnswerCommand{ArtifactID: id, QuestionID: "D1", Authorization: session}); ErrorCode(err) != "annotation_author_not_allowed" {
		t.Fatalf("session withdraw err = %v", err)
	}
	if _, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{ArtifactID: id, Anchor: ArtifactAnchor{Type: "question", Question: "D1"}, Body: "forged decision", Authorization: user}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("a note with a question anchor was accepted: %v", err)
	}

	// The agent resolves an answer; answering that question again opens a new
	// one and keeps the resolved record.
	if _, err := service.ResolveAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: d1.ID, ResolutionNote: "Named it Routine in v2", Authorization: session}); err != nil {
		t.Fatal(err)
	}
	again, err := save(0, ArtifactAnswerInput{Question: "D1", Options: []string{"b"}, Comment: "Changed my mind"})
	if err != nil || again[0].ID == d1.ID || again[0].Status != "open" {
		t.Fatalf("answer after a resolution = %+v err=%v", again, err)
	}
	all, _ := service.ListAnnotations(ctx, id, "all")
	if len(all) != 4 {
		t.Fatalf("annotations = %d, want 4 (the resolved D1 and three open)", len(all))
	}
	// The resolved one cannot be reopened over the newer open answer.
	if _, err := service.ReopenAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: d1.ID, Authorization: user}); !ErrorIsKind(err, ErrorConflict) || ErrorCode(err) != "answer_superseded" {
		t.Fatalf("reopen over a newer answer err = %v", err)
	}

	// Withdrawing removes the open answer and nothing else.
	if err := service.WithdrawAnswer(ctx, WithdrawArtifactAnswerCommand{ArtifactID: id, QuestionID: "D1", Authorization: user}); err != nil {
		t.Fatal(err)
	}
	if err := service.WithdrawAnswer(ctx, WithdrawArtifactAnswerCommand{ArtifactID: id, QuestionID: "D1", Authorization: user}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("second withdraw err = %v", err)
	}
	if resolved, _ := service.ListAnnotations(ctx, id, "resolved"); len(resolved) != 1 || resolved[0].ID != d1.ID {
		t.Fatalf("resolved after withdraw = %+v", resolved)
	}
	// With the newer answer gone, the resolved one can be reopened.
	if _, err := service.ReopenAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: d1.ID, Authorization: user}); err != nil {
		t.Fatalf("reopen after withdraw: %v", err)
	}

	// A new version declares its own questions: D1 lost option "b", D2 is gone.
	v2 := artifactWithQuestions(`<script type="application/json" id="openpoet-questions">[{"id":"D1","prompt":"What do we call it?","options":[{"id":"a","label":"Routine"},{"id":"c","label":"Procedure"}]}]</script>`)
	updated, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: v2, BaseVersion: 1, Authorization: phase4Authorization()})
	if err != nil || updated.Version != 2 || updated.Questions != 1 {
		t.Fatalf("update = %+v err=%v", updated, err)
	}
	if now, _ := service.Questions(ctx, id, 0); now.Version != 2 || len(now.Questions) != 1 {
		t.Fatalf("questions of v2 = %+v", now)
	}
	if before, _ := service.Questions(ctx, id, 1); len(before.Questions) != 3 {
		t.Fatalf("questions of v1 changed: %+v", before)
	}
	if _, err := save(2, ArtifactAnswerInput{Question: "D1", Options: []string{"b"}}); ErrorCode(err) != "unknown_artifact_option" {
		t.Fatalf("option removed in v2 err = %v", err)
	}
	if _, err := save(2, ArtifactAnswerInput{Question: "D2", Options: []string{"form"}}); ErrorCode(err) != "unknown_artifact_question" {
		t.Fatalf("question removed in v2 err = %v", err)
	}
	// It is still answerable on the version that declared it.
	if _, err := save(1, ArtifactAnswerInput{Question: "D2", Options: []string{"form"}}); err != nil {
		t.Fatalf("answer on v1: %v", err)
	}
	// Identical HTML stores no version and no second set of questions.
	same, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: v2, BaseVersion: 2, Authorization: phase4Authorization()})
	if err != nil || same.Created {
		t.Fatalf("identical update = %+v err=%v", same, err)
	}
	var rows int
	if err := db.Get(&rows, `SELECT COUNT(*) FROM artifact_questions WHERE artifact_id = ?`, id); err != nil || rows != 4 {
		t.Fatalf("stored question rows = %d err=%v, want 3 (v1) + 1 (v2)", rows, err)
	}
	_ = database.ArtifactAnnotationKindAnswer
}

func TestNotesKeepWorkingNextToAnswers(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "local-ui"}, Approved: true, ApprovedBy: "local-ui", Reason: "test"}
	published, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: artifactWithQuestions(artifactQuestionsBlock), Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	id := published.Artifact.ID
	note, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{ArtifactID: id, Anchor: ArtifactAnchor{Type: "text", Selector: "h1", Exact: "Plan"}, Body: "Sharper title", Authorization: user})
	if err != nil || note.Kind != "note" || note.QuestionID != "" || note.Answer != nil || note.UpdatedAt != nil {
		t.Fatalf("note = %+v err=%v", note, err)
	}
	if _, err := service.AnswerQuestions(ctx, AnswerArtifactQuestionsCommand{ArtifactID: id, Answers: []ArtifactAnswerInput{{Question: "D1", Options: []string{"a"}}}, Authorization: user}); err != nil {
		t.Fatal(err)
	}
	// Two notes on the same place are still two notes.
	if _, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{ArtifactID: id, Anchor: ArtifactAnchor{Type: "text", Selector: "h1", Exact: "Plan"}, Body: "And shorter", Authorization: user}); err != nil {
		t.Fatal(err)
	}
	open, err := service.ListAnnotations(ctx, id, "open")
	if err != nil || len(open) != 3 {
		t.Fatalf("open = %+v err=%v", open, err)
	}
	kinds := ""
	for _, annotation := range open {
		kinds += annotation.Kind + " "
	}
	if kinds != "note answer note " {
		t.Fatalf("kinds in creation order = %q", kinds)
	}
}
