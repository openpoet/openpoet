package configsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
)

func TestConfigSyncResolvesEncryptedMCPForOpenCodeAndCodex(t *testing.T) {
	cs, project := setupConfigSyncTest(t)
	encryptor, err := security.NewEncryptor("configsync-mcp-runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	cs.decryptFunc = encryptor.Decrypt
	command, _ := secretvalue.Encrypt(encryptor, "node")
	args, _ := secretvalue.Encrypt(encryptor, `["server.js"]`)
	env, _ := secretvalue.Encrypt(encryptor, `{"TOKEN":"config-secret"}`)
	if err := cs.db.CreateMCPServer(context.Background(), &database.MCPServer{
		Name: "encrypted", Command: command, Args: args, Env: env, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	openCode, err := cs.buildOpenCodeMCPConfig(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	entry := openCode["encrypted"].(map[string]interface{})
	parts := entry["command"].([]string)
	if len(parts) != 2 || parts[0] != "node" || parts[1] != "server.js" {
		t.Fatalf("OpenCode command = %#v", parts)
	}
	if entry["environment"].(map[string]string)["TOKEN"] != "config-secret" {
		t.Fatalf("OpenCode environment = %#v", entry["environment"])
	}

	toml, err := cs.buildCodexConfigTOML(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toml, `command = "node"`) || !strings.Contains(toml, `TOKEN = "config-secret"`) {
		t.Fatalf("Codex config did not contain resolved values:\n%s", toml)
	}
	if strings.Contains(toml, "ciphertext") || strings.Contains(toml, command) {
		t.Fatal("Codex config retained the storage envelope")
	}
}

func TestConfigSyncEncryptsNewMCPImportsWhenCompositionProvidesEncryptor(t *testing.T) {
	cs, project := setupConfigSyncTest(t)
	encryptor, err := security.NewEncryptor("configsync-mcp-import-test")
	if err != nil {
		t.Fatal(err)
	}
	cs.SetSecretEncryptor(encryptor)
	content := `{"mcpServers":{"imported":{"command":"node","args":["server.js"],"env":{"TOKEN":"import-secret"}}}}`
	if err := os.WriteFile(filepath.Join(project.Path, ".mcp.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cs.importMCPsFromDisk(context.Background(), project.Path, project); err != nil {
		t.Fatal(err)
	}
	stored, err := cs.db.GetProjectMCPServerByName(context.Background(), project.ID, "imported")
	if err != nil {
		t.Fatal(err)
	}
	for label, value := range map[string]string{"command": stored.Command, "args": stored.Args, "env": stored.Env} {
		if !secretvalue.IsEncrypted(value) {
			t.Fatalf("%s import was stored as plaintext", label)
		}
	}
	resolved, err := secretvalue.Resolve(stored.Env, encryptor.Decrypt)
	if err != nil || !strings.Contains(resolved, "import-secret") {
		t.Fatalf("resolved env = %q, err=%v", resolved, err)
	}
}
