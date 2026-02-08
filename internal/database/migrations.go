package database

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/jmoiron/sqlx"
)

// Migration represents a versioned database schema migration.
type Migration struct {
	Version     int
	Description string
	Up          func(tx *sqlx.Tx) error
}

var migrations = []Migration{
	{Version: 1, Description: "initial schema", Up: migrateV1},
	{Version: 2, Description: "notifications: add permission and hook_notification types", Up: migrateV2},
	{Version: 3, Description: "skills: add category, sort_order, sync_count; add synced_skill_files and skill_versions tables", Up: migrateV3},
	{Version: 4, Description: "ai: add ai_conversations and ai_messages tables", Up: migrateV4},
}

// RunMigrations applies all pending migrations to the database.
func RunMigrations(db *sqlx.DB) error {
	// Create schema_version table if it doesn't exist
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	// Get current version
	var currentVersion int
	err = db.Get(&currentVersion, "SELECT version FROM schema_version LIMIT 1")
	if err != nil {
		if err == sql.ErrNoRows {
			currentVersion = 0
		} else {
			return fmt.Errorf("failed to read schema version: %w", err)
		}
	}

	// Apply pending migrations
	for _, m := range migrations {
		if m.Version <= currentVersion {
			continue
		}

		tx, err := db.Beginx()
		if err != nil {
			return fmt.Errorf("failed to begin transaction for migration v%d: %w", m.Version, err)
		}

		if err := m.Up(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d (%s) failed: %w", m.Version, m.Description, err)
		}

		// Update or insert schema version
		if currentVersion == 0 && m.Version == 1 {
			_, err = tx.Exec("INSERT INTO schema_version (version) VALUES (?)", m.Version)
		} else {
			_, err = tx.Exec("UPDATE schema_version SET version = ?, applied_at = CURRENT_TIMESTAMP", m.Version)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to update schema version to %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration v%d: %w", m.Version, err)
		}

		log.Printf("[DB] Migration v%d: %s - OK", m.Version, m.Description)
		currentVersion = m.Version
	}

	log.Printf("[DB] Schema version: %d", currentVersion)
	return nil
}

// migrateV1 creates the initial schema (all tables + indices).
// Uses CREATE TABLE IF NOT EXISTS so it's idempotent for existing databases.
func migrateV1(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			path TEXT NOT NULL,
			type TEXT NOT NULL CHECK(type IN ('local', 'remote')),
			ssh_host TEXT,
			ssh_port INTEGER,
			ssh_user TEXT,
			ssh_auth_type TEXT CHECK(ssh_auth_type IN ('password', 'key', 'key_passphrase', NULL)),
			ssh_credential_encrypted TEXT,
			ssh_credential_iv TEXT,
			config_synced_at TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			status TEXT NOT NULL CHECK(status IN ('starting', 'running', 'stopped', 'error', 'completed')),
			pid INTEGER,
			start_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			end_time TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS macros (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			script TEXT NOT NULL,
			target_type TEXT NOT NULL CHECK(target_type IN ('local', 'remote', 'any')),
			is_builtin BOOLEAN NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS skills (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			content TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_servers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			command TEXT NOT NULL,
			args TEXT NOT NULL DEFAULT '[]',
			env TEXT NOT NULL DEFAULT '{}',
			enabled BOOLEAN NOT NULL DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS push_subscriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint TEXT UNIQUE NOT NULL,
			p256dh TEXT NOT NULL,
			auth TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS notifications (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
			type TEXT NOT NULL CHECK(type IN ('info', 'warning', 'error', 'question', 'permission', 'hook_notification')),
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			read BOOLEAN NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project_id ON sessions(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_session_id ON notifications(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_read ON notifications(read)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV2 updates the notifications CHECK constraint to include 'permission' and 'hook_notification'.
// For databases created with v1 (which already has the new types), this is a no-op.
func migrateV2(tx *sqlx.Tx) error {
	// Check if constraint already supports 'permission' by trying a test insert
	_, err := tx.Exec("INSERT INTO notifications (session_id, type, title, body) VALUES ('__test__', 'permission', '__test__', '__test__')")
	if err != nil {
		// Constraint doesn't allow 'permission' - need to recreate table
		stmts := []string{
			`CREATE TABLE notifications_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
				type TEXT NOT NULL CHECK(type IN ('info', 'warning', 'error', 'question', 'permission', 'hook_notification')),
				title TEXT NOT NULL,
				body TEXT NOT NULL DEFAULT '',
				read BOOLEAN NOT NULL DEFAULT 0,
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			)`,
			`INSERT INTO notifications_new SELECT * FROM notifications`,
			`DROP TABLE notifications`,
			`ALTER TABLE notifications_new RENAME TO notifications`,
			`CREATE INDEX IF NOT EXISTS idx_notifications_session_id ON notifications(session_id)`,
			`CREATE INDEX IF NOT EXISTS idx_notifications_read ON notifications(read)`,
		}
		for _, s := range stmts {
			if _, err := tx.Exec(s); err != nil {
				return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
			}
		}
	} else {
		// Constraint already allows 'permission' - clean up test row
		tx.Exec("DELETE FROM notifications WHERE session_id = '__test__' AND title = '__test__'")
	}
	return nil
}

// migrateV3 adds category, sort_order, sync_count to skills;
// creates synced_skill_files and skill_versions tables.
func migrateV3(tx *sqlx.Tx) error {
	stmts := []string{
		// New columns on skills
		`ALTER TABLE skills ADD COLUMN category TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE skills ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE skills ADD COLUMN sync_count INTEGER NOT NULL DEFAULT 0`,

		// Track which skill files DevManager wrote to each project
		`CREATE TABLE IF NOT EXISTS synced_skill_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			skill_id INTEGER,
			file_name TEXT NOT NULL,
			synced_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
			FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE SET NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_synced_skill_files_project ON synced_skill_files(project_id)`,

		// Skill version history
		`CREATE TABLE IF NOT EXISTS skill_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			skill_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			content TEXT NOT NULL,
			version INTEGER NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_skill_versions_skill ON skill_versions(skill_id)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV4 adds AI conversation and message tables.
func migrateV4(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ai_conversations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS ai_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id INTEGER NOT NULL,
			role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
			content TEXT NOT NULL,
			tool_calls TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (conversation_id) REFERENCES ai_conversations(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_messages_conversation ON ai_messages(conversation_id)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}
