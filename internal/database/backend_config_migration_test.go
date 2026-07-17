package database

import (
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestMigrateV55ClearsOnlyClaudeCodeBackendConfig(t *testing.T) {
	db, err := sqlx.Connect("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.MustExec(`CREATE TABLE projects (id INTEGER PRIMARY KEY, backend TEXT NOT NULL, backend_config TEXT NOT NULL)`)
	db.MustExec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, backend TEXT NOT NULL, requested_model TEXT NOT NULL, effort TEXT NOT NULL)`)
	db.MustExec(`INSERT INTO projects (id, backend, backend_config) VALUES
		(1, 'claude_code', '{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}'),
		(2, 'claude_code', '{}'),
		(3, 'codex', '{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}')`)
	db.MustExec(`INSERT INTO sessions (id, backend, requested_model, effort) VALUES
		('claude-leaked', 'claude_code', 'gpt-5.6-sol', 'xhigh'),
		('claude-valid', 'claude_code', 'sonnet', 'high'),
		('codex-valid', 'codex', 'gpt-5.6-sol', 'xhigh')`)

	tx := db.MustBegin()
	if err := migrateV55(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var claudeConfig, codexConfig, leakedModel, leakedEffort, validClaudeModel, validCodexModel string
	if err := db.Get(&claudeConfig, `SELECT backend_config FROM projects WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&codexConfig, `SELECT backend_config FROM projects WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowx(`SELECT requested_model, effort FROM sessions WHERE id = 'claude-leaked'`).Scan(&leakedModel, &leakedEffort); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&validClaudeModel, `SELECT requested_model FROM sessions WHERE id = 'claude-valid'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&validCodexModel, `SELECT requested_model FROM sessions WHERE id = 'codex-valid'`); err != nil {
		t.Fatal(err)
	}
	if claudeConfig != "{}" {
		t.Fatalf("Claude Code config = %q, want empty object", claudeConfig)
	}
	if codexConfig != `{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}` {
		t.Fatalf("Codex config changed unexpectedly: %q", codexConfig)
	}
	if leakedModel != "default" || leakedEffort != "default" {
		t.Fatalf("leaked Claude session metadata = model %q effort %q", leakedModel, leakedEffort)
	}
	if validClaudeModel != "sonnet" || validCodexModel != "gpt-5.6-sol" {
		t.Fatalf("valid session metadata changed: Claude %q Codex %q", validClaudeModel, validCodexModel)
	}
}
