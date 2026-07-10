package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/session"
	"openpoet/internal/websocket"

	"github.com/go-chi/chi/v5"
)

func newSessionRouteRequest(method, path, sessionID, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", sessionID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
}

func waitForSessionOutput(t *testing.T, mgr *session.Manager, sessionID, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := mgr.GetSessionOutput(sessionID)
		if err == nil && strings.Contains(string(out), want) {
			return string(out)
		}
		time.Sleep(25 * time.Millisecond)
	}
	out, _ := mgr.GetSessionOutput(sessionID)
	t.Fatalf("session output did not contain %q; output:\n%s", want, string(out))
	return ""
}

func TestStartTaskSessionAndSendPromptToOpenSession(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "openpoet-test.db")
	db, err := database.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	projectDir := t.TempDir()
	backendScript := filepath.Join(t.TempDir(), "echo-backend.sh")
	if err := os.WriteFile(backendScript, []byte(`#!/bin/sh
printf 'TASK_ENV:%s|%s\n' "$OPENPOET_TASK_ID" "$OPENPOET_TASK_TITLE"
while IFS= read -r line; do
  printf 'PROMPT:%s\n' "$line"
done
`), 0755); err != nil {
		t.Fatal(err)
	}

	proj := &database.Project{
		Name:          "session-api-test",
		Path:          projectDir,
		Type:          "local",
		Backend:       string(session.BackendClaudeCode),
		BackendConfig: "{}",
	}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	task := &database.ProjectTask{
		ProjectID:   proj.ID,
		Title:       "Validar prompt em sessão aberta",
		Description: "Garante start com task e envio de prompt.",
		Status:      "todo",
		Priority:    "high",
		ParentID:    sql.NullInt64{},
	}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	hub := websocket.NewHub()
	mgr := session.NewManager(db, hub, "localhost:0")
	api := NewAPI(db, hub, mgr, nil, nil, nil, nil)
	configureSessionPlatformFixture(t, api, db, mgr, backendScript)

	sess, err := api.startManagedSession(ctx, startSessionInput{
		ProjectID: proj.ID,
		TaskID:    &task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		_ = api.stopManagedSession(stopCtx, sess.ID)
	})

	if sess.Status != "running" {
		t.Fatalf("session status = %q, want running", sess.Status)
	}
	linked, err := db.GetTaskForSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("expected linked task: %v", err)
	}
	if linked.ID != task.ID {
		t.Fatalf("linked task id = %d, want %d", linked.ID, task.ID)
	}
	updatedTask, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedTask.Status != "in_progress" {
		t.Fatalf("task status = %q, want in_progress", updatedTask.Status)
	}

	waitForSessionOutput(t, mgr, sess.ID, "TASK_ENV:")
	output := waitForSessionOutput(t, mgr, sess.ID, task.Title)
	if !strings.Contains(output, "TASK_ENV:"+strconv.FormatInt(task.ID, 10)+"|"+task.Title) {
		t.Fatalf("task env was not passed to backend; output:\n%s", output)
	}

	req := newSessionRouteRequest(http.MethodPost, "/api/sessions/"+sess.ID+"/input", sess.ID, `{"text":"validar envio de prompt"}`)
	rr := httptest.NewRecorder()
	api.SendSessionInput(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("SendSessionInput status = %d body=%s", rr.Code, rr.Body.String())
	}
	waitForSessionOutput(t, mgr, sess.ID, "PROMPT:validar envio de prompt")
}

func TestStartTaskSessionAutoSubmitsDefaultPrompt(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "openpoet-test.db")
	db, err := database.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	projectDir := t.TempDir()
	backendScript := filepath.Join(t.TempDir(), "echo-backend.sh")
	if err := os.WriteFile(backendScript, []byte(`#!/bin/sh
printf 'TASK_ENV:%s|%s\n' "$OPENPOET_TASK_ID" "$OPENPOET_TASK_TITLE"
while IFS= read -r line; do
  printf 'PROMPT:%s\n' "$line"
done
`), 0755); err != nil {
		t.Fatal(err)
	}

	proj := &database.Project{
		Name:          "session-auto-start-test",
		Path:          projectDir,
		Type:          "local",
		Backend:       string(session.BackendClaudeCode),
		BackendConfig: "{}",
	}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	task := &database.ProjectTask{
		ProjectID:   proj.ID,
		Title:       "Iniciar automaticamente",
		Description: "Garante auto-start por ferramenta.",
		Status:      "todo",
		Priority:    "high",
		ParentID:    sql.NullInt64{},
	}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	hub := websocket.NewHub()
	mgr := session.NewManager(db, hub, "localhost:0")
	api := NewAPI(db, hub, mgr, nil, nil, nil, nil)
	configureSessionPlatformFixture(t, api, db, mgr, backendScript)

	sess, err := api.startManagedSession(ctx, startSessionInput{
		ProjectID:           proj.ID,
		TaskID:              &task.ID,
		AutoStartTaskPrompt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		_ = api.stopManagedSession(stopCtx, sess.ID)
	})

	waitForSessionOutput(t, mgr, sess.ID, "PROMPT:"+defaultTaskStartPrompt)
}
