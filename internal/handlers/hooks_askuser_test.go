package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// realQuestion is a verbatim AskUserQuestion tool_input captured from a
// production transcript. Both the question text and one option label contain
// double quotes — the shape that used to be truncated by the web client.
const realQuestion = `{
  "questions": [{
    "question": "O que você quer fazer com o slide \"07 Usabilidade — automação e escala\"?",
    "header": "Slide 07",
    "multiSelect": false,
    "options": [
      {"label": "Remover o slide inteiro", "description": "O deck fica com 9 slides"},
      {"label": "Remover o card \"Custo sob controle\"", "description": "Ficam só os cards Scripts", "preview": "┌─ card ─┐"},
      {"label": "Remover menções técnicas restantes", "description": "Linguagem mais de negócio"}
    ]
  }]
}`

func toolInputFromJSON(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m
}

func TestReconcileAskUserAnswersRepairsQuoteTruncation(t *testing.T) {
	questions := extractAskUserQuestions(toolInputFromJSON(t, realQuestion))
	if len(questions) != 1 || len(questions[0].labels) != 3 {
		t.Fatalf("extract failed: %+v", questions)
	}

	// Exactly what the pre-fix client sent, per the production transcript.
	answers := map[string]string{
		"O que você quer fazer com o slide ": "Remover o card ",
	}

	got, annotations := reconcileAskUserAnswers(questions, answers, "sess")

	wantKey := "O que você quer fazer com o slide \"07 Usabilidade — automação e escala\"?"
	wantVal := "Remover o card \"Custo sob controle\""
	if v, ok := got[wantKey]; !ok || v != wantVal {
		t.Fatalf("answer not repaired: got %#v, want %q => %q", got, wantKey, wantVal)
	}
	if len(got) != 1 {
		t.Fatalf("expected the truncated key to be replaced, not added: %#v", got)
	}
	if a, ok := annotations[wantKey].(map[string]interface{}); !ok || a["preview"] != "┌─ card ─┐" {
		t.Fatalf("preview annotation missing: %#v", annotations)
	}
}

func TestReconcileAskUserAnswersRemapsIndexKeys(t *testing.T) {
	questions := extractAskUserQuestions(toolInputFromJSON(t, realQuestion))
	got, _ := reconcileAskUserAnswers(questions, map[string]string{"0": "Sublime Text"}, "sess")

	wantKey := "O que você quer fazer com o slide \"07 Usabilidade — automação e escala\"?"
	if v, ok := got[wantKey]; !ok || v != "Sublime Text" {
		t.Fatalf("index key not remapped: %#v", got)
	}
}

func TestReconcileAskUserAnswersLeavesFreeTextAlone(t *testing.T) {
	questions := extractAskUserQuestions(toolInputFromJSON(t, realQuestion))
	wantKey := "O que você quer fazer com o slide \"07 Usabilidade — automação e escala\"?"

	// A free-text "Other" answer that is a prefix of an option label but is NOT
	// followed by a quote in that label must survive untouched.
	got, _ := reconcileAskUserAnswers(questions, map[string]string{wantKey: "Remover"}, "sess")
	if got[wantKey] != "Remover" {
		t.Fatalf("free-text answer was rewritten: %#v", got)
	}

	// And an answer unrelated to any label stays verbatim.
	got, _ = reconcileAskUserAnswers(questions, map[string]string{wantKey: "nada disso, quero conversar"}, "sess")
	if got[wantKey] != "nada disso, quero conversar" {
		t.Fatalf("free-text answer was rewritten: %#v", got)
	}
}

func TestReconcileAskUserAnswersMultiSelect(t *testing.T) {
	raw := `{"questions":[{"question":"Quais?","multiSelect":true,"options":[
	  {"label":"Remover o card \"A\""},{"label":"Remover o card \"B\""},{"label":"Simples"}]}]}`
	questions := extractAskUserQuestions(toolInputFromJSON(t, raw))

	// Two truncated labels joined the way the client joins multi-select answers.
	got, _ := reconcileAskUserAnswers(questions, map[string]string{
		"Quais?": "Remover o card , Simples",
	}, "sess")
	if got["Quais?"] != "Remover o card , Simples" {
		// Ambiguous prefix (matches both A and B) must NOT be guessed.
		t.Fatalf("ambiguous truncation should be left alone: %#v", got)
	}

	got, _ = reconcileAskUserAnswers(questions, map[string]string{
		"Quais?": "Simples, Remover o card \"B\"",
	}, "sess")
	if got["Quais?"] != "Simples, Remover o card \"B\"" {
		t.Fatalf("intact multi-select answer altered: %#v", got)
	}
}

func TestReconcileAskUserAnswersSingleQuestionFallback(t *testing.T) {
	questions := extractAskUserQuestions(toolInputFromJSON(t, realQuestion))
	got, _ := reconcileAskUserAnswers(questions, map[string]string{"garbled": "Remover o slide inteiro"}, "sess")

	wantKey := "O que você quer fazer com o slide \"07 Usabilidade — automação e escala\"?"
	if got[wantKey] != "Remover o slide inteiro" {
		t.Fatalf("single-question fallback did not fire: %#v", got)
	}
}

