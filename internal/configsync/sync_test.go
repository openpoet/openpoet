package configsync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestClaudeHooksTrackSessionStartForResume(t *testing.T) {
	cs, _ := setupConfigSyncTest(t)
	hooks := cs.buildHooksConfig()
	if _, ok := hooks["SessionStart"]; !ok {
		t.Fatal("Claude hooks do not include SessionStart")
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

func TestSyncToLocalOpenCodeWritesNativeConfig(t *testing.T) {
	cs, project := setupConfigSyncTest(t)
	project.Backend = "opencode"
	project.BackendConfig = `{"model":"anthropic/claude-sonnet-4-5","agent":"plan","permission_mode":"ask","enable_mcp":true}`

	if err := os.WriteFile(filepath.Join(project.Path, "opencode.json"), []byte(`{"autoupdate":false}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.Path, "AGENTS.md"), []byte("agent instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := cs.db.CreateMCPServer(context.Background(), &database.MCPServer{
		Name:    "global",
		Command: "node",
		Args:    `["server.js"]`,
		Env:     `{"TOKEN":"global"}`,
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cs.db.CreateProjectMCPServer(context.Background(), &database.ProjectMCPServer{
		ProjectID: project.ID,
		Name:      "project",
		Command:   "bun",
		Args:      `["x","mcp"]`,
		Env:       `{}`,
		Enabled:   true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := cs.SyncToProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(filepath.Join(project.Path, ".opencode", "skills")); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() {
		t.Fatal(".opencode/skills is not a directory")
	}
	pluginPath := filepath.Join(project.Path, ".opencode", "plugins", "openpoet.js")
	pluginData, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pluginData), "AskUserQuestion") || !strings.Contains(string(pluginData), "opencode_plan_updated") {
		t.Fatalf("OpenCode plugin missing expected hooks/tools:\n%s", string(pluginData))
	}
	assertFileContent(t, filepath.Join(project.Path, "CLAUDE.md"), "agent instructions")

	var config map[string]interface{}
	data, err := os.ReadFile(filepath.Join(project.Path, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("$schema = %q", config["$schema"])
	}
	if config["autoupdate"] != false {
		t.Fatalf("autoupdate = %v, want false", config["autoupdate"])
	}
	if config["model"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("model = %q", config["model"])
	}
	if config["default_agent"] != "plan" {
		t.Fatalf("default_agent = %q", config["default_agent"])
	}
	if config["permission"] != "ask" {
		t.Fatalf("permission = %q", config["permission"])
	}

	mcp, ok := config["mcp"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp config missing or wrong type: %#v", config["mcp"])
	}
	projectMCP := mcp["project"].(map[string]interface{})
	if projectMCP["type"] != "local" {
		t.Fatalf("project MCP type = %q", projectMCP["type"])
	}
	command := projectMCP["command"].([]interface{})
	if len(command) != 3 || command[0] != "bun" || command[1] != "x" || command[2] != "mcp" {
		t.Fatalf("project MCP command = %#v", command)
	}
	globalMCP := mcp["global"].(map[string]interface{})
	env := globalMCP["environment"].(map[string]interface{})
	if env["TOKEN"] != "global" {
		t.Fatalf("global MCP env TOKEN = %q", env["TOKEN"])
	}
}
