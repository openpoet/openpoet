package application

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/voice"
)

var phase3Actor = ActionAuthorization{Actor: Actor{Type: "agent", ID: "helena"}}

func phase3Approval() ActionAuthorization {
	return ActionAuthorization{
		Actor: Actor{Type: "agent", ID: "helena"}, Approved: true,
		ApprovedBy: "presidente", Reason: "requested operation",
	}
}

type phase3Store struct {
	project *database.Project
	session *database.Session
	task    *database.ProjectTask
	endErr  error
}

func (s *phase3Store) GetProject(context.Context, int64) (*database.Project, error) {
	if s.project == nil {
		return nil, sql.ErrNoRows
	}
	copy := *s.project
	return &copy, nil
}

func (s *phase3Store) GetSession(_ context.Context, id string) (*database.Session, error) {
	if s.session == nil || s.session.ID != id {
		return nil, sql.ErrNoRows
	}
	copy := *s.session
	return &copy, nil
}

func (s *phase3Store) GetTask(context.Context, int64) (*database.ProjectTask, error) {
	if s.task == nil {
		return nil, sql.ErrNoRows
	}
	copy := *s.task
	return &copy, nil
}

func (s *phase3Store) GetTaskForSession(context.Context, string) (*database.ProjectTask, error) {
	if s.task == nil {
		return nil, sql.ErrNoRows
	}
	copy := *s.task
	return &copy, nil
}

func (s *phase3Store) EndSession(_ context.Context, _ string, status string) error {
	if s.endErr != nil {
		return s.endErr
	}
	s.session.Status = status
	return nil
}

type phase3SessionManager struct {
	environment map[string]string
	startErr    error
	stopErr     error
	reopenErr   error
	writeErr    error
	running     bool
	writes      [][]byte
	output      []byte
}

func (m *phase3SessionManager) StartSession(_ context.Context, project *database.Project, environment map[string]string) (*database.Session, error) {
	m.environment = environment
	if m.startErr != nil {
		return nil, m.startErr
	}
	return &database.Session{ID: "created", ProjectID: project.ID, Status: "running"}, nil
}

func (m *phase3SessionManager) StartRemoteSession(ctx context.Context, project *database.Project, environment map[string]string, _ func(string, string) (string, error)) (*database.Session, error) {
	return m.StartSession(ctx, project, environment)
}

func (m *phase3SessionManager) ReopenSession(context.Context, *database.Session, *database.Project, map[string]string, func(string, string) (string, error)) error {
	return m.reopenErr
}

func (m *phase3SessionManager) StopSession(context.Context, string) error { return m.stopErr }
func (m *phase3SessionManager) IsSessionRunning(string) bool              { return m.running }
func (m *phase3SessionManager) WriteToSession(_ string, data []byte) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.writes = append(m.writes, append([]byte(nil), data...))
	return nil
}
func (m *phase3SessionManager) GetSessionOutput(string) ([]byte, error) {
	return append([]byte(nil), m.output...), nil
}

type phase3SessionEffects struct{ changes []SessionChange }

func (e *phase3SessionEffects) PublishSessionChange(_ context.Context, change SessionChange) {
	e.changes = append(e.changes, change)
}

