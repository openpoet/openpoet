package automation

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

	"openpoet/internal/application"
	"openpoet/internal/jsonlview"
)

// SessionTranscriptReadPort reads a session's structured transcript (the
// agent's JSONL, never the terminal capture). reason is set (not_found,
// unsupported_backend) when the session has no transcript to read.
type SessionTranscriptReadPort interface {
	SessionTranscript(ctx context.Context, sessionID string) ([]*jsonlview.SessionEvent, string, error)
}

const (
	sessionMessagesDefaultLastN    = 10
	sessionMessagesMaxLastN        = 20
	sessionMessagesMaxHits         = 10
	sessionMessagesDefaultMaxChars = 500
	sessionMessagesMinMaxChars     = 80
	sessionMessagesMaxMaxChars     = 1500
	sessionMessagesSnippetContext  = 100 // characters kept on each side of a search match
	sessionMessagesExpandChunk     = 8000
	sessionMessagesShortIDLen      = 8
	sessionMessagesMaxSearchRunes  = 200
)

const sessionMessagesNotes = "Reads the session's conversation as clean text from its structured transcript (never the terminal screen): " +
	"user prompts and assistant replies, without tool calls, tool results, thinking, system reminders or harness-injected lines; slash commands appear as /name args. " +
	"Default (list): the last_n most recent messages in chronological order, each {id, role, model, at, chars, text, truncated} with text cut to max_chars; " +
	"page backwards with before_id = next_before_id while has_more. " +
	"search: up to 10 most recent messages containing the text (case- and accent-insensitive), each {id, role, at, chars, matches, offset, snippet} with a short context around the first match; before_id pages further back. " +
	"expand: one message in full, in 8000-character chunks {id, role, at, chars, offset, text, next_offset}; pass offset = next_offset for the next chunk (a search hit's offset shows where the match is). " +
	"Ids are short prefixes of the transcript entry uuid; any unique prefix is accepted. Secrets are redacted. " +
	"Fails with session_transcript_unavailable for backends without a structured transcript (copilot, codex, opencode)."

type sessionMessagesPayload struct {
	Role     string `json:"role,omitempty" doc:"user or assistant (default: both)"`
	LastN    int    `json:"last_n,omitempty" doc:"messages per page, 1-20 (default 10); search returns at most 10"`
	BeforeID string `json:"before_id,omitempty" doc:"cursor: only messages older than this id (next_before_id of the previous page)"`
	Search   string `json:"search,omitempty" doc:"text to find (case- and accent-insensitive, at most 200 characters); returns snippets"`
	Expand   string `json:"expand,omitempty" doc:"id of one message to read in full, in 8000-character chunks"`
	Offset   int    `json:"offset,omitempty" doc:"expand only: character offset of the chunk (next_offset of the previous one)"`
	MaxChars int    `json:"max_chars,omitempty" doc:"list only: characters kept per message, 80-1500 (default 500)"`
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

type SessionMessagesListView struct {
	SessionID    string               `json:"session_id"`
	Source       string               `json:"source"` // transcript
	Total        int                  `json:"total"`  // messages of the requested role in the whole transcript
	Messages     []SessionMessageView `json:"messages"`
	HasMore      bool                 `json:"has_more"`
	NextBeforeID string               `json:"next_before_id,omitempty"`
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

type SessionMessagesSearchView struct {
	SessionID    string              `json:"session_id"`
	Source       string              `json:"source"`
	Search       string              `json:"search"`
	Hits         []SessionMessageHit `json:"hits"`
	HasMore      bool                `json:"has_more"`
	NextBeforeID string              `json:"next_before_id,omitempty"`
}

type SessionMessageChunkView struct {
	SessionID  string    `json:"session_id"`
	Source     string    `json:"source"`
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Model      string    `json:"model,omitempty"`
	At         time.Time `json:"at"`
	Chars      int       `json:"chars"`
	Offset     int       `json:"offset"`
	Text       string    `json:"text"`
	NextOffset *int      `json:"next_offset,omitempty"`
}

func (e *sessionPlatformExecutor) validateMessages(input PlatformExecutionInput, target executionCommandTarget) (PlatformValidatedCommand, error) {
	sessionID, err := executionStringID(target, "session id")
	if err != nil {
		return nil, err
	}
	var payload sessionMessagesPayload
	if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
		return nil, err
	}
	if err := normalizeSessionMessagesPayload(&payload); err != nil {
		return nil, err
	}
	mode := "list"
	if payload.Search != "" {
		mode = "search"
	} else if payload.Expand != "" {
		mode = "expand"
	}
	scope := input.ProjectScope
	return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{
		"session_id": sessionID, "mode": mode, "role": payload.Role, "last_n": payload.LastN, "has_cursor": payload.BeforeID != "",
	}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
		if e.transcripts == nil {
			return nil, platformFailure("platform_service_unavailable", "session transcripts are unavailable", true)
		}
		item, err := e.queries.GetSession(ctx, sessionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err != nil || item == nil || !scope.Allows(item.ProjectID) {
			return nil, platformFailure("session_not_found", "session not found", false)
		}
		events, reason, err := e.transcripts.SessionTranscript(ctx, item.ID)
		switch {
		case reason == "not_found":
			return nil, platformFailure("session_not_found", "session not found", false)
		case reason != "":
			return nil, platformFailure("session_transcript_unavailable", "this session has no structured transcript ("+reason+", backend "+item.Backend+")", false)
		case err != nil:
			return nil, platformFailure("session_transcript_unavailable", "the session transcript could not be read", true)
		}
		messages := sessionTranscriptMessages(events)
		switch mode {
		case "search":
			return searchSessionMessages(item.ID, messages, payload)
		case "expand":
			return expandSessionMessage(item.ID, messages, payload)
		default:
			return listSessionMessages(item.ID, messages, payload)
		}
	}}, nil
}