func TestReconcileAskUserAnswersKeepsUnresolvableKeys(t *testing.T) {
	raw := `{"questions":[
	  {"question":"A?","options":[{"label":"x"},{"label":"y"}]},
	  {"question":"B?","options":[{"label":"x"},{"label":"y"}]}]}`
	questions := extractAskUserQuestions(toolInputFromJSON(t, raw))

	// Two questions and a key matching neither: no safe guess exists, so it is
	// passed through untouched rather than attached to the wrong question.
	got, _ := reconcileAskUserAnswers(questions, map[string]string{"C?": "x"}, "sess")
	if got["C?"] != "x" || len(got) != 1 {
		t.Fatalf("unresolvable key mishandled: %#v", got)
	}
}

func TestReconcileAskUserAnswersFlagsSkippedQuestions(t *testing.T) {
	raw := `{"questions":[
	  {"question":"A?","options":[{"label":"x"},{"label":"y"}]},
	  {"question":"B?","options":[{"label":"x"},{"label":"y"}]},
	  {"question":"C?","options":[{"label":"x"},{"label":"y"}]}]}`
	questions := extractAskUserQuestions(toolInputFromJSON(t, raw))

	// Only A was answered. Claude Code filters missing keys out of its summary
	// and reports "Your questions have been answered", so B and C must carry a
	// note or the agent never learns they were skipped.
	_, annotations := reconcileAskUserAnswers(questions, map[string]string{"A?": "x"}, "sess")

	if _, ok := annotations["A?"]; ok {
		t.Fatalf("answered question should not be flagged: %#v", annotations)
	}
	for _, q := range []string{"B?", "C?"} {
		a, ok := annotations[q].(map[string]interface{})
		if !ok || a["notes"] == "" || a["notes"] == nil {
			t.Fatalf("skipped question %s not flagged: %#v", q, annotations)
		}
	}
}

func TestReconcileAskUserAnswersFullyAnsweredHasNoNotes(t *testing.T) {
	raw := `{"questions":[
	  {"question":"A?","options":[{"label":"x"},{"label":"y"}]},
	  {"question":"B?","options":[{"label":"x"},{"label":"y"}]}]}`
	questions := extractAskUserQuestions(toolInputFromJSON(t, raw))

	_, annotations := reconcileAskUserAnswers(questions, map[string]string{"A?": "x", "B?": "y"}, "sess")
	if len(annotations) != 0 {
		t.Fatalf("fully answered prompt should carry no notes: %#v", annotations)
	}
}

func TestRepairQuoteTruncationRequiresUniqueMatch(t *testing.T) {
	candidates := []string{`the "first" one`, `the "second" one`}
	if got, ok := repairQuoteTruncation("the ", candidates); ok {
		t.Fatalf("ambiguous prefix repaired to %q", got)
	}
	if got, ok := repairQuoteTruncation(`the "first" one`, candidates); ok || got != `the "first" one` {
		t.Fatalf("exact value should not be repaired: %q %v", got, ok)
	}
	if got, ok := repairQuoteTruncation("", candidates); ok || got != "" {
		t.Fatalf("empty value should not be repaired: %q %v", got, ok)
	}
}

func TestAskUserClarifyMessageMatchesClaudeCodeWording(t *testing.T) {
	questions := extractAskUserQuestions(toolInputFromJSON(t, realQuestion))
	msg := askUserClarifyMessage(questions, nil)

	for _, want := range []string{
		"The user wants to clarify these questions.",
		"    Start by asking them what they would like to clarify.",
		"    Questions asked:",
		"  (No answer provided)",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("clarify message missing %q:\n%s", want, msg)
		}
	}

	wantKey := "O que você quer fazer com o slide \"07 Usabilidade — automação e escala\"?"
	withAnswer := askUserClarifyMessage(questions, map[string]string{wantKey: "Remover o slide inteiro"})
	if !strings.Contains(withAnswer, "  Answer: Remover o slide inteiro") {
		t.Fatalf("selected answer not carried into clarify message:\n%s", withAnswer)
	}
}

func TestReconcileAskUserAnswersPrefersExactKeyOverFuzzy(t *testing.T) {
	raw := `{"questions":[{"question":"A?","options":[{"label":"x"},{"label":"y"}]}]}`
	questions := extractAskUserQuestions(toolInputFromJSON(t, raw))

	// A stale client can send both the literal text and an index key for the
	// same question. The exact key must win, deterministically, every time.
	for i := 0; i < 50; i++ {
		got, _ := reconcileAskUserAnswers(questions, map[string]string{"A?": "x", "0": "y"}, "sess")
		if got["A?"] != "x" {
			t.Fatalf("run %d: exact key lost to fuzzy key: %#v", i, got)
		}
	}
}

func TestReconcileAskUserAnswersNoQuestionsIsInert(t *testing.T) {
	got, ann := reconcileAskUserAnswers(nil, map[string]string{"A?": "x"}, "sess")
	if got["A?"] != "x" || len(ann) != 0 {
		t.Fatalf("unexpected result with no questions: %#v %#v", got, ann)
	}
}
