package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
)

type rotationFixture struct {
	project    *Project
	ai         *AIConfig
	global     *MCPServer
	projectMCP *ProjectMCPServer
	tool       *ProjectTool
}

func newTestEncryptor(t *testing.T, key string) *security.Encryptor {
	t.Helper()
	encryptor, err := security.NewEncryptor(key)
	if err != nil {
		t.Fatal(err)
	}
	return encryptor
}

// seedEncryptedValues stores one encrypted value in every storage shape:
// 3 column pairs, 1 encrypted setting, 5 envelopes (9 total).
func seedEncryptedValues(t *testing.T, db *DB, encryptor *security.Encryptor) rotationFixture {
	t.Helper()
	ctx := context.Background()
	seal := func(plaintext string) (string, string) {
		ciphertext, iv, err := encryptor.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		return ciphertext, iv
	}
	envelope := func(plaintext string) string {
		value, err := secretvalue.Encrypt(encryptor, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	sshCipher, sshIV := seal("ssh-password")
	project := &Project{
		Name: "remote", Path: "/home/dev/remote", Type: "remote", Backend: "claude_code", BackendConfig: "{}",
		SSHCredentialEncrypted: sql.NullString{String: sshCipher, Valid: true},
		SSHCredentialIV:        sql.NullString{String: sshIV, Valid: true},
	}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	aiCipher, aiIV := seal("ai-api-key")
	ai := &AIConfig{Name: "provider", ProviderType: "anthropic", APIKeyEncrypted: aiCipher, APIKeyIV: aiIV, Model: "m", ExtraJSON: "{}"}
	if err := db.CreateAIConfig(ctx, ai); err != nil {
		t.Fatal(err)
	}
	devCipher, devIV := seal("tunnel-device-key")
	if err := db.CreatePairedDevice(ctx, &PairedDevice{ID: "device-1", DeviceName: "phone", EncryptionKey: devCipher, EncryptionKeyIV: devIV}); err != nil {
		t.Fatal(err)
	}
	settingCipher, settingIV := seal("setting-secret")
	for key, value := range map[string]string{
		"anthropic_api_key": settingCipher, "anthropic_api_key_iv": settingIV, "anthropic_api_key_preview": "sk-...",
		"legacy_plain_key": "plaintext-not-encrypted",
	} {
		if err := db.SetSetting(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	global := &MCPServer{
		Name: "global", Command: envelope("node secret.js"), Args: `["plain"]`, Env: envelope(`{"TOKEN":"t"}`),
		CommandWindows: envelope("node.exe secret.js"), Enabled: true,
	}
	if err := db.CreateMCPServer(ctx, global); err != nil {
		t.Fatal(err)
	}
	projectMCP := &ProjectMCPServer{ProjectID: project.ID, Name: "project", Command: envelope("bun x mcp"), Args: `[]`, Env: `{}`, Enabled: true}
	if err := db.CreateProjectMCPServer(ctx, projectMCP); err != nil {
		t.Fatal(err)
	}
	tool := &ProjectTool{ProjectID: project.ID, Name: "deploy", Command: envelope("deploy --token private"), Parameters: `{}`, Enabled: true}
	if err := db.CreateProjectTool(ctx, tool); err != nil {
		t.Fatal(err)
	}
	return rotationFixture{project: project, ai: ai, global: global, projectMCP: projectMCP, tool: tool}
}

// assertAllDecryptWith checks every seeded value decrypts with encryptor.
func assertAllDecryptWith(t *testing.T, db *DB, encryptor *security.Encryptor, fixture rotationFixture) {
	t.Helper()
	ctx := context.Background()
	open := func(label, ciphertext, iv, want string) {
		t.Helper()
		got, err := encryptor.Decrypt(ciphertext, iv)
		if err != nil || got != want {
			t.Fatalf("%s: decrypt = %q, %v", label, got, err)
		}
	}
	resolve := func(label, value, want string) {
		t.Helper()
		got, err := secretvalue.Resolve(value, encryptor.Decrypt)
		if err != nil || got != want {
			t.Fatalf("%s: resolve = %q, %v", label, got, err)
		}
	}
	project, _ := db.GetProject(ctx, fixture.project.ID)
	open("project ssh", project.SSHCredentialEncrypted.String, project.SSHCredentialIV.String, "ssh-password")
	ai, _ := db.GetAIConfig(ctx, fixture.ai.ID)
	open("ai config", ai.APIKeyEncrypted, ai.APIKeyIV, "ai-api-key")
	device, _ := db.GetPairedDevice(ctx, "device-1")
	open("paired device", device.EncryptionKey, device.EncryptionKeyIV, "tunnel-device-key")
	settingCipher, _ := db.GetSetting(ctx, "anthropic_api_key")
	settingIV, _ := db.GetSetting(ctx, "anthropic_api_key_iv")
	open("setting", settingCipher, settingIV, "setting-secret")
	if plain, _ := db.GetSetting(ctx, "legacy_plain_key"); plain != "plaintext-not-encrypted" {
		t.Fatalf("plaintext setting changed: %q", plain)
	}
	global, _ := db.GetMCPServer(ctx, fixture.global.ID)
	resolve("global command", global.Command, "node secret.js")
	resolve("global env", global.Env, `{"TOKEN":"t"}`)
	resolve("global command_windows", global.CommandWindows, "node.exe secret.js")
	if global.Args != `["plain"]` {
		t.Fatalf("plaintext args changed: %q", global.Args)
	}
	projectMCP, _ := db.GetProjectMCPServer(ctx, fixture.projectMCP.ID)
	resolve("project MCP command", projectMCP.Command, "bun x mcp")
	tool, _ := db.GetProjectTool(ctx, fixture.tool.ID)
	resolve("custom tool command", tool.Command, "deploy --token private")
}

func TestFindEncryptedValue(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	if _, _, found, err := db.FindEncryptedValue(ctx); err != nil || found {
		t.Fatalf("fresh database: found=%v err=%v", found, err)
	}
	// Legacy plaintext settings are not encrypted data.
	if err := db.SetSetting(ctx, "openai_api_key", "plaintext"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := db.FindEncryptedValue(ctx); found {
		t.Fatal("plaintext setting reported as encrypted data")
	}

	encryptor := newTestEncryptor(t, "find-encrypted-test")
	envelope, _ := secretvalue.Encrypt(encryptor, "node server.js")
	if err := db.CreateMCPServer(ctx, &MCPServer{Name: "only-envelope", Command: envelope, Args: `[]`, Env: `{}`, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ciphertext, iv, found, err := db.FindEncryptedValue(ctx)
	if err != nil || !found {
		t.Fatalf("envelope not found: %v", err)
	}
	if plaintext, err := encryptor.Decrypt(ciphertext, iv); err != nil || plaintext != "node server.js" {
		t.Fatalf("sample does not decrypt: %v", err)
	}
}

func TestRotateEncryptionKeyDryRunExecuteAndRefusal(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	oldKey := newTestEncryptor(t, "rotation-old-key")
	newKey := newTestEncryptor(t, "rotation-new-key")
	fixture := seedEncryptedValues(t, db, oldKey)

	// Wrong old key: refuse, change nothing.
	wrong := newTestEncryptor(t, "not-the-old-key")
	if _, err := RotateEncryptionKey(ctx, db, wrong.Decrypt, newKey, true); err == nil ||
		!strings.Contains(err.Error(), "9 encrypted value(s)") || strings.Contains(err.Error(), "ssh-password") {
		t.Fatalf("wrong old key err = %v", err)
	}
	assertAllDecryptWith(t, db, oldKey, fixture)

	// Dry run: counts only, nothing changes, new key optional.
	dry, err := RotateEncryptionKey(ctx, db, oldKey.Decrypt, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := KeyRotationReport{ColumnValues: 3, SettingValues: 1, Envelopes: 5, Total: 9}
	if dry != want {
		t.Fatalf("dry-run report = %+v, want %+v", dry, want)
	}
	assertAllDecryptWith(t, db, oldKey, fixture)

	report, err := RotateEncryptionKey(ctx, db, oldKey.Decrypt, newKey, true)
	if err != nil {
		t.Fatal(err)
	}
	want.Executed = true
	if report != want {
		t.Fatalf("execute report = %+v, want %+v", report, want)
	}
	assertAllDecryptWith(t, db, newKey, fixture)

	// The old key no longer opens anything; rotating again with it refuses.
	if _, err := RotateEncryptionKey(ctx, db, oldKey.Decrypt, newKey, false); err == nil {
		t.Fatal("old key still decrypts after rotation")
	}
}

func TestRotateEncryptionKeyRefusesPartialFailure(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	oldKey := newTestEncryptor(t, "partial-old-key")
	newKey := newTestEncryptor(t, "partial-new-key")
	fixture := seedEncryptedValues(t, db, oldKey)

	// One value sealed with a different key: the whole rotation must refuse.
	stray := newTestEncryptor(t, "stray-key")
	ciphertext, iv, _ := stray.Encrypt("stray")
	if err := db.SetSetting(ctx, "stray_secret", ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "stray_secret_iv", iv); err != nil {
		t.Fatal(err)
	}
	_, err := RotateEncryptionKey(ctx, db, oldKey.Decrypt, newKey, true)
	if err == nil || !strings.Contains(err.Error(), "settings[stray_secret]") {
		t.Fatalf("partial failure err = %v", err)
	}
	assertAllDecryptWith(t, db, oldKey, fixture)
}

// TestEncryptedColumnInventoryCoversSchema guards the rotation inventory: a
// migration that adds an *_iv column must register the pair in
// encryptedPairColumns, or rotation would leave its values on the old key.
func TestEncryptedColumnInventoryCoversSchema(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tables, err := listTables(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, pair := range encryptedPairColumns {
		listed[pair.table+"."+pair.iv] = true
	}
	for _, column := range envelopeColumns {
		columns, err := tableColumns(ctx, db.DB, column.table)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(","+strings.Join(columns, ",")+",", ","+column.column+",") {
			t.Fatalf("envelope column %s.%s does not exist", column.table, column.column)
		}
	}
	for _, table := range tables {
		columns, err := tableColumns(ctx, db.DB, table)
		if err != nil {
			t.Fatal(err)
		}
		for _, column := range columns {
			if strings.HasSuffix(column, "_iv") && !listed[table+"."+column] {
				t.Errorf("%s.%s looks like an encryption IV but is not in encryptedPairColumns", table, column)
			}
		}
	}
}
