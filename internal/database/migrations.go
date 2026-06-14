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
	{Version: 24, Description: "tasks: add awaiting_approval status and verification_doc_id column", Up: migrateV24},
	{Version: 25, Description: "docs: add task_id to temp_documents for task-document linking", Up: migrateV25},
	{Version: 26, Description: "skills: per-project skill config and project-specific skills", Up: migrateV26},
	{Version: 27, Description: "notifications: add link column for context-aware navigation", Up: migrateV27},
	{Version: 28, Description: "ai_suggestions: add unlink_task type to CHECK constraint", Up: migrateV28},
	{Version: 29, Description: "tasks: remove blocked status from CHECK constraint", Up: migrateV29},
	{Version: 30, Description: "ai: add ai_configs and ai_config_assignments tables for multi-provider support", Up: migrateV30},
	{Version: 31, Description: "tunnel: add paired_devices table for remote access authentication", Up: migrateV31},
	{Version: 32, Description: "shares: add project_shares table for cross-project file read access", Up: migrateV32},
	{Version: 33, Description: "mcp: add project_mcp_servers table for per-project MCP server configurations", Up: migrateV33},
	{Version: 34, Description: "projects: add dangerously_skip_permissions column", Up: migrateV34},
	{Version: 35, Description: "multi-backend: add backend and backend_config columns to projects and sessions", Up: migrateV35},
	{Version: 36, Description: "sessions: add skip_permissions for auto-restore on restart", Up: migrateV36},
	{Version: 37, Description: "docs: add session_id to temp_documents for session-document linking", Up: migrateV37},
	{Version: 38, Description: "tags: add project_tags table for project tagging and grouping", Up: migrateV38},
	{Version: 39, Description: "tools: add project_tools table for custom per-project tool definitions", Up: migrateV39},
	{Version: 40, Description: "tags: add global tags table and migrate project_tags to use tag_id FK", Up: migrateV40},
	{Version: 41, Description: "agents: add ai_agents table and agent_id column to ai_conversations", Up: migrateV41},
	{Version: 42, Description: "sessions: add provider_session_id for native backend resume cursors", Up: migrateV42},
	{Version: 43, Description: "codex: persist app-server structured transcript events", Up: migrateV43},
	{Version: 44, Description: "codex: add transcript retention cleanup index", Up: migrateV44},
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

		// Track which skill files OpenPoet wrote to each project
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

