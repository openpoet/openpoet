package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
)

type fakeTranscripts struct {
	events []*jsonlview.SessionEvent
	reason string
	err    error
}

func (f fakeTranscripts) SessionTranscript(context.Context, string) ([]*jsonlview.SessionEvent, string, error) {
	return f.events, f.reason, f.err
}

var transcriptStart = time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)

func transcriptEvent(n int, kind string, blocks ...jsonlview.ContentBlock) *jsonlview.SessionEvent {
	return &jsonlview.SessionEvent{
		Type: kind, UUID: fmt.Sprintf("%08x-1111-2222-3333-444444444444", n),
		Timestamp: transcriptStart.Add(time.Duration(n) * time.Minute),
		Message:   &jsonlview.EventMessage{Role: kind, Model: map[string]string{"assistant": "claude-opus-5-5"}[kind], ContentBlocks: blocks},
	}
}

func textBlock(text string) jsonlview.ContentBlock {
	return jsonlview.ContentBlock{Type: "text", Text: text}
}

type messageStore struct{ session *database.Session }

func (s messageStore) GetSession(context.Context, string) (*database.Session, error) {
	return s.session, nil
}

func messagesExecutor(transcripts SessionTranscriptPort, session *database.Session) *SessionMessageService {
	if transcripts == nil {
		return NewSessionMessageService(messageStore{session: session}, nil)
	}
	return NewSessionMessageService(messageStore{session: session}, transcripts)
}