func normalizeSessionMessagesPayload(payload *sessionMessagesPayload) error {
	payload.Role = strings.ToLower(strings.TrimSpace(payload.Role))
	payload.BeforeID = strings.TrimSpace(payload.BeforeID)
	payload.Search = strings.TrimSpace(payload.Search)
	payload.Expand = strings.TrimSpace(payload.Expand)
	switch {
	case payload.Role != "" && payload.Role != "user" && payload.Role != "assistant":
		return platformFailure("platform_payload_invalid", `payload field "role" must be user or assistant`, false)
	case payload.LastN < 0 || payload.LastN > sessionMessagesMaxLastN:
		return platformFailure("platform_payload_invalid", `payload field "last_n" must be between 1 and 20`, false)
	case payload.MaxChars != 0 && (payload.MaxChars < sessionMessagesMinMaxChars || payload.MaxChars > sessionMessagesMaxMaxChars):
		return platformFailure("platform_payload_invalid", `payload field "max_chars" must be between 80 and 1500`, false)
	case len(payload.BeforeID) > maxExecutionIDRunes || len(payload.Expand) > maxExecutionIDRunes:
		return platformFailure("platform_payload_invalid", "message id is too large", false)
	case utf8.RuneCountInString(payload.Search) > sessionMessagesMaxSearchRunes:
		return platformFailure("platform_payload_invalid", `payload field "search" exceeds 200 characters`, false)
	case payload.Search != "" && payload.Expand != "":
		return platformFailure("platform_payload_invalid", "send either search or expand, not both", false)
	case payload.Offset < 0:
		return platformFailure("platform_payload_invalid", `payload field "offset" must not be negative`, false)
	case payload.Offset > 0 && payload.Expand == "":
		return platformFailure("platform_payload_invalid", `payload field "offset" is only used with expand`, false)
	}
	if payload.LastN == 0 {
		payload.LastN = sessionMessagesDefaultLastN
	}
	if payload.MaxChars == 0 {
		payload.MaxChars = sessionMessagesDefaultMaxChars
	}
	return nil
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
		text, _ = boundedExecutionText(text, 0)
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
	text := strings.TrimSpace(transcriptSystemReminderPattern.ReplaceAllString(transcriptBlocksText(blocks, true), ""))
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
				return -1, platformFailure("session_message_ambiguous", "the message id matches more than one message; send a longer prefix", false)
			}
			found = i
		}
	}
	if found < 0 {
		return -1, platformFailure("session_message_not_found", "no message with this id in the session transcript", false)
	}
	return found, nil
}

