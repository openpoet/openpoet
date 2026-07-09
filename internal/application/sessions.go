package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"openpoet/internal/database"
	runtime "openpoet/internal/session"
)

const (
	maxSessionEnvironmentVariables = 64
	maxSessionEnvironmentBytes     = 64 << 10
	maxSessionInputBytes           = 16 << 10
	maxEvaluationSnapshotBytes     = 64 << 10
)

var sessionEnvironmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type SessionStore interface {
	GetProject(context.Context, int64) (*database.Project, error)
	GetSession(context.Context, string) (*database.Session, error)
	GetTask(context.Context, int64) (*database.ProjectTask, error)
	GetTaskForSession(context.Context, string) (*database.ProjectTask, error)
	EndSession(context.Context, string, string) error
}

type SessionManager interface {
	StartSession(context.Context, *database.Project, map[string]string) (*database.Session, error)
	StartRemoteSession(context.Context, *database.Project, map[string]string, func(string, string) (string, error)) (*database.Session, error)
	ReopenSession(context.Context, *database.Session, *database.Project, map[string]string, func(string, string) (string, error)) error
	StopSession(context.Context, string) error
	IsSessionRunning(string) bool
	WriteToSession(string, []byte) error
	GetSessionOutput(string) ([]byte, error)
}

type SessionConfigurationSyncer interface {
	SyncToProject(context.Context, *database.Project) error
}

type SessionTaskLinker interface {
	LinkSession(context.Context, LinkSessionTaskCommand) (*LinkSessionTaskResult, error)
}

type SessionEvaluator interface {
	EvaluateSession(context.Context, string, string, []byte) bool
}

type SessionPromptHintStore interface {
	StoreImagePromptHint(context.Context, string, string, int) error
}

type SessionChange struct {
	Action  string
	Session *database.Session
	ID      string
	Actor   Actor
}

type SessionEffects interface {
	PublishSessionChange(context.Context, SessionChange)
}

type SessionService struct {
	store      SessionStore
	manager    SessionManager
	syncer     SessionConfigurationSyncer
	linker     SessionTaskLinker
	evaluator  SessionEvaluator
	promptHint SessionPromptHintStore
	decrypt    func(string, string) (string, error)
	effects    SessionEffects
}

func NewSessionService(
	store SessionStore,
	manager SessionManager,
	syncer SessionConfigurationSyncer,
	linker SessionTaskLinker,
	evaluator SessionEvaluator,
	promptHint SessionPromptHintStore,
	decrypt func(string, string) (string, error),
	effects SessionEffects,
) *SessionService {
	return &SessionService{
		store: store, manager: manager, syncer: syncer, linker: linker,
		evaluator: evaluator, promptHint: promptHint, decrypt: decrypt, effects: effects,
	}
}

type CreateSessionCommand struct {
	ProjectID                  int64
	TaskID                     *int64
	Environment                map[string]string
	DangerouslySkipPermissions bool
	AutoStartTaskPrompt        bool
	Authorization              ActionAuthorization
}

