package automation

import (
	"context"
	"database/sql"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessionprompt"
)

// closeCompletedStore is the SessionStore slice sessions.close_completed uses.
type closeCompletedStore struct {
	session *database.Session
	task    *database.ProjectTask
}

func (s *closeCompletedStore) GetProject(context.Context, int64) (*database.Project, error) {
	return nil, sql.ErrNoRows
}
func (s *closeCompletedStore) GetSession(context.Context, string) (*database.Session, error) {
	copy := *s.session
	return &copy, nil
}
func (s *closeCompletedStore) GetTask(context.Context, int64) (*database.ProjectTask, error) {
	copy := *s.task
	return &copy, nil
}
func (s *closeCompletedStore) GetTaskForSession(context.Context, string) (*database.ProjectTask, error) {
	return s.GetTask(context.Background(), 0)
}
func (s *closeCompletedStore) EndSession(_ context.Context, _ string, status string) error {
	s.session.Status = status
	return nil
}
func (s *closeCompletedStore) UpdateSessionLineage(context.Context, string, string, string) error {
	return nil
}
func (s *closeCompletedStore) UpdateSessionWorkspace(context.Context, string, string, string) error {
	return nil
}
func (s *closeCompletedStore) SessionWriteFootprint(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s *closeCompletedStore) ProjectIDsForTags(context.Context, []int64) ([]int64, error) {
	return nil, nil
}

type closeCompletedManager struct{ stopped int }

func (m *closeCompletedManager) StartSession(context.Context, *database.Project, map[string]string) (*database.Session, error) {
	return nil, sql.ErrNoRows
}
func (m *closeCompletedManager) StartRemoteSession(context.Context, *database.Project, map[string]string, func(string, string) (string, error)) (*database.Session, error) {
	return nil, sql.ErrNoRows
}
func (m *closeCompletedManager) ReopenSession(context.Context, *database.Session, *database.Project, map[string]string, func(string, string) (string, error)) error {
	return nil
}
func (m *closeCompletedManager) StopSession(context.Context, string) error { m.stopped++; return nil }
func (m *closeCompletedManager) IsSessionRunning(string) bool              { return m.stopped == 0 }
func (m *closeCompletedManager) WriteToSession(string, []byte) error       { return nil }
func (m *closeCompletedManager) GetSessionOutput(string) ([]byte, error)   { return nil, nil }

type idleSignals struct{}

func (idleSignals) RegisterPromptWaiter(string) (<-chan struct{}, func()) { return nil, func() {} }
func (idleSignals) GetSessionMode(string) string                          { return "idle" }

func closeCompletedExecutor(question *sessionprompt.Question) (*sessionPlatformExecutor, *closeCompletedStore, *closeCompletedManager) {
	store := &closeCompletedStore{
		session: &database.Session{ID: "s1", ProjectID: 20, Status: "running", TaskID: sql.NullInt64{Int64: 849, Valid: true}},
		task:    &database.ProjectTask{ID: 849, ProjectID: 20, Status: application.TaskStatusDone},
	}
	manager := &closeCompletedManager{}
	service := application.NewSessionService(store, manager, nil, nil, nil, nil, nil, nil,
		application.SessionCreationCollaborators{Signals: idleSignals{}})
	queries := questionSessionQueries{session: store.session}
	return &sessionPlatformExecutor{
		service:   service,
		questions: application.NewSessionQuestionService(queries, &questionPort{question: question}, nil),
		queries:   queries, runtime: manager,
	}, store, manager
}

func runCloseCompleted(t *testing.T, executor *sessionPlatformExecutor, scope *ProjectScopeSet) (any, error) {
	t.Helper()
	command, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: "sessions.close_completed", Target: []byte(`{"id":"s1"}`), Payload: []byte(`{}`), ProjectScope: scope,
	})
	if err != nil {
		return nil, err
	}
	return command.Execute(context.Background(), application.ActionAuthorization{
		Actor: application.Actor{Type: "automation_client", ID: "helena"}, Approved: true, ApprovedBy: "helena", Reason: "task done",
	})
}

// sessions.close_completed is published as a policy-approved write, so an agent
// can tidy up finished sessions without a human approval_token per session.
func TestCloseCompletedIsDescribedAsPolicyApprovedWrite(t *testing.T) {
	for _, definition := range sessionPlatformDefinitions() {
		if definition.Name != "sessions.close_completed" {
			continue
		}
		if definition.Risk != application.CapabilityRiskWrite || definition.Approval != application.ApprovalByPolicy {
			t.Fatalf("risk=%s approval=%s, want write/policy", definition.Risk, definition.Approval)
		}
		if definition.Payload == nil || definition.Payload.Notes == "" {
			t.Fatalf("close_completed is not self-describing: %+v", definition.Payload)
		}
		return
	}
	t.Fatal("sessions.close_completed is not registered")
}

func TestCloseCompletedStopsIdleDoneSessionThroughExecutor(t *testing.T) {
	executor, store, manager := closeCompletedExecutor(nil)
	if _, err := runCloseCompleted(t, executor, &ProjectScopeSet{Allowed: map[int64]bool{47: true}}); err == nil || store.session.Status != "running" {
		t.Fatalf("close outside the client's project_filter: err=%v status=%s", err, store.session.Status)
	}
	result, err := runCloseCompleted(t, executor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view := result.(SessionAutomationView); view.Status != "stopped" || manager.stopped != 1 {
		t.Fatalf("view=%+v stops=%d", view, manager.stopped)
	}
}

func TestCloseCompletedRefusesSessionAwaitingInput(t *testing.T) {
	executor, store, manager := closeCompletedExecutor(&sessionprompt.Question{ID: "t_q", Text: "Proceed?"})
	if _, err := runCloseCompleted(t, executor, nil); err == nil || store.session.Status != "running" || manager.stopped != 0 {
		t.Fatalf("closed a session awaiting input: err=%v status=%s", err, store.session.Status)
	}
}
