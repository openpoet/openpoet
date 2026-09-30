package handlers

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// A closed completed session keeps who closed it and why, both in the linked
// task's history and in the event outbox.
func TestRecordSessionClosureWritesTaskHistoryAndOutbox(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "closure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project := &database.Project{Name: "closure", Path: t.TempDir(), Type: "local", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := &database.ProjectTask{ProjectID: project.ID, Title: "finished", Status: "done", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	effects := &platformEffects{db: db}
	effects.RecordSessionClosure(ctx, application.SessionClosure{
		SessionID: "48ec507d", ProjectID: project.ID, TaskID: task.ID, TaskStatus: "done", Mode: "idle",
		Reason: "task validated by Helena", Actor: application.Actor{Type: "automation_client", ID: "helena"}, ApprovedBy: "helena",
	})

	history, err := db.ListTaskHistory(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range history {
		if entry.EventType == "session_closed_completed" {
			found = entry.Actor == "helena" && entry.SessionID.String == "48ec507d" && strings.Contains(entry.Details, "task validated by Helena")
		}
	}
	if !found {
		t.Fatalf("task history lacks the closure record: %+v", history)
	}
	var payload string
	if err := db.GetContext(ctx, &payload, `SELECT payload_json FROM event_outbox WHERE event_type = 'platform.session.closure_recorded' AND aggregate_id = '48ec507d'`); err != nil {
		t.Fatalf("outbox closure event: %v", err)
	}
	if !strings.Contains(payload, "task validated by Helena") || !strings.Contains(payload, `"task_id"`) {
		t.Fatalf("outbox payload = %s", payload)
	}
}

// A permission mode change keeps who changed it, from and to, under which
// authorization and why, in the linked task's history and the event outbox.
func TestRecordSessionPermissionModeChangeWritesTaskHistoryAndOutbox(t *testing.T) {
	ctx := application.WithEventMetadata(context.Background(), application.EventMetadata{CorrelationID: "ain:292"})
	db, err := database.New(filepath.Join(t.TempDir(), "mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project := &database.Project{Name: "mode", Path: t.TempDir(), Type: "local", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	task := &database.ProjectTask{ProjectID: project.ID, Title: "restart rule", Status: "in_progress", Priority: "high"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	effects := &platformEffects{db: db}
	effects.RecordSessionPermissionModeChange(ctx, application.SessionPermissionModeChange{
		SessionID: "50bc056f", ProjectID: project.ID, TaskID: task.ID, Requested: "acceptEdits", From: "auto", To: "acceptEdits",
		Presses: 2, Outcome: "changed", Reason: "owner approves the restart rule by hand", AuthorizationRef: "ain:292",
		Actor: application.Actor{Type: "automation_client", ID: "helena"}, ApprovedBy: "helena",
	})

	history, err := db.ListTaskHistory(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range history {
		if entry.EventType == "session_permission_mode_changed" {
			found = entry.Actor == "helena" && entry.SessionID.String == "50bc056f" &&
				strings.Contains(entry.Details, `"authorization_ref":"ain:292"`) && strings.Contains(entry.Details, `"from":"auto"`)
		}
	}
	if !found {
		t.Fatalf("task history lacks the permission mode record: %+v", history)
	}
	var row struct {
		Payload       string `db:"payload_json"`
		CorrelationID string `db:"correlation_id"`
	}
	if err := db.GetContext(ctx, &row, `SELECT payload_json, correlation_id FROM event_outbox WHERE event_type = 'platform.session.permission_mode_recorded' AND aggregate_id = '50bc056f'`); err != nil {
		t.Fatalf("outbox permission mode event: %v", err)
	}
	if row.CorrelationID != "ain:292" || !strings.Contains(row.Payload, `"to":"acceptEdits"`) || !strings.Contains(row.Payload, "restart rule by hand") {
		t.Fatalf("outbox row = %+v", row)
	}
}
