package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"openpoet/internal/database"
	"openpoet/internal/workspace"
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
	// No leading dash: a base_ref like "--lock" would otherwise be parsed by
	// git as a FLAG on worktree add, not a commit-ish (option injection).
	workspaceRefRe = regexp.MustCompile(`^[a-zA-Z0-9_./:~^{}@][a-zA-Z0-9_./:~^{}\-@]*$`)
)

// WorkspaceService owns the lane lifecycle for Phase 2: create (git worktree
// add + materialize-only sync), list/get, remove (destructive), and session
// leasing. Local claude-style projects only in the MVP.
type WorkspaceService struct {
	db          *database.DB
	git         WorkspaceGitPort
	syncer      WorkspaceSyncer
	provisioner EnvironmentProvisioner
}

// EnvironmentProvisioner runs a project's `.openpoet/environment.yaml` for a
// workspace (Phase 6). nil = plain Phase-2 workspaces (no env execution).
type EnvironmentProvisioner interface {
	Provision(ctx context.Context, projectID int64, workspaceID, workDir string, manifest []byte) (*workspace.ProvisionResult, error)
	Teardown(ctx context.Context, workspaceID string) error
}

func NewWorkspaceService(db *database.DB, git WorkspaceGitPort, syncer WorkspaceSyncer) *WorkspaceService {
	return &WorkspaceService{db: db, git: git, syncer: syncer}
}

// SetEnvironmentProvisioner wires the Phase-6 environment provisioner. Optional:
// without it, workspaces are the plain Phase-2 worktree substrate.
func (s *WorkspaceService) SetEnvironmentProvisioner(p EnvironmentProvisioner) {
	s.provisioner = p
}

// ManifestApprovalRequiredError signals that a workspace could not be provisioned
// because the project's manifest hash is not approved. It carries the SHA so the
// caller can surface it for approval.
type ManifestApprovalRequiredError struct{ SHA string }

func (e *ManifestApprovalRequiredError) Error() string {
	return "manifest_approval_required: content_sha256=" + e.SHA
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

type MergeWorkspaceCommand struct {
	WorkspaceID   string
	Authorization ActionAuthorization
}

// MergeResult carries the merge outcome. A conflict is a business outcome, not
// an execution error, so the file list rides the success path (error returns
// carry only a code+message and would drop it).
type MergeResult struct {
	Merged        bool
	ConflictFiles []string
	Workspace     *database.Workspace
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
	// A previous lane with unmerged commits leaves its branch behind (branch -d
	// refuses, by design — that IS the safety net). Recreating the name
	// attaches to the surviving branch instead of failing forever on -b.
	branchExists := false
	if _, err := s.git.RunGit(ctx, project, "rev-parse", "--verify", "--quiet", "refs/heads/"+ws.Branch); err == nil {
		branchExists = true
	}
	var addErr error
	if branchExists {
		_, addErr = s.git.RunGit(ctx, project, "worktree", "add", ws.Path, ws.Branch)
	} else {
		_, addErr = s.git.RunGit(ctx, project, "worktree", "add", "-b", ws.Branch, ws.Path, ws.BaseRef)
	}
	if addErr != nil {
		_ = s.db.SetWorkspaceStatus(ctx, ws.ID, "failed", "workspace.failed", actor)
		return nil, fmt.Errorf("git worktree add: %w", addErr)
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

	// Phase 6: if the PROJECT ships an approved `.openpoet/environment.yaml`, run
	// its environment in this workspace (setup → port → service → health) before
	// marking ready. The manifest is read from the main checkout (it may be
	// uncommitted, so it never rides in the worktree ref).
	if s.provisioner != nil {
		if manifest, ok := readProjectManifest(project.Path); ok {
			result, provErr := s.provisioner.Provision(ctx, project.ID, ws.ID, ws.Path, manifest)
			if provErr != nil {
				_ = s.db.SetWorkspaceStatus(ctx, ws.ID, "failed", "workspace.failed", actor)
				var approval *workspace.ApprovalRequiredError
				if errors.As(provErr, &approval) {
					return nil, &ManifestApprovalRequiredError{SHA: approval.SHA}
				}
				return nil, fmt.Errorf("environment provision: %w", provErr)
			}
			// Snapshot the rendered coordinates so a reopened session inherits them.
			resources, _ := json.Marshal(result)
			_ = s.db.SetWorkspaceResources(ctx, ws.ID, string(resources), workspace.ManifestSHA256(manifest))
		}
	}

	if err := s.db.SetWorkspaceStatus(ctx, ws.ID, "ready", "workspace.ready", actor); err != nil {
		return nil, err
	}
	return s.db.GetWorkspace(ctx, ws.ID)
}

// readProjectManifest returns the raw `.openpoet/environment.yaml` bytes from a
// project's main checkout, or ok=false when the project ships no manifest.
func readProjectManifest(projectPath string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(projectPath, ".openpoet", "environment.yaml"))
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return nil, false
	}
	return b, true
}

