package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/database"
	runtime "openpoet/internal/session"
)

const (
	maxSessionEnvironmentVariables = 64
	maxSessionEnvironmentBytes     = 64 << 10
	maxSessionInputBytes           = 16 << 10
	maxEvaluationSnapshotBytes     = 64 << 10

	DefaultTaskStartPrompt  = "Start working on the assigned task."
	PlanningTaskStartPrompt = "Plan the implementation for the assigned task. Enter plan mode and present the plan before making any changes."

	// sessionConfigSyncWaitBudget bounds how long session start waits for the
	// pre-session configuration sync; sessionConfigSyncTimeout bounds the sync
	// itself once it continues in the background.
	sessionConfigSyncWaitBudget = 20 * time.Second
	sessionConfigSyncTimeout    = 2 * time.Minute
)

var sessionEnvironmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type SessionStore interface {
	GetProject(context.Context, int64) (*database.Project, error)
	GetSession(context.Context, string) (*database.Session, error)
	GetTask(context.Context, int64) (*database.ProjectTask, error)
	GetTaskForSession(context.Context, string) (*database.ProjectTask, error)
	EndSession(context.Context, string, string) error
	UpdateSessionLineage(ctx context.Context, sessionID, parentSessionID, spawnedBy string) error
	// UpdateSessionWorkspace repoints a session at a working tree; SessionWriteFootprint
	// lists what it has written. Both serve Isolate, which moves a live session
	// into its own lane.
	UpdateSessionWorkspace(ctx context.Context, sessionID, workspaceID, workDir string) error
	SessionWriteFootprint(ctx context.Context, sessionID string) ([]string, error)
	ProjectIDsForTags(ctx context.Context, tagIDs []int64) ([]int64, error)
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

// SessionEnvironmentProvider materializes provider credentials only at the
// runtime boundary. Implementations must not persist or publish the returned
// values.
type SessionEnvironmentProvider interface {
	SessionEnvironment(context.Context, *database.Project) (map[string]string, error)
}

type SessionNameStore interface {
	RenameSession(context.Context, string, string) error
}

type SessionTaskNotifier interface {
	NotifyTaskLoaded(context.Context, string, *database.ProjectTask) error
}

// SessionInputSubmitter owns backend-specific Enter timing. UI and Automation
// share this port so Codex/local/remote sessions receive identical input.
type SessionInputSubmitter interface {
	SubmitSessionLine(context.Context, string, string) error
}

// SessionInitialPromptSubmitter waits for a newly created backend to be ready
// before submitting its first prompt. This is separate from ordinary session
// input because Claude Code can discard Enter while its TUI is still starting.
type SessionInitialPromptSubmitter interface {
	SubmitInitialSessionPrompt(context.Context, string, string) error
}

// SessionRuntimeSettings applies mutable runtime metadata through the same
// backend-specific channel used by the interactive UI.
type SessionRuntimeSettings interface {
	SetSessionModel(context.Context, string, string) (*database.Session, error)
	SetSessionEffort(context.Context, string, string) (*database.Session, error)
}

// SessionSignalPort exposes hook-derived per-session signals to the
// application layer: the prompt-accepted ack (UserPromptSubmit) and the
// execution-mode tracker. Implemented by the HookHandler.
type SessionSignalPort interface {
	// RegisterPromptWaiter returns a channel that fires on the session's next
	// UserPromptSubmit hook, plus a cancel func. Registration is synchronous so
	// the caller can register BEFORE writing input (no lost-wakeup race).
	RegisterPromptWaiter(sessionID string) (<-chan struct{}, func())
	GetSessionMode(sessionID string) string
}

type SessionCreationCollaborators struct {
	Environment  SessionEnvironmentProvider
	Names        SessionNameStore
	Tasks        SessionTaskNotifier
	Input        SessionInputSubmitter
	InitialInput SessionInitialPromptSubmitter
	Settings     SessionRuntimeSettings
	Workspaces   SessionWorkspaceProvider
	Signals      SessionSignalPort
	WorkRuns     WorkRunStarter
	Now          func() time.Time
}

// WorkRunStarter records a durable work run for a programmatically-spawned
// session (the dormant execution_target_type='session' slot). Implemented by
// *WorkRunService.
type WorkRunStarter interface {
	Start(ctx context.Context, command StartWorkRunCommand) (*database.WorkRun, error)
}

// SessionWorkspaceProvider resolves and leases workspace lanes for sessions.
// Implemented by *WorkspaceService.
type SessionWorkspaceProvider interface {
	// ResolveForSession validates a lane can host a new session. A non-empty
	// reservationToken means the lane arrives already claimed by that token
	// (the isolation path), so 'leased' is the expected status rather than 'ready'.
	ResolveForSession(ctx context.Context, project *database.Project, workspaceID, reservationToken string) (*database.Workspace, error)
	Reserve(ctx context.Context, workspaceID string) (string, error)
	Bind(ctx context.Context, workspaceID, token, sessionID string) error
	ReleaseReservation(ctx context.Context, workspaceID, token string) error
	ReLease(ctx context.Context, workspaceID, sessionID string) error
	ReleaseForSession(ctx context.Context, sessionID string) error
	// ResolveIsolation implements isolation:"auto"/"always": returns "" to use the
	// project's main checkout, or a lane id — leased from the pool or provisioned
	// on demand — together with the reservation token already claiming it.
	ResolveIsolation(ctx context.Context, projectID int64, mode string, authorization ActionAuthorization) (IsolationDecision, error)
	// Get loads a lane; DirtyPaths reports which of the given project-relative
	// paths carry uncommitted changes. Both serve Isolate.
	Get(ctx context.Context, id string) (*database.Workspace, error)
	DirtyPaths(ctx context.Context, project *database.Project, candidates []string) ([]string, error)
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
	creation   SessionCreationCollaborators
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
	creation ...SessionCreationCollaborators,
) *SessionService {
	service := &SessionService{
		store: store, manager: manager, syncer: syncer, linker: linker,
		evaluator: evaluator, promptHint: promptHint, decrypt: decrypt, effects: effects,
	}
	if len(creation) > 0 {
		service.creation = creation[0]
	}
	if service.creation.Now == nil {
		service.creation.Now = time.Now
	}
	return service
}

type CreateSessionCommand struct {
	ProjectID                  int64
	TaskID                     *int64
	Environment                map[string]string
	DangerouslySkipPermissions bool
	AutoStartTaskPrompt        bool
	PlanningMode               bool
	CustomPrompt               string
	// WorkspaceID runs the session inside an existing ready workspace lane
	// (its git worktree becomes the runner cwd and sync target).
	WorkspaceID string
	// Isolation:"auto" leases an idle pooled workspace when the project's main
	// path is already busy (Phase 6), so a second concurrent session lands in a
	// sandbox instead of colliding on the main checkout.
	Isolation string
	// Backend overrides the project's backend for THIS session (Phase 7.3 —
	// heterogeneous fan-out: e.g. a codex worker inside a claude_code project).
	Backend       string
	Authorization ActionAuthorization
}

// syncProjectConfiguration pushes OpenPoet configuration (skills, hooks,
// backend config) to the project before a session starts. It is best-effort
// by design: a slow or unreachable SSH host must not block or abort session
// startup. The sync runs detached from the request context because a browser
// abort must not cancel its in-flight SQLite queries — a canceled query
// poisons the single shared connection until restart. If the sync outlives
// the wait budget the session proceeds with the last synced configuration.
func (s *SessionService) syncProjectConfiguration(ctx context.Context, project *database.Project) {
	if s.syncer == nil {
		return
	}
	done := make(chan error, 1)
	go func() {
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionConfigSyncTimeout)
		defer cancel()
		done <- s.syncer.SyncToProject(syncCtx, project)
	}()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("[Session] config sync failed for project %d (session will start anyway): %v", project.ID, err)
		}
	case <-time.After(sessionConfigSyncWaitBudget):
		log.Printf("[Session] config sync for project %d exceeded %s; starting session without waiting", project.ID, sessionConfigSyncWaitBudget)
	}
}

