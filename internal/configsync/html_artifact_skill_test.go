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

func TestEnsureHTMLArtifactSkillUpdatesOnlyAnUneditedCopy(t *testing.T) {
	ctx := context.Background()
	const oldBuiltIn = "The previous built-in guide.\n\n# HTML artifacts\n"
	htmlArtifactSkillSuperseded[htmlArtifactSkillHash(oldBuiltIn)] = true
	t.Cleanup(func() { delete(htmlArtifactSkillSuperseded, htmlArtifactSkillHash(oldBuiltIn)) })
	// The text shipped as revision 1 is known, so installations that still
	// have it are brought up to date.
	if !htmlArtifactSkillSuperseded["326d24eab92e2d87211c3291790053c376570be2409b32eff246051139045e06"] {
		t.Fatal("the revision 1 text is not listed as superseded")
	}
	if htmlArtifactSkillSuperseded[htmlArtifactSkillHash(htmlArtifactSkillContent)] {
		t.Fatal("the current text is listed as superseded")
	}

	// An installation from before this revision: registered, no revision recorded.
	install := func(name, content string) *database.DB {
		db, err := database.New(filepath.Join(t.TempDir(), name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if content != "" {
			if err := db.CreateSkillWithVersion(ctx, &database.Skill{Name: HTMLArtifactSkillName, Content: content, Enabled: true, Category: "documentation"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.SetSetting(ctx, htmlArtifactSkillSetting, "1"); err != nil {
			t.Fatal(err)
		}
		return db
	}

	// Never edited: replaced by the current guide, keeping the old text as a version.
	db := install("unedited", oldBuiltIn)
	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows := htmlArtifactSkillRows(t, db)
	if len(rows) != 1 || rows[0].Content != htmlArtifactSkillContent || !rows[0].Enabled {
		t.Fatalf("unedited skill after the update = %+v", rows)
	}
	var versions []string
	if err := db.Select(&versions, `SELECT content FROM skill_versions WHERE skill_id = ? ORDER BY version`, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0] != oldBuiltIn || versions[1] != htmlArtifactSkillContent {
		t.Fatalf("skill versions = %d, want the old text and the new one", len(versions))
	}
	if revision, _ := db.GetSetting(ctx, htmlArtifactSkillRevisionSetting); revision != "2" {
		t.Fatalf("recorded revision = %q", revision)
	}
	// The user edits the updated guide: the next start leaves it alone.
	rows[0].Content = "edited after the update"
	if err := db.UpdateSkill(ctx, &rows[0]); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	if rows = htmlArtifactSkillRows(t, db); rows[0].Content != "edited after the update" {
		t.Fatalf("edited skill was replaced: %+v", rows)
	}

	// Edited before the update: left as it is, and not offered again.
	db = install("edited", "my own guide")
	for i := 0; i < 2; i++ {
		if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if rows = htmlArtifactSkillRows(t, db); len(rows) != 1 || rows[0].Content != "my own guide" {
		t.Fatalf("edited skill = %+v", rows)
	}
	if revision, _ := db.GetSetting(ctx, htmlArtifactSkillRevisionSetting); revision != "2" {
		t.Fatalf("recorded revision for an edited skill = %q", revision)
	}

	// Deleted before the update: not created again.
	db = install("deleted", "")
	if err := EnsureHTMLArtifactSkill(ctx, db); err != nil {
		t.Fatal(err)
	}
	if rows = htmlArtifactSkillRows(t, db); len(rows) != 0 {
		t.Fatalf("deleted skill came back: %+v", rows)
	}
}

// The guide's question examples have to be what the server accepts and what
// the bridge looks for.
func TestHTMLArtifactSkillTeachesQuestions(t *testing.T) {
	content := htmlArtifactSkillContent
	for _, phrase := range []string{
		`id="openpoet-questions"`, `type="application/json"`, "data-openpoet-question", "data-openpoet-option", "data-openpoet-answer",
		"data-openpoet-selected", "data-openpoet-answered", "window.openpoet.answer", "openpoet:answers",
		"same across versions", "never inside", "every tab, step and accordion in the DOM", "openpoet_get_link", "sandbox allow-scripts", "connect-src 'none'",
	} {
		if !strings.Contains(content, phrase) {
			t.Fatalf("skill content lacks %q", phrase)
		}
	}
	block := regexp.MustCompile(`(?s)<script type="application/json" id="openpoet-questions">.*?</script>`).FindString(content)
	if block == "" {
		t.Fatal("skill content has no questions block")
	}
	questions, err := application.ParseArtifactQuestions("<html><body>" + block + "</body></html>")
	if err != nil || len(questions) != 3 {
		t.Fatalf("the skill's questions block = %+v err=%v", questions, err)
	}
	types := questions[0].Type + " " + questions[1].Type + " " + questions[2].Type
	if types != "single multi text" {
		t.Fatalf("question types in the example = %q", types)
	}
	// The elements of the example name questions the block declares.
	for _, id := range regexp.MustCompile(`data-openpoet-question="([^"]+)"`).FindAllStringSubmatch(content, -1) {
		found := false
		for _, question := range questions {
			found = found || question.ID == id[1]
		}
		if !found {
			t.Fatalf("the example marks a question %q the block does not declare", id[1])
		}
	}
}
