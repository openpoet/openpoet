package automation

import (
	"context"

	"openpoet/internal/application"
)

const sessionMessagesNotes = "Reads the session's conversation as clean text from its structured transcript (never the terminal screen): " +
	"user prompts and assistant replies, without tool calls, tool results, thinking, system reminders or harness-injected lines; slash commands appear as /name args. " +
	"The result has mode list, search or expand. list (default): messages[] = the last_n most recent in chronological order, each {id, role, model, at, chars, text, truncated} with text cut to max_chars; " +
	"page backwards with before_id = next_before_id while has_more. " +
	"search: hits[] = up to 10 most recent messages containing the text (case- and accent-insensitive), each {id, role, at, chars, matches, offset, snippet}; before_id pages further back. " +
	"expand: message = one message in full, in 8000-character chunks {id, role, at, chars, offset, text, next_offset}; pass offset = next_offset for the next chunk (a search hit's offset shows where the match is). " +
	"Ids are short prefixes of the transcript entry uuid; any unique prefix is accepted. Secrets are redacted. " +
	"Codex sessions (app-server runtime) are read from the transcript OpenPoet records for them. " +
	"Fails with session_transcript_unavailable for backends without a structured transcript (copilot, opencode, codex on the tui runtime). " +
	"A session on a remote (SSH) project is read from its host: allow up to 20 s, after which it fails with session_transcript_timeout (host slow or unreachable; try later); an ended remote session is read once and then served from memory. " +
	"Each read needs its own fresh idempotency_key: resending a key replays its first answer, or idempotency_in_progress while that read still runs. " +
	"Same service as the MCP tool openpoet_session_messages and GET /api/sessions/{id}/messages."

type sessionMessagesPayload struct {
	Role     string `json:"role,omitempty" doc:"user or assistant (default: both)"`
	LastN    int    `json:"last_n,omitempty" doc:"messages per page, 1-20 (default 10); search returns at most 10"`
	BeforeID string `json:"before_id,omitempty" doc:"cursor: only messages older than this id (next_before_id of the previous page)"`
	Search   string `json:"search,omitempty" doc:"text to find (case- and accent-insensitive, at most 200 characters); returns snippets"`
	Expand   string `json:"expand,omitempty" doc:"id of one message to read in full, in 8000-character chunks"`
	Offset   int    `json:"offset,omitempty" doc:"expand only: character offset of the chunk (next_offset of the previous one)"`
	MaxChars int    `json:"max_chars,omitempty" doc:"list only: characters kept per message, 80-1500 (default 500)"`
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
	query := application.SessionMessagesQuery{
		SessionID: sessionID, Role: payload.Role, LastN: payload.LastN, BeforeID: payload.BeforeID,
		Search: payload.Search, Expand: payload.Expand, Offset: payload.Offset, MaxChars: payload.MaxChars,
	}
	if err := query.Normalize(); err != nil {
		return nil, platformFailure("platform_payload_invalid", err.Error(), false)
	}
	scope := input.ProjectScope
	query.ProjectAllowed = scope.Allows
	return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{
		"session_id": sessionID, "mode": query.Mode(), "role": query.Role, "last_n": query.LastN, "has_cursor": query.BeforeID != "",
	}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
		if e.messages == nil {
			return nil, platformFailure("platform_service_unavailable", "session transcripts are unavailable", true)
		}
		return e.messages.Read(ctx, query)
	}}, nil
}
