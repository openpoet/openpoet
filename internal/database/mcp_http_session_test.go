package database

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCleanupMCPHTTPSessionsBoundsStoredHistory(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if err := db.UpsertMCPHTTPSession(ctx, &MCPHTTPSession{
		ID:                "mcp-active",
		OpenPoetSessionID: "openpoet-1",
		Context:           "session",
		Status:            "active",
		LastMethod:        "initialize",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMCPHTTPSession(ctx, &MCPHTTPSession{
		ID:                "mcp-closed-old",
		OpenPoetSessionID: "openpoet-2",
		Context:           "session",
		Status:            "closed",
		LastMethod:        "DELETE",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseMCPHTTPSession(ctx, "mcp-closed-old", "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE mcp_http_sessions SET closed_at = ?, last_used_at = ? WHERE id = ?",
		time.Now().Add(-10*24*time.Hour), time.Now().Add(-10*24*time.Hour), "mcp-closed-old"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if err := db.CreateMCPHTTPSessionEvent(ctx, &MCPHTTPSessionEvent{
			MCPSessionID: "mcp-active",
			Method:       fmt.Sprintf("method-%d", i),
			EventType:    "request",
			Status:       "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE mcp_http_session_events SET created_at = ? WHERE mcp_session_id = ? AND method = ?",
		time.Now().Add(-20*24*time.Hour), "mcp-active", "method-0"); err != nil {
		t.Fatal(err)
	}

	stats, err := db.CleanupMCPHTTPSessions(
		ctx,
		time.Now().Add(-7*24*time.Hour),
		time.Now().Add(-14*24*time.Hour),
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsDeleted != 1 {
		t.Fatalf("sessions deleted = %d, want 1", stats.SessionsDeleted)
	}
	if stats.ExpiredDeleted != 1 {
		t.Fatalf("expired events deleted = %d, want 1", stats.ExpiredDeleted)
	}
	if stats.OverflowDeleted != 2 {
		t.Fatalf("overflow events deleted = %d, want 2", stats.OverflowDeleted)
	}

	var sessionCount int
	if err := db.GetContext(ctx, &sessionCount, "SELECT COUNT(*) FROM mcp_http_sessions"); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 {
		t.Fatalf("session count = %d, want 1", sessionCount)
	}

	events, err := db.ListMCPHTTPSessionEvents(ctx, "mcp-active", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("remaining events = %d, want 2", len(events))
	}
}