func TestSessionServiceCreateGuardsEnvironmentAndPublishesAfterStart(t *testing.T) {
	store := &phase3Store{project: &database.Project{ID: 7, Type: "local"}}
	manager := &phase3SessionManager{}
	effects := &phase3SessionEffects{}
	service := NewSessionService(store, manager, nil, nil, nil, nil, nil, effects)

	_, err := service.Create(context.Background(), CreateSessionCommand{
		ProjectID: 7, Environment: map[string]string{"TOKEN": "value"}, Authorization: phase3Actor,
	})
	if !ErrorIsKind(err, ErrorValidation) || manager.environment != nil || len(effects.changes) != 0 {
		t.Fatalf("unapproved environment must be rejected before start: err=%v env=%v effects=%d", err, manager.environment, len(effects.changes))
	}

	authorized := phase3Approval()
	authorized.AllowEnvironment = true
	created, err := service.Create(context.Background(), CreateSessionCommand{
		ProjectID: 7, Environment: map[string]string{"TOKEN": "value"}, Authorization: authorized,
	})
	if err != nil || created.ID != "created" || manager.environment["TOKEN"] != "value" {
		t.Fatalf("authorized create failed: session=%v env=%v err=%v", created, manager.environment, err)
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "created" {
		t.Fatalf("successful create must publish exactly once: %#v", effects.changes)
	}
}

func TestSessionServiceStateAndEffectBoundaries(t *testing.T) {
	persistErr := errors.New("persist failed")
	store := &phase3Store{
		project: &database.Project{ID: 7, Type: "local"},
		session: &database.Session{ID: "s1", ProjectID: 7, Status: "running"},
		endErr:  persistErr,
	}
	manager := &phase3SessionManager{running: true}
	effects := &phase3SessionEffects{}
	service := NewSessionService(store, manager, nil, nil, nil, nil, nil, effects)

	if _, err := service.Stop(context.Background(), StopSessionCommand{SessionID: "s1", Authorization: phase3Actor}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("stop must require explicit approval, got %v", err)
	}
	if _, err := service.Stop(context.Background(), StopSessionCommand{SessionID: "s1", Authorization: phase3Approval()}); !errors.Is(err, persistErr) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	if len(effects.changes) != 0 {
		t.Fatalf("failed persistence must not publish effects: %#v", effects.changes)
	}

	store.endErr = nil
	if _, err := service.Stop(context.Background(), StopSessionCommand{SessionID: "s1", Authorization: phase3Approval()}); err != nil {
		t.Fatal(err)
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "stopped" {
		t.Fatalf("successful stop must publish once: %#v", effects.changes)
	}
	if err := service.SendInput(context.Background(), SendSessionInputCommand{SessionID: "s1", Text: "hello", Authorization: phase3Actor}); !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("input into stopped session must conflict, got %v", err)
	}
}

type phase3HookPort struct {
	err      error
	response HookPermissionResponse
	task     string
}

func (p *phase3HookPort) RespondPermission(_ context.Context, _ string, response HookPermissionResponse) error {
	p.response = response
	return p.err
}

func (p *phase3HookPort) RespondTaskNotification(_ context.Context, sessionID string) error {
	p.task = sessionID
	return p.err
}

type phase3HookEffects struct{ changes []HookResponseChange }

func (e *phase3HookEffects) PublishHookResponse(_ context.Context, change HookResponseChange) {
	e.changes = append(e.changes, change)
}

func TestHookResponseServiceRequiresApprovalAndPublishesPostSuccess(t *testing.T) {
	port := &phase3HookPort{err: errors.New("not pending")}
	effects := &phase3HookEffects{}
	service := NewHookResponseService(port, effects)
	command := RespondPermissionCommand{
		SessionID: "s1", Response: HookPermissionResponse{Behavior: "allow"}, Authorization: phase3Approval(),
	}
	if err := service.RespondPermission(context.Background(), command); err == nil || len(effects.changes) != 0 {
		t.Fatalf("failed response must not publish: err=%v effects=%v", err, effects.changes)
	}
	port.err = nil
	if err := service.RespondPermission(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(effects.changes) != 1 || effects.changes[0].Behavior != "allow" {
		t.Fatalf("unexpected hook effects: %#v", effects.changes)
	}
	command.Authorization = phase3Actor
	if err := service.RespondPermission(context.Background(), command); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("permission response must require approval, got %v", err)
	}
}

type phase3Watcher struct {
	status  string
	reason  string
	err     error
	stopped bool
}

func (w *phase3Watcher) StartSessionWatcher(context.Context, string) (string, string, error) {
	return w.status, w.reason, w.err
}

func (w *phase3Watcher) StopSessionWatcher(context.Context, string) (bool, error) {
	return w.stopped, w.err
}

type phase3WatcherEffects struct{ changes []SessionWatcherChange }

func (e *phase3WatcherEffects) PublishSessionWatcherChange(_ context.Context, change SessionWatcherChange) {
	e.changes = append(e.changes, change)
}

func TestSessionWatcherServiceHonorsStateAndUnavailableResult(t *testing.T) {
	store := &phase3Store{session: &database.Session{ID: "s1", Status: "completed"}}
	watcher := &phase3Watcher{status: "unavailable", reason: "unsupported_backend"}
	effects := &phase3WatcherEffects{}
	service := NewSessionEventWatcherService(store, watcher, effects)
	if _, err := service.Start(context.Background(), StartSessionWatcherCommand{SessionID: "s1", Authorization: phase3Actor}); !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("completed session must not start watcher, got %v", err)
	}
	store.session.Status = "running"
	result, err := service.Start(context.Background(), StartSessionWatcherCommand{SessionID: "s1", Authorization: phase3Actor})
	if err != nil || result.Status != "unavailable" || result.Reason != "unsupported_backend" {
		t.Fatalf("unexpected watcher result: %#v err=%v", result, err)
	}
	if len(effects.changes) != 1 {
		t.Fatalf("successful unavailable resolution is observable: %#v", effects.changes)
	}
}

