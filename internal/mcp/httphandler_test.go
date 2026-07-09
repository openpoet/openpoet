package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"openpoet/internal/database"
)

func TestHTTPHandlerPersistsAndRestoresSession(t *testing.T) {
	db := setupMCPHTTPTestDB(t)
	ctx := context.Background()

	h := NewHTTPHandler("http://127.0.0.1:1", func() string { return "" }, func() ToolPolicy {
		return ToolPolicy{Mode: "allow_all"}
	}, db)

	initReq := newMCPHTTPTestRequest(http.MethodPost, "/mcp?session_id=openpoet-session-1", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	initRec := httptest.NewRecorder()
	h.ServeHTTP(initRec, initReq)

	if initRec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d body=%s", initRec.Code, initRec.Body.String())
	}
	mcpSessionID := initRec.Header().Get("Mcp-Session-Id")
	if mcpSessionID == "" {
		t.Fatal("initialize did not return Mcp-Session-Id")
	}

	persisted, err := db.GetMCPHTTPSession(ctx, mcpSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "active" {
		t.Fatalf("persisted status = %q, want active", persisted.Status)
	}
	if persisted.OpenPoetSessionID != "openpoet-session-1" {
		t.Fatalf("openpoet_session_id = %q", persisted.OpenPoetSessionID)
	}
	if persisted.Context != "session" {
		t.Fatalf("context = %q, want session", persisted.Context)
	}
	if persisted.RequestCount != 1 {
		t.Fatalf("request_count after initialize = %d, want 1", persisted.RequestCount)
	}

	// Simulate an OpenPoet process restart: a new handler has no in-memory MCP sessions.
	restoredHandler := NewHTTPHandler("http://127.0.0.1:1", func() string { return "" }, func() ToolPolicy {
		return ToolPolicy{Mode: "allow_all"}
	}, db)

	callReq := newMCPHTTPTestRequest(http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"unknown/method"}`)
	callReq.Header.Set("Mcp-Session-Id", mcpSessionID)
	callRec := httptest.NewRecorder()
	restoredHandler.ServeHTTP(callRec, callReq)

	if callRec.Code != http.StatusOK {
		t.Fatalf("restored request status = %d body=%s", callRec.Code, callRec.Body.String())
	}
	if got := callRec.Header().Get("Mcp-Session-Id"); got != mcpSessionID {
		t.Fatalf("restored response Mcp-Session-Id = %q, want %q", got, mcpSessionID)
	}

	persisted, err = db.GetMCPHTTPSession(ctx, mcpSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "active" {
		t.Fatalf("status after restored request = %q, want active", persisted.Status)
	}
	if persisted.RequestCount != 2 {
		t.Fatalf("request_count after restored request = %d, want 2", persisted.RequestCount)
	}
	if persisted.LastMethod != "unknown/method" {
		t.Fatalf("last_method = %q, want unknown/method", persisted.LastMethod)
	}

	events, err := db.ListMCPHTTPSessionEvents(ctx, mcpSessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMCPHTTPEvent(events, "restore", "ok") {
		t.Fatalf("missing restore event in %#v", events)
	}
	if !hasMCPHTTPEvent(events, "request", "error") {
		t.Fatalf("missing error request event in %#v", events)
	}

	deleteReq := newMCPHTTPTestRequest(http.MethodDelete, "/mcp", "")
	deleteReq.Header.Set("Mcp-Session-Id", mcpSessionID)
	deleteRec := httptest.NewRecorder()
	restoredHandler.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	persisted, err = db.GetMCPHTTPSession(ctx, mcpSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "closed" {
		t.Fatalf("status after delete = %q, want closed", persisted.Status)
	}
	if !persisted.ClosedAt.Valid {
		t.Fatal("closed_at was not set")
	}
}

func setupMCPHTTPTestDB(t *testing.T) *database.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "openpoet-mcp-http-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	db, err := database.New(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newMCPHTTPTestRequest(method, target, body string) *http.Request {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, target, reader)
	req.RemoteAddr = "127.0.0.1:1234"
	return req
}

func hasMCPHTTPEvent(events []database.MCPHTTPSessionEvent, eventType, status string) bool {
	for _, event := range events {
		if event.EventType == eventType && event.Status == status {
			return true
		}
	}
	return false
}
