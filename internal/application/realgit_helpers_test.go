package application

import (
	"context"
	"os"
	"os/exec"
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

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
