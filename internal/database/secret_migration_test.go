package database

import (
	"context"
	"errors"
	"testing"

	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
)

func TestLegacyRuntimeSecretMigrationDryRunExecuteAndIdempotence(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := &Project{Name: "migration", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	global := &MCPServer{Name: "global", Command: "node", Args: `["server.js"]`, Env: `{"TOKEN":"global-secret"}`, Enabled: true}
	projectMCP := &ProjectMCPServer{ProjectID: project.ID, Name: "project", Command: "bun", Args: `["x","mcp"]`, Env: `{}`, Enabled: true}
	tool := &ProjectTool{ProjectID: project.ID, Name: "deploy", Command: "deploy --token private", Parameters: `{}`, Enabled: true}
	if err := db.CreateMCPServer(ctx, global); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProjectMCPServer(ctx, projectMCP); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProjectTool(ctx, tool); err != nil {
		t.Fatal(err)
	}

	dryRun, err := MigrateLegacyRuntimeSecrets(ctx, db, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Executed || dryRun.GlobalMCPRecords != 1 || dryRun.ProjectMCPRecords != 1 || dryRun.CustomToolRecords != 1 || dryRun.Fields != 7 {
		t.Fatalf("dry-run report = %+v", dryRun)
	}
	storedGlobal, _ := db.GetMCPServer(ctx, global.ID)
	if storedGlobal.Command != "node" || secretvalue.IsEncrypted(storedGlobal.Command) {
		t.Fatalf("dry run mutated global MCP: %+v", storedGlobal)
	}

	encryptor, err := security.NewEncryptor("legacy-runtime-migration-test")
	if err != nil {
		t.Fatal(err)
	}
	report, err := MigrateLegacyRuntimeSecrets(ctx, db, encryptor, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Executed || report.GlobalMCPRecords != 1 || report.ProjectMCPRecords != 1 || report.CustomToolRecords != 1 || report.Fields != 7 {
		t.Fatalf("execute report = %+v", report)
	}
	storedGlobal, _ = db.GetMCPServer(ctx, global.ID)
	storedProject, _ := db.GetProjectMCPServer(ctx, projectMCP.ID)
	storedTool, _ := db.GetProjectTool(ctx, tool.ID)
	for label, value := range map[string]string{
		"global command": storedGlobal.Command, "global args": storedGlobal.Args, "global env": storedGlobal.Env,
		"project command": storedProject.Command, "project args": storedProject.Args, "project env": storedProject.Env,
		"tool command": storedTool.Command,
	} {
		if !secretvalue.IsEncrypted(value) {
			t.Fatalf("%s was not encrypted", label)
		}
	}
	resolved, err := secretvalue.Resolve(storedTool.Command, encryptor.Decrypt)
	if err != nil || resolved != tool.Command {
		t.Fatalf("tool round trip = %q, err=%v", resolved, err)
	}

	second, err := MigrateLegacyRuntimeSecrets(ctx, db, encryptor, true)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Executed || second.GlobalMCPRecords != 0 || second.ProjectMCPRecords != 0 || second.CustomToolRecords != 0 || second.Fields != 0 {
		t.Fatalf("second report = %+v", second)
	}
}

type failingMigrationEncryptor struct {
	delegate *security.Encryptor
	calls    int
	failAt   int
}

func (e *failingMigrationEncryptor) Encrypt(value string) (string, string, error) {
	e.calls++
	if e.calls == e.failAt {
		return "", "", errors.New("injected encryption failure")
	}
	return e.delegate.Encrypt(value)
}

func TestLegacyRuntimeSecretMigrationRollsBackEveryTable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := &Project{Name: "rollback", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	global := &MCPServer{Name: "global", Command: "node", Args: `[]`, Env: `{}`, Enabled: true}
	tool := &ProjectTool{ProjectID: project.ID, Name: "tool", Command: "printf rollback", Parameters: `{}`, Enabled: true}
	if err := db.CreateMCPServer(ctx, global); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProjectTool(ctx, tool); err != nil {
		t.Fatal(err)
	}
	delegate, err := security.NewEncryptor("rollback-migration-test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = MigrateLegacyRuntimeSecrets(ctx, db, &failingMigrationEncryptor{delegate: delegate, failAt: 4}, true)
	if err == nil {
		t.Fatal("expected migration failure")
	}
	storedGlobal, _ := db.GetMCPServer(ctx, global.ID)
	storedTool, _ := db.GetProjectTool(ctx, tool.ID)
	if storedGlobal.Command != global.Command || storedGlobal.Args != global.Args || storedGlobal.Env != global.Env || storedTool.Command != tool.Command {
		t.Fatalf("rollback failed: global=%+v tool=%+v", storedGlobal, storedTool)
	}
}
