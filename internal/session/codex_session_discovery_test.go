package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverCodexProviderSessionIDPrefersOpenPoetForCWD(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	writeCodexMeta(t, codexHome, "2026/06/14/older.jsonl", "native-newer", workDir, "terminal", base.Add(2*time.Minute))
	writeCodexMeta(t, codexHome, "2026/06/14/openpoet.jsonl", "openpoet-id", workDir, "openpoet", base.Add(time.Minute))
	writeCodexMeta(t, codexHome, "2026/06/14/other.jsonl", "other-id", otherDir, "openpoet", base.Add(3*time.Minute))

	got, err := discoverCodexProviderSessionID(codexHome, workDir, base)
	if err != nil {
		t.Fatalf("discoverCodexProviderSessionID returned error: %v", err)
	}
	if got != "openpoet-id" {
		t.Fatalf("discoverCodexProviderSessionID = %q, want openpoet-id", got)
	}
}

func TestDiscoverCodexProviderSessionIDHonorsSince(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	writeCodexMeta(t, codexHome, "2026/06/14/old.jsonl", "old-id", workDir, "openpoet", base.Add(-time.Minute))
	writeCodexMeta(t, codexHome, "2026/06/14/new.jsonl", "new-id", workDir, "openpoet", base.Add(time.Minute))

	got, err := discoverCodexProviderSessionID(codexHome, workDir, base)
	if err != nil {
		t.Fatalf("discoverCodexProviderSessionID returned error: %v", err)
	}
	if got != "new-id" {
		t.Fatalf("discoverCodexProviderSessionID = %q, want new-id", got)
	}
}

func TestCodexProviderSessionIDExists(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	ts := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	writeCodexMeta(t, codexHome, "2026/06/14/session.jsonl", "provider-id", workDir, "openpoet", ts)

	if !codexProviderSessionIDExists(codexHome, "provider-id") {
		t.Fatal("expected provider-id to exist")
	}
	if codexProviderSessionIDExists(codexHome, "wrong-openpoet-session-id") {
		t.Fatal("unexpected invalid provider id match")
	}
}

func writeCodexMeta(t *testing.T, codexHome, relPath, id, cwd, originator string, ts time.Time) {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := `{"timestamp":"` + ts.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"` + id + `","timestamp":"` + ts.Format(time.RFC3339Nano) + `","cwd":"` + filepath.ToSlash(cwd) + `","originator":"` + originator + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}