type phase3FilePort struct {
	writes []FileWrite
	err    error
}

func (p *phase3FilePort) WriteFiles(_ context.Context, _ *database.Project, writes []FileWrite) error {
	if p.err != nil {
		return p.err
	}
	p.writes = append([]FileWrite(nil), writes...)
	return nil
}

type phase3FileEffects struct{ changes []FileMutationChange }

func (e *phase3FileEffects) PublishFileMutation(_ context.Context, change FileMutationChange) {
	e.changes = append(e.changes, change)
}

func TestFileMutationServiceConfinesPathsAndPublishesPostSuccess(t *testing.T) {
	store := &phase3Store{project: &database.Project{ID: 7, Type: "local"}}
	port := &phase3FilePort{}
	effects := &phase3FileEffects{}
	service := NewFileMutationService(store, port, effects)

	_, err := service.UploadProjectFile(context.Background(), UploadProjectFileCommand{
		ProjectID: 7, Path: "../../outside", Data: []byte("x"), Authorization: phase3Approval(),
	})
	if !ErrorIsKind(err, ErrorValidation) || len(port.writes) != 0 {
		t.Fatalf("path traversal reached file adapter: err=%v writes=%v", err, port.writes)
	}

	port.err = errors.New("disk full")
	_, err = service.UploadProjectFile(context.Background(), UploadProjectFileCommand{
		ProjectID: 7, Path: "safe/file.txt", Data: []byte("x"), Authorization: phase3Approval(),
	})
	if err == nil || len(effects.changes) != 0 {
		t.Fatalf("failed write must not publish: err=%v effects=%v", err, effects.changes)
	}

	port.err = nil
	write, err := service.UploadProjectFile(context.Background(), UploadProjectFileCommand{
		ProjectID: 7, Path: "safe/./file.txt", Data: []byte("x"), Authorization: phase3Approval(),
	})
	if err != nil || write.Path != "safe/file.txt" || len(effects.changes) != 1 {
		t.Fatalf("safe normalized write failed: write=%#v err=%v effects=%v", write, err, effects.changes)
	}
}

func TestPasteImageUsesBoundedDecodedDataAndSafeGeneratedPath(t *testing.T) {
	store := &phase3Store{
		project: &database.Project{ID: 7, Type: "local"},
		session: &database.Session{ID: "s1", ProjectID: 7, Status: "running"},
	}
	port := &phase3FilePort{}
	service := NewFileMutationService(store, port, nil)
	service.now = func() time.Time { return time.Unix(0, 123) }
	write, err := service.PasteSessionImage(context.Background(), PasteSessionImageCommand{
		SessionID: "s1", Directory: "images", DataURL: "data:image/png;base64,eA==", Authorization: phase3Actor,
	})
	if err != nil || write.Path != "images/paste_123.png" || string(write.Data) != "x" {
		t.Fatalf("unexpected paste result: %#v err=%v", write, err)
	}
}

type phase3GitPort struct {
	ensured bool
	paths   []string
	message string
	err     error
}

