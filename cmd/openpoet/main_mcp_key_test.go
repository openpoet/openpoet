package main

import (
	"context"
	"path/filepath"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/security"
)

func TestLoadMCPAPIKeyEncryptedLegacyAndFailClosed(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "mcp-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	encryptor, err := security.NewEncryptor("mcp-key-test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	secret := "dm_newly-generated-secret"
	ciphertext, iv, err := encryptor.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "mcp_api_key", ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "mcp_api_key_iv", iv); err != nil {
		t.Fatal(err)
	}
	if got := loadMCPAPIKey(ctx, db, encryptor); got != secret {
		t.Fatalf("encrypted key = %q, want generated secret", got)
	}

	if err := db.SetSetting(ctx, "mcp_api_key", "dm_legacy-plaintext"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "mcp_api_key_iv", ""); err != nil {
		t.Fatal(err)
	}
	if got := loadMCPAPIKey(ctx, db, encryptor); got != "dm_legacy-plaintext" {
		t.Fatalf("legacy key = %q", got)
	}

	if err := db.SetSetting(ctx, "mcp_api_key", ciphertext); err != nil {
		t.Fatal(err)
	}
	if got := loadMCPAPIKey(ctx, db, encryptor); got != "" {
		t.Fatalf("ciphertext without IV must fail closed, got %q", got)
	}
	if err := db.SetSetting(ctx, "mcp_api_key_iv", "corrupt"); err != nil {
		t.Fatal(err)
	}
	if got := loadMCPAPIKey(ctx, db, encryptor); got != "" {
		t.Fatalf("corrupt encrypted key must fail closed, got %q", got)
	}
}
