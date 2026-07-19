package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
)

// fakeWorkspaceGit records git invocations and simulates worktree add/remove
// on the real filesystem so the service's stat checks hold.
type fakeWorkspaceGit struct {
	commands           [][]string
	failOn             string
	branchExists       bool
	dirtyMain          bool
	mergeConflictFiles string
	abortRan           bool
}

func (f *fakeWorkspaceGit) RunGit(_ context.Context, project *database.Project, args ...string) (string, error) {
	f.commands = append(f.commands, args)
	joined := strings.Join(args, " ")
	if f.failOn != "" && strings.HasPrefix(joined, f.failOn) {
		return "", fmt.Errorf("simulated git failure: %s", joined)
	}
	switch {
	case joined == "rev-parse --is-inside-work-tree":
		return "true\n", nil
	case joined == "rev-parse --abbrev-ref HEAD":
		return "main\n", nil
	case args[0] == "rev-parse" && args[1] == "--verify":
		if f.branchExists {
			return "", nil
		}
		return "", fmt.Errorf("unknown ref")
	case args[0] == "worktree" && args[1] == "add":
		path := args[2]
		if path == "-b" {
			path = args[4]
		}
		return "", os.MkdirAll(path, 0o755)
	case args[0] == "worktree" && args[1] == "remove":
		return "", os.RemoveAll(args[len(args)-1])
	case joined == "status --porcelain --untracked-files=no":
		if f.dirtyMain {
			return " M file.go\n", nil
		}
		return "", nil
	case joined == "diff --name-only --diff-filter=U":
		return f.mergeConflictFiles, nil
	case args[0] == "merge" && len(args) > 1 && args[1] == "--abort":
		f.abortRan = true
		return "", nil
	}
	return "", nil
}

type fakeWorkspaceSyncer struct {
	materialized []string
}

func (f *fakeWorkspaceSyncer) MaterializeToWorkspace(_ context.Context, project *database.Project) error {
	f.materialized = append(f.materialized, project.Path)
	return nil
}

var workspaceTestAuth = ActionAuthorization{Actor: Actor{Type: "agent", ID: "helena"}}

func applicationErrorCode(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func workspaceTestFixture(t *testing.T) (*WorkspaceService, *fakeWorkspaceGit, *fakeWorkspaceSyncer, *database.Project, *database.Project) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.New(filepath.Join(dir, "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	localRoot := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(localRoot, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := &database.Project{Name: "local", Path: localRoot, Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(ctx, local); err != nil {
		t.Fatal(err)
	}
	remote := &database.Project{Name: "remote", Path: "/srv/repo", Type: "remote", Backend: "claude_code"}
	if err := db.CreateProject(ctx, remote); err != nil {
		t.Fatal(err)
	}
	git := &fakeWorkspaceGit{}
	syncer := &fakeWorkspaceSyncer{}
	return NewWorkspaceService(db, git, syncer), git, syncer, local, remote
}

func TestWorkspaceCreateBecomesReady(t *testing.T) {
	svc, git, syncer, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	ws, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-a", Authorization: workspaceTestAuth})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Status != "ready" || ws.Kind != "worktree" {
		t.Fatalf("workspace = %s/%s, want ready/worktree", ws.Status, ws.Kind)
	}
	if ws.Branch != "openpoet/lane-a" || ws.BaseRef != "main" {
		t.Fatalf("branch/base = %s/%s", ws.Branch, ws.BaseRef)
	}
	wantPath := filepath.Join(local.Path, ".openpoet", "worktrees", "lane-a")
	if ws.Path != wantPath {
		t.Fatalf("path = %s, want %s", ws.Path, wantPath)
	}
	if len(syncer.materialized) != 1 || syncer.materialized[0] != ws.Path {
		t.Fatalf("materialize-only sync ran %v, want exactly the lane path", syncer.materialized)
	}
	// The exclude line landed before the worktree was created.
	exclude, err := os.ReadFile(filepath.Join(local.Path, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(exclude), "/.openpoet/") {
		t.Fatalf("exclude line missing: %q err=%v", exclude, err)
	}
	sawWorktreeAdd := false
	for _, cmd := range git.commands {
		if cmd[0] == "worktree" && cmd[1] == "add" {
			sawWorktreeAdd = true
			if cmd[2] != "-b" || cmd[3] != "openpoet/lane-a" {
				t.Fatalf("worktree add args = %v", cmd)
			}
		}
	}
	if !sawWorktreeAdd {
		t.Fatal("git worktree add never ran")
	}
}

func TestWorkspaceNameCollisionTyped(t *testing.T) {
	svc, _, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-a", Authorization: workspaceTestAuth}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-a", Authorization: workspaceTestAuth})
	if code := applicationErrorCode(err); code != "workspace_name_conflict" {
		t.Fatalf("duplicate name error = %v (code %q), want workspace_name_conflict", err, code)
	}
	rows, err := svc.List(ctx, local.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, ws := range rows {
		if ws.Status != "removed" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active rows = %d, want 1 (no half-created row)", active)
	}
}

