package database

import (
	"context"
	"testing"
	"time"
)

func TestReadAutomationSnapshotIncludesConsistentOutboxCursor(t *testing.T) {
	db := eventOutboxTestDB(t)
	ctx := context.Background()
	project := &Project{Name: "snapshot", Path: "/tmp/snapshot", Type: "local"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := &ProjectTask{ProjectID: project.ID, Title: "Snapshot", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	session := &Session{ID: "snapshot-session", ProjectID: project.ID, Status: "running", Name: "Snapshot", StartTime: time.Now()}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNotification(ctx, &Notification{SessionID: session.ID, Type: "info", Title: "Snapshot"}); err != nil {
		t.Fatal(err)
	}
	event := appendTestEvent(t, db, "snapshot-event", time.Now())

	snapshot, err := db.ReadAutomationSnapshot(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != event.Sequence {
		t.Fatalf("snapshot cursor=%d, want %d", snapshot.Cursor, event.Sequence)
	}
	if len(snapshot.Projects) != 1 || len(snapshot.Tasks) != 1 || len(snapshot.Sessions) != 1 || len(snapshot.Notifications) != 1 {
		t.Fatalf("snapshot counts projects=%d tasks=%d sessions=%d notifications=%d",
			len(snapshot.Projects), len(snapshot.Tasks), len(snapshot.Sessions), len(snapshot.Notifications))
	}
}
