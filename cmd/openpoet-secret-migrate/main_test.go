package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
)

func TestSecretMigrationCLIDryRunAndExecute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli-migration.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &database.MCPServer{Name: "cli", Command: "node private-cli", Args: `[]`, Env: `{}`, Enabled: true}
	if err := db.CreateMCPServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var dryOutput bytes.Buffer
	if err := run([]string{"--db", path}, &dryOutput); err != nil {
		t.Fatal(err)
	}
	var dry database.LegacyRuntimeSecretMigrationReport
	if err := json.Unmarshal(dryOutput.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if dry.GlobalMCPRecords != 1 || strings.Contains(dryOutput.String(), "private-cli") || strings.Contains(dryOutput.String(), "executed") {
		t.Fatalf("dry-run output = %s", dryOutput.String())
	}

	t.Setenv("OPENPOET_ENCRYPT_KEY", "cli-migration-test-key")
	var executeOutput bytes.Buffer
	if err := run([]string{"--db", path, "--execute"}, &executeOutput); err != nil {
		t.Fatal(err)
	}
	var executed database.LegacyRuntimeSecretMigrationReport
	if err := json.Unmarshal(executeOutput.Bytes(), &executed); err != nil {
		t.Fatal(err)
	}
	if executed.GlobalMCPRecords != 1 || strings.Contains(executeOutput.String(), "private-cli") || strings.Contains(executeOutput.String(), "executed") {
		t.Fatalf("execute output = %s", executeOutput.String())
	}

	db, err = database.OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.GetMCPServer(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !secretvalue.IsEncrypted(stored.Command) {
		t.Fatal("CLI did not encrypt the stored command")
	}
}

func TestSecretMigrationCLIRequiresExplicitDatabase(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--db") {
		t.Fatalf("missing DB error = %v", err)
	}
}
