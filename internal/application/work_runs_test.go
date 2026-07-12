package application

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openpoet/internal/database"
)

type workRunClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *workRunClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *workRunClock) Add(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func newWorkRunTestService(db *database.DB, clock *workRunClock) *WorkRunService {
	var mu sync.Mutex
	next := 0
	return NewWorkRunService(db, WorkRunServiceOptions{
		Now: clock.Now,
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			next++
			return "00000000-0000-4000-8000-" + leftPad12(next)
		},
	})
}

func leftPad12(value int) string {
	text := "000000000000" + fmt.Sprint(value)
	return text[len(text)-12:]
}

func TestWorkRunTracksActiveTimeAcrossPauseResumeAndReplay(t *testing.T) {
	db := applicationTestDB(t)
	clock := &workRunClock{now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)}
	service := newWorkRunTestService(db, clock)
	actor := Actor{Type: "automation_client", ID: "helena"}

	run, err := service.Start(context.Background(), StartWorkRunCommand{
		Title: "Implementar integração", Description: "Work core", Source: "helena", ExpectedMinutes: 45,
		Actor: actor, CorrelationID: "corr-1", IdempotencyKey: "start-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(90 * time.Second)
	version := run.Version
	paused, err := service.Pause(context.Background(), TransitionWorkRunCommand{
		WorkRunID: run.ID, ExpectedVersion: &version, Actor: actor, IdempotencyKey: "pause-1",
	})
	if err != nil || paused.AccumulatedActiveSeconds != 90 || paused.Version != 2 {
		t.Fatalf("pause run=%+v err=%v", paused, err)
	}
	replayed, err := service.Pause(context.Background(), TransitionWorkRunCommand{
		WorkRunID: run.ID, ExpectedVersion: &version, Actor: actor, IdempotencyKey: "pause-1",
	})
	if err != nil || replayed.Version != 2 || replayed.AccumulatedActiveSeconds != 90 {
		t.Fatalf("pause replay run=%+v err=%v", replayed, err)
	}
	clock.Add(time.Hour)
	version = paused.Version
	resumed, err := service.Resume(context.Background(), TransitionWorkRunCommand{
		WorkRunID: run.ID, ExpectedVersion: &version, Actor: actor, IdempotencyKey: "resume-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(3 * time.Minute)
	version = resumed.Version
	completed, err := service.Complete(context.Background(), TransitionWorkRunCommand{
		WorkRunID: run.ID, ExpectedVersion: &version, Actor: actor, IdempotencyKey: "complete-1",
	})
	if err != nil || completed.Status != database.WorkRunStatusCompleted || completed.ActiveSeconds != 270 || completed.Version != 4 {
		t.Fatalf("complete run=%+v err=%v", completed, err)
	}
	audit, err := service.ListAudit(context.Background(), run.ID, 10)
	if err != nil || len(audit) != 4 {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 20)
	if err != nil || len(events) != 4 || events[0].EventType != "work_run.started" || events[3].EventType != "work_run.completed" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestWorkRunSupportsMultipleParallelRunsAndOptimisticConflict(t *testing.T) {
	db := applicationTestDB(t)
	clock := &workRunClock{now: time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)}
	service := newWorkRunTestService(db, clock)
	actor := Actor{Type: "automation_client", ID: "helena"}
	first, err := service.Start(context.Background(), StartWorkRunCommand{Title: "A", Source: "helena", ExpectedMinutes: 10, Actor: actor, IdempotencyKey: "start-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(context.Background(), StartWorkRunCommand{Title: "B", Source: "helena", ExpectedMinutes: 20, Actor: actor, IdempotencyKey: "start-b"}); err != nil {
		t.Fatal(err)
	}
	active, err := service.List(context.Background(), database.WorkRunFilter{ActiveOnly: true})
	if err != nil || len(active) != 2 {
		t.Fatalf("active=%+v err=%v", active, err)
	}

	version := first.Version
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	for _, key := range []string{"pause-a", "pause-b"} {
		wait.Add(1)
		go func(key string) {
			defer wait.Done()
			_, err := service.Pause(context.Background(), TransitionWorkRunCommand{
				WorkRunID: first.ID, ExpectedVersion: &version, Actor: actor, IdempotencyKey: key,
			})
			errorsSeen <- err
		}(key)
	}
	wait.Wait()
	close(errorsSeen)
	successes, conflicts := 0, 0
	for err := range errorsSeen {
		if err == nil {
			successes++
			continue
		}
		var appErr *Error
		if errors.As(err, &appErr) && appErr.Code == "work_run_version_conflict" {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestWorkRunRejectsOversizedMetadataAndTarget(t *testing.T) {
	db := applicationTestDB(t)
	clock := &workRunClock{now: time.Date(2026, 7, 9, 13, 30, 0, 0, time.UTC)}
	service := newWorkRunTestService(db, clock)
	_, err := service.Start(context.Background(), StartWorkRunCommand{
		Title: "Invalid", Description: strings.Repeat("x", maxWorkRunDescriptionLength+1),
		Source: "helena", ExpectedMinutes: 10,
	})
	assertApplicationErrorCode(t, err, "work_run_description_invalid")
	_, err = service.Start(context.Background(), StartWorkRunCommand{
		Title: "Invalid target", Source: "helena", ExpectedMinutes: 10,
		ExecutionTarget: &database.WorkRunExecutionTarget{Type: "repository", ID: strings.Repeat("x", maxExecutionTargetID+1)},
	})
	assertApplicationErrorCode(t, err, "execution_target_invalid")
	_, err = service.Start(context.Background(), StartWorkRunCommand{
		Title: "Invalid metadata", Source: "helena", ExpectedMinutes: 10,
		IdempotencyKey: strings.Repeat("x", maxWorkRunMetadataLength+1),
	})
	assertApplicationErrorCode(t, err, "idempotency_key_invalid")
}

func assertApplicationErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var applicationError *Error
	if !errors.As(err, &applicationError) || applicationError.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
}

func TestWorkRunSurvivesDatabaseRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &workRunClock{now: time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)}
	service := newWorkRunTestService(db, clock)
	run, err := service.Start(context.Background(), StartWorkRunCommand{Title: "Durável", Source: "helena", ExpectedMinutes: 30, IdempotencyKey: "restart-start"})
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewWorkRunService(reopened, WorkRunServiceOptions{Now: clock.Now})
	loaded, err := restarted.Get(context.Background(), run.ID)
	if err != nil || loaded.Status != database.WorkRunStatusRunning || loaded.ActiveSeconds != 120 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestWorkRunCompletesLinkedProjectTaskOnlyWhenExplicit(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "work-run-task")
	taskService := NewProjectTaskService(db, nil)
	task := createTaskThroughService(t, taskService, project.ID, "Task vinculada", nil)
	clock := &workRunClock{now: time.Date(2026, 7, 9, 14, 30, 0, 0, time.UTC)}
	service := newWorkRunTestService(db, clock)
	actor := Actor{Type: "automation_client", ID: "helena"}

	first, err := service.Start(context.Background(), StartWorkRunCommand{
		Title: "Run sem fechamento", Source: "helena", ExpectedMinutes: 10,
		ProjectTaskID: &task.ID, Actor: actor, IdempotencyKey: "task-run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), TransitionWorkRunCommand{
		WorkRunID: first.ID, Actor: actor, IdempotencyKey: "task-complete-1",
	}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := taskService.Get(context.Background(), project.ID, task.ID)
	if err != nil || unchanged.Status == TaskStatusDone {
		t.Fatalf("task should remain open: %+v err=%v", unchanged, err)
	}

	second, err := service.Start(context.Background(), StartWorkRunCommand{
		Title: "Run com fechamento", Source: "helena", ExpectedMinutes: 10,
		ProjectTaskID: &task.ID, Actor: actor, IdempotencyKey: "task-run-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), TransitionWorkRunCommand{
		WorkRunID: second.ID, CompleteProjectTask: true, Actor: actor, IdempotencyKey: "task-complete-2",
	}); err != nil {
		t.Fatal(err)
	}
	done, err := taskService.Get(context.Background(), project.ID, task.ID)
	if err != nil || done.Status != TaskStatusDone {
		t.Fatalf("task should be done: %+v err=%v", done, err)
	}
}

func TestPlanUpsertPreservesStableRefsAndCancelsOmittedItems(t *testing.T) {
	db := applicationTestDB(t)
	clock := &workRunClock{now: time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)}
	service := newWorkRunTestService(db, clock)
	actor := Actor{Type: "automation_client", ID: "helena"}
	first, err := service.UpsertPlan(context.Background(), UpsertPlanCommand{
		ExternalRef: "helena:daily:2026-07-09", Kind: "daily", Title: "Plano do dia",
		PeriodStart: "2026-07-09", PeriodEnd: "2026-07-09", Timezone: "America/Sao_Paulo",
		Items: []UpsertPlanItem{
			{ExternalRef: "day:one", Title: "Primeiro", SortOrder: 20},
			{ExternalRef: "day:two", Title: "Segundo", SortOrder: 10},
		}, Actor: actor, IdempotencyKey: "plan-1",
	})
	if err != nil || first.Version != 1 || len(first.Items) != 2 || first.Items[0].ExternalRef != "day:two" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	version := first.Version
	secondCommand := UpsertPlanCommand{
		ExternalRef: first.ExternalRef, Kind: "daily", Title: "Plano revisado",
		PeriodStart: "2026-07-09", PeriodEnd: "2026-07-09", Timezone: "America/Sao_Paulo",
		Items:           []UpsertPlanItem{{ExternalRef: "day:one", Title: "Primeiro revisado", SortOrder: 5}},
		ExpectedVersion: &version, Actor: actor, IdempotencyKey: "plan-2",
	}
	second, err := service.UpsertPlan(context.Background(), secondCommand)
	if err != nil || second.Version != 2 || len(second.Items) != 2 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	byRef := map[string]database.PlanItem{}
	for _, item := range second.Items {
		byRef[item.ExternalRef] = item
	}
	if byRef["day:one"].Status != "planned" || byRef["day:two"].Status != "cancelled" {
		t.Fatalf("replace semantics items=%+v", second.Items)
	}
	replayed, err := service.UpsertPlan(context.Background(), secondCommand)
	if err != nil || replayed.Version != 2 || len(replayed.Items) != 2 {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	run, err := service.Start(context.Background(), StartWorkRunCommand{
		Title: "Executar primeiro", Source: "plan", ExpectedMinutes: 25,
		PlanItemRef: stringPointer("day:one"), Actor: actor, IdempotencyKey: "start-plan-item",
	})
	if err != nil || run.PlanItemRef == nil || *run.PlanItemRef != "day:one" {
		t.Fatalf("linked run=%+v err=%v", run, err)
	}
}

func stringPointer(value string) *string { return &value }
