package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/automation"
	"openpoet/internal/database"
)

func TestParseScopesRequiresExplicitKnownScopes(t *testing.T) {
	scopes, names, err := parseScopes("tasks:read, tasks:write,tasks:read")
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || len(names) != 2 || scopes[0] != automation.ScopeTasksRead || scopes[1] != automation.ScopeTasksWrite {
		t.Fatalf("unexpected scopes: %#v %#v", scopes, names)
	}
	if _, _, err := parseScopes(""); err == nil {
		t.Fatal("expected empty scopes to fail")
	}
	if _, _, err := parseScopes("root:all"); err == nil {
		t.Fatal("expected unknown scope to fail")
	}
}

func TestRunPrintsTokenOnceAndPersistsOnlyDigest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "openpoet.db")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run([]string{
		"--db", dbPath,
		"--name", "helena-test",
		"--scopes", "tasks:read,tasks:write,events:read",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, stderr.String())
	}
	var output provisionOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output.ClientID == "" || !strings.HasPrefix(output.Token, "opav1_") {
		t.Fatalf("unexpected output: %#v", output)
	}

	db, err := database.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var plaintextMatches int
	if err := db.Get(&plaintextMatches, `SELECT COUNT(*) FROM automation_clients WHERE CAST(token_hash AS TEXT) = ?`, output.Token); err != nil {
		t.Fatal(err)
	}
	if plaintextMatches != 0 {
		t.Fatal("plaintext token was persisted")
	}
	var commands int
	if err := db.Get(&commands, "SELECT COUNT(*) FROM automation_commands"); err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("provisioning unexpectedly created commands: %d", commands)
	}
}

func TestRunCanWriteTokenDirectlyToPrivateFile(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "openpoet.db")
	tokenPath := filepath.Join(root, "secrets", "helena.token")
	if err := os.Mkdir(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run([]string{
		"--db", dbPath,
		"--name", "helena-file",
		"--scopes", "tasks:read,events:read",
		"--token-file", tokenPath,
	}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var output provisionOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Token != "" || output.TokenFile != tokenPath || strings.Contains(stdout.String(), "opav1_") {
		t.Fatalf("token leaked to stdout: %s", stdout.String())
	}
	secret, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(secret), "opav1_") {
		t.Fatalf("unexpected token file content: %q", secret)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o", info.Mode().Perm())
	}
	if err := run([]string{
		"--db", dbPath,
		"--name", "must-not-overwrite",
		"--scopes", "tasks:read",
		"--token-file", tokenPath,
	}, io.Discard, io.Discard); err == nil {
		t.Fatal("expected existing token file to be refused")
	}
}