func TestWorkspaceRejectsEscapingNames(t *testing.T) {
	svc, git, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	for _, name := range []string{"../../etc", "a/b", "..", ".hidden", "UPPER", "a..b"} {
		_, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: name, Authorization: workspaceTestAuth})
		if code := applicationErrorCode(err); code != "workspace_name_invalid" {
			t.Fatalf("name %q error = %v (code %q), want workspace_name_invalid", name, err, code)
		}
	}
	for _, cmd := range git.commands {
		if cmd[0] == "worktree" {
			t.Fatalf("escaping name still reached git: %v", cmd)
		}
	}
	rows, _ := svc.List(ctx, local.ID, "", 10)
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// Phase 7.3 superseded the phase-2 local-only restriction: remote projects ARE
// supported, but only through the remote FS seam — a syncer without it must
// fail CLOSED before any git command runs (this fixture's syncer has no remote
// FS implementation).
func TestWorkspaceRemoteNotSupported(t *testing.T) {
	svc, git, _, _, remote := workspaceTestFixture(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: remote.ID, Name: "lane-r", Authorization: workspaceTestAuth})
	if err == nil || !strings.Contains(err.Error(), "remote workspace filesystem is unavailable") {
		t.Fatalf("remote without an FS seam must fail closed, got %v", err)
	}
	sawWorktree := false
	for _, cmd := range git.commands {
		if len(cmd) > 0 && cmd[0] == "worktree" {
			sawWorktree = true
		}
	}
	if sawWorktree {
		t.Fatalf("remote create without FS seam still ran worktree commands: %v", git.commands)
	}
	ready, _ := svc.List(ctx, remote.ID, "ready", 10)
	if len(ready) != 0 {
		t.Fatalf("ready rows for failed remote create = %d, want 0", len(ready))
	}
}

func TestWorkspaceRemoveRequiresApprovalAndTearsDown(t *testing.T) {
	svc, _, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	ws, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-a", Authorization: workspaceTestAuth})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Remove(ctx, RemoveWorkspaceCommand{WorkspaceID: ws.ID, Authorization: workspaceTestAuth}); err == nil {
		t.Fatal("remove without explicit approval succeeded")
	}
	approved := workspaceTestAuth
	approved.Approved = true
	approved.ApprovedBy = "warden"
	approved.Reason = "test teardown"
	removed, err := svc.Remove(ctx, RemoveWorkspaceCommand{WorkspaceID: ws.ID, Authorization: approved})
	if err != nil {
		t.Fatal(err)
	}
	if removed.Status != "removed" {
		t.Fatalf("status = %s, want removed", removed.Status)
	}
	if _, err := os.Stat(ws.Path); err == nil {
		t.Fatal("lane directory still exists after remove")
	}
}

// TestWorkspaceCreateFailureMarksFailed pins the failure leg the review found
// untested: a git failure flips the row to 'failed' (+event), and — because
// failed rows do not occupy the name — the same name is retryable WITHOUT a
// destructive-tier cleanup.
func TestWorkspaceCreateFailureMarksFailed(t *testing.T) {
	svc, git, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	git.failOn = "worktree add"
	_, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-f", Authorization: workspaceTestAuth})
	if err == nil {
		t.Fatal("create with failing git succeeded")
	}
	rows, err := svc.List(ctx, local.ID, "failed", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("failed rows = %d (%v), want 1", len(rows), err)
	}
	// The name is NOT blocked by the failed residue.
	git.failOn = ""
	ws, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-f", Authorization: workspaceTestAuth})
	if err != nil {
		t.Fatalf("retry after failure blocked: %v", err)
	}
	if ws.Status != "ready" {
		t.Fatalf("retry status = %s, want ready", ws.Status)
	}
}

