package database

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func TestCleanupCodexTranscriptEventsDeletesExpiredClosedSessionsOnly(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	project := &Project{Name: "codex-cleanup", Path: "/tmp/codex-cleanup", Type: "local", Backend: "codex"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	oldClosed := mustCreateCodexCleanupSession(t, db, project.ID, "old-closed", "stopped")
	legacyClosed := mustCreateCodexCleanupSession(t, db, project.ID, "legacy-closed", "stopped")
	recentClosed := mustCreateCodexCleanupSession(t, db, project.ID, "recent-closed", "stopped")
	active := mustCreateCodexCleanupSession(t, db, project.ID, "active", "running")

	_, err := db.ExecContext(ctx, "UPDATE sessions SET end_time = ? WHERE id = ?", time.Now().Add(-15*24*time.Hour), oldClosed)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "UPDATE sessions SET end_time = ? WHERE id = ?", time.Now().Add(-2*24*time.Hour), recentClosed)
	if err != nil {
		t.Fatal(err)
	}

	for _, sessionID := range []string{oldClosed, legacyClosed, recentClosed, active} {
		for i := 1; i <= 3; i++ {
			if err := db.InsertCodexTranscriptEvent(ctx, &CodexTranscriptEvent{
				SessionID: sessionID,
				EventID:   i,
				Kind:      "assistant",
				Text:      fmt.Sprintf("%s-%d", sessionID, i),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	stats, err := db.CleanupCodexTranscriptEvents(ctx, 14, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ExpiredDeleted != 6 {
		t.Fatalf("ExpiredDeleted = %d, want 6", stats.ExpiredDeleted)
	}
	if stats.OverflowDeleted != 0 {
		t.Fatalf("OverflowDeleted = %d, want 0", stats.OverflowDeleted)
	}

	requireCodexCleanupEventCount(t, db, oldClosed, 0)
	requireCodexCleanupEventCount(t, db, legacyClosed, 0)
	requireCodexCleanupEventCount(t, db, recentClosed, 3)
	requireCodexCleanupEventCount(t, db, active, 3)
}

func TestCleanupCodexTranscriptEventsCapsEventsPerSession(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	project := &Project{Name: "codex-cap", Path: "/tmp/codex-cap", Type: "local", Backend: "codex"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sessionID := mustCreateCodexCleanupSession(t, db, project.ID, "capped", "running")

	for i := 1; i <= 6; i++ {
		if err := db.InsertCodexTranscriptEvent(ctx, &CodexTranscriptEvent{
			SessionID: sessionID,
			EventID:   i,
			Kind:      "assistant",
			Text:      fmt.Sprintf("event-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := db.CleanupCodexTranscriptEvents(ctx, 14, 4)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ExpiredDeleted != 0 {
		t.Fatalf("ExpiredDeleted = %d, want 0", stats.ExpiredDeleted)
	}
	if stats.OverflowDeleted != 2 {
		t.Fatalf("OverflowDeleted = %d, want 2", stats.OverflowDeleted)
	}

	events, err := db.ListCodexTranscriptEvents(ctx, sessionID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4", len(events))
	}
	for i, event := range events {
		wantEventID := i + 3
		if event.EventID != wantEventID {
			t.Fatalf("events[%d].EventID = %d, want %d", i, event.EventID, wantEventID)
		}
	}
}

func mustCreateCodexCleanupSession(t *testing.T, db *DB, projectID int64, id, status string) string {
	t.Helper()
	if err := db.CreateSession(context.Background(), &Session{
		ID:        id,
		ProjectID: projectID,
		Status:    status,
		Name:      id,
		StartTime: time.Now().Add(-16 * 24 * time.Hour),
		Backend:   "codex",
		TaskID:    sql.NullInt64{Valid: false},
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func requireCodexCleanupEventCount(t *testing.T, db *DB, sessionID string, want int) {
	t.Helper()
	var got int
	if err := db.GetContext(context.Background(), &got, "SELECT COUNT(*) FROM codex_transcript_events WHERE session_id = ?", sessionID); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("event count for %s = %d, want %d", sessionID, got, want)
	}
}