func (p *phase3GitPort) EnsureRepository(context.Context, *database.Project) error {
	p.ensured = true
	return p.err
}
func (p *phase3GitPort) Stage(_ context.Context, _ *database.Project, paths []string) error {
	p.paths = append([]string(nil), paths...)
	return p.err
}
func (p *phase3GitPort) Unstage(_ context.Context, _ *database.Project, paths []string) error {
	p.paths = append([]string(nil), paths...)
	return p.err
}
func (p *phase3GitPort) Commit(_ context.Context, _ *database.Project, message string) (GitCommitResult, error) {
	p.message = message
	return GitCommitResult{Hash: "abc123", Message: "ok"}, p.err
}

type phase3GitEffects struct{ changes []GitMutationChange }

func (e *phase3GitEffects) PublishGitMutation(_ context.Context, change GitMutationChange) {
	e.changes = append(e.changes, change)
}

func TestGitMutationServiceUsesTypedPathsWithoutExecution(t *testing.T) {
	store := &phase3Store{project: &database.Project{ID: 7}}
	port := &phase3GitPort{}
	effects := &phase3GitEffects{}
	service := NewGitMutationService(store, port, effects)
	files := []string{"src/a.go", "docs/read me.md"}
	if err := service.Stage(context.Background(), StageGitFilesCommand{ProjectID: 7, Files: files, Authorization: phase3Actor}); err != nil {
		t.Fatal(err)
	}
	if !port.ensured || !reflect.DeepEqual(port.paths, files) || len(effects.changes) != 1 {
		t.Fatalf("typed git adapter received unexpected data: ensured=%v paths=%v effects=%v", port.ensured, port.paths, effects.changes)
	}
	if err := service.Stage(context.Background(), StageGitFilesCommand{ProjectID: 7, Files: []string{"../secret"}, Authorization: phase3Actor}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("git traversal must be rejected, got %v", err)
	}
	if _, err := service.Commit(context.Background(), CommitGitCommand{ProjectID: 7, Message: "test", Authorization: phase3Actor}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("commit must require explicit approval, got %v", err)
	}
}

type phase3VoicePort struct {
	data     []byte
	filename string
	language string
	err      error
}

func (p *phase3VoicePort) TranscribeAudio(_ context.Context, data []byte, filename, language string) (*voice.TranscriptionResult, error) {
	p.data, p.filename, p.language = data, filename, language
	if p.err != nil {
		return nil, p.err
	}
	return &voice.TranscriptionResult{Text: "transcript"}, nil
}

func TestVoiceTranscriptionServiceValidatesBeforeAdapter(t *testing.T) {
	port := &phase3VoicePort{}
	service := NewVoiceTranscriptionService(port)
	if _, err := service.Transcribe(context.Background(), TranscribeVoiceCommand{
		Audio: []byte("audio"), Filename: "../recording.webm", Authorization: phase3Actor,
	}); !ErrorIsKind(err, ErrorValidation) || port.data != nil {
		t.Fatalf("unsafe filename reached adapter: err=%v data=%v", err, port.data)
	}
	result, err := service.Transcribe(context.Background(), TranscribeVoiceCommand{
		Audio: []byte("audio"), Filename: "recording.webm", Language: "pt-BR", Authorization: phase3Actor,
	})
	if err != nil || result.Text != "transcript" || port.filename != "recording.webm" || port.language != "pt-BR" {
		t.Fatalf("unexpected voice result: %#v port=%#v err=%v", result, port, err)
	}
}

func TestPhase3BoundedInputs(t *testing.T) {
	manager := &phase3SessionManager{running: true}
	store := &phase3Store{session: &database.Session{ID: "s1", Status: "running"}}
	service := NewSessionService(store, manager, nil, nil, nil, nil, nil, nil)
	if err := service.SendInput(context.Background(), SendSessionInputCommand{
		SessionID: "s1", Text: strings.Repeat("x", maxSessionInputBytes+1), Authorization: phase3Actor,
	}); !ErrorIsKind(err, ErrorValidation) || len(manager.writes) != 0 {
		t.Fatalf("oversized session input reached manager: err=%v writes=%d", err, len(manager.writes))
	}
}