// migrateV24 adds awaiting_approval status and verification_doc_id column to project_tasks.
func migrateV24(tx *sqlx.Tx) error {
	// First, add the verification_doc_id column
	_, err := tx.Exec(`ALTER TABLE project_tasks ADD COLUMN verification_doc_id TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		// Column may already exist
		log.Printf("migrateV24: ALTER TABLE (add column) note: %v", err)
	}

	// Check if constraint already supports 'awaiting_approval' by trying a test insert
	_, err = tx.Exec("INSERT INTO project_tasks (project_id, title, status) VALUES (0, '__test_v24__', 'awaiting_approval')")
	if err != nil {
		// Constraint doesn't allow 'awaiting_approval' - need to recreate table
		stmts := []string{
			`CREATE TABLE project_tasks_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
				parent_id INTEGER REFERENCES project_tasks_new(id) ON DELETE CASCADE,
				title TEXT NOT NULL,
				description TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT 'todo' CHECK(status IN ('todo', 'in_progress', 'done', 'blocked', 'awaiting_approval')),
				priority TEXT NOT NULL DEFAULT 'medium' CHECK(priority IN ('low', 'medium', 'high', 'urgent')),
				due_date TIMESTAMP,
				sort_order INTEGER NOT NULL DEFAULT 0,
				global_sort_order INTEGER NOT NULL DEFAULT 0,
				due_notified BOOLEAN NOT NULL DEFAULT 0,
				verification_doc_id TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			)`,
			`INSERT INTO project_tasks_new SELECT id, project_id, parent_id, title, description, status, priority, due_date, sort_order, global_sort_order, due_notified, verification_doc_id, created_at, updated_at FROM project_tasks`,
			`DROP TABLE project_tasks`,
			`ALTER TABLE project_tasks_new RENAME TO project_tasks`,
			`CREATE INDEX IF NOT EXISTS idx_project_tasks_project ON project_tasks(project_id)`,
			`CREATE INDEX IF NOT EXISTS idx_project_tasks_parent ON project_tasks(parent_id)`,
			`CREATE INDEX IF NOT EXISTS idx_project_tasks_status ON project_tasks(status)`,
			`CREATE INDEX IF NOT EXISTS idx_project_tasks_due_date ON project_tasks(due_date)`,
		}
		for _, s := range stmts {
			if _, err := tx.Exec(s); err != nil {
				return fmt.Errorf("migrateV24 statement failed: %w\nSQL: %s", err, s)
			}
		}
	} else {
		// Constraint already allows 'awaiting_approval' - clean up test row
		tx.Exec("DELETE FROM project_tasks WHERE project_id = 0 AND title = '__test_v24__'")
	}
	return nil
}

// migrateV25 adds task_id column to temp_documents for linking documents to tasks.
func migrateV25(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE temp_documents ADD COLUMN task_id INTEGER`,
		`CREATE INDEX IF NOT EXISTS idx_temp_documents_task ON temp_documents(task_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			log.Printf("migrateV25: %v (may already exist)", err)
		}
	}
	return nil
}

// migrateV26 adds per-project skill configuration and project-specific skills.
func migrateV26(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE projects ADD COLUMN skill_policy TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS project_skill_config (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			skill_id INTEGER NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			UNIQUE(project_id, skill_id),
			FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
			FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_skill_config_project ON project_skill_config(project_id)`,
		`CREATE TABLE IF NOT EXISTS project_skills (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT 1,
			category TEXT NOT NULL DEFAULT '',
			sort_order INTEGER NOT NULL DEFAULT 0,
			sync_count INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_id, name),
			FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_skills_project ON project_skills(project_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			log.Printf("migrateV26: %v (may already exist)", err)
		}
	}
	return nil
}

// migrateV27 adds a link column to notifications for context-aware navigation.
func migrateV27(tx *sqlx.Tx) error {
	_, err := tx.Exec(`ALTER TABLE notifications ADD COLUMN link TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		log.Printf("migrateV27: %v (column may already exist)", err)
	}
	return nil
}

