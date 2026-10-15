package sessioninput

import (
	"testing"
	"time"
)

func newTestTracker() (*Tracker, *time.Time) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker()
	tracker.now = func() time.Time { return now }
	return tracker, &now
}

func automationInput(text, commandID string) Expected {
	return Expected{Text: text, Actor: "automation_client:orq", ActorType: "automation_client", CommandID: commandID, Via: ViaSendInput}
}

func TestSessionWithoutInputHasNoLastInput(t *testing.T) {
	tracker, _ := newTestTracker()
	if last, ok := tracker.Last("s1"); ok {
		t.Fatalf("last input %+v before any prompt", last)
	}
	tracker.Expect("s1", automationInput("revise o PR", "orq-1"))
	if last, ok := tracker.Last("s1"); ok {
		t.Fatalf("an announced input that was not accepted yet is reported: %+v", last)
	}
}

func TestPromptTypedByAHumanIsHuman(t *testing.T) {
	tracker, _ := newTestTracker()
	got := tracker.Observe("s1", "roda os testes de novo", true)
	if got.Source != SourceHuman || got.Basis != BasisPromptText || got.Via != ViaTerminal || got.Actor != "" || got.SentAt != nil {
		t.Fatalf("typed prompt = %+v, want human from the terminal", got)
	}
}

func TestPromptSentByAutomationCarriesActorAndCommand(t *testing.T) {
	tracker, now := newTestTracker()
	sentAt := *now
	tracker.Expect("s1", automationInput("Revise o PR\n e responda", "orq-42"))
	*now = now.Add(3 * time.Second)
	// The terminal turned the line break into \r and Claude Code wrapped it.
	got := tracker.Observe("s1", "<pasted_content id=\"x\">Revise o PR\r e responda</pasted_content id=\"x\">", true)
	if got.Source != SourceAutomation || got.Actor != "automation_client:orq" || got.CommandID != "orq-42" || got.Via != ViaSendInput || got.Basis != BasisPromptText {
		t.Fatalf("automation prompt = %+v", got)
	}
	if got.SentAt == nil || !got.SentAt.Equal(sentAt) || !got.At.Equal(*now) {
		t.Fatalf("sent_at/at = %v/%v, want %v/%v", got.SentAt, got.At, sentAt, *now)
	}
	if last, ok := tracker.Last("s1"); !ok || last.CommandID != "orq-42" {
		t.Fatalf("Last = %+v (%v)", last, ok)
	}
}

// The incident: a send_input that never became a user message must not turn
// the human's next prompt into automation, nor be consumed by it.
func TestSendInputThatNeverBecomesAPromptDoesNotClaimTheNextOne(t *testing.T) {
	tracker, now := newTestTracker()
	tracker.Expect("s1", automationInput("/compact", "orq-7"))
	*now = now.Add(5 * time.Second)
	if got := tracker.Observe("s1", "volta para o bug do login", true); got.Source != SourceHuman {
		t.Fatalf("human prompt after a swallowed send = %+v", got)
	}
	// Past its TTL the announced input is gone even if its text shows up.
	*now = now.Add(pendingTTL + time.Second)
	if got := tracker.Observe("s1", "/compact", true); got.Source != SourceHuman {
		t.Fatalf("expired input still claimed a prompt: %+v", got)
	}
}

func TestShortInputMustEqualThePrompt(t *testing.T) {
	tracker, _ := newTestTracker()
	tracker.Expect("s1", automationInput("continue", "orq-1"))
	if got := tracker.Observe("s1", "continue, mas sem mexer no schema", true); got.Source != SourceHuman {
		t.Fatalf("a human sentence holding a short automation text = %+v", got)
	}
	if got := tracker.Observe("s1", "continue", true); got.Source != SourceAutomation {
		t.Fatalf("the short text itself = %+v", got)
	}
}

func TestEachAnnouncedInputClaimsOnePrompt(t *testing.T) {
	tracker, _ := newTestTracker()
	tracker.Expect("s1", automationInput("rode make test-web", "orq-1"))
	tracker.Observe("s1", "rode make test-web", true)
	if got := tracker.Observe("s1", "rode make test-web", true); got.Source != SourceHuman {
		t.Fatalf("the same text typed again by a human = %+v", got)
	}
}

func TestUIActorIsHuman(t *testing.T) {
	tracker, _ := newTestTracker()
	tracker.Expect("s1", Expected{Text: "oi", Actor: "user:local-ui", ActorType: "user", Via: ViaUI})
	got := tracker.Observe("s1", "oi", true)
	if got.Source != SourceHuman || got.Actor != "user:local-ui" || got.Via != ViaUI {
		t.Fatalf("UI input = %+v", got)
	}
}

func TestPromptWithoutTextUsesTiming(t *testing.T) {
	tracker, now := newTestTracker()
	tracker.Expect("s1", automationInput("analise o log", "orq-1"))
	*now = now.Add(2 * time.Second)
	if got := tracker.Observe("s1", "", false); got.Source != SourceAutomation || got.Basis != BasisTiming || got.CommandID != "orq-1" {
		t.Fatalf("textless prompt right after a send = %+v", got)
	}
	tracker.Expect("s1", automationInput("analise o log", "orq-2"))
	*now = now.Add(timingWindow + time.Second)
	if got := tracker.Observe("s1", "", false); got.Source != SourceUnknown || got.Basis != BasisNone {
		t.Fatalf("textless prompt long after a send = %+v", got)
	}
}

func TestSessionsAreIndependent(t *testing.T) {
	tracker, _ := newTestTracker()
	tracker.Expect("s1", automationInput("revise o PR 12", "orq-1"))
	if got := tracker.Observe("s2", "revise o PR 12", true); got.Source != SourceHuman {
		t.Fatalf("another session's input claimed s2's prompt: %+v", got)
	}
}