// ResolveAutoWorkspace implements sessions.create isolation:"auto": when the
// project's main checkout is free it returns "" (use the main path, zero
// regression); when the main path is busy it leases an idle POOLED workspace
// (fast second concurrent session, no re-provision); if none is ready it fails
// with no_workspace_ready pointing the caller at the async provision flow.
func (s *WorkspaceService) ResolveAutoWorkspace(ctx context.Context, projectID int64) (string, error) {
	busy, err := s.db.ProjectMainPathBusy(ctx, projectID)
	if err != nil {
		return "", err
	}
	if !busy {
		return "", nil // main path free → use it
	}
	ws, err := s.db.PickIdleWorkspace(ctx, projectID)
	if err != nil {
		return "", err
	}
	if ws == nil {
		return "", validationError("no_workspace_ready", "the main path is busy and no pooled workspace is ready — call workspaces.create and watch for workspace.ready")
	}
	return ws.ID, nil
}

// Discard tears down a workspace's environment (kills services, releases ports)
// and removes the lane. Destructive.
func (s *WorkspaceService) Discard(ctx context.Context, command RemoveWorkspaceCommand) (*database.Workspace, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	if s.provisioner != nil {
		_ = s.provisioner.Teardown(ctx, command.WorkspaceID)
	}
	// Reuse the Phase-2 destructive removal (worktree remove + status).
	return s.Remove(ctx, command)
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
	// Confinement re-check before ANY destructive disk operation: the only
	// paths this service ever deletes live under the managed root. A tampered
	// or corrupted row must never point remove --force anywhere else.
	managedRoot := filepath.Join(project.Path, filepath.FromSlash(workspaceRootRel)) + string(filepath.Separator)
	if !strings.HasPrefix(ws.Path, managedRoot) {
		return nil, validationError("workspace_path_unmanaged", "Workspace path is outside the managed root; refusing to remove")
	}
	actor := EventActorValue(command.Authorization.Actor)
	if _, err := s.git.RunGit(ctx, project, "worktree", "remove", "--force", ws.Path); err != nil {
		switch {
		case !pathExists(ws.Path):
			// Directory already gone (manual cleanup): prune stale metadata.
			_, _ = s.git.RunGit(ctx, project, "worktree", "prune")
		case !s.worktreeRegistered(ctx, project, ws.Path):
			// Directory exists but git no longer tracks it (pruned metadata):
			// confined manual delete, then prune. Without this the row could
			// never reach 'removed'.
			if rmErr := os.RemoveAll(ws.Path); rmErr != nil {
				return nil, fmt.Errorf("removing unregistered lane dir: %w", rmErr)
			}
			_, _ = s.git.RunGit(ctx, project, "worktree", "prune")
		default:
			return nil, fmt.Errorf("git worktree remove: %w", err)
		}
	}
	// Lowercase -d: git itself refuses to delete an unmerged branch — the free
	// safety net for lanes that accumulated commits.
	_, _ = s.git.RunGit(ctx, project, "branch", "-d", ws.Branch)
	if err := s.db.SetWorkspaceStatus(ctx, ws.ID, "removed", "workspace.removed", actor); err != nil {
		return nil, err
	}
	return s.db.GetWorkspace(ctx, ws.ID)
}

