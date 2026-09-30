package database

import (
	"context"
	"testing"
	"time"
)

func TestReopenSessionMovesEndedStatusesToStarting(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	proj := &Project{Name: "p", Path: "/tmp/p", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{"stopped", "completed", "error"} {
		id := "sess-" + status
		if err := db.CreateSession(ctx, &Session{ID: id, ProjectID: proj.ID, Status: "running", Name: id, StartTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := db.EndSession(ctx, id, status); err != nil {
			t.Fatal(err)
		}
		if status == "error" {
			if err := db.SetSessionErrorDetails(ctx, id, "failed to start runner: ssh timeout", "tail"); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.ReopenSession(ctx, id); err != nil {
			t.Fatalf("reopen %s: %v", status, err)
		}
		got, err := db.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "starting" || got.EndTime.Valid || got.ErrorReason != "" {
			t.Fatalf("reopened %s session: status=%s end=%v reason=%q", status, got.Status, got.EndTime, got.ErrorReason)
		}
	}
}

func TestReopenSessionRefusesLiveOrMissingSession(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	proj := &Project{Name: "p", Path: "/tmp/p", Type: "local"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, &Session{ID: "live", ProjectID: proj.ID, Status: "running", Name: "live", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReopenSession(ctx, "live"); err == nil {
		t.Fatal("reopening a running session must fail")
	}
	if err := db.ReopenSession(ctx, "missing"); err == nil {
		t.Fatal("reopening a missing session must fail")
	}
}
