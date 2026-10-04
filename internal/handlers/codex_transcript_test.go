package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/session"
	"openpoet/internal/websocket"
)

func newCodexTranscriptTest(t *testing.T, harness string) (*database.DB, *database.Session) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "openpoet-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project := &database.Project{
		Name: "codex-transcript-test", Path: "/home/dev/project", Type: "remote",
		Backend: string(session.BackendCodex), BackendConfig: `{"runtime":"app-server"}`,
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	sess := &database.Session{
		ID: "codex-session", ProjectID: project.ID, Status: "running", StartTime: time.Now(),
		Backend: string(session.BackendCodex), Model: "default", Effort: "default", Harness: harness,
	}
	if err := db.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return db, sess
}

// TestCodexSessionMessagesReadPersistedTranscript: sessions.messages serves a
// codex app-server session from codex_transcript_events — streamed chunks
// merged into whole messages, commands and reasoning left out.
func TestCodexSessionMessagesReadPersistedTranscript(t *testing.T) {
	db, sess := newCodexTranscriptTest(t, "codex/app-server")
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 8, 43, 0, 0, time.UTC)
	rows := []database.CodexTranscriptEvent{
		{EventID: 1, Kind: "user", Text: "Liste a raiz, somente leitura."},
		{EventID: 2, Kind: "assistant", Text: "Vou"},
		{EventID: 2, Kind: "assistant", Text: " listar.", Append: true},
		{EventID: 3, Kind: "command", Command: "ls", Status: "running"},
		{EventID: 3, Kind: "command", Text: "README.md\n", Append: true},
		{EventID: 4, Kind: "reasoning", Text: "thinking"},
		{EventID: 5, Kind: "assistant", Text: "A raiz tem "},
		{EventID: 5, Kind: "assistant", Text: "README.md (password=hunter2 here).", Append: true},
	}
	for i := range rows {
		rows[i].SessionID = sess.ID
		rows[i].CreatedAt = at.Add(time.Duration(i) * time.Second)
		if err := db.InsertCodexTranscriptEvent(ctx, &rows[i]); err != nil {
			t.Fatal(err)
		}
	}

	h := NewStructuredViewHandler(db, nil, nil)
	service := application.NewSessionMessageService(db, platformSessionEventReader{handler: h})
	result, err := service.Read(ctx, application.SessionMessagesQuery{SessionID: sess.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || len(result.Messages) != 3 {
		t.Fatalf("messages = %+v, want user + two assistant messages", result.Messages)
	}
	want := []struct{ role, text string }{
		{"user", "Liste a raiz, somente leitura."},
		{"assistant", "Vou listar."},
		{"assistant", "A raiz tem README.md ([REDACTED] here)."},
	}
	for i, message := range result.Messages {
		if message.Role != want[i].role || message.Text != want[i].text {
			t.Fatalf("message %d = %s %q, want %s %q", i, message.Role, message.Text, want[i].role, want[i].text)
		}
	}
	if !result.Messages[0].At.Equal(at) {
		t.Fatalf("user message at %v, want %v", result.Messages[0].At, at)
	}

	again, err := service.Read(ctx, application.SessionMessagesQuery{SessionID: sess.ID, Expand: result.Messages[2].ID})
	if err != nil || again.Message == nil || again.Message.Text != want[2].text {
		t.Fatalf("expand by id: result=%+v err=%v (ids must be stable across reads)", again, err)
	}

	assistant, err := service.Read(ctx, application.SessionMessagesQuery{SessionID: sess.ID, Role: "assistant", LastN: 1})
	if err != nil || len(assistant.Messages) != 1 || assistant.Messages[0].Text != want[2].text {
		t.Fatalf("last assistant message: result=%+v err=%v", assistant, err)
	}
}

func TestCodexTUISessionHasNoStructuredTranscript(t *testing.T) {
	db, sess := newCodexTranscriptTest(t, "codex/tui")
	h := NewStructuredViewHandler(db, nil, nil)
	service := application.NewSessionMessageService(db, platformSessionEventReader{handler: h})
	_, err := service.Read(context.Background(), application.SessionMessagesQuery{SessionID: sess.ID})
	var appErr *application.Error
	if !asApplicationError(err, &appErr) || appErr.Code != "session_transcript_unavailable" {
		t.Fatalf("err = %v, want session_transcript_unavailable", err)
	}
}

func asApplicationError(err error, target **application.Error) bool {
	appErr, ok := err.(*application.Error)
	if ok {
		*target = appErr
	}
	return ok
}

// TestCodexParkedQuestionRaisesAttention: a Codex approval parked on the hook
// never reaches the terminal, so the handler itself reports it.
func TestCodexParkedQuestionRaisesAttention(t *testing.T) {
	hub := websocket.NewHub()
	go hub.Run()
	h := NewHookHandler(hub, nil, nil)
	type attention struct{ sessionID, kind, excerpt string }
	got := make(chan attention, 1)
	h.OnAttention = func(sessionID, kind, excerpt string) { got <- attention{sessionID, kind, excerpt} }

	req := httptest.NewRequest(http.MethodPost, "/api/hooks/permission", strings.NewReader(
		`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf build"}}`))
	req.Header.Set("X-Session-ID", "s1")
	req.Header.Set("X-Backend", "codex")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.HandlePermission(rr, req); close(done) }()

	select {
	case a := <-got:
		if a.sessionID != "s1" || a.kind != "tool_permission" || !strings.Contains(a.excerpt, "rm -rf build") {
			t.Fatalf("attention = %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no attention for a parked codex question")
	}
	h.mu.Lock()
	h.pending["s1"].responseCh <- PermissionResponse{Behavior: "deny"}
	h.mu.Unlock()
	<-done
}
