package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"openpoet/internal/database"
)

// WorkspaceGitPort is the thin seam over GitHandler.runGitCmd: local exec or
// cd-over-SSH with timeout, arbitrary git subcommands. Git never runs inside a
// database transaction.
type WorkspaceGitPort interface {
	RunGit(ctx context.Context, project *database.Project, args ...string) (string, error)
}

// WorkspaceSyncer is the materialize-only half of config sync: provision the
// lane's .claude layer with ZERO database writes.
type WorkspaceSyncer interface {
	MaterializeToWorkspace(ctx context.Context, project *database.Project) error
}

const (
	workspaceRootRel     = ".openpoet/worktrees"
	workspaceBranchBase  = "openpoet/"
	workspaceExcludeLine = "/.openpoet/"
)

var (
	workspaceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	workspaceRefRe  = regexp.MustCompile(`^[a-zA-Z0-9_./:~^{}\-@]+$`)
)

// WorkspaceService owns the lane lifecycle for Phase 2: create (git worktree
// add + materialize-only sync), list/get, remove (destructive), and session
// leasing. Local claude-style projects only in the MVP.
type WorkspaceService struct {
	db     *database.DB
	git    WorkspaceGitPort
	syncer WorkspaceSyncer
}

func NewWorkspaceService(db *database.DB, git WorkspaceGitPort, syncer WorkspaceSyncer) *WorkspaceService {
	return &WorkspaceService{db: db, git: git, syncer: syncer}
}

type CreateWorkspaceCommand struct {
	ProjectID     int64
	Name          string
	BaseRef       string
	TaskID        *int64
	KeepOnExit    bool
	Authorization ActionAuthorization
}

type RemoveWorkspaceCommand struct {
	WorkspaceID   string
	Authorization ActionAuthorization
}

// Create provisions a git-worktree lane: row (provisioning) → exclude line →
// git worktree add -b openpoet/<name> → materialize-only sync → ready + event.
func (s *WorkspaceService) Create(ctx context.Context, command CreateWorkspaceCommand) (*database.Workspace, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	if command.ProjectID <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}
	project, err := s.db.GetProject(ctx, command.ProjectID)
	if err != nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	if project.Type != "local" {
		return nil, validationError("workspace_remote_unsupported", "Workspaces support local projects only in this phase")
	}
	if project.Backend != "" && project.Backend != "claude_code" {
		return nil, validationError("workspace_backend_unsupported", "Workspaces support claude_code projects only in this phase")
	}
	name := strings.TrimSpace(command.Name)
	if name == "" {
		name = "ws-" + randomWorkspaceSuffix()
	}
	if !workspaceNameRe.MatchString(name) || strings.Contains(name, "..") {
		return nil, validationError("workspace_name_invalid", "Workspace name must be a short slug (lowercase letters, digits, . _ -)")
	}
	if existing, err := s.db.GetActiveWorkspaceByName(ctx, project.ID, name); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, conflictError("workspace_name_conflict", "A workspace with this name already exists for the project")
	}
	if _, err := s.git.RunGit(ctx, project, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, validationError("workspace_project_not_git", "Project directory is not a git repository")
	}
	baseRef := strings.TrimSpace(command.BaseRef)
	if baseRef == "" {
		out, err := s.git.RunGit(ctx, project, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("resolving base ref: %w", err)
		}
		baseRef = strings.TrimSpace(out)
	}
	if !workspaceRefRe.MatchString(baseRef) {
		return nil, validationError("workspace_base_ref_invalid", "Base ref contains invalid characters")
	}
	lanePath := filepath.Join(project.Path, filepath.FromSlash(workspaceRootRel), name)
	managedRoot := filepath.Join(project.Path, filepath.FromSlash(workspaceRootRel)) + string(filepath.Separator)
	if !strings.HasPrefix(lanePath, managedRoot) {
		return nil, validationError("workspace_name_invalid", "Workspace path escapes the managed root")
	}

	ws := &database.Workspace{
		ProjectID:      project.ID,
		Kind:           "worktree",
		Name:           name,
		Branch:         workspaceBranchBase + name,
		BaseRef:        baseRef,
		Path:           lanePath,
		Status:         "provisioning",
		KeepOnExit:     command.KeepOnExit,
		CreatedByActor: EventActorValue(command.Authorization.Actor),
	}
	if command.TaskID != nil && *command.TaskID > 0 {
		ws.TaskID.Int64 = *command.TaskID
		ws.TaskID.Valid = true
	}
	actor := EventActorValue(command.Authorization.Actor)
	if err := s.db.CreateWorkspace(ctx, ws, actor); err != nil {
		if err == database.ErrWorkspaceNameConflict {
			return nil, conflictError("workspace_name_conflict", "A workspace with this name already exists for the project")
		}
		return nil, err
	}

	// The exclude line must land BEFORE the worktree dir is created, or the
	// main checkout's porcelain shows .openpoet/ as untracked dirt.
	if err := s.ensureExcludeLine(project); err != nil {
		_ = s.db.SetWorkspaceStatus(ctx, ws.ID, "failed", "workspace.failed", actor)
		return nil, fmt.Errorf("preparing git exclude: %w", err)
	}
	if _, err := s.git.RunGit(ctx, project, "worktree", "add", "-b", ws.Branch, ws.Path, ws.BaseRef); err != nil {
		_ = s.db.SetWorkspaceStatus(ctx, ws.ID, "failed", "workspace.failed", actor)
		return nil, fmt.Errorf("git worktree add: %w", err)
	}

	// Materialize the agent-config layer into the lane (zero DB writes).
	if s.syncer != nil {
		laneProject := *project
		laneProject.Path = ws.Path
		if err := s.syncer.MaterializeToWorkspace(ctx, &laneProject); err != nil {
			_ = s.db.SetWorkspaceStatus(ctx, ws.ID, "failed", "workspace.failed", actor)
			return nil, fmt.Errorf("materializing workspace config: %w", err)
		}
	}

	if err := s.db.SetWorkspaceStatus(ctx, ws.ID, "ready", "workspace.ready", actor); err != nil {
		return nil, err
	}
	return s.db.GetWorkspace(ctx, ws.ID)
}