func (s *SessionService) Create(ctx context.Context, command CreateSessionCommand) (*database.Session, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	if command.ProjectID <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}
	project, err := s.store.GetProject(ctx, command.ProjectID)
	if err != nil || project == nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	environment, err := normalizeSessionEnvironment(command.Environment, command.Authorization)
	if err != nil {
		return nil, err
	}
	var linkedTask *database.ProjectTask
	if command.TaskID != nil {
		if *command.TaskID <= 0 {
			return nil, validationError("invalid_task_id", "Task ID must be positive")
		}
		linkedTask, err = s.store.GetTask(ctx, *command.TaskID)
		if err != nil || linkedTask == nil || linkedTask.ProjectID != project.ID {
			return nil, validationError("task_project_mismatch", "Task does not belong to the project")
		}
		injectTaskEnvironment(environment, linkedTask, false)
	}
	if command.DangerouslySkipPermissions {
		if !project.DangerouslySkipPermissions || !command.Authorization.AllowUnsafePermissions {
			return nil, validationError("unsafe_permissions_forbidden", "Unsafe permissions are not allowed for this project and authorization")
		}
		if err := requireExplicitActionApproval(command.Authorization); err != nil {
			return nil, err
		}
		environment["OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS"] = "true"
	}
	if s.syncer != nil {
		if err := s.syncer.SyncToProject(ctx, project); err != nil {
			return nil, fmt.Errorf("sync project configuration: %w", err)
		}
	}
	if s.manager == nil {
		return nil, validationError("session_manager_unavailable", "Session manager is unavailable")
	}
	var created *database.Session
	if project.Type == "remote" {
		if s.decrypt == nil {
			return nil, validationError("session_decryptor_unavailable", "Remote session decryptor is unavailable")
		}
		created, err = s.manager.StartRemoteSession(ctx, project, environment, s.decrypt)
	} else {
		created, err = s.manager.StartSession(ctx, project, environment)
	}
	if err != nil {
		return nil, err
	}
	if linkedTask != nil {
		if s.linker == nil {
			s.compensateFailedCreate(ctx, created.ID)
			return nil, validationError("session_task_linker_unavailable", "Session task linker is unavailable")
		}
		result, linkErr := s.linker.LinkSession(ctx, LinkSessionTaskCommand{
			SessionID: created.ID, TaskID: &linkedTask.ID, Actor: command.Authorization.Actor,
		})
		if linkErr != nil {
			s.compensateFailedCreate(ctx, created.ID)
			return nil, linkErr
		}
		created.Name = result.SessionName
		if command.AutoStartTaskPrompt {
			if err := s.writeLine(created.ID, "Start working on the assigned task."); err != nil {
				s.compensateFailedCreate(ctx, created.ID)
				return nil, err
			}
		}
	}
	s.publish(ctx, SessionChange{Action: "created", Session: created, ID: created.ID, Actor: command.Authorization.Actor})
	return created, nil
}

type StopSessionCommand struct {
	SessionID     string
	Authorization ActionAuthorization
}

func (s *SessionService) Stop(ctx context.Context, command StopSessionCommand) (*database.Session, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return nil, err
	}
	session, err := s.getSession(ctx, command.SessionID)
	if err != nil {
		return nil, err
	}
	if session.Status == "stopped" {
		return session, nil
	}
	if session.Status != "starting" && session.Status != "running" {
		return nil, conflictError("session_not_stoppable", "Only starting or running sessions can be stopped")
	}
	if s.manager == nil {
		return nil, validationError("session_manager_unavailable", "Session manager is unavailable")
	}
	if err := s.manager.StopSession(ctx, session.ID); err != nil && !errors.Is(err, runtime.ErrSessionNotRunning) {
		return nil, err
	}
	if err := s.store.EndSession(ctx, session.ID, "stopped"); err != nil {
		return nil, err
	}
	stored, err := s.store.GetSession(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	s.publish(ctx, SessionChange{Action: "stopped", Session: stored, ID: stored.ID, Actor: command.Authorization.Actor})
	return stored, nil
}

type ReopenSessionCommand struct {
	SessionID                  string
	DangerouslySkipPermissions bool
	Authorization              ActionAuthorization
}