// TestWorkspaceRecreateAttachesSurvivingBranch: a branch left behind by remove
// (unmerged commits, branch -d refused) must not brick the name — recreate
// attaches to the surviving branch instead of failing on -b.
func TestWorkspaceRecreateAttachesSurvivingBranch(t *testing.T) {
	svc, git, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	git.branchExists = true
	ws, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-a", Authorization: workspaceTestAuth})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Status != "ready" {
		t.Fatalf("status = %s", ws.Status)
	}
	for _, cmd := range git.commands {
		if cmd[0] == "worktree" && cmd[1] == "add" && cmd[2] == "-b" {
			t.Fatalf("attach path still used -b: %v", cmd)
		}
	}
}

// TestWorkspaceReservationPreventsDoubleBooking pins the CAS: two concurrent
// creates race on Reserve, and exactly one wins.
func TestWorkspaceReservationPreventsDoubleBooking(t *testing.T) {
	svc, _, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	ws, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-a", Authorization: workspaceTestAuth})
	if err != nil {
		t.Fatal(err)
	}
	token1, err := svc.Reserve(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, ws.ID); err == nil {
		t.Fatal("second reservation of a leased lane succeeded (double-booking)")
	}
	if err := svc.Bind(ctx, ws.ID, token1, "sess-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, ws.ID)
	if !got.LeasedBySessionID.Valid || got.LeasedBySessionID.String != "sess-1" {
		t.Fatalf("lease = %+v, want sess-1", got.LeasedBySessionID)
	}
	// ReLease by the SAME session succeeds (reopen path); by another, fails.
	if err := svc.ReLease(ctx, ws.ID, "sess-1"); err != nil {
		t.Fatalf("self re-lease failed: %v", err)
	}
	if err := svc.ReLease(ctx, ws.ID, "sess-2"); err == nil {
		t.Fatal("foreign re-lease of an occupied lane succeeded")
	}
	if err := svc.ReleaseForSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReLease(ctx, ws.ID, "sess-2"); err != nil {
		t.Fatalf("re-lease of freed lane failed: %v", err)
	}
}

// TestWorkspaceMergeCleanAndConflict pins merge-back: clean merge flips the row
// to 'merged'; a conflict aborts (main left clean) and returns the file list
// on the success path, not as an error.
func TestWorkspaceMergeCleanAndConflict(t *testing.T) {
	svc, git, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	ws, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-m", Authorization: workspaceTestAuth})
	if err != nil {
		t.Fatal(err)
	}
	approved := workspaceTestAuth
	approved.Approved, approved.ApprovedBy, approved.Reason = true, "warden", "merge"

	// Clean merge (fake git returns success for merge).
	res, err := svc.Merge(ctx, MergeWorkspaceCommand{WorkspaceID: ws.ID, Authorization: approved})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged || res.Workspace.Status != "merged" {
		t.Fatalf("clean merge = %+v, want merged", res)
	}

	// Conflict leg: git merge fails, diff returns a file list, abort runs.
	ws2, _ := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-c", Authorization: workspaceTestAuth})
	git.mergeConflictFiles = "calc.go\n"
	git.failOn = "merge --no-ff"
	res, err = svc.Merge(ctx, MergeWorkspaceCommand{WorkspaceID: ws2.ID, Authorization: approved})
	if err != nil {
		t.Fatalf("conflict returned an error (file list would be lost): %v", err)
	}
	if res.Merged || len(res.ConflictFiles) != 1 || res.ConflictFiles[0] != "calc.go" {
		t.Fatalf("conflict result = %+v, want merged=false + [calc.go]", res)
	}
	if !git.abortRan {
		t.Fatal("merge --abort did not run after conflict (main left dirty)")
	}
}

// TestWorkspaceMergeRequiresApprovalAndCleanMain pins the two refusals.
func TestWorkspaceMergeRequiresApprovalAndCleanMain(t *testing.T) {
	svc, git, _, local, _ := workspaceTestFixture(t)
	ctx := context.Background()
	ws, _ := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: local.ID, Name: "lane-m", Authorization: workspaceTestAuth})
	if _, err := svc.Merge(ctx, MergeWorkspaceCommand{WorkspaceID: ws.ID, Authorization: workspaceTestAuth}); err == nil {
		t.Fatal("merge without explicit approval succeeded")
	}
	approved := workspaceTestAuth
	approved.Approved, approved.ApprovedBy, approved.Reason = true, "warden", "merge"
	git.dirtyMain = true
	_, err := svc.Merge(ctx, MergeWorkspaceCommand{WorkspaceID: ws.ID, Authorization: approved})
	if code := applicationErrorCode(err); code != "workspace_main_dirty" {
		t.Fatalf("dirty-main merge error = %v (code %q), want workspace_main_dirty", err, code)
	}
}