// syncWorkspaceConfiguration is syncProjectConfiguration's materialize-only
// twin for workspace lanes: same detached-context, best-effort discipline, but
// the sync never writes disk state back to the database.
func (s *SessionService) syncWorkspaceConfiguration(ctx context.Context, laneProject *database.Project) {
	materializer, ok := s.syncer.(WorkspaceSyncer)
	if s.syncer == nil || !ok {
		return
	}
	done := make(chan error, 1)
	go func() {
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionConfigSyncTimeout)
		defer cancel()
		done <- materializer.MaterializeToWorkspace(syncCtx, laneProject)
	}()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("[Session] workspace materialize sync failed for project %d (session will start anyway): %v", laneProject.ID, err)
		}
	case <-time.After(sessionConfigSyncWaitBudget):
		log.Printf("[Session] workspace materialize sync for project %d exceeded %s; starting without waiting", laneProject.ID, sessionConfigSyncWaitBudget)
	}
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
	// Per-session backend override (Phase 7.3): a shallow Project copy carries
	// the override so every downstream consumer (config sync, runner selection,
	// the persisted session row) sees the effective backend.
	project, err = applySessionBackendOverride(project, command.Backend)
	if err != nil {
		return nil, err
	}

	// Workspace threading: resolve the lane FIRST, then run the entire create
	// flow against a shallow Project copy whose Path is the lane dir — the one
	// string every subsystem (sync, runner cwd, transcript encoding) keys off.
	var workspace *database.Workspace
	reservationToken := ""
	// Isolation ("auto"/"always") decides the working tree before anything slow.
	// It may lease a pooled lane or provision a fresh one, and hands back a lane
	// that is ALREADY reserved: the claim has to happen inside the decision, or a
	// peer create can take the lane in the gap.
	if strings.TrimSpace(command.Isolation) != "" && strings.TrimSpace(command.WorkspaceID) == "" && s.creation.Workspaces != nil {
		decision, isoErr := s.creation.Workspaces.ResolveIsolation(ctx, command.ProjectID, command.Isolation, command.Authorization)
		if isoErr != nil {
			return nil, isoErr
		}
		command.WorkspaceID = decision.WorkspaceID // "" → main checkout is free, use it
		reservationToken = decision.ReservationToken
		if reservationToken != "" {
			// Register the release BEFORE the validations below: an isolation
			// decision that reserved a lane and then failed validation would
			// otherwise strand that lane in 'leased' forever.
			laneID := decision.WorkspaceID
			defer func() {
				if reservationToken != "" {
					_ = s.creation.Workspaces.ReleaseReservation(ctx, laneID, reservationToken)
				}
			}()
		}
	}
	if workspaceID := strings.TrimSpace(command.WorkspaceID); workspaceID != "" {
		if s.creation.Workspaces == nil {
			return nil, validationError("workspace_unsupported", "Workspace sessions are not available")
		}
		workspace, err = s.creation.Workspaces.ResolveForSession(ctx, project, workspaceID, reservationToken)
		if err != nil {
			return nil, err
		}
		if reservationToken == "" {
			// An explicitly requested lane is reserved here: two concurrent creates
			// for the same lane race on this CAS, never on runner startup.
			reservationToken, err = s.creation.Workspaces.Reserve(ctx, workspace.ID)
			if err != nil {
				return nil, err
			}
			defer func() {
				if reservationToken != "" {
					_ = s.creation.Workspaces.ReleaseReservation(ctx, workspace.ID, reservationToken)
				}
			}()
		}
		laneProject := *project
		laneProject.Path = workspace.Path
		project = &laneProject
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
	initialPrompt, err := resolveInitialSessionPrompt(command, linkedTask != nil)
	if err != nil {
		return nil, err
	}
	if s.creation.Environment != nil {
		providerEnvironment, providerErr := s.creation.Environment.SessionEnvironment(ctx, project)
		if providerErr != nil {
			return nil, fmt.Errorf("resolve session provider environment: %w", providerErr)
		}
		if err := mergeTrustedSessionEnvironment(environment, providerEnvironment); err != nil {
			return nil, err
		}
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
	if workspace != nil {
		environment["OPENPOET_WORKDIR"] = workspace.Path
		environment["OPENPOET_WORKSPACE_ID"] = workspace.ID
		// Lanes get materialize-only sync: N workspaces must never write
		// skills or memory docs back into the shared database.
		s.syncWorkspaceConfiguration(ctx, project)
	} else {
		s.syncProjectConfiguration(ctx, project)
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
		if initialPrompt == "" && s.creation.Tasks != nil {
			if err := s.creation.Tasks.NotifyTaskLoaded(ctx, created.ID, linkedTask); err != nil {
				s.compensateFailedCreate(ctx, created.ID)
				return nil, err
			}
		}
	} else {
		if s.creation.Names != nil {
			created.Name = project.Name + " (" + s.creation.Now().Format("15:04:05") + ")"
			if err := s.creation.Names.RenameSession(ctx, created.ID, created.Name); err != nil {
				s.compensateFailedCreate(ctx, created.ID)
				return nil, err
			}
		}
	}
	if initialPrompt != "" {
		if err := s.writeInitialPrompt(ctx, created.ID, initialPrompt); err != nil {
			s.compensateFailedCreate(ctx, created.ID)
			return nil, err
		}
	}
	if workspace != nil {
		if err := s.creation.Workspaces.Bind(ctx, workspace.ID, reservationToken, created.ID); err != nil {
			log.Printf("[Sessions] workspace lease bind failed for %s → %s: %v", workspace.ID, created.ID, err)
		} else {
			reservationToken = "" // consumed — the deferred release must not fire
		}
	}
	s.recordLineageAndWorkRun(ctx, command, created)
	s.publish(ctx, SessionChange{Action: "created", Session: created, ID: created.ID, Actor: command.Authorization.Actor})
	return created, nil
}

// recordLineageAndWorkRun persists who spawned the session and, for
// programmatic (non-UI) spawns, opens a durable work_run bound to the session —
// the first consumer of execution_target_type='session'. Best-effort: a
// lineage/work-run failure never fails the already-started session.
func (s *SessionService) recordLineageAndWorkRun(ctx context.Context, command CreateSessionCommand, created *database.Session) {
	actor := command.Authorization.Actor
	spawnedBy := EventActorValue(actor)
	parent := ""
	if actor.Type == "session" {
		parent = actor.ID
	}
	if err := s.store.UpdateSessionLineage(ctx, created.ID, parent, spawnedBy); err != nil {
		log.Printf("[Sessions] lineage update failed for %s: %v", created.ID, err)
	} else {
		created.ParentSessionID = parent
		created.SpawnedBy = spawnedBy
	}
	// Only programmatic spawns (automation clients or session actors) open a
	// work run; interactive UI sessions do not need one.
	if s.creation.WorkRuns == nil || (actor.Type != "automation_client" && actor.Type != "session") {
		return
	}
	sid := created.ID
	title := "Session " + created.Name
	if title == "Session " {
		title = "Spawned session"
	}
	if _, err := s.creation.WorkRuns.Start(ctx, StartWorkRunCommand{
		Title:           title,
		Source:          "coordinator",
		ExpectedMinutes: 60,
		SessionID:       &sid,
		ExecutionTarget: &database.WorkRunExecutionTarget{Type: "session", ID: sid},
		Actor:           actor,
	}); err != nil {
		log.Printf("[Sessions] work_run for spawned session %s failed: %v", sid, err)
	}
}

func resolveInitialSessionPrompt(command CreateSessionCommand, hasTask bool) (string, error) {
	customPrompt := strings.TrimSpace(command.CustomPrompt)
	if command.PlanningMode && customPrompt != "" {
		return "", validationError("session_start_prompt_conflict", "Planning mode and custom prompt cannot be used together")
	}
	if len([]byte(customPrompt)) > maxSessionInputBytes {
		return "", validationError("session_start_prompt_too_large", "Custom prompt exceeds 16 KiB")
	}
	if strings.IndexByte(customPrompt, 0) >= 0 {
		return "", validationError("session_start_prompt_invalid", "Custom prompt may not contain NUL")
	}
	if customPrompt != "" {
		return customPrompt, nil
	}
	if command.PlanningMode {
		return PlanningTaskStartPrompt, nil
	}
	if hasTask && command.AutoStartTaskPrompt {
		return DefaultTaskStartPrompt, nil
	}
	return "", nil
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
	// A workspace session reopens back into its lane — or fails loudly if the
	// lane directory vanished; silently reopening in the main checkout would
	// dump the session into the wrong working tree.
	laneReopen := false
	if session.WorkDir != "" && session.WorkDir != project.Path {
		// The local Stat only proves anything for local lanes; a remote lane's
		// dir lives on the SSH host and is validated by the reopen itself.
		if project.Type == "local" {
			if _, statErr := os.Stat(session.WorkDir); statErr != nil {
				return nil, conflictError("workspace_gone", "Session workspace directory no longer exists on disk")
			}
		}
		// Re-take the lease BEFORE restarting the runner: a reopened session
		// occupies its lane again, or the lane's remove/double-book guards
		// are blind to it (review finding). Fails if another session holds it.
		if session.WorkspaceID.Valid && session.WorkspaceID.String != "" && s.creation.Workspaces != nil {
			if err := s.creation.Workspaces.ReLease(ctx, session.WorkspaceID.String, session.ID); err != nil {
				return nil, err
			}
		}
		laneProject := *project
		laneProject.Path = session.WorkDir
		project = &laneProject
		laneReopen = true
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
	if laneReopen {
		s.syncWorkspaceConfiguration(ctx, project)
	} else {
		s.syncProjectConfiguration(ctx, project)
	}
	if project.Type == "remote" && s.decrypt == nil {
		return nil, validationError("session_decryptor_unavailable", "Remote session decryptor is unavailable")
	}
	if err := s.manager.ReopenSession(ctx, session, project, environment, s.decrypt); err != nil {
		if laneReopen && session.WorkspaceID.Valid && s.creation.Workspaces != nil {
			// Runner never started: give the lease back so the lane isn't
			// stranded 'leased' by a stopped session.
			_ = s.creation.Workspaces.ReleaseForSession(ctx, session.ID)
		}
		return nil, err
	}
	stored, err := s.store.GetSession(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	s.publish(ctx, SessionChange{Action: "reopened", Session: stored, ID: stored.ID, Actor: command.Authorization.Actor})
	return stored, nil
}

// IsolateSessionCommand moves a LIVE session out of the working tree it shares
// with someone else and into a git worktree lane of its own.
type IsolateSessionCommand struct {
	SessionID string
	// Reason is shown to the agent in its continuity briefing ("conflicts with
	// session X on internal/foo.go"). Free text; empty is allowed.
	Reason string
	// Briefing carries the objective forward. The reopened session does NOT
	// inherit its conversation, so whatever the work still needs must be here.
	Briefing      string
	Authorization ActionAuthorization
}

// IsolateSessionResult reports where the session ended up.
type IsolateSessionResult struct {
	Session   *database.Session
	Workspace *database.Workspace
}

// Isolate is the reactive half of automatic worktree isolation. The conflict
// gate can only REFUSE a contested write, which leaves the losing session stuck
// with nothing to do; this actually moves it into its own lane so both sides can
// work and be merged afterwards.
//
// A live PTY cannot chdir, so the move is: stop → repoint the session row at the
// lane → reopen. Two consequences the caller has to understand — both enforced
// by typed refusals below rather than papered over:
//
//   - THE CONVERSATION DOES NOT CARRY OVER. Claude Code indexes its transcript by
//     the encoded working directory, so --resume cannot cross a cwd change. The
//     reopened session gets Briefing, not its history. Isolating a session that is
//     deep into a task is a real loss; it is cheapest immediately after a gate
//     denial, when the losing session has usually written nothing yet.
//   - UNCOMMITTED WORK WOULD BE LEFT BEHIND. A lane branches from HEAD, so
//     anything this session wrote but did not commit stays in the old tree.
//     Silently abandoning it would be data loss and moving it is destructive
//     surgery that belongs behind its own approval, so this refuses and names the
//     files instead.
func (s *SessionService) Isolate(ctx context.Context, command IsolateSessionCommand) (*IsolateSessionResult, error) {
	// Stopping and restarting somebody's live session is destructive.
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return nil, err
	}
	if s.creation.Workspaces == nil {
		return nil, validationError("workspace_unsupported", "Workspace sessions are not available")
	}
	if s.manager == nil {
		return nil, validationError("session_manager_unavailable", "Session manager is unavailable")
	}
	session, err := s.getSession(ctx, command.SessionID)
	if err != nil {
		return nil, err
	}
	if session.Status != "running" && session.Status != "starting" {
		return nil, conflictError("session_not_live", "Only a live session can be isolated")
	}
	if session.WorkspaceID.Valid && session.WorkspaceID.String != "" {
		return nil, conflictError("session_already_isolated", "Session already runs in its own workspace lane")
	}
	project, err := s.store.GetProject(ctx, session.ProjectID)
	if err != nil || project == nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}

	// Refuse while this session still holds uncommitted work in the shared tree.
	footprint, err := s.store.SessionWriteFootprint(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	if len(footprint) > 0 {
		dirty, err := s.creation.Workspaces.DirtyPaths(ctx, project, footprint)
		if err != nil {
			return nil, err
		}
		if len(dirty) > 0 {
			return nil, conflictError("session_has_uncommitted_work",
				"Session has uncommitted changes in the shared tree ("+strings.Join(dirty, ", ")+
					"); commit them first so the lane branches with the work included")
		}
	}

	decision, err := s.creation.Workspaces.ResolveIsolation(ctx, session.ProjectID, IsolationAlways, command.Authorization)
	if err != nil {
		return nil, err
	}
	if decision.WorkspaceID == "" {
		return nil, conflictError("isolation_unavailable", "No workspace lane could be provisioned for this project")
	}
	// Convert the reservation into this session's real lease BEFORE the restart:
	// while the runner is down the lane must not look free to anybody else.
	if err := s.creation.Workspaces.Bind(ctx, decision.WorkspaceID, decision.ReservationToken, session.ID); err != nil {
		_ = s.creation.Workspaces.ReleaseReservation(ctx, decision.WorkspaceID, decision.ReservationToken)
		return nil, err
	}
	workspace, err := s.creation.Workspaces.Get(ctx, decision.WorkspaceID)
	if err != nil {
		_ = s.creation.Workspaces.ReleaseForSession(ctx, session.ID)
		return nil, err
	}

	if err := s.manager.StopSession(ctx, session.ID); err != nil {
		_ = s.creation.Workspaces.ReleaseForSession(ctx, session.ID)
		return nil, err
	}
	_ = s.store.EndSession(ctx, session.ID, "stopped")

	// From here the session is DOWN. Repointing the row is what makes the reopen
	// land in the lane (Reopen reads work_dir/workspace_id), so a failure after
	// this point leaves a stopped session the user can simply reopen — into the
	// lane, which is where it was headed anyway.
	if err := s.store.UpdateSessionWorkspace(ctx, session.ID, workspace.ID, workspace.Path); err != nil {
		_ = s.creation.Workspaces.ReleaseForSession(ctx, session.ID)
		return nil, err
	}

	reopened, err := s.Reopen(ctx, ReopenSessionCommand{
		SessionID:     session.ID,
		Authorization: command.Authorization,
	})
	if err != nil {
		return nil, err
	}
	if brief := isolationBriefing(workspace, command); brief != "" {
		// Best-effort: the session is already in its lane, and a briefing that
		// failed to land is a message the user can resend — not a reason to
		// unwind a completed move.
		if err := s.writeInitialPrompt(ctx, session.ID, brief); err != nil {
			log.Printf("[isolate] briefing not delivered to session %s: %v", session.ID, err)
		}
	}
	s.publish(ctx, SessionChange{Action: "isolated", Session: reopened, ID: reopened.ID, Actor: command.Authorization.Actor})
	return &IsolateSessionResult{Session: reopened, Workspace: workspace}, nil
}

// isolationBriefing is what the moved session reads first. It has to be
// self-contained: this is a fresh transcript, so the agent knows nothing about
// what it was doing a moment ago.
func isolationBriefing(workspace *database.Workspace, command IsolateSessionCommand) string {
	var b strings.Builder
	b.WriteString("Your work has been moved into an isolated git worktree.\n\n")
	b.WriteString("- Working directory: " + workspace.Path + "\n")
	b.WriteString("- Branch: " + workspace.Branch + " (branched from " + workspace.BaseRef + ")\n")
	if reason := strings.TrimSpace(command.Reason); reason != "" {
		b.WriteString("- Why: " + reason + "\n")
	}
	b.WriteString("\nThis is a separate checkout, so you can edit the same files another " +
		"session is working on — git reconciles the two when this branch is merged back. " +
		"Commit your work on this branch; do not reach outside this directory.\n")
	if briefing := strings.TrimSpace(command.Briefing); briefing != "" {
		b.WriteString("\nYour objective:\n" + briefing + "\n")
	}
	return b.String()
}

type SendSessionInputCommand struct {
	SessionID     string
	Text          string
	Authorization ActionAuthorization
	// RejectIfBusy makes the send fail with session_busy when the session is
	// mid-turn (executing). Opt-in: only coordinator/automation callers set it —
	// the interactive UI must always be able to interrupt/queue.
	RejectIfBusy bool
	// AwaitAck blocks until the agent's UserPromptSubmit hook confirms the
	// prompt was accepted (or a short timeout), reporting acknowledged.
	AwaitAck bool
}

// SendInputResult reports whether the line reached the PTY and whether the
// agent acknowledged it (UserPromptSubmit hook observed).
type SendInputResult struct {
	Submitted    bool
	Acknowledged bool
}

func (s *SessionService) SendInput(ctx context.Context, command SendSessionInputCommand) error {
	_, err := s.SendInputWithAck(ctx, command)
	return err
}

const sessionAckTimeout = 8 * time.Second

func (s *SessionService) SendInputWithAck(ctx context.Context, command SendSessionInputCommand) (SendInputResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return SendInputResult{}, err
	}
	if _, err := s.requireRunningSession(ctx, command.SessionID); err != nil {
		return SendInputResult{}, err
	}
	text := strings.TrimSpace(command.Text)
	if text == "" {
		return SendInputResult{}, validationError("session_input_required", "Session input is required")
	}
	if len([]byte(text)) > maxSessionInputBytes {
		return SendInputResult{}, validationError("session_input_too_large", "Session input exceeds 16 KiB")
	}
	if command.RejectIfBusy && s.creation.Signals != nil {
		if s.creation.Signals.GetSessionMode(command.SessionID) == "executing" {
			return SendInputResult{}, conflictError("session_busy", "Session is mid-turn; retry when it is idle or send with force")
		}
	}
	// Register the ack waiter BEFORE writing, so a fast UserPromptSubmit can't
	// slip between the write and the wait (lost-wakeup race).
	var ackCh <-chan struct{}
	if command.AwaitAck && s.creation.Signals != nil {
		ch, cancel := s.creation.Signals.RegisterPromptWaiter(command.SessionID)
		ackCh = ch
		defer cancel()
	}
	if err := s.writeLine(ctx, command.SessionID, text); err != nil {
		return SendInputResult{}, err
	}
	s.publish(ctx, SessionChange{Action: "input_sent", ID: command.SessionID, Actor: command.Authorization.Actor})
	result := SendInputResult{Submitted: true}
	if ackCh != nil {
		select {
		case <-ackCh:
			result.Acknowledged = true
		case <-time.After(sessionAckTimeout):
		case <-ctx.Done():
		}
	}
	return result, nil
}

type SetSessionModelCommand struct {
	SessionID     string
	Model         string
	Authorization ActionAuthorization
}

func (s *SessionService) SetModel(ctx context.Context, command SetSessionModelCommand) (*database.Session, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	if _, err := s.requireRunningSession(ctx, command.SessionID); err != nil {
		return nil, err
	}
	command.Model = strings.TrimSpace(command.Model)
	if command.Model == "" || utf8.RuneCountInString(command.Model) > 200 {
		return nil, validationError("session_model_invalid", "Session model is required and must not exceed 200 characters")
	}
	if s.creation.Settings == nil {
		return nil, validationError("session_settings_unavailable", "Session runtime settings are unavailable")
	}
	updated, err := s.creation.Settings.SetSessionModel(ctx, command.SessionID, command.Model)
	if err != nil {
		return nil, sessionSettingApplicationError(err)
	}
	s.publish(ctx, SessionChange{Action: "model_changed", Session: updated, ID: command.SessionID, Actor: command.Authorization.Actor})
	return updated, nil
}

type SetSessionEffortCommand struct {
	SessionID     string
	Effort        string
	Authorization ActionAuthorization
}

func (s *SessionService) SetEffort(ctx context.Context, command SetSessionEffortCommand) (*database.Session, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	if _, err := s.requireRunningSession(ctx, command.SessionID); err != nil {
		return nil, err
	}
	command.Effort = strings.TrimSpace(command.Effort)
	if command.Effort == "" || utf8.RuneCountInString(command.Effort) > 50 {
		return nil, validationError("session_effort_invalid", "Session effort is required and must not exceed 50 characters")
	}
	if s.creation.Settings == nil {
		return nil, validationError("session_settings_unavailable", "Session runtime settings are unavailable")
	}
	updated, err := s.creation.Settings.SetSessionEffort(ctx, command.SessionID, command.Effort)
	if err != nil {
		return nil, sessionSettingApplicationError(err)
	}
	s.publish(ctx, SessionChange{Action: "effort_changed", Session: updated, ID: command.SessionID, Actor: command.Authorization.Actor})
	return updated, nil
}

func sessionSettingApplicationError(err error) error {
	switch {
	case errors.Is(err, runtime.ErrInvalidSessionSetting):
		return &Error{Kind: ErrorValidation, Code: "session_setting_invalid", Message: err.Error(), Cause: err}
	case errors.Is(err, runtime.ErrSessionNotRunning):
		return &Error{Kind: ErrorConflict, Code: "session_not_running", Message: err.Error(), Cause: err}
	case errors.Is(err, runtime.ErrSessionSettingUnsupported):
		return &Error{Kind: ErrorValidation, Code: "session_setting_unsupported", Message: err.Error(), Cause: err}
	default:
		return err
	}
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

func (s *SessionService) writeLine(ctx context.Context, sessionID, text string) error {
	if s.creation.Input != nil {
		return s.creation.Input.SubmitSessionLine(ctx, sessionID, text)
	}
	if err := s.manager.WriteToSession(sessionID, []byte(text)); err != nil {
		return err
	}
	return s.manager.WriteToSession(sessionID, []byte("\n"))
}

func (s *SessionService) writeInitialPrompt(ctx context.Context, sessionID, text string) error {
	if s.creation.InitialInput != nil {
		return s.creation.InitialInput.SubmitInitialSessionPrompt(ctx, sessionID, text)
	}
	return s.writeLine(ctx, sessionID, text)
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

func mergeTrustedSessionEnvironment(destination, source map[string]string) error {
	if len(destination)+len(source) > maxSessionEnvironmentVariables {
		return validationError("session_environment_too_large", "At most 64 environment variables are allowed")
	}
	total := 0
	for key, value := range destination {
		total += len(key) + len(value)
	}
	for key, value := range source {
		key = strings.TrimSpace(key)
		if !sessionEnvironmentKeyPattern.MatchString(key) || !trustedProviderEnvironmentKey(key) {
			return validationError("session_environment_key_invalid", "Provider environment key is invalid")
		}
		if strings.IndexByte(value, 0) >= 0 {
			return validationError("session_environment_value_invalid", "Environment values may not contain NUL")
		}
		if previous, exists := destination[key]; exists {
			total -= len(key) + len(previous)
		}
		total += len(key) + len(value)
		if total > maxSessionEnvironmentBytes {
			return validationError("session_environment_too_large", "Environment payload exceeds 64 KiB")
		}
		destination[key] = value
	}
	return nil
}

func trustedProviderEnvironmentKey(key string) bool {
	if !strings.HasPrefix(key, "OPENPOET_") {
		return true
	}
	switch key {
	case "OPENPOET_BACKEND_BINARY", "OPENPOET_REMOTE_PROVIDER_TUNNEL":
		return true
	default:
		return false
	}
}

// applySessionBackendOverride returns a shallow Project copy carrying a valid
// per-session backend override (or the original project when no override).
func applySessionBackendOverride(project *database.Project, backend string) (*database.Project, error) {
	backendOverride := strings.TrimSpace(backend)
	if backendOverride == "" || backendOverride == project.Backend {
		return project, nil
	}
	if !knownSessionBackend(backendOverride) {
		return nil, validationError("session_backend_invalid", "Unknown session backend override")
	}
	overridden := *project
	overridden.Backend = backendOverride
	return &overridden, nil
}

// sanitizeSingleLine flattens any control character (\n, \r, tabs, ...) to a
// space — the briefing's single-line contract (Windows cmd quoting).
func sanitizeSingleLine(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

// knownSessionBackend mirrors the automation layer's backend vocabulary.
func knownSessionBackend(backend string) bool {
	switch backend {
	case "claude_code", "copilot", "acp", "codex", "opencode":
		return true
	}
	return false
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
