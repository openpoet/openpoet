package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"openpoet/internal/database"
)

// TestBuildSSHConfigDefaultKeys: a project saved with "default_keys" (or no
// auth type) carries no credential and must fall back to ~/.ssh keys, like
// browse and configsync — it used to fail with "no authentication methods".
func TestBuildSSHConfigDefaultKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, authType := range []sql.NullString{
		{String: "default_keys", Valid: true},
		{},
	} {
		r := &RemoteRunner{project: &database.Project{
			Name:        "p",
			SSHUser:     sql.NullString{String: "u", Valid: true},
			SSHAuthType: authType,
		}}
		cfg, err := r.buildSSHConfig()
		if err != nil {
			t.Fatalf("auth type %q: %v", authType.String, err)
		}
		if len(cfg.Auth) == 0 {
			t.Fatalf("auth type %q: no auth methods", authType.String)
		}
	}
}
