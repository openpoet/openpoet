// Package sessioninput tells who wrote the last prompt a session's agent
// accepted: a human (typed in the terminal or sent from the UI) or an
// automation (sessions.send_input, sessions.answer_prompt with text, the
// initial prompt of sessions.create, OpenPoet's own agents).
//
// Programmatic senders announce what they are about to type (Expect). When the
// agent reports the prompt it accepted (UserPromptSubmit, or a Codex turn
// start), Observe compares its text with the announced inputs: a match takes
// that sender's identity, no match means somebody typed it. An announced input
// that never becomes a prompt (a slash command, a dialog answer, a rejected
// send) simply expires, so it can never claim a later human prompt.
//
// State is in memory and kept after a session ends (sessions.get and the
// stop event still report it): after a restart a session has no last input
// until its agent accepts the next prompt.
package sessioninput

import (
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Sources reported in LastInput.Source.
const (
	SourceHuman      = "human"
	SourceAutomation = "automation"
	SourceUnknown    = "unknown"
)

// Bases reported in LastInput.Basis: how the source was decided.
const (
	// BasisPromptText: the accepted prompt's text was compared with the
	// announced inputs (a match is that sender; no match is a human typing).
	BasisPromptText = "prompt_text"
	// BasisTiming: the agent reported no prompt text, and an input had been
	// announced moments before.
	BasisTiming = "timing"
	// BasisNone: no text and nothing announced; the origin is unknown.
	BasisNone = "none"
)

// Vias reported in LastInput.Via.
const (
	ViaTerminal        = "terminal"
	ViaSendInput       = "send_input"
	ViaAnswerPrompt    = "answer_prompt"
	ViaInitialPrompt   = "initial_prompt"
	ViaIsolation       = "isolation_briefing"
	ViaRestartRecovery = "restart_recovery"
	ViaUI              = "ui"
	ViaAssistant       = "assistant"
)

const (
	pendingTTL           = 30 * time.Minute
	timingWindow         = 30 * time.Second
	maxPendingPerSession = 16
	// minContainedRunes is the shortest input that may match a prompt that
	// merely contains it; shorter inputs ("1", "yes", "continue") must equal
	// the prompt, so a human sentence that happens to hold them stays human.
	minContainedRunes = 16
)

// LastInput is the origin of the last prompt the session's agent accepted.
type LastInput struct {
	Source string `json:"source"`
	// Actor is the sender ("automation_client:<id>", "user:local-ui",
	// "agent:openpoet-ai", ...); omitted for prompts typed in the terminal.
	Actor string `json:"actor,omitempty"`
	// CommandID is the automation command that sent it, when there was one.
	CommandID string `json:"command_id,omitempty"`
	Via       string `json:"via,omitempty"`
	Basis     string `json:"basis"`
	// At is when the agent accepted the prompt.
	At time.Time `json:"at"`
	// SentAt is when the sender typed it (omitted for terminal input).
	SentAt *time.Time `json:"sent_at,omitempty"`
}

// Expected is an input a sender is about to type into a session.
type Expected struct {
	Text      string
	Actor     string
	ActorType string
	CommandID string
	Via       string
}

type pending struct {
	Expected
	text string // normalized
	at   time.Time
}

type Tracker struct {
	mu      sync.Mutex
	pending map[string][]pending
	last    map[string]LastInput
	now     func() time.Time
}

func NewTracker() *Tracker {
	return &Tracker{pending: make(map[string][]pending), last: make(map[string]LastInput), now: time.Now}
}

// SourceForActorType maps an actor type to a source: the local UI and paired
// devices act for a human, everything else (automation clients, agent
// sessions, OpenPoet's own agents and system jobs) is automation.
func SourceForActorType(actorType string) string {
	if strings.TrimSpace(actorType) == "user" {
		return SourceHuman
	}
	return SourceAutomation
}

// Expect announces an input about to be typed into sessionID.
func (t *Tracker) Expect(sessionID string, in Expected) {
	text := normalize(in.Text)
	if t == nil || sessionID == "" || text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	list := append(t.livePendingLocked(sessionID, now), pending{Expected: in, text: text, at: now})
	if len(list) > maxPendingPerSession {
		list = list[len(list)-maxPendingPerSession:]
	}
	t.pending[sessionID] = list
}

// Observe records that the agent accepted a prompt. hasText is false when
// the agent did not report the prompt's text.
func (t *Tracker) Observe(sessionID, prompt string, hasText bool) LastInput {
	if t == nil || sessionID == "" {
		return LastInput{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	list := t.livePendingLocked(sessionID, now)
	result := LastInput{Source: SourceUnknown, Basis: BasisNone, At: now}
	match := -1
	if hasText {
		result.Source, result.Basis, result.Via = SourceHuman, BasisPromptText, ViaTerminal
		normalized := normalize(prompt)
		for i := len(list) - 1; i >= 0; i-- {
			if matches(normalized, list[i].text) {
				match = i
				break
			}
		}
	} else if n := len(list); n > 0 && now.Sub(list[n-1].at) <= timingWindow {
		match = n - 1
		result.Basis = BasisTiming
	}
	if match >= 0 {
		p := list[match]
		sentAt := p.at
		result.Source = SourceForActorType(p.ActorType)
		result.Actor, result.CommandID, result.Via, result.SentAt = p.Actor, p.CommandID, p.Via, &sentAt
		list = append(list[:match:match], list[match+1:]...)
	}
	if len(list) == 0 {
		delete(t.pending, sessionID)
	} else {
		t.pending[sessionID] = list
	}
	t.last[sessionID] = result
	return result
}

// Last returns the origin of the session's last accepted prompt.
func (t *Tracker) Last(sessionID string) (LastInput, bool) {
	if t == nil {
		return LastInput{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.last[sessionID]
	return last, ok
}

func (t *Tracker) livePendingLocked(sessionID string, now time.Time) []pending {
	list := t.pending[sessionID]
	kept := list[:0]
	for _, p := range list {
		if now.Sub(p.at) <= pendingTTL {
			kept = append(kept, p)
		}
	}
	return kept
}

func matches(prompt, input string) bool {
	if prompt == input {
		return true
	}
	return utf8.RuneCountInString(input) >= minContainedRunes && strings.Contains(prompt, input)
}

// pastedContentTag matches the tags Claude Code wraps pasted blocks in when it
// reports the prompt.
var pastedContentTag = regexp.MustCompile(`</?pasted_content(?:\s[^>]*)?>`)

// normalize ignores what terminals and agents change in transit: pasted-block
// tags and whitespace (line breaks become \r, text is trimmed or re-wrapped).
func normalize(text string) string {
	return strings.Join(strings.Fields(pastedContentTag.ReplaceAllString(text, "")), " ")
}