// Merge folds a lane's branch back into the main checkout: precondition
// main-clean, git merge --no-ff, and on conflict abort + return the file list
// (leaving main clean). Destructive tier. The lane branch is checked out in the
// lane worktree, but merging FROM it into main is not blocked by git's
// worktree lock (only checkout/branch -d/reset OF it would be).
func (s *WorkspaceService) Merge(ctx context.Context, command MergeWorkspaceCommand) (*MergeResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return nil, err
	}
	ws, err := s.Get(ctx, command.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if ws.LeasedBySessionID.Valid && ws.LeasedBySessionID.String != "" {
		return nil, conflictError("workspace_leased", "Workspace is leased by a running session; stop it before merging")
	}
	project, err := s.db.GetProject(ctx, ws.ProjectID)
	if err != nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	// Precondition: the main checkout must be clean, or --no-ff would fail
	// midway and leave a mess.
	status, err := s.git.RunGit(ctx, project, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("checking main worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return nil, conflictError("workspace_main_dirty", "Main working tree has uncommitted changes; commit or stash before merging")
	}
	actor := EventActorValue(command.Authorization.Actor)
	if _, mergeErr := s.git.RunGit(ctx, project, "merge", "--no-ff", "--no-edit", ws.Branch); mergeErr != nil {
		// Non-zero exit is a conflict OR a real failure; git merge's CONFLICT
		// lines go to stdout (dropped on error), so diagnose separately.
		conflicts, _ := s.git.RunGit(ctx, project, "diff", "--name-only", "--diff-filter=U")
		files := splitLines(conflicts)
		// Always abort so main is left clean regardless of which case it was.
		_, _ = s.git.RunGit(ctx, project, "merge", "--abort")
		if len(files) == 0 {
			return nil, fmt.Errorf("git merge failed: %w", mergeErr)
		}
		return &MergeResult{Merged: false, ConflictFiles: files, Workspace: ws}, nil
	}
	if err := s.db.SetWorkspaceStatus(ctx, ws.ID, "merged", "workspace.merged", actor); err != nil {
		return nil, err
	}
	merged, _ := s.db.GetWorkspace(ctx, ws.ID)
	return &MergeResult{Merged: true, Workspace: merged}, nil
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (s *WorkspaceService) worktreeRegistered(ctx context.Context, project *database.Project, path string) bool {
	out, err := s.git.RunGit(ctx, project, "worktree", "list", "--porcelain")
	if err != nil {
		return true // uncertain: stay conservative, refuse the manual delete
	}
	return strings.Contains(out, "worktree "+path)
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

// Reserve atomically claims a ready workspace before the session exists —
// concurrent creates race on one CAS instead of double-booking the lane.
func (s *WorkspaceService) Reserve(ctx context.Context, workspaceID string) (string, error) {
	token := randomWorkspaceSuffix() + randomWorkspaceSuffix()
	if err := s.db.ReserveWorkspace(ctx, workspaceID, token); err != nil {
		return "", conflictError("workspace_not_ready", "Workspace is not ready to be leased")
	}
	return token, nil
}

// Bind converts a reservation into the real session lease.
func (s *WorkspaceService) Bind(ctx context.Context, workspaceID, token, sessionID string) error {
	return s.db.BindWorkspaceLease(ctx, workspaceID, token, sessionID)
}

// ReleaseReservation frees a reservation whose session never started.
func (s *WorkspaceService) ReleaseReservation(ctx context.Context, workspaceID, token string) error {
	return s.db.ReleaseWorkspaceReservation(ctx, workspaceID, token)
}

// ReLease re-takes the lane for a reopened/restored session: a ready lane, or
// one the session still holds from before a crash.
func (s *WorkspaceService) ReLease(ctx context.Context, workspaceID, sessionID string) error {
	if err := s.db.LeaseWorkspaceForSession(ctx, workspaceID, sessionID); err != nil {
		return conflictError("workspace_leased", "Workspace is leased by another session")
	}
	return nil
}

// ReleaseForSession frees whatever lease a session holds (failed reopen path).
func (s *WorkspaceService) ReleaseForSession(ctx context.Context, sessionID string) error {
	return s.db.ReleaseWorkspaceLeaseBySession(ctx, sessionID)
}

func randomWorkspaceSuffix() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf)
}