// sessionMessagesBefore returns the messages of the requested role that are
// older than the before_id cursor (all of them without a cursor).
func sessionMessagesBefore(messages []sessionTranscriptMessage, payload sessionMessagesPayload) ([]sessionTranscriptMessage, int, error) {
	end := len(messages)
	if payload.BeforeID != "" {
		index, err := findSessionMessage(messages, payload.BeforeID)
		if err != nil {
			return nil, 0, err
		}
		end = index
	}
	total := 0
	selected := make([]sessionTranscriptMessage, 0, end)
	for i, message := range messages {
		if payload.Role != "" && message.role != payload.Role {
			continue
		}
		total++
		if i < end {
			selected = append(selected, message)
		}
	}
	return selected, total, nil
}

func listSessionMessages(sessionID string, messages []sessionTranscriptMessage, payload sessionMessagesPayload) (any, error) {
	selected, total, err := sessionMessagesBefore(messages, payload)
	if err != nil {
		return nil, err
	}
	start := max(0, len(selected)-payload.LastN)
	view := SessionMessagesListView{SessionID: sessionID, Source: "transcript", Total: total, Messages: make([]SessionMessageView, 0, len(selected)-start)}
	for _, message := range selected[start:] {
		text, truncated := truncateRunes(message.text, payload.MaxChars)
		view.Messages = append(view.Messages, SessionMessageView{
			ID: message.id, Role: message.role, Model: message.model, At: message.at,
			Chars: len(message.text), Text: text, Truncated: truncated,
		})
	}
	if start > 0 {
		view.HasMore = true
		view.NextBeforeID = selected[start].id
	}
	return view, nil
}

func searchSessionMessages(sessionID string, messages []sessionTranscriptMessage, payload sessionMessagesPayload) (any, error) {
	selected, _, err := sessionMessagesBefore(messages, payload)
	if err != nil {
		return nil, err
	}
	needle := foldRunes([]rune(payload.Search))
	limit := min(payload.LastN, sessionMessagesMaxHits)
	hits := make([]SessionMessageHit, 0, limit)
	view := SessionMessagesSearchView{SessionID: sessionID, Source: "transcript", Search: payload.Search}
	for i := len(selected) - 1; i >= 0; i-- {
		message := selected[i]
		offsets := runeMatches(foldRunes(message.text), needle)
		if len(offsets) == 0 {
			continue
		}
		if len(hits) == limit {
			view.HasMore = true
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
	view.Hits = hits
	if view.HasMore {
		view.NextBeforeID = hits[0].ID
	}
	return view, nil
}

func expandSessionMessage(sessionID string, messages []sessionTranscriptMessage, payload sessionMessagesPayload) (any, error) {
	index, err := findSessionMessage(messages, payload.Expand)
	if err != nil {
		return nil, err
	}
	message := messages[index]
	if payload.Offset >= len(message.text) && payload.Offset > 0 {
		return nil, platformFailure("platform_payload_invalid", "offset is past the end of the message (chars "+strconv.Itoa(len(message.text))+")", false)
	}
	end := min(len(message.text), payload.Offset+sessionMessagesExpandChunk)
	view := SessionMessageChunkView{
		SessionID: sessionID, Source: "transcript", ID: message.id, Role: message.role, Model: message.model,
		At: message.at, Chars: len(message.text), Offset: payload.Offset, Text: string(message.text[payload.Offset:end]),
	}
	if end < len(message.text) {
		view.NextOffset = &end
	}
	return view, nil
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
	start := max(0, offset-sessionMessagesSnippetContext)
	end := min(len(text), offset+length+sessionMessagesSnippetContext)
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
