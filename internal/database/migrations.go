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
	{Version: 5, Description: "meta: add project_meta_documents table", Up: migrateV5},
	{Version: 6, Description: "docs: add temp_documents table", Up: migrateV6},
	{Version: 7, Description: "tasks: add project_tasks table", Up: migrateV7},
	{Version: 8, Description: "session-task integration: session_tasks, ai_suggestions tables, session name/task_id columns", Up: migrateV8},
	{Version: 9, Description: "ai proactive: add source/level/context columns to ai_conversations and ai_suggestions", Up: migrateV9},
	{Version: 10, Description: "tokens: add token_usage table for tracking AI and Claude Code token consumption", Up: migrateV10},
	{Version: 11, Description: "tokens: add subcategory column for AI assistant usage breakdown", Up: migrateV11},
	{Version: 12, Description: "rename project_meta_documents to memory_docs", Up: migrateV12},
	{Version: 13, Description: "docs: add conversation_id to temp_documents", Up: migrateV13},
	{Version: 14, Description: "remove macros table", Up: migrateV14},
	{Version: 15, Description: "docs: add summary and status to temp_documents", Up: migrateV15},
	{Version: 16, Description: "ai_messages: add status and error_info columns for streaming persistence", Up: migrateV16},
	{Version: 17, Description: "projects: add tool_policy column for per-project tool access control", Up: migrateV17},
	{Version: 18, Description: "docs: add message_id to temp_documents", Up: migrateV18},
	{Version: 19, Description: "tasks: add global_sort_order for cross-project ordering", Up: migrateV19},
	{Version: 20, Description: "sessions: add last_activity_at for tracking last output/event", Up: migrateV20},
	{Version: 21, Description: "ai: add feedback_ack to temp_documents and session_id to ai_conversations", Up: migrateV21},
	{Version: 22, Description: "sessions: add plan_content and plan_updated_at for persisting plans", Up: migrateV22},
	{Version: 23, Description: "tasks: add task_history table for activity tracking", Up: migrateV23},
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

