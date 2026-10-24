package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
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

func TestRotateKeyCLIGeneratesNewKeyFileAndReencrypts(t *testing.T) {
	t.Setenv("OPENPOET_OLD_ENCRYPT_KEY", "")
	t.Setenv("OPENPOET_NEW_ENCRYPT_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	oldKeyFile := filepath.Join(dir, security.KeyFileName)
	if err := security.WriteNewKeyFile(oldKeyFile, "cli-old-key"); err != nil {
		t.Fatal(err)
	}
	oldEncryptor, _ := security.NewEncryptor("cli-old-key")
	command, _ := secretvalue.Encrypt(oldEncryptor, "node private-cli")
	server := &database.MCPServer{Name: "cli", Command: command, Args: `[]`, Env: `{}`, Enabled: true}
	if err := db.CreateMCPServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	db.Close()

	newKeyFile := filepath.Join(dir, "openpoet.key.new")
	var dry bytes.Buffer
	if err := run([]string{"rotate-key", "--db", path, "--dry-run", "--old-key-file", oldKeyFile, "--generate-new-key-file", newKeyFile}, &dry); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), `"envelopes":1`) || strings.Contains(dry.String(), `"executed":true`) {
		t.Fatalf("dry-run output = %s", dry.String())
	}
	if _, err := os.Stat(newKeyFile); !os.IsNotExist(err) {
		t.Fatal("dry run created the new key file")
	}

	if err := run([]string{"rotate-key", "--db", path, "--old-key-file", oldKeyFile, "--new-key-file", oldKeyFile}, &bytes.Buffer{}); err == nil {
		t.Fatal("identical old and new keys accepted")
	}

	var out bytes.Buffer
	if err := run([]string{"rotate-key", "--db", path, "--old-key-file", oldKeyFile, "--generate-new-key-file", newKeyFile}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-cli") || !strings.Contains(out.String(), `"total":1`) {
		t.Fatalf("rotate output = %s", out.String())
	}
	info, err := os.Stat(newKeyFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("new key file: %v", err)
	}
	newKey, err := security.ReadKeyFile(newKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	newEncryptor, _ := security.NewEncryptor(newKey)

	db, err = database.OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.GetMCPServer(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext, err := secretvalue.Resolve(stored.Command, newEncryptor.Decrypt); err != nil || plaintext != "node private-cli" {
		t.Fatalf("command not re-encrypted with the new key: %v", err)
	}
	if _, err := secretvalue.Resolve(stored.Command, oldEncryptor.Decrypt); err == nil {
		t.Fatal("old key still decrypts the command")
	}
}

func TestRotateKeyCLIRequiresOldKey(t *testing.T) {
	t.Setenv("OPENPOET_OLD_ENCRYPT_KEY", "")
	path := filepath.Join(t.TempDir(), "rotate.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := run([]string{"rotate-key", "--db", path, "--dry-run"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "old key") {
		t.Fatalf("missing old key err = %v", err)
	}
}

func TestSecretMigrationCLIRequiresExplicitDatabase(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--db") {
		t.Fatalf("missing DB error = %v", err)
	}
}
