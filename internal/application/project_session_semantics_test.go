package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"openpoet/internal/database"
)

type semanticPathValidator struct{ calls []string }

func (v *semanticPathValidator) ValidateProjectPath(_ context.Context, path, projectType string) error {
	v.calls = append(v.calls, projectType+":"+path)
	if projectType == "local" && path == "/definitely/not/present" {
		return errors.New("directory does not exist")
	}
	return nil
}

func TestProjectServiceAppliesSharedPathValidationToLocalAndRemoteInputs(t *testing.T) {
	store := newFakeProjectStore()
	paths := &semanticPathValidator{}
	service := NewProjectService(store, fakeEncryptor{}, nil, paths)
	if _, err := service.Create(context.Background(), database.ProjectInput{
		Name: "missing", Path: "/definitely/not/present", Type: "local", Backend: "claude_code",
	}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("local nonexistent path must be rejected, got %v", err)
	}
	remote, err := service.Create(context.Background(), database.ProjectInput{
		Name: "remote", Path: "/definitely/not/present", Type: "remote", Backend: "claude_code",
		SSHHost: "host", SSHPort: 22, SSHUser: "user", SSHAuthType: "password", SSHCredential: "secret",
	})
	if err != nil || remote == nil {
		t.Fatalf("remote path must not require a local directory: project=%v err=%v", remote, err)
	}
	if len(paths.calls) != 2 {
		t.Fatalf("path validator calls = %v", paths.calls)
	}
}

func TestProjectServiceClearsBackendConfigWhenBackendChanges(t *testing.T) {
	service := NewProjectService(nil, nil, nil)
	current := &database.Project{
		ID: 7, Name: "Switched", Path: t.TempDir(), Type: "local", Backend: "codex",
		BackendConfig: `{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}`,
	}
	updated, err := service.projectFromInput(context.Background(), database.ProjectInput{
		Name: current.Name, Path: current.Path, Type: current.Type, Backend: "claude_code",
	}, current)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BackendConfig != "{}" {
		t.Fatalf("backend config = %q, want cleared object", updated.BackendConfig)
	}

	updated, err = service.projectFromInput(context.Background(), database.ProjectInput{
		Name: current.Name, Path: current.Path, Type: current.Type, Backend: "claude_code",
		BackendConfig: `{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}`,
	}, current)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BackendConfig != "{}" {
		t.Fatalf("explicit foreign backend config = %q, want cleared object", updated.BackendConfig)
	}
}

