package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type automationStartSessionStore struct {
	project *database.Project
	task    *database.ProjectTask
	session *database.Session
}

func (s *automationStartSessionStore) GetProject(context.Context, int64) (*database.Project, error) {
	return s.project, nil
}

func (s *automationStartSessionStore) GetSession(context.Context, string) (*database.Session, error) {
	return s.session, nil
}

func (s *automationStartSessionStore) GetTask(context.Context, int64) (*database.ProjectTask, error) {
	return s.task, nil
}

func (s *automationStartSessionStore) GetTaskForSession(context.Context, string) (*database.ProjectTask, error) {
	return s.task, nil
}

func (s *automationStartSessionStore) ProjectIDsForTags(_ context.Context, _ []int64) ([]int64, error) {
	return nil, nil
}

func (s *automationStartSessionStore) UpdateSessionWorkspace(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *automationStartSessionStore) SessionWriteFootprint(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s *automationStartSessionStore) UpdateSessionLineage(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *automationStartSessionStore) EndSession(_ context.Context, _ string, status string) error {
	if s.session != nil {
		s.session.Status = status
	}
	return nil
}

type automationStartSessionManager struct {
	store  *automationStartSessionStore
	starts int
}

func (m *automationStartSessionManager) StartSession(_ context.Context, project *database.Project, _ map[string]string) (*database.Session, error) {
	m.starts++
	m.store.session = &database.Session{ID: "automation-session", ProjectID: project.ID, Status: "running"}
	return m.store.session, nil
}

func (m *automationStartSessionManager) StartRemoteSession(ctx context.Context, project *database.Project, environment map[string]string, _ func(string, string) (string, error)) (*database.Session, error) {
	return m.StartSession(ctx, project, environment)
}

func (*automationStartSessionManager) ReopenSession(context.Context, *database.Session, *database.Project, map[string]string, func(string, string) (string, error)) error {
	return nil
}

func (m *automationStartSessionManager) StopSession(context.Context, string) error {
	if m.store.session != nil {
		m.store.session.Status = "stopped"
	}
	return nil
}

func (m *automationStartSessionManager) IsSessionRunning(id string) bool {
	return m.store.session != nil && m.store.session.ID == id && m.store.session.Status == "running"
}

func (*automationStartSessionManager) WriteToSession(string, []byte) error { return nil }

func (*automationStartSessionManager) GetSessionOutput(string) ([]byte, error) { return nil, nil }

type automationStartSessionLinker struct{ task *database.ProjectTask }

func (l automationStartSessionLinker) LinkSession(context.Context, application.LinkSessionTaskCommand) (*application.LinkSessionTaskResult, error) {
	return &application.LinkSessionTaskResult{Task: l.task, SessionName: l.task.Title}, nil
}

// automationInitialPromptCapture records prompts delivered by the background
// initial-prompt goroutine; release (when set) blocks delivery until closed.
type automationInitialPromptCapture struct {
	mu      sync.Mutex
	prompts []string
	release chan struct{}
	ctxErr  error
}

