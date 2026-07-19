package application

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
)

// TestSessionBackendOverride: the override rides a shallow project copy (the
// original is never mutated), unknown backends are refused, empty/no-op
// overrides return the project untouched.
func TestSessionBackendOverride(t *testing.T) {
	project := &database.Project{ID: 1, Backend: "claude_code", Path: "/tmp/x"}

	same, err := applySessionBackendOverride(project, "")
	if err != nil || same != project {
		t.Fatalf("empty override must be a no-op (err %v)", err)
	}
	same, err = applySessionBackendOverride(project, "claude_code")
	if err != nil || same != project {
		t.Fatalf("same-backend override must be a no-op (err %v)", err)
	}

	overridden, err := applySessionBackendOverride(project, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if overridden == project || overridden.Backend != "codex" {
		t.Fatalf("override must be a copy with the new backend: %+v", overridden)
	}
	if project.Backend != "claude_code" {
		t.Fatal("original project mutated by override")
	}

	if _, err := applySessionBackendOverride(project, "banana"); err == nil {
		t.Fatal("unknown backend must be refused")
	}
}

type remoteWSFakeGit struct {
	calls [][]string
}

func (g *remoteWSFakeGit) RunGit(_ context.Context, _ *database.Project, args ...string) (string, error) {
	g.calls = append(g.calls, args)
	if len(args) > 0 && args[0] == "rev-parse" {
		if len(args) > 1 && args[1] == "--abbrev-ref" {
			return "main", nil
		}
		if args[1] == "--verify" {
			return "", &fakeGitErr{}
		}
		return "true", nil
	}
	return "", nil
}

type fakeGitErr struct{}

func (e *fakeGitErr) Error() string { return "unknown revision" }

type remoteWSFakeSyncer struct {
	materialized []string
	excluded     []string
}

func (s *remoteWSFakeSyncer) MaterializeToWorkspace(_ context.Context, project *database.Project) error {
	s.materialized = append(s.materialized, project.Path)
	return nil
}

func (s *remoteWSFakeSyncer) EnsureRemoteExcludeLine(_ context.Context, _ *database.Project, line string) error {
	s.excluded = append(s.excluded, line)
	return nil
}

// TestRemoteWorkspaceCreateUsesGitPort: a REMOTE project's workspace is created
// entirely through the git port (worktree add over SSH) and the remote FS seam
// (exclude line via SFTP) — no local-only rejection, no local filesystem call.
func TestRemoteWorkspaceCreateUsesGitPort(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "rws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	project := &database.Project{Name: "rp", Path: "/srv/repo", Type: "remote", Backend: "claude_code"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	git := &remoteWSFakeGit{}
	syncer := &remoteWSFakeSyncer{}
	service := NewWorkspaceService(db, git, syncer)

	ws, err := service.Create(ctx, CreateWorkspaceCommand{
		ProjectID: project.ID, Name: "lane-r",
		Authorization: ActionAuthorization{Actor: Actor{Type: "test", ID: "t"}},
	})
	if err != nil {
		t.Fatalf("remote create must succeed via ports: %v", err)
	}
	if ws.Status != "ready" {
		t.Fatalf("workspace not ready: %s", ws.Status)
	}
	if len(syncer.excluded) != 1 {
		t.Fatalf("exclude line must go through the remote FS seam: %v", syncer.excluded)
	}
	sawWorktreeAdd := false
	for _, call := range git.calls {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "add" {
			sawWorktreeAdd = true
			if !strings.Contains(strings.Join(call, " "), "/srv/repo/.openpoet/worktrees/lane-r") {
				t.Fatalf("worktree add outside the managed root: %v", call)
			}
		}
	}
	if !sawWorktreeAdd {
		t.Fatalf("no worktree add issued via git port: %v", git.calls)
	}
	if len(syncer.materialized) != 1 || syncer.materialized[0] != ws.Path {
		t.Fatalf("lane not materialized via syncer: %v", syncer.materialized)
	}
}