func TestProjectServiceDuplicateClearsConfigWhenBackendChanges(t *testing.T) {
	store := newFakeProjectStore()
	service := NewProjectService(store, fakeEncryptor{}, nil)
	original, err := service.Create(context.Background(), database.ProjectInput{
		Name: "Codex", Path: t.TempDir(), Type: "local", Backend: "codex",
		BackendConfig: `{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.Duplicate(context.Background(), DuplicateProjectCommand{
		ProjectID: original.ID,
		Overrides: database.ProjectInput{Name: "Claude", Backend: "claude_code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.BackendConfig != "{}" {
		t.Fatalf("duplicate backend config = %q, want cleared object", duplicate.BackendConfig)
	}
}

type semanticSessionStore struct {
	project *database.Project
	task    *database.ProjectTask
	ended   string
}

func (s *semanticSessionStore) GetProject(context.Context, int64) (*database.Project, error) {
	copy := *s.project
	return &copy, nil
}
func (s *semanticSessionStore) GetSession(context.Context, string) (*database.Session, error) {
	return nil, sql.ErrNoRows
}
func (s *semanticSessionStore) GetTask(context.Context, int64) (*database.ProjectTask, error) {
	if s.task == nil {
		return nil, sql.ErrNoRows
	}
	copy := *s.task
	return &copy, nil
}
func (s *semanticSessionStore) GetTaskForSession(context.Context, string) (*database.ProjectTask, error) {
	return nil, sql.ErrNoRows
}
func (s *semanticSessionStore) ProjectIDsForTags(_ context.Context, _ []int64) ([]int64, error) {
	return nil, nil
}

func (s *semanticSessionStore) GetMission(_ context.Context, _ int64) (*database.Mission, error) {
	return nil, nil
}

func (s *semanticSessionStore) UpsertMissionWorker(_ context.Context, _ *database.MissionWorker) error {
	return nil
}

func (s *semanticSessionStore) UpdateSessionWorkspace(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *semanticSessionStore) SessionWriteFootprint(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s *semanticSessionStore) UpdateSessionLineage(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *semanticSessionStore) EndSession(_ context.Context, _ string, status string) error {
	s.ended = status
	return nil
}

type semanticSessionManager struct {
	starts      int
	environment map[string]string
	writes      [][]byte
}

func (m *semanticSessionManager) StartSession(_ context.Context, project *database.Project, environment map[string]string) (*database.Session, error) {
	m.starts++
	m.environment = environment
	return &database.Session{ID: "session-1", ProjectID: project.ID, Status: "running"}, nil
}
func (m *semanticSessionManager) StartRemoteSession(ctx context.Context, project *database.Project, environment map[string]string, _ func(string, string) (string, error)) (*database.Session, error) {
	return m.StartSession(ctx, project, environment)
}
func (*semanticSessionManager) ReopenSession(context.Context, *database.Session, *database.Project, map[string]string, func(string, string) (string, error)) error {
	return nil
}
func (*semanticSessionManager) StopSession(context.Context, string) error { return nil }
func (*semanticSessionManager) IsSessionRunning(string) bool              { return false }
func (m *semanticSessionManager) WriteToSession(_ string, value []byte) error {
	m.writes = append(m.writes, append([]byte(nil), value...))
	return nil
}
func (*semanticSessionManager) GetSessionOutput(string) ([]byte, error) { return nil, nil }

type semanticSessionEnvironment struct{ values map[string]string }

func (p semanticSessionEnvironment) SessionEnvironment(context.Context, *database.Project) (map[string]string, error) {
	return p.values, nil
}

type semanticSessionNames struct{ id, name string }

func (s *semanticSessionNames) RenameSession(_ context.Context, id, name string) error {
	s.id, s.name = id, name
	return nil
}

type semanticTaskLinker struct{ task *database.ProjectTask }

func (l semanticTaskLinker) LinkSession(context.Context, LinkSessionTaskCommand) (*LinkSessionTaskResult, error) {
	return &LinkSessionTaskResult{Task: l.task, SessionName: l.task.Title}, nil
}

type semanticTaskNotifier struct {
	session string
	task    int64
}

func (n *semanticTaskNotifier) NotifyTaskLoaded(_ context.Context, session string, task *database.ProjectTask) error {
	n.session, n.task = session, task.ID
	return nil
}

type semanticSyncer struct{ err error }

func (s semanticSyncer) SyncToProject(context.Context, *database.Project) error { return s.err }

func TestSessionServiceCreateOwnsProviderEnvironmentNamingAndTaskNotification(t *testing.T) {
	project := &database.Project{ID: 7, Name: "OpenPoet", Type: "local", Backend: "claude_code"}
	task := &database.ProjectTask{ID: 9, ProjectID: 7, Title: "Integrate Helena", Description: "full integration"}
	store := &semanticSessionStore{project: project, task: task}
	manager := &semanticSessionManager{}
	names := &semanticSessionNames{}
	notifier := &semanticTaskNotifier{}
	effects := &phase3SessionEffects{}
	service := NewSessionService(store, manager, semanticSyncer{}, semanticTaskLinker{task: task}, nil, nil, nil, effects, SessionCreationCollaborators{
		Environment: semanticSessionEnvironment{values: map[string]string{"ANTHROPIC_API_KEY": "runtime-secret"}},
		Names:       names, Tasks: notifier, Now: func() time.Time { return time.Date(2026, 7, 9, 14, 5, 6, 0, time.UTC) },
	})

	taskID := task.ID
	created, err := service.Create(context.Background(), CreateSessionCommand{ProjectID: project.ID, TaskID: &taskID, Authorization: phase3Actor})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != task.Title || manager.environment["ANTHROPIC_API_KEY"] != "runtime-secret" {
		t.Fatalf("session=%+v environment=%v", created, manager.environment)
	}
	if notifier.session != created.ID || notifier.task != task.ID {
		t.Fatalf("task notification = %+v", notifier)
	}
	effectJSON, _ := json.Marshal(effects.changes)
	if string(effectJSON) == "" || containsBytes(effectJSON, []byte("runtime-secret")) {
		t.Fatalf("provider secret crossed effect boundary: %s", effectJSON)
	}

	store.task = nil
	created, err = service.Create(context.Background(), CreateSessionCommand{ProjectID: project.ID, Authorization: phase3Actor})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "OpenPoet (14:05:06)" || names.id != created.ID || names.name != created.Name {
		t.Fatalf("automatic name not persisted: session=%+v names=%+v", created, names)
	}
}

func TestMergeTrustedSessionEnvironmentAllowsRemoteProviderTunnelMarker(t *testing.T) {
	destination := map[string]string{}
	if err := mergeTrustedSessionEnvironment(destination, map[string]string{
		"OPENPOET_REMOTE_PROVIDER_TUNNEL": "1",
	}); err != nil {
		t.Fatalf("trusted tunnel marker must be accepted: %v", err)
	}
	if destination["OPENPOET_REMOTE_PROVIDER_TUNNEL"] != "1" {
		t.Fatalf("tunnel marker not merged: %v", destination)
	}
}

func TestMergeTrustedSessionEnvironmentRejectsUnknownOpenPoetKey(t *testing.T) {
	err := mergeTrustedSessionEnvironment(map[string]string{}, map[string]string{
		"OPENPOET_UNTRUSTED": "1",
	})
	if !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("unknown OPENPOET_ key must be rejected, got %v", err)
	}
}

func TestSessionServiceCreateTreatsConfigurationSyncFailureAsBestEffort(t *testing.T) {
	manager := &semanticSessionManager{}
	service := NewSessionService(
		&semanticSessionStore{project: &database.Project{ID: 7, Name: "OpenPoet", Type: "local", Backend: "claude_code"}},
		manager, semanticSyncer{err: errors.New("sync failed")}, nil, nil, nil, nil, nil,
	)
	if _, err := service.Create(context.Background(), CreateSessionCommand{ProjectID: 7, Authorization: phase3Actor}); err != nil {
		t.Fatalf("configuration sync failure must not abort session creation: %v", err)
	}
	if manager.starts != 1 {
		t.Fatalf("manager should start despite sync failure: %d", manager.starts)
	}
}

func containsBytes(value, target []byte) bool {
	if len(target) == 0 || len(value) < len(target) {
		return false
	}
	for i := 0; i <= len(value)-len(target); i++ {
		matched := true
		for j := range target {
			if value[i+j] != target[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
