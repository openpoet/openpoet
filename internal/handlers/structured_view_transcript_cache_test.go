package handlers

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/session"
)

func newRemoteTranscriptTest(t *testing.T) (*database.DB, *database.Session) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "openpoet-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project := &database.Project{
		Name: "remote-transcript-test", Path: "/Users/someone/project", Type: "remote",
		Backend: string(session.BackendClaudeCode), BackendConfig: "{}",
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	sess := &database.Session{
		ID: "remote-session", ProjectID: project.ID, Status: "running", StartTime: time.Now(),
		Backend: string(session.BackendClaudeCode), Model: "default", Effort: "default", Harness: "claude_code",
	}
	if err := db.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return db, sess
}

func TestEndedRemoteTranscriptIsServedFromCache(t *testing.T) {
	db, sess := newRemoteTranscriptTest(t)
	h := NewStructuredViewHandler(db, nil, nil)
	ctx := context.Background()

	source, reason := h.resolveJSONLSourceContext(ctx, sess.ID)
	if reason != "" || !source.isRemote || source.endedKey != "" {
		t.Fatalf("running remote session: source=%+v reason=%q (a live transcript must never be cached)", source, reason)
	}

	if err := db.EndSession(ctx, sess.ID, "error"); err != nil {
		t.Fatal(err)
	}
	source, _ = h.resolveJSONLSourceContext(ctx, sess.ID)
	if source.endedKey == "" {
		t.Fatal("ended remote session has no cache key")
	}
	cached := []*jsonlview.SessionEvent{{Type: "user", UUID: "u1"}}
	h.cacheEndedTranscript(source.endedKey, cached)

	// The project has no reachable host: only the cache can answer.
	events, reason, err := h.ReadSessionTranscript(ctx, sess.ID)
	if err != nil || reason != "" || len(events) != 1 || events[0].UUID != "u1" {
		t.Fatalf("cached read events=%v reason=%q err=%v", events, reason, err)
	}
}

func TestEndedTranscriptCacheIsBounded(t *testing.T) {
	h := NewStructuredViewHandler(nil, nil, nil)
	for i := 0; i <= maxEndedTranscripts; i++ {
		h.cacheEndedTranscript(string(rune('a'+i)), []*jsonlview.SessionEvent{})
	}
	if len(h.endedTranscripts) != maxEndedTranscripts {
		t.Fatalf("cache holds %d transcripts", len(h.endedTranscripts))
	}
	if _, ok := h.cachedEndedTranscript("a"); ok {
		t.Fatal("oldest transcript was not evicted")
	}
	if _, ok := h.cachedEndedTranscript(string(rune('a' + maxEndedTranscripts))); !ok {
		t.Fatal("newest transcript missing")
	}
	h.cacheEndedTranscript("", []*jsonlview.SessionEvent{})
	if _, ok := h.cachedEndedTranscript(""); ok {
		t.Fatal("a session without an ended key was cached")
	}
}
