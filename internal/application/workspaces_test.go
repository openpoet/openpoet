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
	commands [][]string
	failOn   string
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
	case args[0] == "worktree" && args[1] == "add":
		return "", os.MkdirAll(args[3], 0o755)
	case args[0] == "worktree" && args[1] == "remove":
		return "", os.RemoveAll(args[3])
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

func TestWorkspaceRemoteNotSupported(t *testing.T) {
	svc, git, _, _, remote := workspaceTestFixture(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateWorkspaceCommand{ProjectID: remote.ID, Name: "lane-r", Authorization: workspaceTestAuth})
	if code := applicationErrorCode(err); code != "workspace_remote_unsupported" {
		t.Fatalf("remote error = %v (code %q), want workspace_remote_unsupported", err, code)
	}
	if len(git.commands) != 0 {
		t.Fatalf("remote create still ran git: %v", git.commands)
	}
	rows, _ := svc.List(ctx, remote.ID, "", 10)
	if len(rows) != 0 {
		t.Fatalf("rows for remote project = %d, want 0 (no orphan row)", len(rows))
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
