package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"openpoet/internal/database"
)

// Questions an artifact asks its reader.
//
// An artifact declares its questions in one JSON data block:
//
//	<script type="application/json" id="openpoet-questions">
//	{"questions":[
//	  {"id":"D1","prompt":"What do we call it?","type":"single",
//	   "options":[{"id":"a","label":"Routine"},{"id":"b","label":"Automation"}]},
//	  {"id":"NOTES","prompt":"Anything else?","type":"text"}
//	]}
//	</script>
//
// The block is data, not code: the server reads it when a version is published
// and stores the questions with that version. That stored list is the only
// thing an answer is checked against. The artifact's own script runs in the
// sandboxed frame and can send the page anything; it can never make the server
// accept an answer to a question, or an option, that was not declared, and the
// words of an answer (a comment, a free-text answer) are typed in the OpenPoet
// page, never in the frame.

// ArtifactQuestionsElementID is the id of the JSON block that declares the
// questions of an artifact.
const ArtifactQuestionsElementID = "openpoet-questions"

// Question types.
const (
	ArtifactQuestionSingle = "single" // exactly one option
	ArtifactQuestionMulti  = "multi"  // one or more options
	ArtifactQuestionText   = "text"   // free text, typed in the OpenPoet page
)

const (
	maxArtifactQuestions             = 200
	maxArtifactQuestionOptions       = 30
	maxArtifactQuestionPromptRunes   = 500
	maxArtifactQuestionLabelRunes    = 300
	maxArtifactAnswerQuestionsPerPut = maxArtifactQuestions
)

var (
	artifactScriptTagPattern      = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	artifactQuestionsIDPattern    = regexp.MustCompile(`(?i)(?:^|\s)id\s*=\s*(?:"openpoet-questions"|'openpoet-questions'|openpoet-questions(?:\s|$))`)
	artifactJSONScriptTypePattern = regexp.MustCompile(`(?i)(?:^|\s)type\s*=\s*(?:"application/json"|'application/json'|application/json(?:\s|$))`)
	artifactQuestionIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
)

// ArtifactQuestionOption is one choice of a question.
type ArtifactQuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ArtifactQuestionView is one question a version declares.
type ArtifactQuestionView struct {
	ID      string                   `json:"id"`
	Prompt  string                   `json:"prompt"`
	Type    string                   `json:"type"`
	Options []ArtifactQuestionOption `json:"options,omitempty"`
	// Comment says whether a choice may come with a comment.
	Comment bool `json:"comment"`
}

// ArtifactQuestionsView is the list of questions of one version.
type ArtifactQuestionsView struct {
	ArtifactID string                 `json:"artifact_id"`
	Version    int                    `json:"version"`
	Questions  []ArtifactQuestionView `json:"questions"`
}

// ArtifactAnswerView is what the reader answered: the options chosen (with the
// labels they had when the answer was given) for a choice, or nothing here for
// a free-text question, whose text is the annotation body.
type ArtifactAnswerView struct {
	Type    string                   `json:"type"`
	Options []ArtifactQuestionOption `json:"options,omitempty"`
}

// ArtifactAnswerInput is one answer being saved.
type ArtifactAnswerInput struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	// Text is the answer to a free-text question.
	Text string `json:"text"`
	// Comment accompanies a choice.
	Comment string `json:"comment"`
}

type AnswerArtifactQuestionsCommand struct {
	ArtifactID    string
	Version       int // 0 means the current version
	Answers       []ArtifactAnswerInput
	Authorization ActionAuthorization
}

type WithdrawArtifactAnswerCommand struct {
	ArtifactID    string
	QuestionID    string
	Authorization ActionAuthorization
}

// ArtifactQuestionStore is the persistence the question methods need.
type ArtifactQuestionStore interface {
	ListArtifactQuestions(context.Context, string, int) ([]database.ArtifactQuestion, error)
	UpsertArtifactAnswers(context.Context, []*database.ArtifactAnnotation) error
	DeleteOpenArtifactAnswer(context.Context, string, string) error
}

func invalidArtifactQuestions(format string, args ...any) error {
	return validationError("invalid_artifact_questions",
		"The questions block (<script type=\"application/json\" id=\""+ArtifactQuestionsElementID+"\">) is invalid: "+fmt.Sprintf(format, args...))
}