// runMessages reads with a JSON-shaped query, like the transports send it.
func runMessages(t *testing.T, service *SessionMessageService, payload string) (*SessionMessagesResult, error) {
	t.Helper()
	var fields struct {
		Role     string `json:"role"`
		BeforeID string `json:"before_id"`
		Search   string `json:"search"`
		Expand   string `json:"expand"`
		LastN    int    `json:"last_n"`
		Offset   int    `json:"offset"`
		MaxChars int    `json:"max_chars"`
		Unknown  *int   `json:"unknown"`
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if fields.Unknown != nil {
		return nil, validationError("session_messages_invalid", "unknown field")
	}
	return service.Read(context.Background(), SessionMessagesQuery{
		SessionID: "s1", Role: fields.Role, LastN: fields.LastN, BeforeID: fields.BeforeID,
		Search: fields.Search, Expand: fields.Expand, Offset: fields.Offset, MaxChars: fields.MaxChars,
	})
}

func TestSessionMessagesKeepsOnlyTheCleanConversation(t *testing.T) {
	meta := transcriptEvent(3, "user", textBlock("Base directory for this skill: /x"))
	meta.IsMeta = true
	sidechain := transcriptEvent(4, "assistant", textBlock("subagent chatter"))
	sidechain.IsSidechain = true
	events := []*jsonlview.SessionEvent{
		transcriptEvent(1, "user", textBlock("Faça o deploy\n<system-reminder>ignore me</system-reminder>")),
		transcriptEvent(2, "assistant", jsonlview.ContentBlock{Type: "thinking", Text: "hmm"}, textBlock("Vou olhar."),
			jsonlview.ContentBlock{Type: "tool_use", ToolName: "Bash", ToolInput: map[string]any{"command": "ls"}}),
		meta, sidechain,
		transcriptEvent(5, "user", jsonlview.ContentBlock{Type: "tool_result", Content: "file list"}),
		transcriptEvent(6, "assistant", jsonlview.ContentBlock{Type: "tool_use", ToolName: "Read"}),
		transcriptEvent(7, "user", textBlock("<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>")),
		transcriptEvent(8, "user", textBlock("<local-command-stdout>Set model to opus</local-command-stdout>")),
		transcriptEvent(9, "assistant", textBlock("A chave é API_KEY=sk-abcdefghijklmnopqrstuv")),
		{Type: "assistant", UUID: "0000000a", Message: &jsonlview.EventMessage{Model: "<synthetic>", ContentBlocks: []jsonlview.ContentBlock{textBlock("No response requested.")}}},
		nil,
	}
	result, err := runMessages(t, messagesExecutor(fakeTranscripts{events: events}, &database.Session{ID: "s1", ProjectID: 1}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	view := *result
	var got []string
	for _, message := range view.Messages {
		got = append(got, message.Role+": "+message.Text)
	}
	want := []string{"user: Faça o deploy", "assistant: Vou olhar.", "user: /model opus", "assistant: A chave é [REDACTED]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	if view.Total != 4 || view.HasMore || view.Source != "transcript" {
		t.Fatalf("view = %+v", view)
	}
	first := view.Messages[0]
	if first.ID != "00000001" || !first.At.Equal(transcriptStart.Add(time.Minute)) || first.Chars != len([]rune("Faça o deploy")) || first.Truncated {
		t.Fatalf("first message = %+v", first)
	}
	if view.Messages[1].Model != "claude-opus-5-5" || view.Messages[0].Model != "" {
		t.Fatalf("models = %q %q", view.Messages[0].Model, view.Messages[1].Model)
	}
}

func conversation(n int) []*jsonlview.SessionEvent {
	events := make([]*jsonlview.SessionEvent, 0, n)
	for i := 1; i <= n; i++ {
		kind := "user"
		if i%2 == 0 {
			kind = "assistant"
		}
		events = append(events, transcriptEvent(i, kind, textBlock(fmt.Sprintf("message %d", i))))
	}
	return events
}

func TestSessionMessagesPagesBackwardsWithCursorAndRole(t *testing.T) {
	executor := messagesExecutor(fakeTranscripts{events: conversation(25)}, &database.Session{ID: "s1"})
	result, err := runMessages(t, executor, `{"last_n":10}`)
	if err != nil {
		t.Fatal(err)
	}
	page := *result
	if len(page.Messages) != 10 || page.Messages[0].Text != "message 16" || page.Messages[9].Text != "message 25" || !page.HasMore || page.NextBeforeID != "00000010" {
		t.Fatalf("first page = %+v", page)
	}
	result, err = runMessages(t, executor, `{"last_n":20,"before_id":"`+page.NextBeforeID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	page = *result
	if len(page.Messages) != 15 || page.Messages[0].Text != "message 1" || page.Messages[14].Text != "message 15" || page.HasMore || page.NextBeforeID != "" {
		t.Fatalf("second page = %+v", page)
	}

	result, err = runMessages(t, executor, `{"role":"assistant","last_n":3,"before_id":"0000000b"}`)
	if err != nil {
		t.Fatal(err)
	}
	page = *result
	if page.Total != 12 || len(page.Messages) != 3 || page.Messages[0].Text != "message 6" || page.Messages[2].Text != "message 10" || page.NextBeforeID != "00000006" {
		t.Fatalf("assistant page = %+v", page)
	}
	if _, err := runMessages(t, executor, `{"before_id":"deadbeef"}`); !hasFailureCode(err, "session_message_not_found") {
		t.Fatalf("unknown cursor err = %v", err)
	}
}

func TestSessionMessagesTruncatesToMaxChars(t *testing.T) {
	long := strings.Repeat("palavra ", 100)
	executor := messagesExecutor(fakeTranscripts{events: []*jsonlview.SessionEvent{transcriptEvent(1, "assistant", textBlock(long))}}, &database.Session{ID: "s1"})
	result, err := runMessages(t, executor, `{"max_chars":80}`)
	if err != nil {
		t.Fatal(err)
	}
	message := result.Messages[0]
	if !message.Truncated || !strings.HasSuffix(message.Text, "…") || len([]rune(message.Text)) > 81 || message.Chars != len([]rune(strings.TrimSpace(long))) {
		t.Fatalf("truncated message = %+v", message)
	}
}

func TestSessionMessagesSearchReturnsSnippetsAccentInsensitive(t *testing.T) {
	events := conversation(30)
	events[4] = transcriptEvent(5, "user", textBlock(strings.Repeat("x", 300)+" a Configuração do deploy "+strings.Repeat("y", 300)))
	for i := 10; i < 30; i++ {
		events[i] = transcriptEvent(i+1, events[i].Type, textBlock(fmt.Sprintf("nota %d sobre configuracao e CONFIGURAÇÃO", i+1)))
	}
	executor := messagesExecutor(fakeTranscripts{events: events}, &database.Session{ID: "s1"})
	result, err := runMessages(t, executor, `{"search":"configuração","last_n":20}`)
	if err != nil {
		t.Fatal(err)
	}
	view := *result
	if len(view.Hits) != 10 || !view.HasMore || view.Hits[0].ID != "00000015" || view.Hits[9].ID != "0000001e" || view.NextBeforeID != "00000015" {
		t.Fatalf("first search page = %+v", view)
	}
	if view.Hits[9].Matches != 2 || view.Hits[9].Snippet != "nota 30 sobre configuracao e CONFIGURAÇÃO" {
		t.Fatalf("hit = %+v", view.Hits[9])
	}

	result, err = runMessages(t, executor, `{"search":"CONFIGURACAO","role":"user","before_id":"0000000b"}`)
	if err != nil {
		t.Fatal(err)
	}
	view = *result
	if len(view.Hits) != 1 || view.HasMore {
		t.Fatalf("older search page = %+v", view)
	}
	hit := view.Hits[0]
	if hit.ID != "00000005" || hit.Offset != 303 || !strings.HasPrefix(hit.Snippet, "…") || !strings.HasSuffix(hit.Snippet, "…") ||
		!strings.Contains(hit.Snippet, "a Configuração do deploy") || len([]rune(hit.Snippet)) > 2*SessionMessagesSnippetContext+len("configuração")+2 {
		t.Fatalf("snippet hit = %+v", hit)
	}
}

func TestSessionMessagesExpandReadsWholeMessageInChunks(t *testing.T) {
	body := strings.Repeat("á", 8000) + strings.Repeat("b", 8000) + "fim"
	executor := messagesExecutor(fakeTranscripts{events: []*jsonlview.SessionEvent{
		transcriptEvent(1, "user", textBlock("oi")), transcriptEvent(2, "assistant", textBlock(body)),
	}}, &database.Session{ID: "s1"})
	var chunks []string
	offset := 0
	for {
		result, err := runMessages(t, executor, fmt.Sprintf(`{"expand":"0000-0002","offset":%d}`, offset))
		if err != nil {
			t.Fatal(err)
		}
		chunk := result.Message
		if chunk.ID != "00000002" || chunk.Chars != 16003 || chunk.Offset != offset || chunk.Role != "assistant" {
			t.Fatalf("chunk = %+v", chunk)
		}
		chunks = append(chunks, chunk.Text)
		if chunk.NextOffset == nil {
			break
		}
		offset = *chunk.NextOffset
	}
	if len(chunks) != 3 || strings.Join(chunks, "") != body || chunks[2] != "fim" {
		t.Fatalf("chunks = %d, rebuilt ok = %v", len(chunks), strings.Join(chunks, "") == body)
	}
	if _, err := runMessages(t, executor, `{"expand":"00000002","offset":16003}`); !hasFailureCode(err, "session_messages_invalid") {
		t.Fatalf("offset past end err = %v", err)
	}
}

func TestSessionMessagesIDsGrowUntilUnique(t *testing.T) {
	a := transcriptEvent(1, "user", textBlock("a"))
	b := transcriptEvent(2, "assistant", textBlock("b"))
	a.UUID, b.UUID = "abcdef01-aaaa-0000-0000-000000000000", "abcdef01-bbbb-0000-0000-000000000000"
	executor := messagesExecutor(fakeTranscripts{events: []*jsonlview.SessionEvent{a, b}}, &database.Session{ID: "s1"})
	result, err := runMessages(t, executor, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	messages := result.Messages
	if messages[0].ID != "abcdef01aaaa0000" || messages[1].ID != "abcdef01bbbb0000" {
		t.Fatalf("ids = %q %q", messages[0].ID, messages[1].ID)
	}
	if _, err := runMessages(t, executor, `{"expand":"abcdef01"}`); !hasFailureCode(err, "session_message_ambiguous") {
		t.Fatalf("ambiguous prefix err = %v", err)
	}
}

func TestSessionMessagesFailures(t *testing.T) {
	session := &database.Session{ID: "s1", ProjectID: 12, Backend: "codex"}
	if _, err := runMessages(t, messagesExecutor(fakeTranscripts{reason: "unsupported_backend"}, session), `{}`); !hasFailureCode(err, "session_transcript_unavailable") {
		t.Fatalf("unsupported backend err = %v", err)
	}
	if _, err := runMessages(t, messagesExecutor(fakeTranscripts{err: errors.New("sftp down")}, session), `{}`); !hasFailureCode(err, "session_transcript_unavailable") {
		t.Fatalf("read failure err = %v", err)
	}
	if _, err := runMessages(t, messagesExecutor(nil, session), `{}`); err == nil {
		t.Fatal("missing transcript port was accepted")
	}
	scoped := messagesExecutor(fakeTranscripts{events: conversation(2)}, session)
	if _, err := scoped.Read(context.Background(), SessionMessagesQuery{SessionID: "s1", ProjectAllowed: func(id int64) bool { return id == 47 }}); !hasFailureCode(err, "session_not_found") {
		t.Fatalf("out-of-scope err = %v", err)
	}
	result, err := runMessages(t, messagesExecutor(fakeTranscripts{}, session), `{}`)
	if err != nil || len(result.Messages) != 0 {
		t.Fatalf("empty transcript = %+v, %v", result, err)
	}
	for _, payload := range []string{
		`{"role":"system"}`, `{"last_n":21}`, `{"last_n":-1}`, `{"max_chars":79}`, `{"max_chars":1501}`,
		`{"search":"a","expand":"00000001"}`, `{"offset":10}`, `{"expand":"x","offset":-1}`,
		`{"search":"` + strings.Repeat("a", 201) + `"}`, `{"unknown":1}`,
	} {
		if _, err := runMessages(t, scoped, payload); !hasFailureCode(err, "session_messages_invalid") {
			t.Fatalf("payload %s err = %v", payload, err)
		}
	}
}

func hasFailureCode(err error, code string) bool {
	return ErrorCode(err) == code
}
