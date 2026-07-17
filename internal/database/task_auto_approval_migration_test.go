package database

import (
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestMigrateV56DefaultsExistingProjectsToInherit(t *testing.T) {
	db, err := sqlx.Connect("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE projects (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO projects (id, name) VALUES (1, 'existing');
	`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV56(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var value string
	if err := db.Get(&value, "SELECT task_auto_approve_verification FROM projects WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if value != "inherit" {
		t.Fatalf("existing project override=%q, want inherit", value)
	}
	if _, err := db.Exec("UPDATE projects SET task_auto_approve_verification = 'invalid' WHERE id = 1"); err == nil {
		t.Fatal("expected invalid override to violate the database constraint")
	}
}
