package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSetProjectSkillConfigAtomicCommitsAndRollsBackAsOneAggregate(t *testing.T) {
	ctx := context.Background()
	db, err := New(filepath.Join(t.TempDir(), "skill-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	project := &Project{Name: "project", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	skill := &Skill{Name: "skill", Content: "content", Enabled: true}
	if err := db.CreateSkillWithVersion(ctx, skill); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectSkillConfigAtomic(ctx, project.ID, "custom", map[int64]bool{skill.ID: true}); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetProject(ctx, project.ID)
	if err != nil || stored.SkillPolicy != "custom" {
		t.Fatalf("policy after commit = %q, err=%v", stored.SkillPolicy, err)
	}
	configs, err := db.ListProjectSkillConfigs(ctx, project.ID)
	if err != nil || len(configs) != 1 || configs[0].SkillID != skill.ID || !configs[0].Enabled {
		t.Fatalf("configs after commit = %+v, err=%v", configs, err)
	}

	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_skill_config_insert BEFORE INSERT ON project_skill_config BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectSkillConfigAtomic(ctx, project.ID, "", map[int64]bool{skill.ID: false}); err == nil {
		t.Fatal("expected forced transaction failure")
	}
	stored, err = db.GetProject(ctx, project.ID)
	if err != nil || stored.SkillPolicy != "custom" {
		t.Fatalf("policy was not rolled back: %q, err=%v", stored.SkillPolicy, err)
	}
	configs, err = db.ListProjectSkillConfigs(ctx, project.ID)
	if err != nil || len(configs) != 1 || !configs[0].Enabled {
		t.Fatalf("configs were not rolled back: %+v, err=%v", configs, err)
	}
}
