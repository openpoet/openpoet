package session

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"time"
)

// NotificationTrigger represents a pattern that triggers a notification
type NotificationTrigger struct {
	Pattern     *regexp.Regexp
	Type        string // 'info', 'warning', 'error', 'question'
	Title       string
	ExtractBody func(match []string) string
}

// DefaultTriggers contains the default notification patterns
var DefaultTriggers = []NotificationTrigger{
	{
		// Custom notification tag
		Pattern:     regexp.MustCompile(`\[NOTIFY(?::(\w+))?\]\s*(.+)`),
		Type:        "info",
		Title:       "Notification",
		ExtractBody: func(match []string) string { return match[2] },
	},
	{
		// Question/waiting for input
		Pattern:     regexp.MustCompile(`(?i)(?:waiting for|please (?:enter|provide|confirm)|do you want to|would you like to|are you sure)\s*[?:]?`),
		Type:        "question",
		Title:       "Input Required",
		ExtractBody: func(match []string) string { return match[0] },
	},
	{
		// Error messages
		Pattern:     regexp.MustCompile(`(?i)(?:error|failed|failure|exception|fatal):\s*(.+)`),
		Type:        "error",
		Title:       "Error",
		ExtractBody: func(match []string) string { return match[1] },
	},
	{
		// Warning messages
		Pattern:     regexp.MustCompile(`(?i)(?:warning|warn):\s*(.+)`),
		Type:        "warning",
		Title:       "Warning",
		ExtractBody: func(match []string) string { return match[1] },
	},
	{
		// Task completion
		Pattern:     regexp.MustCompile(`(?i)(?:completed|finished|done|success(?:fully)?)[.!]?\s*$`),
		Type:        "info",
		Title:       "Task Completed",
		ExtractBody: func(match []string) string { return match[0] },
	},
}

// NotificationResult represents a detected notification
type NotificationResult struct {
	Type  string
	Title string
	Body  string
}

// ParseOutputForNotifications checks output for notification triggers
func ParseOutputForNotifications(output string) []NotificationResult {
	var results []NotificationResult
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		for _, trigger := range DefaultTriggers {
			match := trigger.Pattern.FindStringSubmatch(line)
			if match != nil {
				notifType := trigger.Type

				// Handle custom notification type in [NOTIFY:type] format
				if trigger.Title == "Notification" && len(match) > 1 && match[1] != "" {
					notifType = strings.ToLower(match[1])
					if notifType != "info" && notifType != "warning" && notifType != "error" && notifType != "question" {
						notifType = "info"
					}
				}

				results = append(results, NotificationResult{
					Type:  notifType,
					Title: trigger.Title,
					Body:  trigger.ExtractBody(match),
				})
				break // Only match first trigger per line
			}
		}
	}

	return results
}

// NotificationBuffer accumulates output and detects notifications
type NotificationBuffer struct {
	buffer    strings.Builder
	lineCount int
	maxLines  int
	handler   func(NotificationResult)
}

func NewNotificationBuffer(handler func(NotificationResult)) *NotificationBuffer {
	return &NotificationBuffer{
		maxLines: 100, // Keep last 100 lines for context
		handler:  handler,
	}
}

func (nb *NotificationBuffer) Write(data []byte) {
	text := string(data)
	nb.buffer.WriteString(text)

	// Check for notifications in new data
	notifications := ParseOutputForNotifications(text)
	for _, n := range notifications {
		if nb.handler != nil {
			nb.handler(n)
		}
	}

	// Trim buffer if too large
	content := nb.buffer.String()
	lines := strings.Split(content, "\n")
	if len(lines) > nb.maxLines {
		nb.buffer.Reset()
		nb.buffer.WriteString(strings.Join(lines[len(lines)-nb.maxLines:], "\n"))
	}
}

func (nb *NotificationBuffer) GetBuffer() string {
	return nb.buffer.String()
}

// ── Attention sentinel (conflict-radar Phase 1) ─────────────────────────────
//
// The DefaultTriggers above are deliberately NOT wired to live PTY output:
// their bare `error:`/`completed` patterns fire on ordinary build noise and a
// single `go test` run would storm the durable outbox. The sentinel below is
// the narrow, storm-resistant rewiring of this file into the output chain:
// QUESTION patterns only, ANSI stripped, lines reassembled across chunk
// boundaries, and two-layer dedup (per-line hash + per-session cooldown).

