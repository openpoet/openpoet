package configsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsInstructionBlockPerHarness: the steering block names the tool, and
// the managed upsert is idempotent, preserves user content, and replaces
// (never duplicates) the marker-delimited section on re-sync.
func TestDocsInstructionBlockPerHarness(t *testing.T) {
	block := OpenPoetDocsInstructionBlock()
	for _, token := range []string{"openpoet_create_document", docsSteeringBegin, docsSteeringEnd} {
		if !strings.Contains(block, token) {
			t.Fatalf("steering block missing %q", token)
		}
	}
	// Plans and documentation are steered to HTML artifacts first; Docs stay
	// for short plain text and as the fallback.
	for _, token := range []string{
		"openpoet_create_artifact", "openpoet_update_artifact", "openpoet_list_artifact_annotations", "openpoet_resolve_artifact_annotation",
		"self-contained", "no external resources", "navigation", "charts", "html-artifact", "html_file",
		"html_path", "openpoet_patch_artifact", "output_path",
	} {
		if !strings.Contains(block, token) {
			t.Fatalf("steering block missing %q:\n%s", token, block)
		}
	}
	if strings.Index(block, "openpoet_create_artifact") > strings.Index(block, "openpoet_create_document") {
		t.Fatal("the steering block must name the artifact tool before the Docs tool")
	}
	if strings.Count(block, docsSteeringBegin) != 1 || strings.Count(block, docsSteeringEnd) != 1 || !strings.HasSuffix(block, docsSteeringEnd+"\n") {
		t.Fatal("steering block markers are malformed")
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

// A project synced by an earlier release has the old Docs-only wording between
// the markers. Syncing replaces it in place, keeping the user's own text.
func TestDocsSteeringUpgradeReplacesTheOldWording(t *testing.T) {
	old := docsSteeringBegin + `
## Documentation goes to OpenPoet Docs

When you produce documentation, reports, plans, or any long-form deliverable,
create it as an OpenPoet Doc with the ` + "`openpoet_create_document`" + ` tool
instead of harness-native artifacts or loose files.
` + docsSteeringEnd + "\n"
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, []byte("# Team rules\nrun the tests\n\n"+old+"\n## After\nkeep this too\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := upsertDocsSteeringFile(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	content := string(data)
	if strings.Contains(content, "Documentation goes to OpenPoet Docs") {
		t.Fatalf("the old wording survived the sync:\n%s", content)
	}
	if !strings.Contains(content, "openpoet_create_artifact") || strings.Count(content, docsSteeringBegin) != 1 || strings.Count(content, docsSteeringEnd) != 1 {
		t.Fatalf("the new block was not installed exactly once:\n%s", content)
	}
	if !strings.HasPrefix(content, "# Team rules\nrun the tests\n") || !strings.Contains(content, "## After\nkeep this too\n") {
		t.Fatalf("user content around the block was lost:\n%s", content)
	}
	if err := upsertDocsSteeringFile(path); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != content {
		t.Fatal("a second sync changed the file again")
	}
}
