package configsync

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func htmlArtifactSkillRows(t *testing.T, db *database.DB) []database.Skill {
	t.Helper()
	skills, err := db.ListSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rows []database.Skill
	for _, skill := range skills {
		if skill.Name == HTMLArtifactSkillName {
			rows = append(rows, skill)
		}
	}
	return rows
}

func TestEnsureHTMLArtifactSkillRegistersOnceAndNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "skill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows := htmlArtifactSkillRows(t, db)
	if len(rows) != 1 || !rows[0].Enabled || rows[0].Content != htmlArtifactSkillContent || rows[0].Category != "documentation" {
		t.Fatalf("registered skill = %+v", rows)
	}

	// The user edits it: a restart must not put the original back.
	rows[0].Content = "my own guide"
	if err := db.UpdateSkill(ctx, &rows[0]); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	if rows = htmlArtifactSkillRows(t, db); len(rows) != 1 || rows[0].Content != "my own guide" {
		t.Fatalf("edited skill after restart = %+v", rows)
	}

	// The user deletes it: a restart must not bring it back.
	if err := db.DeleteSkill(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	if rows = htmlArtifactSkillRows(t, db); len(rows) != 0 {
		t.Fatalf("deleted skill was registered again: %+v", rows)
	}
}

func TestEnsureHTMLArtifactSkillKeepsAUserSkillWithTheSameName(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "skill-existing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSkillWithVersion(ctx, &database.Skill{Name: HTMLArtifactSkillName, Content: "written by the user", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	if rows := htmlArtifactSkillRows(t, db); len(rows) != 1 || rows[0].Content != "written by the user" || rows[0].Enabled {
		t.Fatalf("user skill was touched: %+v", rows)
	}
}

// The guide has to teach what the viewer enforces, and its own example has to
// be a page the artifact service accepts unchanged.
func TestHTMLArtifactSkillMatchesWhatTheViewerEnforces(t *testing.T) {
	content := htmlArtifactSkillContent
	for _, phrase := range []string{
		"openpoet_create_artifact", "openpoet_update_artifact", "openpoet_list_artifact_annotations", "openpoet_resolve_artifact_annotation",
		"openpoet_read_artifact", "base_version", "self-contained", "2 MiB", "prefers-color-scheme", "prefers-reduced-motion",
		"localStorage", "[REDACTED]", "stable", "html_file",
	} {
		if !strings.Contains(content, phrase) {
			t.Fatalf("skill content lacks %q", phrase)
		}
	}
	// The first line becomes the skill description agents match on.
	first := strings.SplitN(content, "\n", 2)[0]
	if len(first) > 120 || !strings.Contains(first, "HTML artifacts") || strings.HasPrefix(first, "#") {
		t.Fatalf("first line is not a usable description: %q", first)
	}

	example := regexp.MustCompile("(?s)```html\n(.*?)\n```").FindStringSubmatch(content)
	if example == nil {
		t.Fatal("skill content has no HTML starting point")
	}
	redacted, err := application.CheckArtifactHTML(example[1])
	if err != nil || redacted {
		t.Fatalf("the skill's own example would not publish cleanly: redacted=%v err=%v", redacted, err)
	}
	// Nothing the sandbox would block: no external scripts, styles, fonts or images.
	if external := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//|<link\b|@import|url\(\s*["']?https?:`).FindString(example[1]); external != "" {
		t.Fatalf("the skill's example loads an external resource: %q", external)
	}
	for _, required := range []string{"<!doctype html>", `name="viewport"`, "prefers-color-scheme: dark", "prefers-reduced-motion", "background:var(--bg)"} {
		if !strings.Contains(example[1], required) {
			t.Fatalf("the skill's example lacks %q", required)
		}
	}
}
