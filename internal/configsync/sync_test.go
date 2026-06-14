package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/database"
)

func setupConfigSyncTest(t *testing.T) (*ConfigSyncer, *database.Project) {
	t.Helper()
	root := t.TempDir()
	db, err := database.New(filepath.Join(root, "openpoet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	project := &database.Project{
		Name:          "test-project",
		Path:          projectDir,
		Type:          "local",
		Backend:       "codex",
		BackendConfig: "{}",
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}

	return NewConfigSyncer(db, nil, ""), project
}

func writeMemoryFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", filepath.Base(path), string(got), want)
	}
}

func TestSyncCodexMemoryDocLocalUsesNewerClaude(t *testing.T) {
	cs, project := setupConfigSyncTest(t)
	now := time.Now()
	agentsPath := filepath.Join(project.Path, "AGENTS.md")
	claudePath := filepath.Join(project.Path, "CLAUDE.md")

	writeMemoryFile(t, agentsPath, "old agents", now.Add(-2*time.Hour))
	writeMemoryFile(t, claudePath, "new claude", now.Add(-time.Hour))

	source, err := cs.syncCodexMemoryDocLocal(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if source != "CLAUDE.md" {
		t.Fatalf("source = %q, want CLAUDE.md", source)
	}

	assertFileContent(t, agentsPath, "new claude")
	assertFileContent(t, claudePath, "new claude")
	doc, err := cs.db.GetMemoryDoc(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != "new claude" {
		t.Fatalf("memory doc = %q, want new claude", doc.Content)
	}
}

func TestSyncCodexMemoryDocLocalUsesNewerAgents(t *testing.T) {
	cs, project := setupConfigSyncTest(t)
	now := time.Now()
	agentsPath := filepath.Join(project.Path, "AGENTS.md")
	claudePath := filepath.Join(project.Path, "CLAUDE.md")

	writeMemoryFile(t, claudePath, "old claude", now.Add(-2*time.Hour))
	writeMemoryFile(t, agentsPath, "new agents", now.Add(-time.Hour))

	source, err := cs.syncCodexMemoryDocLocal(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if source != "AGENTS.md" {
		t.Fatalf("source = %q, want AGENTS.md", source)
	}

	assertFileContent(t, agentsPath, "new agents")
	assertFileContent(t, claudePath, "new agents")
	doc, err := cs.db.GetMemoryDoc(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != "new agents" {
		t.Fatalf("memory doc = %q, want new agents", doc.Content)
	}
}

func TestSyncCodexMemoryDocLocalUsesNewerDBMemoryDoc(t *testing.T) {
	cs, project := setupConfigSyncTest(t)
	now := time.Now()
	agentsPath := filepath.Join(project.Path, "AGENTS.md")
	claudePath := filepath.Join(project.Path, "CLAUDE.md")

	writeMemoryFile(t, agentsPath, "old agents", now.Add(-2*time.Hour))
	writeMemoryFile(t, claudePath, "old claude", now.Add(-2*time.Hour))
	if _, err := cs.db.UpsertMemoryDoc(context.Background(), project.ID, "new db", "test", ""); err != nil {
		t.Fatal(err)
	}

	source, err := cs.syncCodexMemoryDocLocal(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if source != "memory doc" {
		t.Fatalf("source = %q, want memory doc", source)
	}

	assertFileContent(t, agentsPath, "new db")
	assertFileContent(t, claudePath, "new db")
	doc, err := cs.db.GetMemoryDoc(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != "new db" {
		t.Fatalf("memory doc = %q, want new db", doc.Content)
	}
}
