package configsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
)

// TestMaterializeOnlySyncSkipsWriteBack pins the workspace-lane invariant: a
// materialize-only sync provisions the lane's .claude layer from the DB and
// performs ZERO database writes — no skill import, no synced-file tracking, no
// memory-doc merge. N lanes must never ping-pong shared state through SQLite.
func TestMaterializeOnlySyncSkipsWriteBack(t *testing.T) {
	dir := t.TempDir()
	db, err := database.New(filepath.Join(dir, "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	laneDir := filepath.Join(dir, "lane")
	if err := os.MkdirAll(laneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	project := &database.Project{Name: "p", Path: laneDir, Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	// One global skill that must materialize into the lane.
	skill := &database.Skill{Name: "greeting", Content: "Say hello.", Enabled: true}
	if err := db.CreateSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}
	// One DB memory doc that must materialize into the lane's CLAUDE.md.
	if _, err := db.UpsertMemoryDoc(ctx, project.ID, "canonical memory", "test", "seed"); err != nil {
		t.Fatal(err)
	}
	// Plant lane-local dirt that a full sync WOULD import: a rogue skill and a
	// newer CLAUDE.md marker.
	rogueDir := filepath.Join(laneDir, ".claude", "skills", "rogue")
	if err := os.MkdirAll(rogueDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rogueDir, "SKILL.md"), []byte("---\nname: rogue\n---\nlane-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(laneDir, "CLAUDE.md"), []byte("LANE-MARKER must not reach the DB"), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsBefore := countRows(t, db, "skills")
	projectSkillsBefore := countRows(t, db, "project_skills")
	trackedBefore := countRows(t, db, "synced_skill_files")

	syncer := NewConfigSyncer(db, nil, "127.0.0.1:0")
	if err := syncer.MaterializeToWorkspace(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Materialization happened: bridge, settings hooks block, skill, CLAUDE.md.
	if _, err := os.Stat(filepath.Join(laneDir, ".claude", "hooks", "bridge.sh")); err != nil {
		t.Fatalf("bridge.sh not materialized: %v", err)
	}
	settings, err := os.ReadFile(filepath.Join(laneDir, ".claude", "settings.json"))
	if err != nil || !containsStr(string(settings), "hooks") {
		t.Fatalf("settings.json hooks block missing: %v %s", err, settings)
	}
	skillFile, err := os.ReadFile(filepath.Join(laneDir, ".claude", "skills", "greeting", "SKILL.md"))
	if err != nil || !containsStr(string(skillFile), "Say hello.") {
		t.Fatalf("skill not materialized: %v", err)
	}
	claudeMD, err := os.ReadFile(filepath.Join(laneDir, "CLAUDE.md"))
	if err != nil || string(claudeMD) != "canonical memory" {
		t.Fatalf("CLAUDE.md = %q, want DB-authoritative content", claudeMD)
	}

	// ZERO write-back: no rogue skill import, no tracking rows, no memory merge.
	if got := countRows(t, db, "skills"); got != skillsBefore {
		t.Fatalf("skills rows %d → %d (write-back leaked)", skillsBefore, got)
	}
	if got := countRows(t, db, "project_skills"); got != projectSkillsBefore {
		t.Fatalf("project_skills rows %d → %d (rogue skill imported)", projectSkillsBefore, got)
	}
	if got := countRows(t, db, "synced_skill_files"); got != trackedBefore {
		t.Fatalf("synced_skill_files rows %d → %d (tracking written)", trackedBefore, got)
	}
	doc, err := db.GetMemoryDoc(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(doc.Content, "LANE-MARKER") {
		t.Fatal("lane CLAUDE.md marker leaked into the memory doc")
	}
}

func countRows(t *testing.T, db *database.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
		t.Fatal(err)
	}
	return n
}

func containsStr(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
