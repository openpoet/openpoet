package handlers

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"

	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/session"
)

// codexTranscriptReadLimit bounds the rows read for one codex transcript. Each
// streamed delta is a row, and the retention job keeps at most
// DefaultCodexTranscriptMaxEvents per session; a running session may briefly
// hold more until the next cleanup.
const codexTranscriptReadLimit = 2 * database.DefaultCodexTranscriptMaxEvents

// readCodexTranscript builds a session's conversation from the transcript the
// Codex app-server runner persists (codex_transcript_events). Codex writes no
// Claude-style JSONL, so this table is its structured transcript. The TUI
// runtime records nothing there: reason is unsupported_backend for it.
func (h *StructuredViewHandler) readCodexTranscript(ctx context.Context, sess *database.Session) ([]*jsonlview.SessionEvent, string, error) {
	if strings.EqualFold(strings.TrimSpace(sess.Harness), "codex/tui") {
		return nil, "unsupported_backend", nil
	}
	rows, err := h.db.ListCodexTranscriptEvents(ctx, sess.ID, codexTranscriptReadLimit)
	if err != nil {
		return nil, "", err
	}
	return codexTranscriptSessionEvents(sess.ID, rows), "", nil
}

// codexTranscriptSessionEvents merges streamed chunks (same event id, append
// set) into one message per event and keeps the user and assistant messages.
// Commands, reasoning, plans and statuses are not conversation text.
func codexTranscriptSessionEvents(sessionID string, rows []database.CodexTranscriptEvent) []*jsonlview.SessionEvent {
	type merged struct {
		kind string
		text strings.Builder
		row  database.CodexTranscriptEvent
	}
	ids := make([]int, len(rows))
	appends := make([]bool, len(rows))
	for i, row := range rows {
		ids[i], appends[i] = row.EventID, row.Append
	}
	// A runner that started without the saved history numbered from 1 again;
	// those events must not merge into the older ones with the same number.
	ids = session.CodexTranscriptSequenceIDs(ids, appends)
	order := make([]*merged, 0, len(rows))
	byID := make(map[int]*merged, len(rows))
	for i, row := range rows {
		row.EventID = ids[i]
		item, ok := byID[row.EventID]
		if !ok {
			item = &merged{kind: row.Kind, row: row}
			byID[row.EventID] = item
			order = append(order, item)
		} else if !row.Append {
			item.text.Reset()
		}
		if row.Kind != "" {
			item.kind = row.Kind
		}
		item.text.WriteString(row.Text)
	}
	events := make([]*jsonlview.SessionEvent, 0, len(order))
	for _, item := range order {
		if item.kind != "user" && item.kind != "assistant" {
			continue
		}
		text := item.text.String()
		if strings.TrimSpace(text) == "" {
			continue
		}
		events = append(events, &jsonlview.SessionEvent{
			Type:      item.kind,
			UUID:      codexTranscriptMessageUUID(sessionID, item.row.EventID),
			Timestamp: item.row.CreatedAt,
			SessionID: sessionID,
			Message: &jsonlview.EventMessage{
				Role:          item.kind,
				ContentBlocks: []jsonlview.ContentBlock{{Type: "text", Text: text}},
			},
		})
	}
	return events
}

// codexTranscriptMessageUUID derives a stable message id from the session and
// the transcript event id, so message ids survive re-reads.
func codexTranscriptMessageUUID(sessionID string, eventID int) string {
	sum := sha1.Sum([]byte(sessionID + ":" + strconv.Itoa(eventID)))
	return hex.EncodeToString(sum[:16])
}
