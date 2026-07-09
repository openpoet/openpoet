package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/websocket"

	"github.com/go-chi/chi/v5"
)

func projectTaskRouteRequest(method, path string, projectID, taskID int64, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", strconv.FormatInt(projectID, 10))
	routeCtx.URLParams.Add("taskId", strconv.FormatInt(taskID, 10))
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
}

func projectTaskHandlerTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "project-task-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func projectTaskHandlerTestProject(t *testing.T, db *database.DB, name string) *database.Project {
	t.Helper()
	project := &database.Project{
		Name: name, Path: "/tmp/" + name, Type: "local", Backend: "claude_code", BackendConfig: "{}",
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	return project
}

func TestUpdateProjectTaskStatusUsesApplicationServiceOrdering(t *testing.T) {
	ctx := context.Background()
	db := projectTaskHandlerTestDB(t)
	project := projectTaskHandlerTestProject(t, db, "handler-status")
	first := &database.ProjectTask{ProjectID: project.ID, Title: "First", Status: "todo", Priority: "medium"}
	second := &database.ProjectTask{ProjectID: project.ID, Title: "Second", Status: "todo", Priority: "medium"}
	third := &database.ProjectTask{ProjectID: project.ID, Title: "Third", Status: "todo", Priority: "medium"}
	for _, task := range []*database.ProjectTask{first, second, third} {
		if err := db.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	api := NewAPI(db, websocket.NewHub(), nil, nil, nil, nil, nil)
	req := projectTaskRouteRequest(
		http.MethodPut,
		"/api/projects/"+strconv.FormatInt(project.ID, 10)+"/tasks/"+strconv.FormatInt(first.ID, 10),
		project.ID,
		first.ID,
		`{"status":"done"}`,
	)
	rr := httptest.NewRecorder()
	api.UpdateProjectTask(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("UpdateProjectTask status = %d body=%s", rr.Code, rr.Body.String())
	}

	first, _ = db.GetTask(ctx, first.ID)
	second, _ = db.GetTask(ctx, second.ID)
	third, _ = db.GetTask(ctx, third.ID)
	if first.Status != "done" || first.SortOrder != 0 || first.GlobalSortOrder != 0 {
		t.Fatalf("done task ordering = %+v", first)
	}
	if second.SortOrder != 1 || third.SortOrder != 2 {
		t.Fatalf("active task ordering = second:%d third:%d", second.SortOrder, third.SortOrder)
	}
	history, err := db.ListTaskHistory(ctx, first.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	statusChanges := 0
	for _, entry := range history {
		if entry.EventType == "status_change" {
			statusChanges++
		}
	}
	if statusChanges != 1 {
		t.Fatalf("status history entries = %d, want 1", statusChanges)
	}
}

func TestLinkSessionTaskInvalidJSONPreservesExistingLink(t *testing.T) {
	ctx := context.Background()
	db := projectTaskHandlerTestDB(t)
	project := projectTaskHandlerTestProject(t, db, "handler-invalid-link")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Existing", Status: "in_progress", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	session := &database.Session{
		ID: "handler-invalid-link-session", ProjectID: project.ID, Status: "running",
		Name: "Task: Existing", TaskID: sql.NullInt64{Int64: task.ID, Valid: true},
		StartTime: time.Now(), Backend: "claude_code",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	api := NewAPI(db, websocket.NewHub(), nil, nil, nil, nil, nil)
	req := newSessionRouteRequest(http.MethodPost, "/api/sessions/"+session.ID+"/task", session.ID, `{`)
	rr := httptest.NewRecorder()
	api.LinkSessionTask(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("LinkSessionTask status = %d body=%s", rr.Code, rr.Body.String())
	}

	stored, err := db.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.TaskID.Valid || stored.TaskID.Int64 != task.ID || stored.Name != session.Name {
		t.Fatalf("invalid JSON mutated session: %+v", stored)
	}
}
