package security

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEncryptedData() (string, string, bool, error) { return "", "", false, nil }

func encryptedWith(t *testing.T, key string) func() (string, string, bool, error) {
	t.Helper()
	encryptor, err := NewEncryptor(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, iv, err := encryptor.Encrypt("stored-credential")
	if err != nil {
		t.Fatal(err)
	}
	return func() (string, string, bool, error) { return ciphertext, iv, true, nil }
}

func mustNotProbe(t *testing.T) func() (string, string, bool, error) {
	return func() (string, string, bool, error) {
		t.Fatal("database probed although a key was available")
		return "", "", false, nil
	}
}

func TestResolveKeyEnvWins(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	if err := os.WriteFile(keyFile, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveKey(KeyOptions{EnvKey: "env-key", KeyFile: keyFile, FindEncrypted: mustNotProbe(t)})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Key != "env-key" || resolved.Source != KeySourceEnv {
		t.Fatalf("resolved = %s, want env", resolved.Source)
	}
}

func TestResolveKeyEnvDoesNotCreateKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	if _, err := ResolveKey(KeyOptions{EnvKey: "env-key", KeyFile: keyFile, FindEncrypted: mustNotProbe(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key file created with OPENPOET_ENCRYPT_KEY set: %v", err)
	}
}

func TestResolveKeyUsesKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	if err := os.WriteFile(keyFile, []byte("  file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveKey(KeyOptions{KeyFile: keyFile, FindEncrypted: mustNotProbe(t)})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Key != "file-key" || resolved.Source != KeySourceFile || !resolved.Persisted {
		t.Fatalf("resolved source = %s persisted=%v", resolved.Source, resolved.Persisted)
	}
}

func TestResolveKeyRejectsEmptyKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	if err := os.WriteFile(keyFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveKey(KeyOptions{KeyFile: keyFile, FindEncrypted: noEncryptedData}); err == nil {
		t.Fatal("empty key file accepted; it must never be silently replaced")
	}
	if data, _ := os.ReadFile(keyFile); string(data) != "\n" {
		t.Fatal("empty key file was rewritten")
	}
}

func TestResolveKeyFreshInstallGeneratesRandomKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	resolved, err := ResolveKey(KeyOptions{KeyFile: keyFile, FindEncrypted: noEncryptedData})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Source != KeySourceGenerated || !resolved.Persisted {
		t.Fatalf("source = %s persisted=%v", resolved.Source, resolved.Persisted)
	}
	raw, err := base64.StdEncoding.DecodeString(resolved.Key)
	if err != nil || len(raw) != 32 {
		t.Fatalf("generated key is not 32 random bytes (len %d, err %v)", len(raw), err)
	}
	if resolved.Key == LegacyDerivedKey() {
		t.Fatal("fresh install used the legacy key")
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %#o, want 0600", perm)
	}
	stored, err := ReadKeyFile(keyFile)
	if err != nil || stored != resolved.Key {
		t.Fatalf("key file does not hold the generated key (err %v)", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(keyFile))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %d entries", len(entries))
	}

	again, err := ResolveKey(KeyOptions{KeyFile: keyFile, FindEncrypted: mustNotProbe(t)})
	if err != nil || again.Key != resolved.Key || again.Source != KeySourceFile {
		t.Fatalf("second start did not reuse the key file: source=%s err=%v", again.Source, err)
	}

	other, err := ResolveKey(KeyOptions{KeyFile: filepath.Join(t.TempDir(), KeyFileName), FindEncrypted: noEncryptedData})
	if err != nil || other.Key == resolved.Key {
		t.Fatalf("two fresh installs got the same key (err %v)", err)
	}
}

func TestResolveKeyLegacyFallbackWhenEncryptedDataExists(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	var logs []string
	resolved, err := ResolveKey(KeyOptions{
		KeyFile:       keyFile,
		FindEncrypted: encryptedWith(t, LegacyDerivedKey()),
		Logf:          func(format string, args ...any) { logs = append(logs, format) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Key != LegacyDerivedKey() || resolved.Source != KeySourceLegacy || !resolved.Persisted {
		t.Fatalf("source = %s persisted=%v", resolved.Source, resolved.Persisted)
	}
	if stored, err := ReadKeyFile(keyFile); err != nil || stored != LegacyDerivedKey() {
		t.Fatalf("legacy key not persisted (err %v)", err)
	}
	if info, _ := os.Stat(keyFile); info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %#o", info.Mode().Perm())
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "PREDICTABLE") {
		t.Fatal("legacy fallback did not log the predictable-key warning")
	}

	// The persisted legacy key keeps decrypting the existing data.
	ciphertext, iv, _, _ := encryptedWith(t, LegacyDerivedKey())()
	encryptor, _, err := LoadEncryptor(KeyOptions{KeyFile: keyFile, FindEncrypted: mustNotProbe(t)})
	if err != nil {
		t.Fatal(err)
	}
	if plaintext, err := encryptor.Decrypt(ciphertext, iv); err != nil || plaintext != "stored-credential" {
		t.Fatalf("legacy data no longer decrypts: %v", err)
	}
}

func TestResolveKeyLegacyMismatchIsNotPersisted(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	resolved, err := ResolveKey(KeyOptions{KeyFile: keyFile, FindEncrypted: encryptedWith(t, "some-other-key")})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Source != KeySourceLegacy || resolved.Persisted {
		t.Fatalf("source = %s persisted=%v", resolved.Source, resolved.Persisted)
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a legacy key that does not decrypt the data was persisted")
	}
}

func TestResolveKeyProbeErrorNeverGeneratesKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	_, err := ResolveKey(KeyOptions{KeyFile: keyFile, FindEncrypted: func() (string, string, bool, error) {
		return "", "", false, errors.New("database locked")
	}})
	if err == nil {
		t.Fatal("probe error ignored")
	}
	if _, statErr := os.Stat(keyFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("key generated although existing data could not be ruled out")
	}
}

func TestWriteNewKeyFileNeverOverwrites(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	if err := WriteNewKeyFile(keyFile, "first"); err != nil {
		t.Fatal(err)
	}
	if err := WriteNewKeyFile(keyFile, "second"); err == nil {
		t.Fatal("existing key file overwritten")
	}
	if stored, _ := ReadKeyFile(keyFile); stored != "first" {
		t.Fatal("key file content changed")
	}
}

func TestNewEncryptorRejectsEmptyKey(t *testing.T) {
	t.Setenv("OPENPOET_ENCRYPT_KEY", "")
	if _, err := NewEncryptor(""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("NewEncryptor(\"\") err = %v, want ErrInvalidKey (no implicit default key)", err)
	}
}