// ParseArtifactQuestions reads the questions an artifact declares. A document
// without the block declares none. A block that is present but wrong is an
// error, so the publisher fixes it instead of shipping questions nobody can
// answer.
func ParseArtifactQuestions(html string) ([]ArtifactQuestionView, error) {
	// Cheap exit for the common case: most artifacts ask nothing.
	if !strings.Contains(strings.ToLower(html), ArtifactQuestionsElementID) {
		return nil, nil
	}
	var blocks []string
	// Script elements are scanned in document order, the way the HTML parser
	// reads them, so markup that only looks like the block inside another
	// script's text is not mistaken for it.
	for _, match := range artifactScriptTagPattern.FindAllStringSubmatch(html, -1) {
		attributes, body := match[1], match[2]
		if !artifactQuestionsIDPattern.MatchString(attributes) {
			continue
		}
		if !artifactJSONScriptTypePattern.MatchString(attributes) {
			return nil, invalidArtifactQuestions("it must be a data block, with type=\"application/json\"")
		}
		blocks = append(blocks, body)
	}
	switch len(blocks) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, invalidArtifactQuestions("the document has %d of them; declare every question in one block", len(blocks))
	}

	type rawQuestion struct {
		ID      string                   `json:"id"`
		Prompt  string                   `json:"prompt"`
		Type    string                   `json:"type"`
		Options []ArtifactQuestionOption `json:"options"`
		Comment *bool                    `json:"comment"`
	}
	var raw []rawQuestion
	body := strings.TrimSpace(blocks[0])
	if strings.HasPrefix(body, "[") {
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			return nil, invalidArtifactQuestions("it is not valid JSON (%v)", err)
		}
	} else {
		var wrapper struct {
			Questions []rawQuestion `json:"questions"`
		}
		if err := json.Unmarshal([]byte(body), &wrapper); err != nil {
			return nil, invalidArtifactQuestions("it is not valid JSON (%v)", err)
		}
		raw = wrapper.Questions
	}
	if len(raw) > maxArtifactQuestions {
		return nil, invalidArtifactQuestions("%d questions declared; the limit is %d", len(raw), maxArtifactQuestions)
	}

	questions := make([]ArtifactQuestionView, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for index, item := range raw {
		id := strings.TrimSpace(item.ID)
		if !artifactQuestionIDPattern.MatchString(id) {
			return nil, invalidArtifactQuestions("question %d has the id %q; an id is 1 to 64 letters, digits, '_', '-', '.' or ':', starting with a letter or digit", index+1, item.ID)
		}
		if seen[id] {
			return nil, invalidArtifactQuestions("the id %q is used by more than one question", id)
		}
		seen[id] = true
		prompt := singleLine(item.Prompt)
		if prompt == "" {
			return nil, invalidArtifactQuestions("question %q has no prompt", id)
		}
		if utf8.RuneCountInString(prompt) > maxArtifactQuestionPromptRunes {
			return nil, invalidArtifactQuestions("the prompt of question %q is longer than %d characters", id, maxArtifactQuestionPromptRunes)
		}
		kind := strings.ToLower(strings.TrimSpace(item.Type))
		if kind == "" {
			kind = ArtifactQuestionSingle
			if len(item.Options) == 0 {
				kind = ArtifactQuestionText
			}
		}
		question := ArtifactQuestionView{ID: id, Prompt: prompt, Type: kind, Comment: item.Comment == nil || *item.Comment}
		switch kind {
		case ArtifactQuestionText:
			if len(item.Options) > 0 {
				return nil, invalidArtifactQuestions("question %q is a text question and cannot have options", id)
			}
			question.Comment = false // the answer itself is the text
		case ArtifactQuestionSingle, ArtifactQuestionMulti:
			minimum := 2
			if kind == ArtifactQuestionMulti {
				minimum = 1
			}
			if len(item.Options) < minimum {
				return nil, invalidArtifactQuestions("question %q (%s) needs at least %d option(s)", id, kind, minimum)
			}
			if len(item.Options) > maxArtifactQuestionOptions {
				return nil, invalidArtifactQuestions("question %q has %d options; the limit is %d", id, len(item.Options), maxArtifactQuestionOptions)
			}
			options := make(map[string]bool, len(item.Options))
			for _, option := range item.Options {
				optionID := strings.TrimSpace(option.ID)
				if !artifactQuestionIDPattern.MatchString(optionID) {
					return nil, invalidArtifactQuestions("question %q has an option with the id %q; an id is 1 to 64 letters, digits, '_', '-', '.' or ':'", id, option.ID)
				}
				if options[optionID] {
					return nil, invalidArtifactQuestions("question %q uses the option id %q more than once", id, optionID)
				}
				options[optionID] = true
				label := singleLine(option.Label)
				if label == "" {
					return nil, invalidArtifactQuestions("option %q of question %q has no label", optionID, id)
				}
				if utf8.RuneCountInString(label) > maxArtifactQuestionLabelRunes {
					return nil, invalidArtifactQuestions("the label of option %q of question %q is longer than %d characters", optionID, id, maxArtifactQuestionLabelRunes)
				}
				question.Options = append(question.Options, ArtifactQuestionOption{ID: optionID, Label: label})
			}
		default:
			return nil, invalidArtifactQuestions("question %q has the type %q; use \"single\", \"multi\" or \"text\"", id, item.Type)
		}
		questions = append(questions, question)
	}
	return questions, nil
}

