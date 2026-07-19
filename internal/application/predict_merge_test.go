package application

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"openpoet/internal/database"
)

// realGitPort runs actual git against the project path (local-only test port).
type realGitPort struct{}

func (realGitPort) RunGit(ctx context.Context, project *database.Project, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = project.Path
	out, err := cmd.Output()
	return string(out), err
}

type noopSyncer struct{}

func (noopSyncer) MaterializeToWorkspace(context.Context, *database.Project) error { return nil }

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestPredictMergeCleanAndConflict: a lane-only change predicts clean; edits on
// both sides of the same file predict conflict naming the file.
func TestPredictMergeCleanAndConflict(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "pm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	if err := writeTestFile(filepath.Join(repo, "a.go"), "package a\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-qm", "init")

	project := &database.Project{Name: "pm", Path: repo, Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	service := NewWorkspaceService(db, realGitPort{}, noopSyncer{})
	ws, err := service.Create(ctx, CreateWorkspaceCommand{
		ProjectID: project.ID, Name: "pm-lane",
		Authorization: ActionAuthorization{Actor: Actor{Type: "test", ID: "t"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Lane-only change → clean prediction.
	if err := writeTestFile(filepath.Join(ws.Path, "a.go"), "package a // lane\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, ws.Path, "commit", "-qam", "lane edit")
	prediction, err := service.PredictMerge(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !prediction.Clean || len(prediction.ConflictFiles) != 0 {
		t.Fatalf("lane-only change must predict clean: %+v", prediction)
	}

	// Same file edited on main → conflict naming a.go.
	if err := writeTestFile(filepath.Join(repo, "a.go"), "package a // main\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "commit", "-qam", "main edit")
	prediction, err = service.PredictMerge(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prediction.Clean {
		t.Fatalf("both-sides edit must predict conflict: %+v", prediction)
	}
	found := false
	for _, f := range prediction.ConflictFiles {
		if f == "a.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("conflict prediction must name a.go: %+v", prediction)
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
