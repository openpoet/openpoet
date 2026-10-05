package application

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
)

// SessionTranscriptPort reads a session's structured transcript (the agent's
// JSONL, never the terminal capture). reason is set (not_found,
// unsupported_backend) when the session has no transcript to read.
type SessionTranscriptPort interface {
	SessionTranscript(ctx context.Context, sessionID string) ([]*jsonlview.SessionEvent, string, error)
}

// SessionMessageStore is the session lookup sessions messages need.
type SessionMessageStore interface {
	GetSession(context.Context, string) (*database.Session, error)
}

const (
	SessionMessagesDefaultLastN    = 10
	SessionMessagesMaxLastN        = 20
	SessionMessagesMaxHits         = 10
	SessionMessagesDefaultMaxChars = 500
	SessionMessagesMinMaxChars     = 80
	SessionMessagesMaxMaxChars     = 1500
	SessionMessagesSnippetContext  = 100 // characters kept on each side of a search match
	SessionMessagesExpandChunk     = 8000
	sessionMessagesShortIDLen      = 8
	sessionMessagesMaxSearchRunes  = 200
	sessionMessagesMaxIDBytes      = 200
)

// SessionMessagesQuery selects one of three reads: the latest messages
// (default), a search (Search set) or one whole message (Expand set).
type SessionMessagesQuery struct {
	SessionID string
	Role      string
	LastN     int
	BeforeID  string
	Search    string
	Expand    string
	Offset    int
	MaxChars  int
	// ProjectAllowed, when set, hides sessions of projects the caller may not see.
	ProjectAllowed func(projectID int64) bool
}

type SessionMessageView struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Model     string    `json:"model,omitempty"`
	At        time.Time `json:"at"`
	Chars     int       `json:"chars"`
	Text      string    `json:"text"`
	Truncated bool      `json:"truncated"`
}

type SessionMessageHit struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	At      time.Time `json:"at"`
	Chars   int       `json:"chars"`
	Matches int       `json:"matches"` // occurrences in this message
	Offset  int       `json:"offset"`  // character offset of the first match
	Snippet string    `json:"snippet"`
}

type SessionMessageChunk struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Model      string    `json:"model,omitempty"`
	At         time.Time `json:"at"`
	Chars      int       `json:"chars"`
	Offset     int       `json:"offset"`
	Text       string    `json:"text"`
	NextOffset *int      `json:"next_offset,omitempty"`
}

// SessionMessagesResult carries Messages (mode list), Hits (mode search) or
// Message (mode expand).
type SessionMessagesResult struct {
	SessionID    string               `json:"session_id"`
	Source       string               `json:"source"` // transcript
	Mode         string               `json:"mode"`   // list | search | expand
	Total        int                  `json:"total,omitempty"`
	Search       string               `json:"search,omitempty"`
	Messages     []SessionMessageView `json:"messages,omitempty"`
	Hits         []SessionMessageHit  `json:"hits,omitempty"`
	Message      *SessionMessageChunk `json:"message,omitempty"`
	HasMore      bool                 `json:"has_more"`
	NextBeforeID string               `json:"next_before_id,omitempty"`
}

// SessionMessageService reads a session's conversation as clean text.
type SessionMessageService struct {
	store       SessionMessageStore
	transcripts SessionTranscriptPort
}

func NewSessionMessageService(store SessionMessageStore, transcripts SessionTranscriptPort) *SessionMessageService {
	return &SessionMessageService{store: store, transcripts: transcripts}
}

// Normalize applies defaults and rejects out-of-range fields.
func (q *SessionMessagesQuery) Normalize() error {
	q.SessionID = strings.TrimSpace(q.SessionID)
	q.Role = strings.ToLower(strings.TrimSpace(q.Role))
	q.BeforeID = strings.TrimSpace(q.BeforeID)
	q.Search = strings.TrimSpace(q.Search)
	q.Expand = strings.TrimSpace(q.Expand)
	switch {
	case q.Role != "" && q.Role != "user" && q.Role != "assistant":
		return validationError("session_messages_invalid", `role must be user or assistant`)
	case q.LastN < 0 || q.LastN > SessionMessagesMaxLastN:
		return validationError("session_messages_invalid", `last_n must be between 1 and 20`)
	case q.MaxChars != 0 && (q.MaxChars < SessionMessagesMinMaxChars || q.MaxChars > SessionMessagesMaxMaxChars):
		return validationError("session_messages_invalid", `max_chars must be between 80 and 1500`)
	case len(q.BeforeID) > sessionMessagesMaxIDBytes || len(q.Expand) > sessionMessagesMaxIDBytes:
		return validationError("session_messages_invalid", "message id is too large")
	case utf8.RuneCountInString(q.Search) > sessionMessagesMaxSearchRunes:
		return validationError("session_messages_invalid", `search exceeds 200 characters`)
	case q.Search != "" && q.Expand != "":
		return validationError("session_messages_invalid", "send either search or expand, not both")
	case q.Offset < 0:
		return validationError("session_messages_invalid", `offset must not be negative`)
	case q.Offset > 0 && q.Expand == "":
		return validationError("session_messages_invalid", `offset is only used with expand`)
	}
	if q.LastN == 0 {
		q.LastN = SessionMessagesDefaultLastN
	}
	if q.MaxChars == 0 {
		q.MaxChars = SessionMessagesDefaultMaxChars
	}
	return nil
}