// attentionQuestionPatterns match only text that plausibly means "the agent
// stopped and is waiting for a human answer". Keep them narrow: a false
// negative costs one missed badge; a false positive per output chunk floods
// the outbox.
var attentionQuestionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\((?:y/n|yes/no)\)`),
	regexp.MustCompile(`(?i)\[(?:y/n|yes/no)\]`),
	regexp.MustCompile(`(?i)do you want to (?:proceed|continue)`),
	regexp.MustCompile(`(?i)would you like to (?:proceed|continue)`),
	regexp.MustCompile(`(?i)waiting for your input`),
	regexp.MustCompile(`(?i)press enter to (?:continue|confirm)`),
	regexp.MustCompile(`(?i)are you sure you want to`),
}

// attentionANSIPattern strips CSI/OSC escape sequences before matching, so TUI
// styling never hides (or fabricates) a question.
var attentionANSIPattern = regexp.MustCompile(`\x1b\[[?0-9;]*[a-zA-Z]|\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b[^[\]].?`)

const (
	attentionDedupTTL        = 5 * time.Minute
	attentionSessionCooldown = 10 * time.Second
	attentionMaxPartial      = 4096
	attentionMaxExcerpt      = 200
)

// Attention is one sentinel detection.
type Attention struct {
	Kind    string // currently always "question"
	Excerpt string
}

// AttentionSentinel keeps per-session line-reassembly and dedup state. It is
// safe for concurrent Feed calls from multiple runner goroutines.
type AttentionSentinel struct {
	mu       sync.Mutex
	partial  map[string]string
	seen     map[string]map[string]time.Time
	lastEmit map[string]time.Time
	now      func() time.Time
}

func NewAttentionSentinel() *AttentionSentinel {
	return &AttentionSentinel{
		partial:  make(map[string]string),
		seen:     make(map[string]map[string]time.Time),
		lastEmit: make(map[string]time.Time),
		now:      time.Now,
	}
}

// Feed scans one raw PTY output chunk for a session and returns any new
// attention detections. Incomplete trailing lines are buffered and re-scanned
// when the rest arrives; a prompt with no trailing newline (the common case
// for real question prompts) is scanned as the partial line and deduplicated
// by content hash if it later completes.
func (s *AttentionSentinel) Feed(sessionID string, data []byte) []Attention {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := s.partial[sessionID] + string(data)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	segments := strings.Split(text, "\n")
	tail := segments[len(segments)-1]
	if len(tail) > attentionMaxPartial {
		tail = tail[len(tail)-attentionMaxPartial:]
	}
	s.partial[sessionID] = tail
	candidates := segments[:len(segments)-1]
	if strings.TrimSpace(tail) != "" {
		candidates = append(candidates, tail)
	}

	var out []Attention
	nowTs := s.now()
	for _, line := range candidates {
		clean := strings.TrimSpace(attentionANSIPattern.ReplaceAllString(line, ""))
		if clean == "" || !matchesAttentionQuestion(clean) {
			continue
		}
		hash := attentionHash(clean)
		sessionSeen := s.seen[sessionID]
		if sessionSeen == nil {
			sessionSeen = make(map[string]time.Time)
			s.seen[sessionID] = sessionSeen
		}
		if len(sessionSeen) > 256 {
			// Bound long-session growth: expired dedup entries are useless.
			for h, t := range sessionSeen {
				if nowTs.Sub(t) >= attentionDedupTTL {
					delete(sessionSeen, h)
				}
			}
		}
		if t, ok := sessionSeen[hash]; ok && nowTs.Sub(t) < attentionDedupTTL {
			continue
		}
		if t, ok := s.lastEmit[sessionID]; ok && nowTs.Sub(t) < attentionSessionCooldown {
			continue
		}
		sessionSeen[hash] = nowTs
		s.lastEmit[sessionID] = nowTs
		excerpt := clean
		if len(excerpt) > attentionMaxExcerpt {
			excerpt = excerpt[:attentionMaxExcerpt]
		}
		out = append(out, Attention{Kind: "question", Excerpt: excerpt})
	}
	return out
}

// Forget drops all sentinel state for an ended session.
func (s *AttentionSentinel) Forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.partial, sessionID)
	delete(s.seen, sessionID)
	delete(s.lastEmit, sessionID)
}

func matchesAttentionQuestion(line string) bool {
	for _, pattern := range attentionQuestionPatterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

// attentionHash normalizes digits out of the line before hashing so a live
// prompt whose only variation is a counter ("waiting for your input (12s)")
// deduplicates as ONE question instead of re-emitting on every redraw.
func attentionHash(line string) string {
	normalized := attentionDigitsPattern.ReplaceAllString(line, "#")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

var attentionDigitsPattern = regexp.MustCompile(`[0-9]+`)