func (s *SessionService) Reopen(ctx context.Context, command ReopenSessionCommand) (*database.Session, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	session, err := s.getSession(ctx, command.SessionID)
	if err != nil {
		return nil, err
	}
	if session.Status != "stopped" && session.Status != "completed" {
		return nil, conflictError("session_not_reopenable", "Only stopped or completed sessions can be reopened")
	}
	if s.manager == nil {
		return nil, validationError("session_manager_unavailable", "Session manager is unavailable")
	}
	if s.manager.IsSessionRunning(session.ID) {
		return nil, conflictError("session_already_running", "Session is already running")
	}
	project, err := s.store.GetProject(ctx, session.ProjectID)
	if err != nil || project == nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	environment := map[string]string{}
	if linkedTask, _ := s.store.GetTaskForSession(ctx, session.ID); linkedTask != nil {
		injectTaskEnvironment(environment, linkedTask, true)
	}
	if command.DangerouslySkipPermissions {
		if !project.DangerouslySkipPermissions || !command.Authorization.AllowUnsafePermissions {
			return nil, validationError("unsafe_permissions_forbidden", "Unsafe permissions are not allowed for this project and authorization")
		}
		if err := requireExplicitActionApproval(command.Authorization); err != nil {
			return nil, err
		}
		environment["OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS"] = "true"
	}
	if s.syncer != nil {
		if err := s.syncer.SyncToProject(ctx, project); err != nil {
			return nil, fmt.Errorf("sync project configuration: %w", err)
		}
	}
	if project.Type == "remote" && s.decrypt == nil {
		return nil, validationError("session_decryptor_unavailable", "Remote session decryptor is unavailable")
	}
	if err := s.manager.ReopenSession(ctx, session, project, environment, s.decrypt); err != nil {
		return nil, err
	}
	stored, err := s.store.GetSession(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	s.publish(ctx, SessionChange{Action: "reopened", Session: stored, ID: stored.ID, Actor: command.Authorization.Actor})
	return stored, nil
}

type SendSessionInputCommand struct {
	SessionID     string
	Text          string
	Authorization ActionAuthorization
}

func (s *SessionService) SendInput(ctx context.Context, command SendSessionInputCommand) error {
	if err := requireActionActor(command.Authorization); err != nil {
		return err
	}
	if _, err := s.requireRunningSession(ctx, command.SessionID); err != nil {
		return err
	}
	text := strings.TrimSpace(command.Text)
	if text == "" {
		return validationError("session_input_required", "Session input is required")
	}
	if len([]byte(text)) > maxSessionInputBytes {
		return validationError("session_input_too_large", "Session input exceeds 16 KiB")
	}
	if err := s.writeLine(command.SessionID, text); err != nil {
		return err
	}
	s.publish(ctx, SessionChange{Action: "input_sent", ID: command.SessionID, Actor: command.Authorization.Actor})
	return nil
}

type EvaluateSessionCommand struct {
	SessionID     string
	Authorization ActionAuthorization
}

func (s *SessionService) Evaluate(ctx context.Context, command EvaluateSessionCommand) (bool, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return false, err
	}
	if _, err := s.requireRunningSession(ctx, command.SessionID); err != nil {
		return false, err
	}
	if s.evaluator == nil {
		return false, validationError("session_evaluator_unavailable", "Session evaluator is unavailable")
	}
	output, err := s.manager.GetSessionOutput(command.SessionID)
	if err != nil {
		return false, err
	}
	if len(output) > maxEvaluationSnapshotBytes {
		output = append([]byte(nil), output[len(output)-maxEvaluationSnapshotBytes:]...)
	}
	generated := s.evaluator.EvaluateSession(ctx, command.SessionID, "session_request", output)
	s.publish(ctx, SessionChange{Action: "evaluated", ID: command.SessionID, Actor: command.Authorization.Actor})
	return generated, nil
}

type StoreImagePromptHintCommand struct {
	SessionID     string
	UserPrompt    string
	ImageCount    int
	Authorization ActionAuthorization
}