// Mode is list, search or expand.
func (q SessionMessagesQuery) Mode() string {
	switch {
	case q.Search != "":
		return "search"
	case q.Expand != "":
		return "expand"
	}
	return "list"
}

func (s *SessionMessageService) Read(ctx context.Context, query SessionMessagesQuery) (*SessionMessagesResult, error) {
	if err := query.Normalize(); err != nil {
		return nil, err
	}
	if query.SessionID == "" {
		return nil, validationError("session_messages_invalid", "session_id is required")
	}
	if s == nil || s.store == nil || s.transcripts == nil {
		return nil, safeBackendError("session transcripts are unavailable", nil)
	}
	item, err := s.store.GetSession(ctx, query.SessionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil || item == nil || query.ProjectAllowed != nil && !query.ProjectAllowed(item.ProjectID) {
		return nil, notFoundError("session_not_found", "session not found", nil)
	}
	events, reason, err := s.transcripts.SessionTranscript(ctx, item.ID)
	switch {
	case reason == "not_found":
		return nil, notFoundError("session_not_found", "session not found", nil)
	case reason != "":
		return nil, conflictError("session_transcript_unavailable", "this session has no structured transcript ("+reason+", backend "+item.Backend+")")
	case errors.Is(err, context.DeadlineExceeded):
		return nil, &Error{Kind: ErrorConflict, Code: "session_transcript_timeout", Message: "the session's remote host did not deliver the transcript in time (slow or unreachable); try again later", Cause: err}
	case err != nil:
		return nil, &Error{Kind: ErrorConflict, Code: "session_transcript_unavailable", Message: "the session transcript could not be read", Cause: err}
	}
	messages := sessionTranscriptMessages(events)
	switch query.Mode() {
	case "search":
		return searchSessionMessages(item.ID, messages, query)
	case "expand":
		return expandSessionMessage(item.ID, messages, query)
	default:
		return listSessionMessages(item.ID, messages, query)
	}
}

// sessionTranscriptMessage is one conversation message in clean text.
type sessionTranscriptMessage struct {
	id    string // short, unique within the transcript
	uuid  string // transcript entry uuid without dashes (prefix lookups)
	role  string
	model string
	at    time.Time
	text  []rune
}

var (
	transcriptSystemReminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	transcriptCommandNamePattern    = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	transcriptCommandArgsPattern    = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	transcriptBashInputPattern      = regexp.MustCompile(`(?s)^<bash-input>(.*?)</bash-input>$`)
	transcriptANSIPattern           = regexp.MustCompile("\\x1b\\[[?0-9;]*[a-zA-Z]|\\x1b\\][^\\x07]*(?:\\x07|\\x1b\\\\)|\\x1b[^[\\]].?")
)

// Harness-generated user lines that are not part of the conversation.
var transcriptHarnessPrefixes = []string{
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<bash-stdout>", "<bash-stderr>", "<task-notification>",
}

func sessionTranscriptMessages(events []*jsonlview.SessionEvent) []sessionTranscriptMessage {
	messages := make([]sessionTranscriptMessage, 0, len(events)/2)
	for index, event := range events {
		if event == nil || event.Message == nil || event.IsSidechain || event.IsMeta || event.IsSummary {
			continue
		}
		var text string
		switch event.Type {
		case "user":
			text = transcriptUserText(event.Message.ContentBlocks)
		case "assistant":
			text = transcriptBlocksText(event.Message.ContentBlocks, false)
		default:
			continue
		}
		text = cleanTranscriptText(text)
		if text == "" || event.Message.Model == "<synthetic>" && text == "No response requested." {
			continue // nothing said, or Claude Code's filler after an interrupted turn
		}
		message := sessionTranscriptMessage{
			uuid: strings.ReplaceAll(strings.ToLower(event.UUID), "-", ""),
			role: event.Type, at: event.Timestamp, text: []rune(text),
		}
		if event.Type == "assistant" {
			message.model = event.Message.Model
		}
		if message.uuid == "" {
			message.uuid = "m" + strconv.Itoa(index)
		}
		messages = append(messages, message)
	}
	assignSessionMessageIDs(messages)
	return messages
}

// cleanTranscriptText strips terminal escapes and control characters and
// redacts secrets.
func cleanTranscriptText(text string) string {
	text = transcriptANSIPattern.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, text)
	return strings.TrimSpace(redactReportSecrets(text))
}