// artifactQuestionRows turns parsed questions into the rows stored with a
// version.
func artifactQuestionRows(questions []ArtifactQuestionView) ([]database.ArtifactQuestion, error) {
	rows := make([]database.ArtifactQuestion, 0, len(questions))
	for _, question := range questions {
		options := question.Options
		if options == nil {
			options = []ArtifactQuestionOption{}
		}
		encoded, err := json.Marshal(options)
		if err != nil {
			return nil, err
		}
		rows = append(rows, database.ArtifactQuestion{
			QuestionID: question.ID, Type: question.Type, Prompt: question.Prompt,
			OptionsJSON: string(encoded), AllowComment: question.Comment,
		})
	}
	return rows, nil
}

func artifactQuestionView(row *database.ArtifactQuestion) ArtifactQuestionView {
	view := ArtifactQuestionView{ID: row.QuestionID, Prompt: row.Prompt, Type: row.Type, Comment: row.AllowComment}
	_ = json.Unmarshal([]byte(row.OptionsJSON), &view.Options)
	return view
}

// Questions returns the questions one version declares. version 0 means the
// current version.
func (s *ArtifactService) Questions(ctx context.Context, artifactID string, version int) (ArtifactQuestionsView, error) {
	id, err := normalizeArtifactID(artifactID)
	if err != nil {
		return ArtifactQuestionsView{}, err
	}
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactQuestionsView{}, err
	}
	if version == 0 {
		version = artifact.CurrentVersion
	}
	if version < 1 || version > artifact.CurrentVersion {
		return ArtifactQuestionsView{}, notFoundError("artifact_version_not_found", "Artifact version not found", nil)
	}
	rows, err := s.store.ListArtifactQuestions(ctx, id, version)
	if err != nil {
		return ArtifactQuestionsView{}, err
	}
	view := ArtifactQuestionsView{ArtifactID: id, Version: version, Questions: make([]ArtifactQuestionView, 0, len(rows))}
	for i := range rows {
		view.Questions = append(view.Questions, artifactQuestionView(&rows[i]))
	}
	return view, nil
}

