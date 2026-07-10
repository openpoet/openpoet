package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
)

func TestRolloutSecretMigrationRunsOnDisposableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-secret.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &database.MCPServer{Name: "rollout", Command: "node rollout-private", Args: `[]`, Env: `{}`, Enabled: true}
	if err := db.CreateMCPServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := migrateRuntimeSecrets(context.Background(), path, "rollout-secret-test-key")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Executed || report.GlobalMCPRecords != 1 || report.Fields != 3 {
		t.Fatalf("migration report = %+v", report)
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
		t.Fatal("rollout migration left plaintext")
	}
	encryptor, _ := security.NewEncryptor("rollout-secret-test-key")
	resolved, err := secretvalue.Resolve(stored.Command, encryptor.Decrypt)
	if err != nil || resolved != server.Command {
		t.Fatalf("resolved command = %q, err=%v", resolved, err)
	}
}

func TestSecretMigrationPlanIsPostBackupAndPreRestart(t *testing.T) {
	plan, err := BuildPlan(OperationApply)
	if err != nil {
		t.Fatal(err)
	}
	secret := stepIndex(plan, "secret-migration")
	if secret <= stepIndex(plan, "backup") || secret >= stepIndex(plan, "start") {
		t.Fatalf("secret migration order is unsafe: %+v", plan)
	}
}

func TestRestoreSQLiteBackupReplacesOfflineMigratedDatabase(t *testing.T) {
	directory := t.TempDir()
	livePath := filepath.Join(directory, "live.db")
	backupPath := filepath.Join(directory, "backup.db")
	live := createTestDatabase(t, livePath)
	if _, err := live.Exec(`INSERT INTO entries(value) VALUES ('before-migration')`); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := BackupSQLite(livePath, backupPath); err != nil {
		t.Fatal(err)
	}

	live, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Exec(`UPDATE entries SET value='after-migration'`); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSQLiteBackup(backupPath, livePath); err != nil {
		t.Fatal(err)
	}

	restored, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var value string
	if err := restored.QueryRow(`SELECT value FROM entries`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "before-migration" {
		t.Fatalf("restored value = %q", value)
	}
}