// migrateV6 adds the temp_documents table for AI-generated temporary documents.
func migrateV6(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS temp_documents (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV5 adds the project_meta_documents table for per-project meta docs.
func migrateV5(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_meta_documents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
			content TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1,
			last_updated_by TEXT NOT NULL DEFAULT '',
			summary TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_project_meta_project ON project_meta_documents(project_id)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV7 adds the project_tasks table for per-project task management.
func migrateV7(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			parent_id INTEGER REFERENCES project_tasks(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'todo' CHECK(status IN ('todo', 'in_progress', 'done', 'blocked')),
			priority TEXT NOT NULL DEFAULT 'medium' CHECK(priority IN ('low', 'medium', 'high', 'urgent')),
			due_date TIMESTAMP,
			sort_order INTEGER NOT NULL DEFAULT 0,
			due_notified BOOLEAN NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_project ON project_tasks(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_parent ON project_tasks(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_status ON project_tasks(status)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_due_date ON project_tasks(due_date)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV8 adds session-task integration tables and columns.
func migrateV8(tx *sqlx.Tx) error {
	stmts := []string{
		// Add name and task_id columns to sessions
		`ALTER TABLE sessions ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN task_id INTEGER REFERENCES project_tasks(id) ON DELETE SET NULL`,

		// Junction table: session <-> task relationship
		`CREATE TABLE IF NOT EXISTS session_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			task_id INTEGER NOT NULL REFERENCES project_tasks(id) ON DELETE CASCADE,
			role TEXT NOT NULL DEFAULT 'works_on' CHECK(role IN ('works_on', 'created_from', 'registered_as')),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(session_id, task_id, role)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tasks_session ON session_tasks(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tasks_task ON session_tasks(task_id)`,

		// AI suggestions table
		`CREATE TABLE IF NOT EXISTS ai_suggestions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
			project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
			type TEXT NOT NULL CHECK(type IN ('link_task', 'create_task', 'update_task', 'complete_task')),
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			context_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'accepted', 'dismissed')),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV9 adds proactive AI interaction framework columns.
func migrateV9(tx *sqlx.Tx) error {
	stmts := []string{
		// ai_conversations: track source (user vs ai-initiated)
		`ALTER TABLE ai_conversations ADD COLUMN source TEXT NOT NULL DEFAULT 'user'`,
		`ALTER TABLE ai_conversations ADD COLUMN proactive_level TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ai_conversations ADD COLUMN proactive_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ai_conversations ADD COLUMN proactive_context TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE ai_conversations ADD COLUMN is_read INTEGER NOT NULL DEFAULT 1`,

		// ai_suggestions: add level and link to conversation
		`ALTER TABLE ai_suggestions ADD COLUMN level TEXT NOT NULL DEFAULT 'standard'`,
		`ALTER TABLE ai_suggestions ADD COLUMN conversation_id INTEGER REFERENCES ai_conversations(id) ON DELETE SET NULL`,

		// Indexes
		`CREATE INDEX IF NOT EXISTS idx_ai_conversations_source ON ai_conversations(source)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_suggestions_conversation ON ai_suggestions(conversation_id)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV10 adds the token_usage table for tracking token consumption.
func migrateV10(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS token_usage (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source TEXT NOT NULL CHECK(source IN ('ai_assistant', 'claude_code')),
			project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
			session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
			conversation_id INTEGER REFERENCES ai_conversations(id) ON DELETE SET NULL,
			model TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_source ON token_usage(source)`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_project ON token_usage(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_session ON token_usage(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_conversation ON token_usage(conversation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_created ON token_usage(created_at)`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV11 adds subcategory column to token_usage for AI assistant breakdown.
func migrateV11(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE token_usage ADD COLUMN subcategory TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_subcategory ON token_usage(subcategory)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV12 renames project_meta_documents to memory_docs.
func migrateV12(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE project_meta_documents RENAME TO memory_docs`,
		`DROP INDEX IF EXISTS idx_project_meta_project`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_docs_project ON memory_docs(project_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV13 adds conversation_id to temp_documents for cleanup on conversation deletion.
func migrateV13(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE temp_documents ADD COLUMN conversation_id INTEGER`,
		`CREATE INDEX IF NOT EXISTS idx_temp_documents_conversation ON temp_documents(conversation_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV14 removes the macros table.
func migrateV14(tx *sqlx.Tx) error {
	_, err := tx.Exec(`DROP TABLE IF EXISTS macros`)
	return err
}

// migrateV15 adds summary and status columns to temp_documents for persistence in chat history.
func migrateV15(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE temp_documents ADD COLUMN summary TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE temp_documents ADD COLUMN status TEXT NOT NULL DEFAULT 'pending'`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV16 adds status and error_info columns to ai_messages for streaming persistence.
func migrateV16(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE ai_messages ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`,
		`ALTER TABLE ai_messages ADD COLUMN error_info TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV17 adds tool_policy column to projects for per-project tool access control.
func migrateV17(tx *sqlx.Tx) error {
	_, err := tx.Exec(`ALTER TABLE projects ADD COLUMN tool_policy TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateV18 adds message_id to temp_documents for associating doc cards with specific messages.
func migrateV18(tx *sqlx.Tx) error {
	_, err := tx.Exec(`ALTER TABLE temp_documents ADD COLUMN message_id INTEGER NOT NULL DEFAULT 0`)
	return err
}

// migrateV19 adds global_sort_order to project_tasks for cross-project ordering in the global view.
func migrateV19(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE project_tasks ADD COLUMN global_sort_order INTEGER NOT NULL DEFAULT 0`,
		`UPDATE project_tasks SET global_sort_order = id`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV20 adds last_activity_at to sessions for tracking last output/event.
func migrateV20(tx *sqlx.Tx) error {
	_, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN last_activity_at TIMESTAMP`)
	return err
}

// migrateV21 adds feedback_ack to temp_documents for tracking AI acknowledgment of proposal outcomes,
// and session_id to ai_conversations for persisting Claude Code session IDs across restarts.
func migrateV21(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE temp_documents ADD COLUMN feedback_ack INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ai_conversations ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func migrateV22(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE sessions ADD COLUMN plan_content TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN plan_updated_at TIMESTAMP`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func migrateV23(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS task_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL REFERENCES project_tasks(id) ON DELETE CASCADE,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL,
			details TEXT NOT NULL DEFAULT '{}',
			actor TEXT NOT NULL DEFAULT 'system',
			session_id TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_task_history_task ON task_history(task_id)`,
		`CREATE INDEX idx_task_history_created ON task_history(created_at)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
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
