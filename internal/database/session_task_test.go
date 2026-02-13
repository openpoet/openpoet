package database

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func setupTestDB(t *testing.T) *DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "devmanager-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestGetSessionsForTask_IsolatesPrimaryRoles(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create a project
	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	// Create two tasks
	taskA := &ProjectTask{ProjectID: proj.ID, Title: "Task A", Status: "in_progress", Priority: "medium"}
	taskB := &ProjectTask{ProjectID: proj.ID, Title: "Task B", Status: "in_progress", Priority: "medium"}
	if err := db.CreateTask(ctx, taskA); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, taskB); err != nil {
		t.Fatal(err)
	}

	// Create two sessions (Session 1 is older, Session 2 is newer)
	sess1 := &Session{ID: "sess-1", ProjectID: proj.ID, Status: "stopped", Name: "Session 1", StartTime: time.Now().Add(-1 * time.Hour)}
	sess2 := &Session{ID: "sess-2", ProjectID: proj.ID, Status: "stopped", Name: "Session 2", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess1); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, sess2); err != nil {
		t.Fatal(err)
	}

	// Link Session 1 to Task A with 'created_from' (primary)
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-1", TaskID: taskA.ID, Role: "created_from"}); err != nil {
		t.Fatal(err)
	}
	// Link Session 2 to Task B with 'created_from' (primary)
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-2", TaskID: taskB.ID, Role: "created_from"}); err != nil {
		t.Fatal(err)
	}
	// Also link Session 2 to Task A with 'works_on' (secondary — should be excluded)
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-2", TaskID: taskA.ID, Role: "works_on"}); err != nil {
		t.Fatal(err)
	}

	// GetSessionsForTask for Task A should return ONLY Session 1
	sessions, err := db.GetSessionsForTask(ctx, taskA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for Task A, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-1" {
		t.Errorf("expected session 'sess-1' for Task A, got '%s'", sessions[0].ID)
	}

	// GetSessionsForTask for Task B should return ONLY Session 2
	sessions, err = db.GetSessionsForTask(ctx, taskB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for Task B, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-2" {
		t.Errorf("expected session 'sess-2' for Task B, got '%s'", sessions[0].ID)
	}
}

func TestGetTaskSessionSummary_IsolatesPrimaryRoles(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create a project
	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	// Create two tasks
	taskA := &ProjectTask{ProjectID: proj.ID, Title: "Task A", Status: "in_progress", Priority: "medium"}
	taskB := &ProjectTask{ProjectID: proj.ID, Title: "Task B", Status: "in_progress", Priority: "medium"}
	if err := db.CreateTask(ctx, taskA); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, taskB); err != nil {
		t.Fatal(err)
	}

	// Create sessions
	sess1 := &Session{ID: "sess-1", ProjectID: proj.ID, Status: "stopped", Name: "Session 1", StartTime: time.Now().Add(-1 * time.Hour)}
	sess2 := &Session{ID: "sess-2", ProjectID: proj.ID, Status: "running", Name: "Session 2", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess1); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, sess2); err != nil {
		t.Fatal(err)
	}

	// Primary links
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-1", TaskID: taskA.ID, Role: "created_from"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-2", TaskID: taskB.ID, Role: "created_from"}); err != nil {
		t.Fatal(err)
	}
	// Secondary link (should be excluded from summary)
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-2", TaskID: taskA.ID, Role: "works_on"}); err != nil {
		t.Fatal(err)
	}

	summary, err := db.GetTaskSessionSummary(ctx, proj.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(summary) != 2 {
		t.Fatalf("expected 2 task summaries, got %d", len(summary))
	}

	// Build a map for easy lookup
	summaryMap := make(map[int64]struct {
		SessionCount         int
		ActiveCount          int
		StoppedCount         int
		LatestSession        string
		LatestStoppedSession string
	})
	for _, s := range summary {
		summaryMap[s.TaskID] = struct {
			SessionCount         int
			ActiveCount          int
			StoppedCount         int
			LatestSession        string
			LatestStoppedSession string
		}{s.SessionCount, s.ActiveCount, s.StoppedCount, s.LatestSession, s.LatestStoppedSession}
	}

	// Task A: only sess-1 (stopped)
	infoA, ok := summaryMap[taskA.ID]
	if !ok {
		t.Fatal("Task A not found in summary")
	}
	if infoA.SessionCount != 1 {
		t.Errorf("Task A: expected session_count=1, got %d", infoA.SessionCount)
	}
	if infoA.ActiveCount != 0 {
		t.Errorf("Task A: expected active_count=0, got %d", infoA.ActiveCount)
	}
	if infoA.StoppedCount != 1 {
		t.Errorf("Task A: expected stopped_count=1, got %d", infoA.StoppedCount)
	}
	if infoA.LatestSession != "sess-1" {
		t.Errorf("Task A: expected latest_session='sess-1', got '%s'", infoA.LatestSession)
	}
	if infoA.LatestStoppedSession != "sess-1" {
		t.Errorf("Task A: expected latest_stopped_session='sess-1', got '%s'", infoA.LatestStoppedSession)
	}

	// Task B: only sess-2 (running)
	infoB, ok := summaryMap[taskB.ID]
	if !ok {
		t.Fatal("Task B not found in summary")
	}
	if infoB.SessionCount != 1 {
		t.Errorf("Task B: expected session_count=1, got %d", infoB.SessionCount)
	}
	if infoB.ActiveCount != 1 {
		t.Errorf("Task B: expected active_count=1, got %d", infoB.ActiveCount)
	}
	if infoB.LatestSession != "sess-2" {
		t.Errorf("Task B: expected latest_session='sess-2', got '%s'", infoB.LatestSession)
	}
}

func TestGetSessionsForTask_IncludesRegisteredAs(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	task := &ProjectTask{ProjectID: proj.ID, Title: "Task C", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sess := &Session{ID: "sess-3", ProjectID: proj.ID, Status: "running", Name: "Session 3", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Link with 'registered_as' (primary role — should be included)
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-3", TaskID: task.ID, Role: "registered_as"}); err != nil {
		t.Fatal(err)
	}

	sessions, err := db.GetSessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-3" {
		t.Errorf("expected 'sess-3', got '%s'", sessions[0].ID)
	}
}

func TestGetSessionsForTask_ExcludesWorksOn(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	task := &ProjectTask{ProjectID: proj.ID, Title: "Task D", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sess := &Session{ID: "sess-4", ProjectID: proj.ID, Status: "running", Name: "Session 4", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Link with only 'works_on' (secondary — should be excluded)
	if err := db.CreateSessionTask(ctx, &SessionTask{SessionID: "sess-4", TaskID: task.ID, Role: "works_on"}); err != nil {
		t.Fatal(err)
	}

	sessions, err := db.GetSessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		for _, s := range sessions {
			fmt.Printf("  unexpected session: %s\n", s.ID)
		}
		t.Fatalf("expected 0 sessions (works_on should be excluded), got %d", len(sessions))
	}
}
