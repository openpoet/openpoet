package configsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsInstructionBlockPerHarness: the steering block names the tool and the
// mission link, and the managed upsert is idempotent, preserves user content,
// and replaces (never duplicates) the marker-delimited section on re-sync.
func TestDocsInstructionBlockPerHarness(t *testing.T) {
	block := OpenPoetDocsInstructionBlock()
	for _, token := range []string{"openpoet_create_document", "mission_id", docsSteeringBegin, docsSteeringEnd} {
		if !strings.Contains(block, token) {
			t.Fatalf("steering block missing %q", token)
		}
	}

	dir := t.TempDir()

	// Fresh file (claude .claude/CLAUDE.md, codex/opencode AGENTS.md paths).
	fresh := filepath.Join(dir, "CLAUDE.md")
	if err := upsertDocsSteeringFile(fresh); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(fresh)
	if !strings.Contains(string(first), "openpoet_create_document") {
		t.Fatal("fresh managed file missing the block")
	}

	// Idempotent: a second sync must not duplicate the section.
	if err := upsertDocsSteeringFile(fresh); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(fresh)
	if string(second) != string(first) {
		t.Fatal("re-sync changed an already-correct managed file")
	}
	if strings.Count(string(second), docsSteeringBegin) != 1 {
		t.Fatal("re-sync duplicated the managed section")
	}

	// User content survives; only the managed section is replaced.
	userFile := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(userFile, []byte("# My rules\nkeep me\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := upsertDocsSteeringFile(userFile); err != nil {
		t.Fatal(err)
	}
	merged, _ := os.ReadFile(userFile)
	if !strings.Contains(string(merged), "keep me") || !strings.Contains(string(merged), "openpoet_create_document") {
		t.Fatalf("managed append lost content: %s", merged)
	}
	if err := upsertDocsSteeringFile(userFile); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(userFile)
	if strings.Count(string(again), docsSteeringBegin) != 1 {
		t.Fatal("managed section duplicated in user file")
	}
}