func (c *automationInitialPromptCapture) SubmitInitialSessionPrompt(ctx context.Context, _ string, prompt string) error {
	if c.release != nil {
		<-c.release
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ctxErr = ctx.Err()
	c.prompts = append(c.prompts, prompt)
	return nil
}

func (c *automationInitialPromptCapture) waitPrompts(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		got := append([]string(nil), c.prompts...)
		c.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type automationTaskNotificationCapture struct{ calls int }

func (c *automationTaskNotificationCapture) NotifyTaskLoaded(context.Context, string, *database.ProjectTask) error {
	c.calls++
	return nil
}

func TestAutomationSessionCreateStartsTaskImmediatelyWithoutUINotification(t *testing.T) {
	taskID := int64(9)
	tests := []struct {
		name       string
		payload    string
		wantPrompt string
	}{
		{name: "omitted start option uses default", payload: `{"task_id":9}`, wantPrompt: application.DefaultTaskStartPrompt},
		{name: "legacy false cannot request modal", payload: `{"task_id":9,"auto_start_task_prompt":false}`, wantPrompt: application.DefaultTaskStartPrompt},
		{name: "planning mode", payload: `{"task_id":9,"planning_mode":true}`, wantPrompt: application.PlanningTaskStartPrompt},
		{name: "custom prompt", payload: `{"task_id":9,"custom_prompt":"Inspect the failing flow first."}`, wantPrompt: "Inspect the failing flow first."},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &automationStartSessionStore{
				project: &database.Project{ID: 7, Name: "OpenPoet", Type: "local", Backend: "claude_code"},
				task: &database.ProjectTask{
					ID: taskID, ProjectID: 7, Title: "Fix remote start", Status: "todo", Priority: "high",
					ParentID: sql.NullInt64{},
				},
			}
			manager := &automationStartSessionManager{store: store}
			prompts := &automationInitialPromptCapture{}
			notifications := &automationTaskNotificationCapture{}
			service := application.NewSessionService(
				store, manager, nil, automationStartSessionLinker{task: store.task}, nil, nil, nil, nil,
				application.SessionCreationCollaborators{Tasks: notifications, InitialInput: prompts},
			)
			executor := &sessionPlatformExecutor{service: service, runtime: manager}
			command, err := executor.Validate(context.Background(), PlatformExecutionInput{
				Handler: "sessions.create", Target: json.RawMessage(`{"project_id":7}`), Payload: json.RawMessage(test.payload),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := command.Execute(context.Background(), application.ActionAuthorization{
				Actor: application.Actor{Type: "automation_client", ID: "helena"},
			})
			if err != nil {
				t.Fatal(err)
			}
			view, ok := result.(SessionAutomationView)
			if !ok {
				t.Fatalf("result type = %T, want SessionAutomationView", result)
			}
			if view.Status != "running" || !view.RuntimeActive {
				t.Fatalf("session did not start immediately: %+v", view)
			}
			if got := prompts.waitPrompts(t, 1); len(got) != 1 || got[0] != test.wantPrompt {
				t.Fatalf("initial prompts = %#v, want %q", got, test.wantPrompt)
			}
			if notifications.calls != 0 {
				t.Fatalf("programmatic create emitted %d UI task notifications", notifications.calls)
			}
			if manager.starts != 1 {
				t.Fatalf("session starts = %d, want 1", manager.starts)
			}
		})
	}
}

func TestAutomationSessionCreateRejectsConflictingStartOptionsBeforeStarting(t *testing.T) {
	store := &automationStartSessionStore{project: &database.Project{ID: 7, Type: "local"}}
	manager := &automationStartSessionManager{store: store}
	executor := &sessionPlatformExecutor{
		service: application.NewSessionService(store, manager, nil, nil, nil, nil, nil, nil),
		runtime: manager,
	}
	_, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: "sessions.create", Target: json.RawMessage(`{"project_id":7}`),
		Payload: json.RawMessage(`{"task_id":9,"planning_mode":true,"custom_prompt":"custom"}`),
	})
	if err == nil {
		t.Fatal("conflicting planning_mode and custom_prompt were accepted")
	}
	if manager.starts != 0 {
		t.Fatalf("validation started %d sessions", manager.starts)
	}
}

// Regression for the home-server incident: the initial prompt used to be
// delivered inside the create call, so a client timeout aborted it and the
// session ended in error. Create must return while delivery is still
// blocked, a canceled client context must not reach the delivery, and the
// session must stay running.
func TestAutomationSessionCreateReturnsBeforePromptDeliveryAndSurvivesClientCancel(t *testing.T) {
	taskID := int64(9)
	store := &automationStartSessionStore{
		project: &database.Project{ID: 12, Name: "home server", Type: "local", Backend: "claude_code"},
		task:    &database.ProjectTask{ID: taskID, ProjectID: 12, Title: "Fix home server", Status: "todo", Priority: "high"},
	}
	manager := &automationStartSessionManager{store: store}
	prompts := &automationInitialPromptCapture{release: make(chan struct{})}
	service := application.NewSessionService(
		store, manager, nil, automationStartSessionLinker{task: store.task}, nil, nil, nil, nil,
		application.SessionCreationCollaborators{InitialInput: prompts},
	)
	executor := &sessionPlatformExecutor{service: service, runtime: manager}
	command, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: "sessions.create", Target: json.RawMessage(`{"project_id":12}`), Payload: json.RawMessage(`{"task_id":9}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan any, 1)
	go func() {
		result, err := command.Execute(ctx, application.ActionAuthorization{Actor: application.Actor{Type: "automation_client", ID: "helena"}})
		if err != nil {
			done <- err
			return
		}
		done <- result
	}()
	var result any
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sessions.create blocked on initial prompt delivery")
	}
	view, ok := result.(SessionAutomationView)
	if !ok {
		t.Fatalf("create failed: %v", result)
	}
	cancel() // the client gives up after create returned
	close(prompts.release)
	if got := prompts.waitPrompts(t, 1); len(got) != 1 {
		t.Fatalf("initial prompt was not delivered after the client left: %#v", got)
	}
	prompts.mu.Lock()
	ctxErr := prompts.ctxErr
	prompts.mu.Unlock()
	if ctxErr != nil {
		t.Fatalf("initial prompt delivery saw the client's cancellation: %v", ctxErr)
	}
	if view.Status != "running" || store.session.Status != "running" || !manager.IsSessionRunning(view.ID) {
		t.Fatalf("session did not survive: view=%+v stored=%s", view, store.session.Status)
	}
}