// AnswerQuestions saves the reader's answers to questions of one version: all
// of them or none. Each answer is checked against the questions that version
// declared when it was published. An answer replaces the open answer its
// question already has.
func (s *ArtifactService) AnswerQuestions(ctx context.Context, command AnswerArtifactQuestionsCommand) ([]ArtifactAnnotationView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	// Same rule as notes: an answer is the reader's decision, and the agent
	// that reads it must be able to trust that the reader gave it.
	if strings.TrimSpace(command.Authorization.Actor.Type) == "session" {
		return nil, validationError("annotation_author_not_allowed", "Answers are given by the user reading the artifact, not by a session")
	}
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return nil, err
	}
	if len(command.Answers) == 0 {
		return nil, validationError("answers_required", "At least one answer is required")
	}
	if len(command.Answers) > maxArtifactAnswerQuestionsPerPut {
		return nil, validationError("too_many_answers", "Too many answers in one request")
	}
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return nil, err
	}
	version := command.Version
	if version == 0 {
		version = artifact.CurrentVersion
	}
	if version < 1 || version > artifact.CurrentVersion {
		return nil, notFoundError("artifact_version_not_found", "Artifact version not found", nil)
	}
	rows, err := s.store.ListArtifactQuestions(ctx, id, version)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]ArtifactQuestionView, len(rows))
	for i := range rows {
		question := artifactQuestionView(&rows[i])
		declared[question.ID] = question
	}

	actor := command.Authorization.Actor.eventValue()
	annotations := make([]*database.ArtifactAnnotation, 0, len(command.Answers))
	answered := make(map[string]bool, len(command.Answers))
	for _, input := range command.Answers {
		questionID := strings.TrimSpace(input.Question)
		question, ok := declared[questionID]
		if !ok {
			return nil, validationError("unknown_artifact_question",
				fmt.Sprintf("Version %d of this artifact does not declare a question %q", version, singleLine(questionID)))
		}
		if answered[questionID] {
			return nil, validationError("duplicate_artifact_answer", fmt.Sprintf("Question %q is answered more than once in this request", questionID))
		}
		answered[questionID] = true

		answer := ArtifactAnswerView{Type: question.Type}
		var body string
		if question.Type == ArtifactQuestionText {
			if len(input.Options) > 0 {
				return nil, validationError("invalid_artifact_answer", fmt.Sprintf("Question %q is a text question and takes no option", questionID))
			}
			if body, err = boundedRedactedInput(input.Text, maxArtifactAnnotationBodyRunes, "Answer text", true); err != nil {
				return nil, err
			}
		} else {
			labels := make(map[string]string, len(question.Options))
			for _, option := range question.Options {
				labels[option.ID] = option.Label
			}
			chosen := make(map[string]bool, len(input.Options))
			for _, optionID := range input.Options {
				optionID = strings.TrimSpace(optionID)
				if _, ok := labels[optionID]; !ok {
					return nil, validationError("unknown_artifact_option",
						fmt.Sprintf("Question %q does not declare an option %q", questionID, singleLine(optionID)))
				}
				chosen[optionID] = true
			}
			if len(chosen) == 0 {
				return nil, validationError("invalid_artifact_answer", fmt.Sprintf("Question %q needs an option", questionID))
			}
			if question.Type == ArtifactQuestionSingle && len(chosen) != 1 {
				return nil, validationError("invalid_artifact_answer", fmt.Sprintf("Question %q takes exactly one option", questionID))
			}
			// Declared order, so the same choice is always stored the same way.
			for _, option := range question.Options {
				if chosen[option.ID] {
					answer.Options = append(answer.Options, option)
				}
			}
			if strings.TrimSpace(input.Text) != "" {
				return nil, validationError("invalid_artifact_answer", fmt.Sprintf("Question %q is a choice; put the remark in comment", questionID))
			}
			if body, err = boundedRedactedInput(input.Comment, maxArtifactAnnotationBodyRunes, "Answer comment", false); err != nil {
				return nil, err
			}
		}
		answerJSON, err := json.Marshal(answer)
		if err != nil {
			return nil, err
		}
		anchorJSON, err := json.Marshal(ArtifactAnchor{Type: "question", Question: questionID})
		if err != nil {
			return nil, err
		}
		annotations = append(annotations, &database.ArtifactAnnotation{
			ArtifactID: id, Version: version, AnchorJSON: string(anchorJSON), Quote: question.Prompt, Body: body,
			CreatedBy: actor, QuestionID: questionID, AnswerJSON: string(answerJSON),
		})
	}
	if err := s.store.UpsertArtifactAnswers(ctx, annotations); err != nil {
		return nil, err
	}
	views := make([]ArtifactAnnotationView, 0, len(annotations))
	for _, annotation := range annotations {
		stored, err := s.store.GetArtifactAnnotation(ctx, annotation.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, artifactAnnotationView(stored))
	}
	s.annotationsChanged(ctx, artifact, "answers_saved")
	return views, nil
}

// WithdrawAnswer removes the open answer to one question. An answer the agent
// already resolved stays as the record of what was decided.
func (s *ArtifactService) WithdrawAnswer(ctx context.Context, command WithdrawArtifactAnswerCommand) error {
	if err := requireActionActor(command.Authorization); err != nil {
		return err
	}
	if strings.TrimSpace(command.Authorization.Actor.Type) == "session" {
		return validationError("annotation_author_not_allowed", "Answers are withdrawn by the user reading the artifact, not by a session")
	}
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return err
	}
	questionID := strings.TrimSpace(command.QuestionID)
	if !artifactQuestionIDPattern.MatchString(questionID) {
		return validationError("invalid_artifact_question", "Question id is invalid")
	}
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return err
	}
	if err := s.store.DeleteOpenArtifactAnswer(ctx, id, questionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return notFoundError("answer_not_found", "This question has no open answer", err)
		}
		return err
	}
	s.annotationsChanged(ctx, artifact, "answer_withdrawn")
	return nil
}
