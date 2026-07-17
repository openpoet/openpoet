package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"openpoet/internal/database"
)

type recordingTaskEffects struct {
	mu       sync.Mutex
	tasks    []TaskChange
	history  []*database.TaskHistory
	sessions []SessionRename
	verified []*database.ProjectTask
}

func (e *recordingTaskEffects) PublishTaskChange(change TaskChange) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, change)
}

func (e *recordingTaskEffects) PublishTaskHistory(history *database.TaskHistory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history, history)
}

func (e *recordingTaskEffects) PublishSessionRename(rename SessionRename) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions = append(e.sessions, rename)
}

func (e *recordingTaskEffects) RequestTaskVerification(task *database.ProjectTask) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.verified = append(e.verified, task)
}

func applicationTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "application.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createApplicationProject(t *testing.T, db *database.DB, name string) *database.Project {
	t.Helper()
	project := &database.Project{Name: name, Path: "/tmp/" + name, Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	return project
}

func createTaskThroughService(t *testing.T, service *ProjectTaskService, projectID int64, title string, parentID *int64) *database.ProjectTask {
	t.Helper()
	task, err := service.Create(context.Background(), CreateProjectTaskCommand{
		ProjectID: projectID, Title: title, Priority: "medium", ParentID: parentID, Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCreateTaskRollsBackWhenHistoryFails(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "rollback")
	if _, err := db.Exec(`
		CREATE TRIGGER reject_task_history BEFORE INSERT ON task_history
		BEGIN SELECT RAISE(ABORT, 'history unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	effects := &recordingTaskEffects{}
	service := NewProjectTaskService(db, effects)

	_, err := service.Create(context.Background(), CreateProjectTaskCommand{
		ProjectID: project.ID, Title: "Must roll back", Actor: UserActor(),
	})
	if err == nil {
		t.Fatal("expected history failure")
	}
	tasks, err := db.ListTasksByProject(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("task mutation committed without history: %+v", tasks)
	}
	if len(effects.tasks) != 0 || len(effects.history) != 0 {
		t.Fatal("external effects ran before a successful commit")
	}
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("event committed for rolled-back task: %+v", events)
	}
}

func TestCreateTaskCommitsExactlyOneOutboxEvent(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "outbox-commit")
	service := NewProjectTaskService(db, nil)
	ctx := WithEventMetadata(context.Background(), EventMetadata{
		Actor: Actor{Type: "automation", ID: "helena"}, CorrelationID: "command-123",
	})

	task, err := service.Create(ctx, CreateProjectTaskCommand{
		ProjectID: project.ID, Title: "Transactional event", Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d, want exactly one: %+v", len(events), events)
	}
	event := events[0]
	if event.EventType != ProjectTaskEventCreated || event.AggregateID != fmt.Sprint(task.ID) {
		t.Fatalf("unexpected event identity: %+v", event)
	}
	if event.Actor != "automation:helena" || event.CorrelationID != "command-123" || event.SchemaVersion != 1 {
		t.Fatalf("unexpected event metadata: %+v", event)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["task_id"] != float64(task.ID) || payload["project_id"] != float64(project.ID) {
		t.Fatalf("unexpected event payload: %+v", payload)
	}
}

func TestCreateTaskRollsBackWhenOutboxFails(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "outbox-rollback")
	if _, err := db.Exec(`
		CREATE TRIGGER reject_event_outbox BEFORE INSERT ON event_outbox
		BEGIN SELECT RAISE(ABORT, 'outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	effects := &recordingTaskEffects{}
	service := NewProjectTaskService(db, effects)

	_, err := service.Create(context.Background(), CreateProjectTaskCommand{
		ProjectID: project.ID, Title: "Must remain atomic", Actor: UserActor(),
	})
	if err == nil {
		t.Fatal("expected outbox failure")
	}
	tasks, listErr := db.ListTasksByProject(context.Background(), project.ID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(tasks) != 0 {
		t.Fatalf("task committed without event: %+v", tasks)
	}
	var historyCount int
	if err := db.GetContext(context.Background(), &historyCount,
		"SELECT COUNT(*) FROM task_history WHERE project_id = ?", project.ID); err != nil {
		t.Fatal(err)
	}
	if historyCount != 0 {
		t.Fatalf("history committed without event: %d", historyCount)
	}
	if len(effects.tasks) != 0 || len(effects.history) != 0 {
		t.Fatal("external effects ran before outbox commit")
	}
}

func TestUpdateStatusUsesCanonicalOrderingAndAtomicHistory(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "ordering")
	effects := &recordingTaskEffects{}
	service := NewProjectTaskService(db, effects)
	first := createTaskThroughService(t, service, project.ID, "First", nil)
	second := createTaskThroughService(t, service, project.ID, "Second", nil)
	third := createTaskThroughService(t, service, project.ID, "Third", nil)

	done := TaskStatusDone
	updated, err := service.Update(context.Background(), UpdateProjectTaskCommand{
		ProjectID: project.ID, TaskID: first.ID, Status: &done, Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SortOrder != 0 || updated.GlobalSortOrder != 0 {
		t.Fatalf("done task retained ordering: %+v", updated)
	}
	second, _ = db.GetTask(context.Background(), second.ID)
	third, _ = db.GetTask(context.Background(), third.ID)
	if second.SortOrder != 1 || third.SortOrder != 2 {
		t.Fatalf("active tasks were not renumbered: second=%d third=%d", second.SortOrder, third.SortOrder)
	}

	todo := TaskStatusTodo
	reopened, err := service.Update(context.Background(), UpdateProjectTaskCommand{
		ProjectID: project.ID, TaskID: first.ID, Status: &todo, Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.SortOrder != 3 || reopened.GlobalSortOrder == 0 {
		t.Fatalf("reopened task did not receive canonical ordering: %+v", reopened)
	}
	history, err := db.ListTaskHistory(context.Background(), first.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	statusChanges := 0
	for _, entry := range history {
		if entry.EventType == "status_change" {
			statusChanges++
		}
	}
	if statusChanges != 2 {
		t.Fatalf("status history entries = %d, want 2", statusChanges)
	}
}

func TestTaskVerificationAutoApprovalPrecedenceAndAudit(t *testing.T) {
	tests := []struct {
		name             string
		globalEnabled    bool
		projectOverride  string
		wantStatus       string
		wantScope        string
		wantVerification bool
	}{
		{name: "global enabled with inherited project", globalEnabled: true, projectOverride: TaskAutoApproveInherit, wantStatus: TaskStatusDone, wantScope: "global"},
		{name: "project enabled over global disabled", globalEnabled: false, projectOverride: TaskAutoApproveEnabled, wantStatus: TaskStatusDone, wantScope: "project"},
		{name: "project disabled over global enabled", globalEnabled: true, projectOverride: TaskAutoApproveDisabled, wantStatus: TaskStatusAwaitingApproval, wantVerification: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := applicationTestDB(t)
			project := createApplicationProject(t, db, "auto-approval-"+strings.ReplaceAll(tt.name, " ", "-"))
			project.TaskAutoApproveVerification = tt.projectOverride
			if err := db.UpdateProject(context.Background(), project); err != nil {
				t.Fatal(err)
			}
			if err := db.SetSetting(context.Background(), TaskAutoApproveVerificationSetting, strconv.FormatBool(tt.globalEnabled)); err != nil {
				t.Fatal(err)
			}

			effects := &recordingTaskEffects{}
			service := NewProjectTaskService(db, effects)
			task := createTaskThroughService(t, service, project.ID, "Complete me", nil)
			updated, err := service.ChangeStatus(context.Background(), ChangeTaskStatusCommand{
				ProjectID: project.ID, TaskID: task.ID, Status: TaskStatusAwaitingApproval,
				Actor: Actor{Type: "agent", ID: "session-1"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if updated.Status != tt.wantStatus {
				t.Fatalf("status=%q, want %q", updated.Status, tt.wantStatus)
			}
			if got := len(effects.verified); (got == 1) != tt.wantVerification {
				t.Fatalf("verification requests=%d, wantVerification=%v", got, tt.wantVerification)
			}

			history, err := db.ListTaskHistory(context.Background(), task.ID, 20)
			if err != nil {
				t.Fatal(err)
			}
			var autoApproval *database.TaskHistory
			for i := range history {
				if history[i].EventType == "verification_auto_approved" {
					autoApproval = &history[i]
					break
				}
			}
			if tt.wantScope == "" {
				if autoApproval != nil {
					t.Fatalf("unexpected auto-approval history: %+v", autoApproval)
				}
				return
			}
			if autoApproval == nil {
				t.Fatalf("missing auto-approval history: %+v", history)
			}
			if autoApproval.Actor != "system:verification-auto-approval" || autoApproval.CreatedAt.IsZero() {
				t.Fatalf("incomplete audit identity/time: %+v", autoApproval)
			}
			var details map[string]any
			if err := json.Unmarshal([]byte(autoApproval.Details), &details); err != nil {
				t.Fatal(err)
			}
			if details["config_scope"] != tt.wantScope || details["project_override"] != tt.projectOverride || details["triggered_by"] != "agent:session-1" {
				t.Fatalf("unexpected audit details: %+v", details)
			}
			if details["requested_status"] != TaskStatusAwaitingApproval || details["new"] != TaskStatusDone {
				t.Fatalf("missing transition audit: %+v", details)
			}
		})
	}
}

func TestTaskVerificationAutoApprovalDoesNotApplyRetroactively(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "auto-approval-non-retroactive")
	effects := &recordingTaskEffects{}
	service := NewProjectTaskService(db, effects)
	task := createTaskThroughService(t, service, project.ID, "Already pending", nil)

	pending, err := service.ChangeStatus(context.Background(), ChangeTaskStatusCommand{
		ProjectID: project.ID, TaskID: task.ID, Status: TaskStatusAwaitingApproval, Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != TaskStatusAwaitingApproval {
		t.Fatalf("initial status=%q", pending.Status)
	}
	if err := db.SetSetting(context.Background(), TaskAutoApproveVerificationSetting, "true"); err != nil {
		t.Fatal(err)
	}
	description := "Edited after enabling the global setting"
	stillPending, err := service.Update(context.Background(), UpdateProjectTaskCommand{
		ProjectID: project.ID, TaskID: task.ID, Description: &description,
		Status: pointerToString(TaskStatusAwaitingApproval), Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stillPending.Status != TaskStatusAwaitingApproval {
		t.Fatalf("existing awaiting task changed retroactively to %q", stillPending.Status)
	}
	history, err := db.ListTaskHistory(context.Background(), task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range history {
		if entry.EventType == "verification_auto_approved" {
			t.Fatalf("existing awaiting task received auto-approval audit: %+v", entry)
		}
	}
}

func TestBulkApproveVerificationReportsPartialResultsAuditsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	projectOne := createApplicationProject(t, db, "bulk-one")
	projectTwo := createApplicationProject(t, db, "bulk-two")
	effects := &recordingTaskEffects{}
	service := NewProjectTaskService(db, effects)

	pendingOne := createTaskThroughService(t, service, projectOne.ID, "Pending one", nil)
	pendingTwo := createTaskThroughService(t, service, projectTwo.ID, "Pending two", nil)
	alreadyDone := createTaskThroughService(t, service, projectOne.ID, "Already done", nil)
	notPending := createTaskThroughService(t, service, projectOne.ID, "Not pending", nil)
	for _, task := range []*database.ProjectTask{pendingOne, pendingTwo} {
		if _, err := service.ChangeStatus(ctx, ChangeTaskStatusCommand{
			ProjectID: task.ProjectID, TaskID: task.ID, Status: TaskStatusAwaitingApproval, Actor: UserActor(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.ChangeStatus(ctx, ChangeTaskStatusCommand{
		ProjectID: alreadyDone.ProjectID, TaskID: alreadyDone.ID, Status: TaskStatusDone, Actor: UserActor(),
	}); err != nil {
		t.Fatal(err)
	}

	doc := &database.TempDocument{
		ID: "bulk-doc", Title: "Verification", Content: "ok", Status: "pending",
		TaskID: sql.NullInt64{Int64: pendingOne.ID, Valid: true},
	}
	if err := db.CreateTempDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskVerificationDoc(ctx, pendingOne.ID, doc.ID); err != nil {
		t.Fatal(err)
	}

	actor := Actor{Type: "automation", ID: "helena"}
	result, err := service.ApproveVerificationBulk(ctx, BulkApproveVerificationCommand{
		TaskIDs: []int64{pendingOne.ID, pendingTwo.ID, alreadyDone.ID, notPending.ID, 999999},
		Actor:   actor, BulkID: "command:bulk-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requested != 5 || result.Approved != 2 || result.AlreadyDone != 1 || result.Failed != 2 {
		t.Fatalf("unexpected bulk result: %+v", result)
	}
	if result.Results[3].Outcome != "failed" || result.Results[3].ErrorCode != "not_awaiting_approval" ||
		result.Results[4].Outcome != "failed" || result.Results[4].ErrorCode != "task_not_found" {
		t.Fatalf("partial failure details missing: %+v", result.Results)
	}
	for _, taskID := range []int64{pendingOne.ID, pendingTwo.ID} {
		stored, getErr := db.GetTask(ctx, taskID)
		if getErr != nil || stored.Status != TaskStatusDone {
			t.Fatalf("task %d was not approved: task=%+v err=%v", taskID, stored, getErr)
		}
		history, historyErr := db.ListTaskHistory(ctx, taskID, 20)
		if historyErr != nil {
			t.Fatal(historyErr)
		}
		bulkEntries := 0
		for _, entry := range history {
			if entry.EventType != "verification_approved" {
				continue
			}
			bulkEntries++
			if entry.Actor != "automation:helena" || entry.CreatedAt.IsZero() {
				t.Fatalf("bulk audit identity/time missing: %+v", entry)
			}
			var details map[string]any
			if err := json.Unmarshal([]byte(entry.Details), &details); err != nil {
				t.Fatal(err)
			}
			if details["bulk"] != true || details["approval_mode"] != "bulk" || details["bulk_id"] != "command:bulk-1" {
				t.Fatalf("bulk audit metadata missing: %+v", details)
			}
		}
		if bulkEntries != 1 {
			t.Fatalf("task %d bulk audit count=%d, want 1", taskID, bulkEntries)
		}
	}
	storedDoc, err := db.GetTempDocument(ctx, doc.ID)
	if err != nil || storedDoc.Status != "approved" {
		t.Fatalf("verification document did not use individual approval semantics: doc=%+v err=%v", storedDoc, err)
	}

	replay, err := service.ApproveVerificationBulk(ctx, BulkApproveVerificationCommand{
		TaskIDs: []int64{pendingOne.ID, pendingTwo.ID}, Actor: actor, BulkID: "command:bulk-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Approved != 0 || replay.AlreadyDone != 2 || replay.Failed != 0 {
		t.Fatalf("bulk replay is not idempotent: %+v", replay)
	}
	for _, taskID := range []int64{pendingOne.ID, pendingTwo.ID} {
		history, _ := db.ListTaskHistory(ctx, taskID, 20)
		bulkEntries := 0
		for _, entry := range history {
			if entry.EventType == "verification_approved" {
				bulkEntries++
			}
		}
		if bulkEntries != 1 {
			t.Fatalf("idempotent replay duplicated task %d audit: %+v", taskID, history)
		}
	}
}

func TestBulkApproveAllPendingHonorsProjectScope(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	projectOne := createApplicationProject(t, db, "bulk-scope-one")
	projectTwo := createApplicationProject(t, db, "bulk-scope-two")
	service := NewProjectTaskService(db, nil)
	taskOne := createTaskThroughService(t, service, projectOne.ID, "Scoped one", nil)
	taskTwo := createTaskThroughService(t, service, projectTwo.ID, "Scoped two", nil)
	for _, task := range []*database.ProjectTask{taskOne, taskTwo} {
		if _, err := service.ChangeStatus(ctx, ChangeTaskStatusCommand{
			ProjectID: task.ProjectID, TaskID: task.ID, Status: TaskStatusAwaitingApproval, Actor: UserActor(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := service.ApproveVerificationBulk(ctx, BulkApproveVerificationCommand{
		AllPending: true, ProjectID: &projectOne.ID, Actor: UserActor(), BulkID: "ui:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requested != 1 || result.Approved != 1 || result.Failed != 0 {
		t.Fatalf("unexpected scoped result: %+v", result)
	}
	storedOne, _ := db.GetTask(ctx, taskOne.ID)
	storedTwo, _ := db.GetTask(ctx, taskTwo.ID)
	if storedOne.Status != TaskStatusDone || storedTwo.Status != TaskStatusAwaitingApproval {
		t.Fatalf("project scope leaked: one=%s two=%s", storedOne.Status, storedTwo.Status)
	}
}

func TestBulkApproveVerificationValidatesSelectionMode(t *testing.T) {
	service := NewProjectTaskService(applicationTestDB(t), nil)
	if _, err := service.ApproveVerificationBulk(context.Background(), BulkApproveVerificationCommand{}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("missing selection error=%v", err)
	}
	if _, err := service.ApproveVerificationBulk(context.Background(), BulkApproveVerificationCommand{
		TaskIDs: []int64{1}, AllPending: true,
	}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("ambiguous selection error=%v", err)
	}
}

func pointerToString(value string) *string {
	return &value
}

func TestParentMustStayInProjectAndCannotCycle(t *testing.T) {
	db := applicationTestDB(t)
	one := createApplicationProject(t, db, "parent-one")
	two := createApplicationProject(t, db, "parent-two")
	service := NewProjectTaskService(db, nil)
	foreignParent := createTaskThroughService(t, service, two.ID, "Foreign", nil)

	_, err := service.Create(context.Background(), CreateProjectTaskCommand{
		ProjectID: one.ID, Title: "Invalid child", ParentID: &foreignParent.ID, Actor: UserActor(),
	})
	if !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("cross-project parent error = %v", err)
	}

	parent := createTaskThroughService(t, service, one.ID, "Parent", nil)
	child := createTaskThroughService(t, service, one.ID, "Child", &parent.ID)
	_, err = service.Update(context.Background(), UpdateProjectTaskCommand{
		ProjectID: one.ID, TaskID: parent.ID, ParentID: &child.ID, Actor: UserActor(),
	})
	if !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("cycle error = %v", err)
	}
	stored, err := db.GetTask(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ParentID.Valid {
		t.Fatalf("cycle attempt changed parent: %+v", stored.ParentID)
	}
}

func TestInvalidRelinkKeepsExistingTaskAndName(t *testing.T) {
	db := applicationTestDB(t)
	one := createApplicationProject(t, db, "link-one")
	two := createApplicationProject(t, db, "link-two")
	service := NewProjectTaskService(db, &recordingTaskEffects{})
	existing := createTaskThroughService(t, service, one.ID, "Existing", nil)
	foreign := createTaskThroughService(t, service, two.ID, "Foreign", nil)
	session := &database.Session{
		ID: "session-link", ProjectID: one.ID, Status: "running", Name: "Task: Existing",
		TaskID: sql.NullInt64{Int64: existing.ID, Valid: true}, StartTime: time.Now(), Backend: "claude_code",
	}
	if err := db.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	_, err := service.LinkSession(context.Background(), LinkSessionTaskCommand{
		SessionID: session.ID, TaskID: &foreign.ID, Actor: UserActor(),
	})
	if !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("relink error = %v", err)
	}
	stored, err := db.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.TaskID.Valid || stored.TaskID.Int64 != existing.ID || stored.Name != session.Name {
		t.Fatalf("invalid relink mutated session: %+v", stored)
	}
}

func TestLinkSessionCommitsRenameStatusAndHistoryTogether(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "valid-link")
	effects := &recordingTaskEffects{}
	service := NewProjectTaskService(db, effects)
	task := createTaskThroughService(t, service, project.ID, "Work", nil)
	session := &database.Session{
		ID: "session-valid-link", ProjectID: project.ID, Status: "running", Name: "Original",
		StartTime: time.Now(), Backend: "claude_code",
	}
	if err := db.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	result, err := service.LinkSession(context.Background(), LinkSessionTaskCommand{
		SessionID: session.ID, TaskID: &task.ID, Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.Status != TaskStatusInProgress || result.SessionName != "Task: Work" {
		t.Fatalf("unexpected link result: %+v", result)
	}
	stored, err := db.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != result.SessionName || stored.TaskID.Int64 != task.ID {
		t.Fatalf("link was not committed atomically: %+v", stored)
	}
	history, err := db.ListTaskHistory(context.Background(), task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]bool{}
	for _, entry := range history {
		events[entry.EventType] = true
	}
	for _, event := range []string{"session_linked", "task_assigned", "status_change"} {
		if !events[event] {
			t.Fatalf("missing link history event %q: %+v", event, history)
		}
	}
}

func TestReorderKeepsDoneTaskOrdersAtZero(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "reorder-done")
	service := NewProjectTaskService(db, nil)
	active := createTaskThroughService(t, service, project.ID, "Active", nil)
	done, err := service.Create(context.Background(), CreateProjectTaskCommand{
		ProjectID: project.ID, Title: "Done", Status: TaskStatusDone, Actor: UserActor(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.ReorderProject(context.Background(), project.ID, []database.ReorderItem{
		{ID: done.ID, SortOrder: 9},
		{ID: active.ID, SortOrder: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.ReorderGlobal(context.Background(), []database.GlobalReorderItem{
		{ID: done.ID, GlobalSortOrder: 9},
		{ID: active.ID, GlobalSortOrder: 1},
	}); err != nil {
		t.Fatal(err)
	}

	storedDone, err := db.GetTask(context.Background(), done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedDone.SortOrder != 0 || storedDone.GlobalSortOrder != 0 {
		t.Fatalf("done task acquired active ordering: %+v", storedDone)
	}
	storedActive, err := db.GetTask(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedActive.SortOrder <= 0 || storedActive.GlobalSortOrder <= 0 {
		t.Fatalf("active task lost ordering: %+v", storedActive)
	}
}