func (s *SessionService) StoreImagePromptHint(ctx context.Context, command StoreImagePromptHintCommand) error {
	if err := requireActionActor(command.Authorization); err != nil {
		return err
	}
	if _, err := s.requireRunningSession(ctx, command.SessionID); err != nil {
		return err
	}
	command.UserPrompt = strings.TrimSpace(command.UserPrompt)
	if utf8.RuneCountInString(command.UserPrompt) > 4000 {
		return validationError("image_prompt_too_large", "Image prompt exceeds 4000 characters")
	}
	if command.ImageCount <= 0 || command.ImageCount > 20 {
		return validationError("image_count_invalid", "Image count must be between 1 and 20")
	}
	if s.promptHint == nil {
		return validationError("image_prompt_store_unavailable", "Image prompt store is unavailable")
	}
	if err := s.promptHint.StoreImagePromptHint(ctx, command.SessionID, command.UserPrompt, command.ImageCount); err != nil {
		return err
	}
	s.publish(ctx, SessionChange{Action: "image_prompt_stored", ID: command.SessionID, Actor: command.Authorization.Actor})
	return nil
}

func (s *SessionService) getSession(ctx context.Context, sessionID string) (*database.Session, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || utf8.RuneCountInString(sessionID) > 200 {
		return nil, validationError("invalid_session_id", "Session ID is required and must not exceed 200 characters")
	}
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		if errors.Is(err, sql.ErrNoRows) || session == nil {
			return nil, notFoundError("session_not_found", "Session not found", err)
		}
		return nil, err
	}
	return session, nil
}

func (s *SessionService) requireRunningSession(ctx context.Context, sessionID string) (*database.Session, error) {
	if s.manager == nil {
		return nil, validationError("session_manager_unavailable", "Session manager is unavailable")
	}
	session, err := s.getSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.Status != "running" || !s.manager.IsSessionRunning(session.ID) {
		return nil, conflictError("session_not_running", "Session is not running")
	}
	return session, nil
}

func (s *SessionService) writeLine(sessionID, text string) error {
	if err := s.manager.WriteToSession(sessionID, []byte(text)); err != nil {
		return err
	}
	return s.manager.WriteToSession(sessionID, []byte("\n"))
}

func (s *SessionService) compensateFailedCreate(ctx context.Context, sessionID string) {
	_ = s.manager.StopSession(ctx, sessionID)
	_ = s.store.EndSession(ctx, sessionID, "error")
}

func (s *SessionService) publish(ctx context.Context, change SessionChange) {
	if s.effects != nil {
		s.effects.PublishSessionChange(ctx, change)
	}
}

func normalizeSessionEnvironment(values map[string]string, boundary ActionAuthorization) (map[string]string, error) {
	result := make(map[string]string, len(values)+4)
	if len(values) == 0 {
		return result, nil
	}
	if !boundary.AllowEnvironment {
		return nil, validationError("session_environment_forbidden", "Environment injection is not authorized")
	}
	if err := requireExplicitActionApproval(boundary); err != nil {
		return nil, err
	}
	if len(values) > maxSessionEnvironmentVariables {
		return nil, validationError("session_environment_too_large", "At most 64 environment variables are allowed")
	}
	total := 0
	for key, value := range values {
		key = strings.TrimSpace(key)
		if !sessionEnvironmentKeyPattern.MatchString(key) || strings.HasPrefix(key, "OPENPOET_") {
			return nil, validationError("session_environment_key_invalid", "Environment keys must be safe and may not use the OPENPOET_ namespace")
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, validationError("session_environment_value_invalid", "Environment values may not contain NUL")
		}
		total += len(key) + len(value)
		if total > maxSessionEnvironmentBytes {
			return nil, validationError("session_environment_too_large", "Environment payload exceeds 64 KiB")
		}
		result[key] = value
	}
	return result, nil
}

func injectTaskEnvironment(environment map[string]string, task *database.ProjectTask, resumed bool) {
	environment["OPENPOET_TASK_ID"] = fmt.Sprintf("%d", task.ID)
	environment["OPENPOET_TASK_TITLE"] = task.Title
	prefix := "OpenPoet has assigned"
	if resumed {
		prefix = "This is a RESUMED OpenPoet session for"
	}
	environment["OPENPOET_APPEND_SYSTEM_PROMPT"] = fmt.Sprintf(
		"%s task #%d titled %q. Call openpoet_get_my_task to read its current details before work.",
		prefix, task.ID, task.Title,
	)
}