func transcriptBlocksText(blocks []jsonlview.ContentBlock, images bool) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Type == "text" && strings.TrimSpace(block.Text) != "":
			parts = append(parts, block.Text)
		case block.Type == "image" && images:
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n\n")
}

func transcriptUserText(blocks []jsonlview.ContentBlock) string {
	text := transcriptSystemReminderPattern.ReplaceAllString(transcriptBlocksText(blocks, true), "")
	// A long prompt typed as a bracketed paste is recorded inside
	// <pasted_content> tags; the reader wants the text the user sent.
	text = strings.TrimSpace(unwrapPastedPrompt(text))
	for _, prefix := range transcriptHarnessPrefixes {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	if match := transcriptCommandNamePattern.FindStringSubmatch(text); match != nil {
		command := strings.TrimSpace(match[1])
		if args := transcriptCommandArgsPattern.FindStringSubmatch(text); args != nil && strings.TrimSpace(args[1]) != "" {
			command += " " + strings.TrimSpace(args[1])
		}
		return command
	}
	if match := transcriptBashInputPattern.FindStringSubmatch(text); match != nil {
		return "! " + strings.TrimSpace(match[1])
	}
	return text
}

// assignSessionMessageIDs gives each message the shortest uuid prefix (at
// least 8 characters) that no other message shares.
func assignSessionMessageIDs(messages []sessionTranscriptMessage) {
	for length := sessionMessagesShortIDLen; ; length *= 2 {
		counts := make(map[string]int, len(messages))
		pending := false
		for i := range messages {
			if messages[i].id == "" {
				counts[prefixOf(messages[i].uuid, length)]++
			}
		}
		for i := range messages {
			if messages[i].id != "" {
				continue
			}
			prefix := prefixOf(messages[i].uuid, length)
			if counts[prefix] == 1 || len(prefix) == len(messages[i].uuid) {
				messages[i].id = prefix
			} else {
				pending = true
			}
		}
		if !pending {
			return
		}
	}
}

func prefixOf(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}

// findSessionMessage resolves an id (or any unique uuid prefix) to an index.
func findSessionMessage(messages []sessionTranscriptMessage, id string) (int, error) {
	id = strings.ReplaceAll(strings.ToLower(id), "-", "")
	found := -1
	for i := range messages {
		if messages[i].id == id {
			return i, nil
		}
		if id != "" && strings.HasPrefix(messages[i].uuid, id) {
			if found >= 0 {
				return -1, validationError("session_message_ambiguous", "the message id matches more than one message; send a longer prefix")
			}
			found = i
		}
	}
	if found < 0 {
		return -1, notFoundError("session_message_not_found", "no message with this id in the session transcript", nil)
	}
	return found, nil
}

// sessionMessagesBefore returns the messages of the requested role that are
// older than the before_id cursor (all of them without a cursor), and how
// many messages of that role the transcript holds.
func sessionMessagesBefore(messages []sessionTranscriptMessage, query SessionMessagesQuery) ([]sessionTranscriptMessage, int, error) {
	end := len(messages)
	if query.BeforeID != "" {
		index, err := findSessionMessage(messages, query.BeforeID)
		if err != nil {
			return nil, 0, err
		}
		end = index
	}
	total := 0
	selected := make([]sessionTranscriptMessage, 0, end)
	for i, message := range messages {
		if query.Role != "" && message.role != query.Role {
			continue
		}
		total++
		if i < end {
			selected = append(selected, message)
		}
	}
	return selected, total, nil
}

func listSessionMessages(sessionID string, messages []sessionTranscriptMessage, query SessionMessagesQuery) (*SessionMessagesResult, error) {
	selected, total, err := sessionMessagesBefore(messages, query)
	if err != nil {
		return nil, err
	}
	start := max(0, len(selected)-query.LastN)
	result := &SessionMessagesResult{SessionID: sessionID, Source: "transcript", Mode: "list", Total: total, Messages: make([]SessionMessageView, 0, len(selected)-start)}
	for _, message := range selected[start:] {
		text, truncated := truncateRunes(message.text, query.MaxChars)
		result.Messages = append(result.Messages, SessionMessageView{
			ID: message.id, Role: message.role, Model: message.model, At: message.at,
			Chars: len(message.text), Text: text, Truncated: truncated,
		})
	}
	if start > 0 {
		result.HasMore = true
		result.NextBeforeID = selected[start].id
	}
	return result, nil
}

func searchSessionMessages(sessionID string, messages []sessionTranscriptMessage, query SessionMessagesQuery) (*SessionMessagesResult, error) {
	selected, _, err := sessionMessagesBefore(messages, query)
	if err != nil {
		return nil, err
	}
	needle := foldRunes([]rune(query.Search))
	limit := min(query.LastN, SessionMessagesMaxHits)
	hits := make([]SessionMessageHit, 0, limit)
	result := &SessionMessagesResult{SessionID: sessionID, Source: "transcript", Mode: "search", Search: query.Search}
	for i := len(selected) - 1; i >= 0; i-- {
		message := selected[i]
		offsets := runeMatches(foldRunes(message.text), needle)
		if len(offsets) == 0 {
			continue
		}
		if len(hits) == limit {
			result.HasMore = true
			break
		}
		hits = append(hits, SessionMessageHit{
			ID: message.id, Role: message.role, At: message.at, Chars: len(message.text),
			Matches: len(offsets), Offset: offsets[0], Snippet: sessionMessageSnippet(message.text, offsets[0], len(needle)),
		})
	}
	// Newest-first scan, chronological answer (like the list mode).
	for left, right := 0, len(hits)-1; left < right; left, right = left+1, right-1 {
		hits[left], hits[right] = hits[right], hits[left]
	}
	result.Hits = hits
	if result.HasMore {
		result.NextBeforeID = hits[0].ID
	}
	return result, nil
}

func expandSessionMessage(sessionID string, messages []sessionTranscriptMessage, query SessionMessagesQuery) (*SessionMessagesResult, error) {
	index, err := findSessionMessage(messages, query.Expand)
	if err != nil {
		return nil, err
	}
	message := messages[index]
	if query.Offset >= len(message.text) && query.Offset > 0 {
		return nil, validationError("session_messages_invalid", "offset is past the end of the message (chars "+strconv.Itoa(len(message.text))+")")
	}
	end := min(len(message.text), query.Offset+SessionMessagesExpandChunk)
	chunk := &SessionMessageChunk{
		ID: message.id, Role: message.role, Model: message.model, At: message.at,
		Chars: len(message.text), Offset: query.Offset, Text: string(message.text[query.Offset:end]),
	}
	if end < len(message.text) {
		chunk.NextOffset = &end
	}
	return &SessionMessagesResult{SessionID: sessionID, Source: "transcript", Mode: "expand", Message: chunk, HasMore: chunk.NextOffset != nil}, nil
}

func truncateRunes(text []rune, maximum int) (string, bool) {
	if len(text) <= maximum {
		return string(text), false
	}
	return strings.TrimRightFunc(string(text[:maximum]), unicode.IsSpace) + "…", true
}

// sessionMessageSnippet keeps a short context around one match, flattened to
// a single line and marked with … where it was cut.
func sessionMessageSnippet(text []rune, offset, length int) string {
	start := max(0, offset-SessionMessagesSnippetContext)
	end := min(len(text), offset+length+SessionMessagesSnippetContext)
	snippet := strings.Join(strings.Fields(string(text[start:end])), " ")
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(text) {
		snippet += "…"
	}
	return snippet
}

// runeMatches returns the rune offsets of every non-overlapping occurrence.
func runeMatches(haystack, needle []rune) []int {
	if len(needle) == 0 {
		return nil
	}
	var offsets []int
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i] == needle[0] && string(haystack[i:i+len(needle)]) == string(needle) {
			offsets = append(offsets, i)
			i += len(needle) - 1
		}
	}
	return offsets
}

// foldRunes lowercases and strips common Latin diacritics rune by rune, so
// offsets in the folded text are offsets in the original.
func foldRunes(text []rune) []rune {
	folded := make([]rune, len(text))
	for i, r := range text {
		r = unicode.ToLower(r)
		if base, ok := latinBaseLetters[r]; ok {
			r = base
		}
		folded[i] = r
	}
	return folded
}

var latinBaseLetters = func() map[rune]rune {
	table := map[rune]rune{}
	for base, variants := range map[rune]string{
		'a': "áàâãäåā", 'e': "éèêëē", 'i': "íìîïī", 'o': "óòôõöøō", 'u': "úùûüū",
		'c': "ç", 'n': "ñ", 'y': "ýÿ",
	} {
		for _, variant := range variants {
			table[variant] = base
		}
	}
	return table
}()
