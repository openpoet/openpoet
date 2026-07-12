package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
)

func TestBuildMCPConfigResolvesEncryptedAndLegacyValues(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "session-secret-mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project := &database.Project{Name: "runtime", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	encryptor, err := security.NewEncryptor("session-mcp-runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	command, _ := secretvalue.Encrypt(encryptor, "node")
	args, _ := secretvalue.Encrypt(encryptor, `["server.js"]`)
	env, _ := secretvalue.Encrypt(encryptor, `{"TOKEN":"runtime-secret"}`)
	if err := db.CreateMCPServer(ctx, &database.MCPServer{
		Name: "encrypted", Command: command, Args: args, Env: env, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProjectMCPServer(ctx, &database.ProjectMCPServer{
		ProjectID: project.ID, Name: "legacy", Command: "bun", Args: `["x","mcp"]`, Env: `{}`, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	manager := &Manager{db: db}
	manager.SetSecretDecryptor(encryptor.Decrypt)
	raw, err := manager.buildMCPConfigJSON(ctx, project, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	encrypted := config.MCPServers["encrypted"]
	if encrypted.Command != "node" || len(encrypted.Args) != 1 || encrypted.Args[0] != "server.js" || encrypted.Env["TOKEN"] != "runtime-secret" {
		t.Fatalf("encrypted runtime config was not resolved: %+v", encrypted)
	}
	legacy := config.MCPServers["legacy"]
	if legacy.Command != "bun" || len(legacy.Args) != 2 {
		t.Fatalf("legacy runtime config changed: %+v", legacy)
	}
	if strings.Contains(raw, "ciphertext") || strings.Contains(raw, command) {
		t.Fatal("runtime config retained the storage envelope")
	}
}

func TestBuildMCPConfigFailsClosedWithoutDecryptor(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "session-secret-mcp-missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project := &database.Project{Name: "runtime", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	encryptor, _ := security.NewEncryptor("session-mcp-missing-test")
	command, _ := secretvalue.Encrypt(encryptor, "private-command")
	if err := db.CreateMCPServer(ctx, &database.MCPServer{Name: "encrypted", Command: command, Args: `[]`, Env: `{}`, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err = (&Manager{db: db}).buildMCPConfigJSON(ctx, project, "session-1")
	if err == nil || strings.Contains(err.Error(), "private-command") || strings.Contains(err.Error(), command) {
		t.Fatalf("unsafe missing-decryptor error: %v", err)
	}
}
