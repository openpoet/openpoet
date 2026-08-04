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
	"sort"
	"strings"
	"sync"

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
	// provisionLocks serializes lane resolution per project id (see
	// lockProjectProvision). The zero sync.Map is ready to use.
	provisionLocks sync.Map
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
	if project.Type != "local" && project.Type != "remote" {
		return nil, validationError("workspace_project_type_unsupported", "Workspaces support local and remote projects")
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
	if err := s.ensureExcludeLineFor(ctx, project); err != nil {
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
	if s.provisioner != nil && project.Type == "local" {
		if manifest, ok := readProjectManifest(project.Path); ok {
			result, provErr := s.provisioner.Provision(ctx, project.ID, ws.ID, ws.Path, manifest)
			if provErr != nil {
				// Tear down ANY partial provision before failing: an approval refusal
				// executed nothing, but a setup/health failure may have allocated a
				// port and spawned earlier services — Teardown kills those processes
				// and releases their port rows so a failed provision never leaks a
				// port (which would eventually exhaust the allocator range) or orphans
				// a service.
				var approval *workspace.ApprovalRequiredError
				if !errors.As(provErr, &approval) {
					_ = s.provisioner.Teardown(ctx, ws.ID)
				}
				_ = s.db.SetWorkspaceStatus(ctx, ws.ID, "failed", "workspace.failed", actor)
				if approval != nil {
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

// Isolation modes accepted by sessions.create / start_worker.
const (
	// IsolationNever always runs in the project's main checkout (the default).
	IsolationNever = "never"
	// IsolationAuto uses the main checkout while it is free, and an isolated lane
	// once a live session already holds it.
	IsolationAuto = "auto"
	// IsolationAlways always runs in an isolated lane, even when the main checkout
	// is free — what an orchestrator picks when it already knows two workstreams
	// will overlap.
	IsolationAlways = "always"
)

// IsolationDecision is the outcome of resolving a session's isolation mode.
// ReservationToken is non-empty whenever a lane was claimed as part of the
// decision: the caller must NOT reserve it again, and must release the token if
// the session then fails to start.
type IsolationDecision struct {
	WorkspaceID      string
	ReservationToken string
}

// ResolveIsolation decides which working tree a new session runs in.
//
// Before this, isolation:"auto" only ever LEASED a pre-existing pooled lane, and
// failed with no_workspace_ready whenever the main path was busy and the pool
// happened to be empty. That made automatic isolation unusable in practice: an
// orchestrator had to pre-provision lanes it could not know it would need, and
// the outcome of "two sessions want one project" was an error rather than two
// trees. Both modes now provision on demand.
//
// The whole decision is serialized per project, for two reasons: concurrent
// `git worktree add` in one repository contends on .git metadata, and an unlocked
// pick-then-provision leaves a window in which a peer leases the lane we just
// created. The cost is that N simultaneous workers provision their lanes in
// sequence rather than in parallel.
//
// Known limit of "auto": the busy check reads persisted sessions, so two creates
// racing on a FREE main checkout both see it free and both use it. The conflict
// gate is what catches that; "always" is the deterministic choice for fan-out.
func (s *WorkspaceService) ResolveIsolation(ctx context.Context, projectID int64, mode string, authorization ActionAuthorization) (IsolationDecision, error) {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case "", IsolationNever:
		return IsolationDecision{}, nil
	case IsolationAuto, IsolationAlways:
	default:
		return IsolationDecision{}, validationError("invalid_isolation",
			`Isolation must be "auto" (a lane only when the main checkout is busy), "always" or "never"`)
	}

	unlock := s.lockProjectProvision(projectID)
	defer unlock()

	if normalized == IsolationAuto {
		busy, err := s.db.ProjectMainPathBusy(ctx, projectID)
		if err != nil {
			return IsolationDecision{}, err
		}
		if !busy {
			return IsolationDecision{}, nil // main checkout free → zero-overhead default
		}
	}

	// Prefer an already-provisioned idle lane: leasing one is instant, while
	// provisioning runs git and, when the project ships a manifest, its whole
	// environment. Pick-and-reserve is a single atomic statement.
	token := randomWorkspaceSuffix() + randomWorkspaceSuffix()
	laneID, err := s.db.ReserveIdleWorkspace(ctx, projectID, token)
	if err != nil {
		return IsolationDecision{}, err
	}
	if laneID != "" {
		return IsolationDecision{WorkspaceID: laneID, ReservationToken: token}, nil
	}

	created, err := s.Create(ctx, CreateWorkspaceCommand{
		ProjectID:     projectID,
		Name:          autoLaneName(),
		Authorization: authorization,
	})
	if err != nil {
		return IsolationDecision{}, err
	}
	if err := s.db.ReserveWorkspace(ctx, created.ID, token); err != nil {
		return IsolationDecision{}, conflictError("workspace_not_ready",
			"the freshly provisioned lane could not be reserved")
	}
	return IsolationDecision{WorkspaceID: created.ID, ReservationToken: token}, nil
}

// DirtyPaths reports which of the candidate project-relative paths currently
// carry uncommitted changes in the project's working tree.
//
// It is the safety check before moving a live session to another tree: a lane
// branches from HEAD, so anything written but not committed would silently stay
// behind in the old checkout.
func (s *WorkspaceService) DirtyPaths(ctx context.Context, project *database.Project, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	out, err := s.git.RunGit(ctx, project, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("reading working tree status: %w", err)
	}
	dirty := make(map[string]bool)
	for _, line := range splitLines(out) {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: two status chars, a space, then the path. A rename is
		// "R  old -> new"; the destination is the one that exists now.
		path := strings.TrimSpace(line[3:])
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		dirty[strings.Trim(strings.TrimSpace(path), `"`)] = true
	}
	var hits []string
	for _, candidate := range candidates {
		if dirty[candidate] {
			hits = append(hits, candidate)
		}
	}
	sort.Strings(hits)
	return hits, nil
}

// autoLaneName marks lanes the platform opened by itself, keeping them
// distinguishable from lanes a human named and expects to find again later.
func autoLaneName() string {
	return "auto-" + randomWorkspaceSuffix()
}

// lockProjectProvision serializes lane resolution for one project. Per-project
// rather than global: two different repositories never contend with each other.
func (s *WorkspaceService) lockProjectProvision(projectID int64) func() {
	actual, _ := s.provisionLocks.LoadOrStore(projectID, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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

// RemoteWorkspaceFS is the remote counterpart of the local FS seams —
// implemented by the config syncer (it owns the SSH/SFTP machinery).
type RemoteWorkspaceFS interface {
	EnsureRemoteExcludeLine(ctx context.Context, project *database.Project, line string) error
}

// ensureExcludeLineFor routes the exclude write to the host that owns the repo.
func (s *WorkspaceService) ensureExcludeLineFor(ctx context.Context, project *database.Project) error {
	if project.Type == "local" {
		return s.ensureExcludeLine(project)
	}
	remoteFS, ok := s.syncer.(RemoteWorkspaceFS)
	if !ok {
		return fmt.Errorf("remote workspace filesystem is unavailable")
	}
	return remoteFS.EnsureRemoteExcludeLine(ctx, project, workspaceExcludeLine)
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
		case project.Type != "local":
			// Remote lane: no local-FS fallbacks. Only accept the failure when
			// git itself no longer tracks the worktree (then prune metadata);
			// an SSH outage or live registration must fail CLOSED so the row
			// stays retryable instead of orphaning the lane on the host.
			if s.worktreeRegistered(ctx, project, ws.Path) {
				return nil, fmt.Errorf("git worktree remove (remote): %w", err)
			}
			_, _ = s.git.RunGit(ctx, project, "worktree", "prune")
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
	// Precondition: the main checkout must have no uncommitted TRACKED changes
	// (they make --no-ff fail midway). Untracked files (e.g. the config-sync
	// materialized .claude/ layer) never break a merge and must not block it.
	status, err := s.git.RunGit(ctx, project, "status", "--porcelain", "--untracked-files=no")
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

// ResolveForSession validates that a workspace can host a new session. A
// non-empty reservationToken means the caller already claimed this lane inside
// ResolveIsolation, so 'leased'-with-our-token is the expected state; an empty
// token means the lane must still be idle and ready.
func (s *WorkspaceService) ResolveForSession(ctx context.Context, project *database.Project, workspaceID, reservationToken string) (*database.Workspace, error) {
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
	if reservationToken != "" {
		if ws.Status != "leased" || ws.LeasedBySessionID.String != "pending:"+reservationToken {
			return nil, conflictError("workspace_reservation_lost",
				"the reserved lane is no longer held by this create")
		}
	} else if ws.Status != "ready" {
		return nil, conflictError("workspace_not_ready", fmt.Sprintf("Workspace is %s, not ready", ws.Status))
	}
	if project.Type == "local" {
		if _, err := os.Stat(ws.Path); err != nil {
			return nil, conflictError("workspace_gone", "Workspace directory no longer exists on disk")
		}
	}
	// Remote lanes: the path lives on the SSH host — existence is proven by the
	// git/worktree operations themselves; a local Stat would always fail.
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

// MergePrediction is the pre-merge forecast (Phase 7.5).
type MergePrediction struct {
	Clean bool `json:"clean"`
	// ConflictFiles are the files touched on BOTH sides since the merge base —
	// the conflict candidate set (a superset of real textual conflicts; empty
	// means the merge is guaranteed clean).
	ConflictFiles []string `json:"conflict_files"`
}

// PredictMerge forecasts a workspace merge without touching the tree. The
// verdict comes from `git merge-tree --write-tree` (exit 0 = clean); the file
// list on conflict comes from the both-sides-touched diff intersection
// (runGit drops stdout on non-zero exit, so merge-tree's own listing is not
// recoverable through the port). Works for local AND remote projects — every
// step is a read-only git command through the port.
func (s *WorkspaceService) PredictMerge(ctx context.Context, workspaceID string) (*MergePrediction, error) {
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	project, err := s.db.GetProject(ctx, ws.ProjectID)
	if err != nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	if _, err := s.git.RunGit(ctx, project, "merge-tree", "--write-tree", "--name-only", "HEAD", ws.Branch); err == nil {
		return &MergePrediction{Clean: true, ConflictFiles: []string{}}, nil
	}
	// merge-tree failed: EITHER real conflicts OR an old git without
	// --write-tree (possible on remote hosts). The diff intersection decides:
	// empty overlap → clean (sound modulo rename conflicts, which the real
	// merge still aborts on safely); non-empty → the candidate list.
	base, err := s.git.RunGit(ctx, project, "merge-base", "HEAD", ws.Branch)
	if err != nil {
		return nil, fmt.Errorf("merge-base: %w", err)
	}
	baseRef := strings.TrimSpace(base)
	mainSide, err := s.git.RunGit(ctx, project, "diff", "--name-only", baseRef+"..HEAD")
	if err != nil {
		return nil, fmt.Errorf("diff main side: %w", err)
	}
	laneSide, err := s.git.RunGit(ctx, project, "diff", "--name-only", baseRef+".."+ws.Branch)
	if err != nil {
		return nil, fmt.Errorf("diff lane side: %w", err)
	}
	mainTouched := map[string]bool{}
	for _, f := range splitLines(mainSide) {
		mainTouched[f] = true
	}
	var overlap []string
	for _, f := range splitLines(laneSide) {
		if mainTouched[f] {
			overlap = append(overlap, f)
		}
	}
	if len(overlap) == 0 {
		return &MergePrediction{Clean: true, ConflictFiles: []string{}}, nil
	}
	return &MergePrediction{Clean: false, ConflictFiles: overlap}, nil
}

// laneChangedFiles lists the files a lane changed since it diverged from the
// main line — the lane's own contribution, which is what two lanes have to be
// compared on. Read-only, and works for local and remote projects alike.
func (s *WorkspaceService) laneChangedFiles(ctx context.Context, project *database.Project, branch string) ([]string, error) {
	base, err := s.git.RunGit(ctx, project, "merge-base", "HEAD", branch)
	if err != nil {
		return nil, fmt.Errorf("merge-base: %w", err)
	}
	out, err := s.git.RunGit(ctx, project, "diff", "--name-only", strings.TrimSpace(base)+".."+branch)
	if err != nil {
		return nil, fmt.Errorf("diff lane side: %w", err)
	}
	return splitLines(out), nil
}

// LaneCollision names another lane this one shares changed files with.
type LaneCollision struct {
	WorkspaceID string   `json:"workspace_id"`
	Name        string   `json:"name"`
	Files       []string `json:"files"`
}

// MergePlanEntry is one lane's place in the integration order.
type MergePlanEntry struct {
	Order         int             `json:"order"`
	WorkspaceID   string          `json:"workspace_id"`
	Name          string          `json:"name"`
	Branch        string          `json:"branch"`
	ChangedFiles  []string        `json:"changed_files"`
	Clean         bool            `json:"clean"`
	ConflictFiles []string        `json:"conflict_files"`
	CollidesWith  []LaneCollision `json:"collides_with"`
}

// MergePlan is a proposed integration ORDER for a project's open lanes.
//
// It answers the question a merge storm makes expensive to answer by trial:
// which of these branches can go in untouched, and which two are about to fight
// over the same file. PredictMerge alone cannot say — it compares each lane
// against main, so two lanes that both rewrote util.go each predict "clean"
// while being guaranteed to collide with each other.
type MergePlan struct {
	ProjectID int64            `json:"project_id"`
	Entries   []MergePlanEntry `json:"entries"`
	// Independent is the count of lanes that touch no other lane's files: they
	// can be merged in any order, in parallel with each other.
	Independent int `json:"independent"`
	// RevalidateBeforeEachMerge is always true and says so out loud: every merge
	// moves HEAD, which invalidates the `clean` verdict of every lane still
	// queued. The ORDER stays valid (it comes from lane-vs-lane comparison, not
	// from HEAD), but the caller must re-run PredictMerge before each merge.
	RevalidateBeforeEachMerge bool `json:"revalidate_before_each_merge"`
}

// PlanMerges orders a project's mergeable lanes least-entangled first.
//
// The ordering rule is "merge what nobody else is touching, then the lanes with
// the fewest partners". Merging an entangled lane early forces every later lane
// to rebase around it; going in the other direction keeps the surprise
// concentrated in the lanes that were always going to need a human.
//
// Everything here is read-only git — no tree is touched and nothing is merged.
func (s *WorkspaceService) PlanMerges(ctx context.Context, projectID int64) (*MergePlan, error) {
	project, err := s.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	lanes, err := s.db.ListWorkspaces(ctx, projectID, "", 200)
	if err != nil {
		return nil, err
	}
	plan := &MergePlan{ProjectID: projectID, Entries: []MergePlanEntry{}, RevalidateBeforeEachMerge: true}

	type candidate struct {
		lane    database.Workspace
		changed []string
		clean   bool
		vsMain  []string
	}
	var candidates []candidate
	for _, lane := range lanes {
		// Only lanes that still exist and could be merged. A removed or merged
		// lane has nothing left to integrate.
		if lane.Status == "removed" || lane.Status == "merged" || lane.Status == "failed" {
			continue
		}
		changed, err := s.laneChangedFiles(ctx, project, lane.Branch)
		if err != nil {
			// A lane whose branch git cannot read (pruned, mid-provision) is
			// skipped rather than failing the whole plan — a partial order beats
			// no answer at all.
			continue
		}
		if len(changed) == 0 {
			continue // nothing to merge
		}
		prediction, err := s.PredictMerge(ctx, lane.ID)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			lane: lane, changed: changed, clean: prediction.Clean, vsMain: prediction.ConflictFiles,
		})
	}

	// Pairwise file intersection: the lane-vs-lane signal PredictMerge cannot give.
	collisions := make(map[string][]LaneCollision, len(candidates))
	for i := range candidates {
		for j := range candidates {
			if i == j {
				continue
			}
			shared := intersectSorted(candidates[i].changed, candidates[j].changed)
			if len(shared) == 0 {
				continue
			}
			collisions[candidates[i].lane.ID] = append(collisions[candidates[i].lane.ID], LaneCollision{
				WorkspaceID: candidates[j].lane.ID, Name: candidates[j].lane.Name, Files: shared,
			})
		}
	}

	// Least entangled first; ties broken by name so the plan is reproducible.
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := len(collisions[candidates[i].lane.ID]), len(collisions[candidates[j].lane.ID])
		if di != dj {
			return di < dj
		}
		return candidates[i].lane.Name < candidates[j].lane.Name
	})

	for idx, c := range candidates {
		partners := collisions[c.lane.ID]
		if partners == nil {
			partners = []LaneCollision{}
			plan.Independent++
		}
		sort.Slice(partners, func(a, b int) bool { return partners[a].Name < partners[b].Name })
		conflicts := c.vsMain
		if conflicts == nil {
			conflicts = []string{}
		}
		plan.Entries = append(plan.Entries, MergePlanEntry{
			Order: idx + 1, WorkspaceID: c.lane.ID, Name: c.lane.Name, Branch: c.lane.Branch,
			ChangedFiles: c.changed, Clean: c.clean, ConflictFiles: conflicts, CollidesWith: partners,
		})
	}
	return plan, nil
}

// intersectSorted returns the sorted common elements of two file lists.
func intersectSorted(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, item := range b {
		inB[item] = struct{}{}
	}
	var shared []string
	for _, item := range a {
		if _, ok := inB[item]; ok {
			shared = append(shared, item)
		}
	}
	sort.Strings(shared)
	return shared
}
