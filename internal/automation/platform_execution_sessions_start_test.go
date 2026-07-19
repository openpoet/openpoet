package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

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

func (s *automationStartSessionStore) GetMission(_ context.Context, _ int64) (*database.Mission, error) {
	return nil, nil
}

func (s *automationStartSessionStore) UpsertMissionWorker(_ context.Context, _ *database.MissionWorker) error {
	return nil
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

type automationInitialPromptCapture struct{ prompts []string }

func (c *automationInitialPromptCapture) SubmitInitialSessionPrompt(_ context.Context, _ string, prompt string) error {
	c.prompts = append(c.prompts, prompt)
	return nil
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
			if len(prompts.prompts) != 1 || prompts.prompts[0] != test.wantPrompt {
				t.Fatalf("initial prompts = %#v, want %q", prompts.prompts, test.wantPrompt)
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
