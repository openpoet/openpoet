package database

import (
	"context"
	"os"
	"testing"
	"time"
)

func setupTestDB(t *testing.T) *DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "openpoet-test-*.db")
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

func TestLinkSessionToTask(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	task := &ProjectTask{ProjectID: proj.ID, Title: "Task A", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sess := &Session{ID: "sess-1", ProjectID: proj.ID, Status: "running", Name: "Session 1", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Session should have no task initially
	linkedTask, err := db.GetTaskForSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if linkedTask != nil {
		t.Fatal("expected no linked task initially")
	}

	// Link session to task
	if err := db.LinkSessionToTask(ctx, "sess-1", task.ID); err != nil {
		t.Fatal(err)
	}

	// Now should return the task
	linkedTask, err = db.GetTaskForSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if linkedTask == nil {
		t.Fatal("expected linked task after LinkSessionToTask")
	}
	if linkedTask.ID != task.ID {
		t.Errorf("expected task ID %d, got %d", task.ID, linkedTask.ID)
	}
}

func TestGetSessionsForTask(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	taskA := &ProjectTask{ProjectID: proj.ID, Title: "Task A", Status: "in_progress", Priority: "medium"}
	taskB := &ProjectTask{ProjectID: proj.ID, Title: "Task B", Status: "in_progress", Priority: "medium"}
	if err := db.CreateTask(ctx, taskA); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, taskB); err != nil {
		t.Fatal(err)
	}

	sess1 := &Session{ID: "sess-1", ProjectID: proj.ID, Status: "stopped", Name: "Session 1", StartTime: time.Now().Add(-1 * time.Hour)}
	sess2 := &Session{ID: "sess-2", ProjectID: proj.ID, Status: "running", Name: "Session 2", StartTime: time.Now()}
	sess3 := &Session{ID: "sess-3", ProjectID: proj.ID, Status: "running", Name: "Session 3 (no task)", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess1); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, sess2); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, sess3); err != nil {
		t.Fatal(err)
	}

	// Link sessions to tasks via task_id
	db.LinkSessionToTask(ctx, "sess-1", taskA.ID)
	db.LinkSessionToTask(ctx, "sess-2", taskB.ID)
	// sess-3 has no task

	// Task A should have only sess-1
	sessions, err := db.GetSessionsForTask(ctx, taskA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for Task A, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-1" {
		t.Errorf("expected session 'sess-1', got '%s'", sessions[0].ID)
	}

	// Task B should have only sess-2
	sessions, err = db.GetSessionsForTask(ctx, taskB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for Task B, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-2" {
		t.Errorf("expected session 'sess-2', got '%s'", sessions[0].ID)
	}
}

func TestGetTaskSessionSummary(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	taskA := &ProjectTask{ProjectID: proj.ID, Title: "Task A", Status: "in_progress", Priority: "medium"}
	taskB := &ProjectTask{ProjectID: proj.ID, Title: "Task B", Status: "in_progress", Priority: "medium"}
	if err := db.CreateTask(ctx, taskA); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, taskB); err != nil {
		t.Fatal(err)
	}

	sess1 := &Session{ID: "sess-1", ProjectID: proj.ID, Status: "stopped", Name: "Session 1", StartTime: time.Now().Add(-1 * time.Hour)}
	sess2 := &Session{ID: "sess-2", ProjectID: proj.ID, Status: "running", Name: "Session 2", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess1); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, sess2); err != nil {
		t.Fatal(err)
	}

	db.LinkSessionToTask(ctx, "sess-1", taskA.ID)
	db.LinkSessionToTask(ctx, "sess-2", taskB.ID)

	summary, err := db.GetTaskSessionSummary(ctx, proj.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(summary) != 2 {
		t.Fatalf("expected 2 task summaries, got %d", len(summary))
	}

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

	// Task A: sess-1 (stopped)
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

	// Task B: sess-2 (running)
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
}

func TestTaskSessionSummaryPrefersMostRecentlyLinkedSession(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	task := &ProjectTask{ProjectID: proj.ID, Title: "Task A", Status: "in_progress", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	olderSession := &Session{ID: "sess-older", ProjectID: proj.ID, Status: "stopped", Name: "Older Session", StartTime: time.Now().Add(-2 * time.Hour)}
	newerSession := &Session{ID: "sess-newer", ProjectID: proj.ID, Status: "stopped", Name: "Newer Session", StartTime: time.Now().Add(-1 * time.Hour)}
	if err := db.CreateSession(ctx, olderSession); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, newerSession); err != nil {
		t.Fatal(err)
	}

	if err := db.LinkSessionToTask(ctx, newerSession.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := db.LinkSessionToTask(ctx, olderSession.ID, task.ID); err != nil {
		t.Fatal(err)
	}

	summary, err := db.GetTaskSessionSummary(ctx, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) != 1 {
		t.Fatalf("expected 1 task summary, got %d", len(summary))
	}
	if summary[0].LatestStoppedSession != olderSession.ID {
		t.Fatalf("latest_stopped_session = %q, want %q", summary[0].LatestStoppedSession, olderSession.ID)
	}

	sessions, err := db.GetSessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].ID != olderSession.ID {
		t.Fatalf("first task session = %q, want %q", sessions[0].ID, olderSession.ID)
	}
}

func TestGetSessionsForTask_NoSessions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	proj := &Project{Name: "test-project", Path: "/tmp/test", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	task := &ProjectTask{ProjectID: proj.ID, Title: "Lonely Task", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sessions, err := db.GetSessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}