// migrateV28 adds unlink_task to ai_suggestions type CHECK constraint.
// SQLite requires table recreation to modify CHECK constraints.
func migrateV28(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE ai_suggestions_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
			project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
			type TEXT NOT NULL CHECK(type IN ('link_task', 'create_task', 'update_task', 'complete_task', 'unlink_task')),
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			context_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'accepted', 'dismissed')),
			level TEXT NOT NULL DEFAULT 'standard',
			conversation_id INTEGER REFERENCES ai_conversations(id) ON DELETE SET NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO ai_suggestions_new SELECT id, session_id, project_id, type, title, description, context_json, status, level, conversation_id, created_at FROM ai_suggestions`,
		`DROP TABLE ai_suggestions`,
		`ALTER TABLE ai_suggestions_new RENAME TO ai_suggestions`,
		`CREATE INDEX IF NOT EXISTS idx_ai_suggestions_session ON ai_suggestions(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_suggestions_conversation ON ai_suggestions(conversation_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV28 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV29(tx *sqlx.Tx) error {
	// Convert any existing blocked tasks to todo
	if _, err := tx.Exec(`UPDATE project_tasks SET status = 'todo' WHERE status = 'blocked'`); err != nil {
		return fmt.Errorf("migrateV29 update blocked→todo failed: %w", err)
	}
	stmts := []string{
		`CREATE TABLE project_tasks_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			parent_id INTEGER REFERENCES project_tasks_new(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'todo' CHECK(status IN ('todo', 'in_progress', 'done', 'awaiting_approval')),
			priority TEXT NOT NULL DEFAULT 'medium' CHECK(priority IN ('low', 'medium', 'high', 'urgent')),
			due_date TIMESTAMP,
			sort_order INTEGER NOT NULL DEFAULT 0,
			global_sort_order INTEGER NOT NULL DEFAULT 0,
			due_notified BOOLEAN NOT NULL DEFAULT 0,
			verification_doc_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO project_tasks_new SELECT id, project_id, parent_id, title, description, status, priority, due_date, sort_order, global_sort_order, due_notified, verification_doc_id, created_at, updated_at FROM project_tasks`,
		`DROP TABLE project_tasks`,
		`ALTER TABLE project_tasks_new RENAME TO project_tasks`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_project ON project_tasks(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_parent ON project_tasks(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_status ON project_tasks(status)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tasks_due_date ON project_tasks(due_date)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV29 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateV30 adds ai_configs and ai_config_assignments tables for multi-provider support.
// Migrates existing flat settings into a "Default" configuration if ai_provider is set.
func migrateV30(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ai_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			provider_type TEXT NOT NULL,
			api_key_encrypted TEXT NOT NULL DEFAULT '',
			api_key_iv TEXT NOT NULL DEFAULT '',
			api_key_preview TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			base_url TEXT NOT NULL DEFAULT '',
			extra_json TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS ai_config_assignments (
			slot TEXT PRIMARY KEY,
			config_id INTEGER REFERENCES ai_configs(id) ON DELETE SET NULL
		)`,
		`INSERT OR IGNORE INTO ai_config_assignments(slot, config_id) VALUES ('ai_chat', NULL)`,
		`INSERT OR IGNORE INTO ai_config_assignments(slot, config_id) VALUES ('ai_background', NULL)`,
		`INSERT OR IGNORE INTO ai_config_assignments(slot, config_id) VALUES ('claude_session', NULL)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV30 failed: %w\nSQL: %s", err, s)
		}
	}

	// Migrate existing flat settings into a "Default" config
	var providerType string
	err := tx.Get(&providerType, "SELECT value FROM settings WHERE key = 'ai_provider'")
	if err != nil || providerType == "" {
		// No provider configured — skip migration, slots stay NULL (auto-detect)
		return nil
	}

	// Read existing settings
	getSetting := func(key string) string {
		var val string
		_ = tx.Get(&val, "SELECT value FROM settings WHERE key = ?", key)
		return val
	}

	model := getSetting("ai_model")
	baseURL := ""
	apiKeyEnc := getSetting("anthropic_api_key")
	apiKeyIV := getSetting("anthropic_api_key_iv")
	apiKeyPreview := getSetting("anthropic_api_key_preview")

	// For ollama providers, use ollama-specific settings
	if providerType == "ollama" || providerType == "ollama-sdk" {
		baseURL = getSetting("ollama_base_url")
		if m := getSetting("ollama_model"); m != "" {
			model = m
		}
		if apiKeyEnc == "" {
			apiKeyEnc = getSetting("ollama_api_key")
			apiKeyIV = getSetting("ollama_api_key_iv")
			apiKeyPreview = getSetting("ollama_api_key_preview")
		}
	}

	// Create "Default" config
	result, err := tx.Exec(
		`INSERT INTO ai_configs (name, provider_type, api_key_encrypted, api_key_iv, api_key_preview, model, base_url)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"Default", providerType, apiKeyEnc, apiKeyIV, apiKeyPreview, model, baseURL,
	)
	if err != nil {
		// If insert fails (e.g. duplicate name), skip assignment
		log.Printf("[Migration V30] Could not create Default config: %v", err)
		return nil
	}

	configID, _ := result.LastInsertId()

	// Assign the Default config to all 3 slots
	for _, slot := range []string{"ai_chat", "ai_background", "claude_session"} {
		if _, err := tx.Exec("UPDATE ai_config_assignments SET config_id = ? WHERE slot = ?", configID, slot); err != nil {
			return fmt.Errorf("migrateV30 assign %s failed: %w", slot, err)
		}
	}

	log.Printf("[Migration V30] Migrated existing settings into 'Default' config (id=%d, type=%s)", configID, providerType)
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

func migrateV31(tx *sqlx.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS paired_devices (
		id TEXT PRIMARY KEY,
		device_name TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		encryption_key TEXT NOT NULL DEFAULT '',
		encryption_key_iv TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		revoked INTEGER DEFAULT 0
	)`)
	return err
}

func migrateV33(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_mcp_servers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			command TEXT NOT NULL,
			args TEXT NOT NULL DEFAULT '[]',
			env TEXT NOT NULL DEFAULT '{}',
			enabled BOOLEAN NOT NULL DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_id, name),
			FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_mcp_servers_project ON project_mcp_servers(project_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV33 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV34(tx *sqlx.Tx) error {
	_, err := tx.Exec(`ALTER TABLE projects ADD COLUMN dangerously_skip_permissions INTEGER NOT NULL DEFAULT 0`)
	if err != nil {
		return fmt.Errorf("migrateV34 failed: %w", err)
	}
	return nil
}

func migrateV32(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_shares (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			shared_project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_id, shared_project_id),
			CHECK(project_id != shared_project_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_shares_project ON project_shares(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_shares_shared ON project_shares(shared_project_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV32 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV35(tx *sqlx.Tx) error {
	stmts := []string{
		`ALTER TABLE projects ADD COLUMN backend TEXT NOT NULL DEFAULT 'claude_code'`,
		`ALTER TABLE projects ADD COLUMN backend_config TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE sessions ADD COLUMN backend TEXT NOT NULL DEFAULT 'claude_code'`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV35 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV36(tx *sqlx.Tx) error {
	_, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN skip_permissions INTEGER NOT NULL DEFAULT 0`)
	if err != nil {
		return fmt.Errorf("migrateV36 failed: %w", err)
	}
	return nil
}

func migrateV37(tx *sqlx.Tx) error {
	for _, q := range []string{
		`ALTER TABLE temp_documents ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_temp_documents_session ON temp_documents(session_id)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrateV37 failed: %w", err)
		}
	}
	return nil
}

func migrateV38(tx *sqlx.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS project_tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			tag TEXT NOT NULL,
			color TEXT NOT NULL DEFAULT '#7aa2f7',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_id, tag)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tags_tag ON project_tags(tag)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tags_project ON project_tags(project_id)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrateV38 failed: %w", err)
		}
	}
	return nil
}

func migrateV39(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_tools (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			command TEXT NOT NULL,
			parameters TEXT NOT NULL DEFAULT '{}',
			confirm INTEGER NOT NULL DEFAULT 0,
			working_dir TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_id, name),
			FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tools_project ON project_tools(project_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV39 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV40(tx *sqlx.Tx) error {
	stmts := []string{
		// Create global tags table
		`CREATE TABLE IF NOT EXISTS tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			color TEXT NOT NULL DEFAULT '#7aa2f7',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		// Migrate existing tags from project_tags into global tags table
		`INSERT OR IGNORE INTO tags (name, color)
		 SELECT DISTINCT tag, color FROM project_tags`,
		// Recreate project_tags with tag_id FK
		`CREATE TABLE project_tags_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_id, tag_id)
		)`,
		// Migrate existing assignments
		`INSERT INTO project_tags_new (project_id, tag_id, created_at)
		 SELECT pt.project_id, t.id, pt.created_at
		 FROM project_tags pt JOIN tags t ON pt.tag = t.name`,
		// Swap tables
		`DROP TABLE project_tags`,
		`ALTER TABLE project_tags_new RENAME TO project_tags`,
		`CREATE INDEX IF NOT EXISTS idx_project_tags_project ON project_tags(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tags_tag ON project_tags(tag_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV40 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV41(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ai_agents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			system_prompt TEXT NOT NULL DEFAULT '',
			tool_policy TEXT NOT NULL DEFAULT '',
			project_filter TEXT NOT NULL DEFAULT '',
			is_default INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO ai_agents (name, system_prompt, tool_policy, project_filter, is_default, enabled)
		 VALUES ('Default', '', '', '', 1, 1)`,
		`ALTER TABLE ai_conversations ADD COLUMN agent_id INTEGER REFERENCES ai_agents(id) ON DELETE SET NULL`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV41 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV42(tx *sqlx.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN provider_session_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrateV42 failed: %w", err)
	}
	return nil
}

func migrateV43(tx *sqlx.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS codex_transcript_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			event_id INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'status',
			text TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			command TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			append INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_transcript_session_id ON codex_transcript_events(session_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_transcript_event_id ON codex_transcript_events(session_id, event_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateV43 failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func migrateV44(tx *sqlx.Tx) error {
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_status_end_time ON sessions(status, end_time)`)
	if err != nil {
		return fmt.Errorf("migrateV44 failed: %w", err)
	}
	return nil
}