// ensureExcludeLine appends the managed-root pattern to .git/info/exclude once.
// info/exclude is unversioned, so this never dirties the repo.
func (s *WorkspaceService) ensureExcludeLine(project *database.Project) error {
	gitDir := filepath.Join(project.Path, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		// .git may be a file (worktree/submodule); resolve conservatively by
		// refusing — the MVP targets plain repos.
		if err != nil {
			return fmt.Errorf("project has no .git directory: %w", err)
		}
		return fmt.Errorf("project .git is not a directory (nested worktree?)")
	}
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return err
	}
	excludePath := filepath.Join(infoDir, "exclude")
	existing, _ := os.ReadFile(excludePath)
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == workspaceExcludeLine {
			return nil
		}
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + workspaceExcludeLine + "\n")
	return err
}

func (s *WorkspaceService) Get(ctx context.Context, id string) (*database.Workspace, error) {
	ws, err := s.db.GetWorkspace(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, notFoundError("workspace_not_found", "Workspace not found", nil)
	}
	return ws, nil
}

func (s *WorkspaceService) List(ctx context.Context, projectID int64, status string, limit int) ([]database.Workspace, error) {
	return s.db.ListWorkspaces(ctx, projectID, status, limit)
}

// Remove tears a lane down: git worktree remove --force (a lane always carries
// materialized untracked .claude files), best-effort branch -d, row → removed.
// Destructive tier: the caller must arrive with an explicit approval.
func (s *WorkspaceService) Remove(ctx context.Context, command RemoveWorkspaceCommand) (*database.Workspace, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return nil, err
	}
	ws, err := s.Get(ctx, command.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if ws.Status == "removed" {
		return ws, nil
	}
	if ws.LeasedBySessionID.Valid && ws.LeasedBySessionID.String != "" {
		return nil, conflictError("workspace_leased", "Workspace is leased by a running session; stop it first")
	}
	project, err := s.db.GetProject(ctx, ws.ProjectID)
	if err != nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	actor := EventActorValue(command.Authorization.Actor)
	if _, err := s.git.RunGit(ctx, project, "worktree", "remove", "--force", ws.Path); err != nil {
		// The directory may already be gone (manual cleanup); prune metadata
		// and continue if so, otherwise surface the failure.
		if _, statErr := os.Stat(ws.Path); statErr == nil {
			return nil, fmt.Errorf("git worktree remove: %w", err)
		}
		_, _ = s.git.RunGit(ctx, project, "worktree", "prune")
	}
	// Lowercase -d: git itself refuses to delete an unmerged branch — the free
	// safety net for lanes that accumulated commits.
	_, _ = s.git.RunGit(ctx, project, "branch", "-d", ws.Branch)
	if err := s.db.SetWorkspaceStatus(ctx, ws.ID, "removed", "workspace.removed", actor); err != nil {
		return nil, err
	}
	return s.db.GetWorkspace(ctx, ws.ID)
}

// ResolveForSession validates that a workspace can host a new session.
func (s *WorkspaceService) ResolveForSession(ctx context.Context, project *database.Project, workspaceID string) (*database.Workspace, error) {
	ws, err := s.db.GetWorkspace(ctx, strings.TrimSpace(workspaceID))
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, notFoundError("workspace_not_found", "Workspace not found", nil)
	}
	if ws.ProjectID != project.ID {
		return nil, validationError("workspace_project_mismatch", "Workspace belongs to a different project")
	}
	if ws.Status != "ready" {
		return nil, conflictError("workspace_not_ready", fmt.Sprintf("Workspace is %s, not ready", ws.Status))
	}
	if _, err := os.Stat(ws.Path); err != nil {
		return nil, conflictError("workspace_gone", "Workspace directory no longer exists on disk")
	}
	return ws, nil
}

// Lease binds the workspace to a freshly-started session.
func (s *WorkspaceService) Lease(ctx context.Context, workspaceID, sessionID string) error {
	return s.db.LeaseWorkspace(ctx, workspaceID, sessionID)
}

func randomWorkspaceSuffix() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf)
}
