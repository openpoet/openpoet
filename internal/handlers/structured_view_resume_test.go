package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/session"
	"openpoet/internal/websocket"
)

func newClaudeStructuredViewTest(t *testing.T) (*database.DB, *database.Project, *database.Session, *websocket.Hub) {
	t.Helper()

	db, err := database.New(filepath.Join(t.TempDir(), "openpoet-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	project := &database.Project{
		Name:          "structured-view-resume-test",
		Path:          t.TempDir(),
		Type:          "local",
		Backend:       string(session.BackendClaudeCode),
		BackendConfig: "{}",
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}

	sess := &database.Session{
		ID:        "openpoet-new-session",
		ProjectID: project.ID,
		Status:    "running",
		StartTime: time.Now(),
		Backend:   string(session.BackendClaudeCode),
		Model:     "default",
		Effort:    "default",
		Harness:   "claude_code",
	}
	if err := db.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	hub := websocket.NewHub()
	t.Cleanup(hub.Stop)
	return db, project, sess, hub
}

func TestStructuredViewResolvesResumedClaudeTranscript(t *testing.T) {
	db, project, sess, hub := newClaudeStructuredViewTest(t)
	const resumedClaudeSessionID = "claude-previous-session"
	if err := db.UpdateSessionProviderSessionID(context.Background(), sess.ID, resumedClaudeSessionID); err != nil {
		t.Fatal(err)
	}

	handler := NewStructuredViewHandler(db, hub, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/events", nil)
	source, reason := handler.resolveJSONLSource(req, sess.ID)
	if reason != "" {
		t.Fatalf("resolveJSONLSource reason = %q", reason)
	}

	want := jsonlview.ResolveJSONLPath(project.Path, resumedClaudeSessionID)
	if source.localPath != want {
		t.Fatalf("structured view source = %q, want resumed transcript %q", source.localPath, want)
	}
	if source.sessionID != resumedClaudeSessionID {
		t.Fatalf("source session id = %q, want %q", source.sessionID, resumedClaudeSessionID)
	}
}

func TestClaudeHookPersistsResumedTranscriptID(t *testing.T) {
	db, _, sess, hub := newClaudeStructuredViewTest(t)
	mgr := session.NewManager(db, hub, "localhost:8080")
	handler := NewHookHandler(hub, nil, mgr)
	changed := make(chan string, 1)
	mgr.OnProviderSessionIDChange = func(sessionID, providerSessionID string) {
		if sessionID != sess.ID {
			t.Errorf("source change session id = %q, want %q", sessionID, sess.ID)
		}
		changed <- providerSessionID
	}

	req := httptest.NewRequest(http.MethodPost, "/api/hooks/event", strings.NewReader(`{
		"hook_event_name":"UserPromptSubmit",
		"session_id":"claude-previous-session",
		"prompt":"continue"
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", sess.ID)
	recorder := httptest.NewRecorder()

	handler.HandleEvent(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HandleEvent status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	updated, err := db.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProviderSessionID != "claude-previous-session" {
		t.Fatalf("provider session id = %q, want resumed Claude transcript id", updated.ProviderSessionID)
	}
	select {
	case providerSessionID := <-changed:
		if providerSessionID != "claude-previous-session" {
			t.Fatalf("source change provider id = %q", providerSessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("provider session source change callback was not called")
	}
}
