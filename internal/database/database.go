package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type DB struct {
	*sqlx.DB
}

func New(path string) (*DB, error) {
	db, err := sqlx.Connect("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite doesn't support concurrent writes

	d := &DB{DB: db}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return d, nil
}

func (d *DB) migrate() error {
	return RunMigrations(d.DB)
}

// Project operations
func (d *DB) CreateProject(ctx context.Context, p *Project) error {
	query := `INSERT INTO projects (name, path, type, ssh_host, ssh_port, ssh_user, ssh_auth_type, ssh_credential_encrypted, ssh_credential_iv)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, p.Name, p.Path, p.Type, p.SSHHost, p.SSHPort, p.SSHUser, p.SSHAuthType, p.SSHCredentialEncrypted, p.SSHCredentialIV)
	if err != nil {
		return err
	}
	p.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetProject(ctx context.Context, id int64) (*Project, error) {
	var p Project
	err := d.GetContext(ctx, &p, "SELECT * FROM projects WHERE id = ?", id)
	return &p, err
}

func (d *DB) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	var p Project
	err := d.GetContext(ctx, &p, "SELECT * FROM projects WHERE name = ?", name)
	return &p, err
}

func (d *DB) ListProjects(ctx context.Context) ([]Project, error) {
	var projects []Project
	err := d.SelectContext(ctx, &projects, "SELECT * FROM projects ORDER BY name")
	return projects, err
}

func (d *DB) UpdateProject(ctx context.Context, p *Project) error {
	query := `UPDATE projects SET name=?, path=?, type=?, ssh_host=?, ssh_port=?, ssh_user=?, ssh_auth_type=?, ssh_credential_encrypted=?, ssh_credential_iv=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, p.Name, p.Path, p.Type, p.SSHHost, p.SSHPort, p.SSHUser, p.SSHAuthType, p.SSHCredentialEncrypted, p.SSHCredentialIV, time.Now(), p.ID)
	return err
}

func (d *DB) UpdateProjectConfigSyncedAt(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE projects SET config_synced_at=? WHERE id=?", time.Now(), id)
	return err
}

func (d *DB) DeleteProject(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM projects WHERE id = ?", id)
	return err
}

// Session operations
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	query := `INSERT INTO sessions (id, project_id, status, pid, name, task_id, start_time) VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := d.ExecContext(ctx, query, s.ID, s.ProjectID, s.Status, s.PID, s.Name, s.TaskID, s.StartTime)
	return err
}

func (d *DB) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := d.GetContext(ctx, &s, "SELECT * FROM sessions WHERE id = ?", id)
	return &s, err
}

func (d *DB) ListSessions(ctx context.Context) ([]Session, error) {
	var sessions []Session
	err := d.SelectContext(ctx, &sessions, "SELECT * FROM sessions ORDER BY start_time DESC")
	return sessions, err
}

func (d *DB) ListSessionsByProject(ctx context.Context, projectID int64) ([]Session, error) {
	var sessions []Session
	err := d.SelectContext(ctx, &sessions, "SELECT * FROM sessions WHERE project_id = ? ORDER BY start_time DESC", projectID)
	return sessions, err
}

func (d *DB) ListActiveSessions(ctx context.Context) ([]Session, error) {
	var sessions []Session
	err := d.SelectContext(ctx, &sessions, "SELECT * FROM sessions WHERE status IN ('starting', 'running') ORDER BY start_time DESC")
	return sessions, err
}

func (d *DB) UpdateSessionStatus(ctx context.Context, id string, status string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET status=? WHERE id=?", status, id)
	return err
}

func (d *DB) UpdateSessionPID(ctx context.Context, id string, pid int) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET pid=? WHERE id=?", pid, id)
	return err
}

func (d *DB) EndSession(ctx context.Context, id string, status string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET status=?, end_time=? WHERE id=?", status, time.Now(), id)
	return err
}

func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	return err
}

// Macro operations
func (d *DB) CreateMacro(ctx context.Context, m *Macro) error {
	query := `INSERT INTO macros (name, description, script, target_type, is_builtin) VALUES (?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, m.Name, m.Description, m.Script, m.TargetType, m.IsBuiltin)
	if err != nil {
		return err
	}
	m.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetMacro(ctx context.Context, id int64) (*Macro, error) {
	var m Macro
	err := d.GetContext(ctx, &m, "SELECT * FROM macros WHERE id = ?", id)
	return &m, err
}

func (d *DB) GetMacroByName(ctx context.Context, name string) (*Macro, error) {
	var m Macro
	err := d.GetContext(ctx, &m, "SELECT * FROM macros WHERE name = ?", name)
	return &m, err
}

func (d *DB) ListMacros(ctx context.Context) ([]Macro, error) {
	var macros []Macro
	err := d.SelectContext(ctx, &macros, "SELECT * FROM macros ORDER BY is_builtin DESC, name")
	return macros, err
}

func (d *DB) UpdateMacro(ctx context.Context, m *Macro) error {
	query := `UPDATE macros SET name=?, description=?, script=?, target_type=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, m.Name, m.Description, m.Script, m.TargetType, time.Now(), m.ID)
	return err
}

func (d *DB) DeleteMacro(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM macros WHERE id = ? AND is_builtin = 0", id)
	return err
}

// Skill operations
func (d *DB) CreateSkill(ctx context.Context, s *Skill) error {
	query := `INSERT INTO skills (name, content, enabled, category, sort_order) VALUES (?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, s.Name, s.Content, s.Enabled, s.Category, s.SortOrder)
	if err != nil {
		return err
	}
	s.ID, _ = result.LastInsertId()
	// Re-fetch to get DB-generated timestamps
	fresh, err := d.GetSkill(ctx, s.ID)
	if err == nil {
		*s = *fresh
	}
	return nil
}

func (d *DB) GetSkill(ctx context.Context, id int64) (*Skill, error) {
	var s Skill
	err := d.GetContext(ctx, &s, "SELECT * FROM skills WHERE id = ?", id)
	return &s, err
}

func (d *DB) ListSkills(ctx context.Context) ([]Skill, error) {
	var skills []Skill
	err := d.SelectContext(ctx, &skills, "SELECT * FROM skills ORDER BY sort_order, name")
	return skills, err
}

func (d *DB) ListEnabledSkills(ctx context.Context) ([]Skill, error) {
	var skills []Skill
	err := d.SelectContext(ctx, &skills, "SELECT * FROM skills WHERE enabled = 1 ORDER BY sort_order, name")
	return skills, err
}

func (d *DB) UpdateSkill(ctx context.Context, s *Skill) error {
	query := `UPDATE skills SET name=?, content=?, enabled=?, category=?, sort_order=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, s.Name, s.Content, s.Enabled, s.Category, s.SortOrder, time.Now(), s.ID)
	if err != nil {
		return err
	}
	// Re-fetch to get updated timestamps
	fresh, err := d.GetSkill(ctx, s.ID)
	if err == nil {
		*s = *fresh
	}
	return nil
}

func (d *DB) DeleteSkill(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM skills WHERE id = ?", id)
	return err
}

func (d *DB) IncrementSkillSyncCount(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE skills SET sync_count = sync_count + 1 WHERE id = ?", id)
	return err
}

// SyncedSkillFile operations
func (d *DB) ListSyncedSkillFiles(ctx context.Context, projectID int64) ([]SyncedSkillFile, error) {
	var files []SyncedSkillFile
	err := d.SelectContext(ctx, &files, "SELECT * FROM synced_skill_files WHERE project_id = ?", projectID)
	return files, err
}

func (d *DB) UpsertSyncedSkillFile(ctx context.Context, projectID int64, skillID int64, fileName string) error {
	query := `INSERT INTO synced_skill_files (project_id, skill_id, file_name, synced_at)
			  VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			  ON CONFLICT(id) DO UPDATE SET skill_id=excluded.skill_id, file_name=excluded.file_name, synced_at=CURRENT_TIMESTAMP`
	// Check if already exists for this project+skill
	var existing SyncedSkillFile
	err := d.GetContext(ctx, &existing, "SELECT * FROM synced_skill_files WHERE project_id = ? AND skill_id = ?", projectID, skillID)
	if err == nil {
		// Update existing
		_, err = d.ExecContext(ctx, "UPDATE synced_skill_files SET file_name=?, synced_at=CURRENT_TIMESTAMP WHERE id=?", fileName, existing.ID)
		return err
	}
	// Insert new
	_, err = d.ExecContext(ctx, query, projectID, skillID, fileName)
	return err
}

func (d *DB) DeleteSyncedSkillFile(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM synced_skill_files WHERE id = ?", id)
	return err
}

func (d *DB) DeleteSyncedSkillFilesForProject(ctx context.Context, projectID int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM synced_skill_files WHERE project_id = ?", projectID)
	return err
}

// SkillVersion operations
func (d *DB) CreateSkillVersion(ctx context.Context, v *SkillVersion) error {
	query := `INSERT INTO skill_versions (skill_id, name, content, version) VALUES (?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, v.SkillID, v.Name, v.Content, v.Version)
	if err != nil {
		return err
	}
	v.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) ListSkillVersions(ctx context.Context, skillID int64) ([]SkillVersion, error) {
	var versions []SkillVersion
	err := d.SelectContext(ctx, &versions, "SELECT * FROM skill_versions WHERE skill_id = ? ORDER BY version DESC", skillID)
	return versions, err
}

func (d *DB) GetNextSkillVersion(ctx context.Context, skillID int64) (int, error) {
	var maxVersion sql.NullInt64
	err := d.GetContext(ctx, &maxVersion, "SELECT MAX(version) FROM skill_versions WHERE skill_id = ?", skillID)
	if err != nil || !maxVersion.Valid {
		return 1, nil
	}
	return int(maxVersion.Int64) + 1, nil
}

func (d *DB) GetSkillVersion(ctx context.Context, id int64) (*SkillVersion, error) {
	var v SkillVersion
	err := d.GetContext(ctx, &v, "SELECT * FROM skill_versions WHERE id = ?", id)
	return &v, err
}

// MCP Server operations
func (d *DB) CreateMCPServer(ctx context.Context, m *MCPServer) error {
	query := `INSERT INTO mcp_servers (name, command, args, env, enabled) VALUES (?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, m.Name, m.Command, m.Args, m.Env, m.Enabled)
	if err != nil {
		return err
	}
	m.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetMCPServer(ctx context.Context, id int64) (*MCPServer, error) {
	var m MCPServer
	err := d.GetContext(ctx, &m, "SELECT * FROM mcp_servers WHERE id = ?", id)
	return &m, err
}

func (d *DB) ListMCPServers(ctx context.Context) ([]MCPServer, error) {
	var servers []MCPServer
	err := d.SelectContext(ctx, &servers, "SELECT * FROM mcp_servers ORDER BY name")
	return servers, err
}

func (d *DB) ListEnabledMCPServers(ctx context.Context) ([]MCPServer, error) {
	var servers []MCPServer
	err := d.SelectContext(ctx, &servers, "SELECT * FROM mcp_servers WHERE enabled = 1 ORDER BY name")
	return servers, err
}

func (d *DB) UpdateMCPServer(ctx context.Context, m *MCPServer) error {
	query := `UPDATE mcp_servers SET name=?, command=?, args=?, env=?, enabled=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, m.Name, m.Command, m.Args, m.Env, m.Enabled, time.Now(), m.ID)
	return err
}

func (d *DB) DeleteMCPServer(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM mcp_servers WHERE id = ?", id)
	return err
}

// Settings operations
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := d.GetContext(ctx, &value, "SELECT value FROM settings WHERE key = ?", key)
	return value, err
}

func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	query := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`
	_, err := d.ExecContext(ctx, query, key, value)
	return err
}

func (d *DB) GetAllSettings(ctx context.Context) (map[string]string, error) {
	var settings []Setting
	if err := d.SelectContext(ctx, &settings, "SELECT * FROM settings"); err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, s := range settings {
		result[s.Key] = s.Value
	}
	return result, nil
}

func (d *DB) DeleteSetting(ctx context.Context, key string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	return err
}

// Push Subscription operations
func (d *DB) CreatePushSubscription(ctx context.Context, sub *PushSubscription) error {
	query := `INSERT INTO push_subscriptions (endpoint, p256dh, auth) VALUES (?, ?, ?) ON CONFLICT(endpoint) DO UPDATE SET p256dh=excluded.p256dh, auth=excluded.auth`
	result, err := d.ExecContext(ctx, query, sub.Endpoint, sub.P256dh, sub.Auth)
	if err != nil {
		return err
	}
	sub.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	var subs []PushSubscription
	err := d.SelectContext(ctx, &subs, "SELECT * FROM push_subscriptions")
	return subs, err
}

func (d *DB) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	return err
}

// Notification operations
func (d *DB) CreateNotification(ctx context.Context, n *Notification) error {
	query := `INSERT INTO notifications (session_id, type, title, body) VALUES (?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, n.SessionID, n.Type, n.Title, n.Body)
	if err != nil {
		return err
	}
	n.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetNotification(ctx context.Context, id int64) (*Notification, error) {
	var n Notification
	err := d.GetContext(ctx, &n, "SELECT * FROM notifications WHERE id = ?", id)
	return &n, err
}

func (d *DB) ListNotifications(ctx context.Context, limit int) ([]Notification, error) {
	var notifications []Notification
	err := d.SelectContext(ctx, &notifications, "SELECT * FROM notifications ORDER BY created_at DESC LIMIT ?", limit)
	return notifications, err
}

func (d *DB) ListUnreadNotifications(ctx context.Context) ([]Notification, error) {
	var notifications []Notification
	err := d.SelectContext(ctx, &notifications, "SELECT * FROM notifications WHERE read = 0 ORDER BY created_at DESC")
	return notifications, err
}

func (d *DB) MarkNotificationRead(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE notifications SET read = 1 WHERE id = ?", id)
	return err
}

func (d *DB) MarkAllNotificationsRead(ctx context.Context) error {
	_, err := d.ExecContext(ctx, "UPDATE notifications SET read = 1")
	return err
}

func (d *DB) DeleteNotification(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM notifications WHERE id = ?", id)
	return err
}

// AI Conversation operations
func (d *DB) CreateAIConversation(ctx context.Context, c *AIConversation) error {
	if c.Source == "" {
		c.Source = "user"
	}
	if c.ProactiveContext == "" {
		c.ProactiveContext = "{}"
	}
	isRead := 1
	if c.Source == "ai" {
		isRead = 0
	}
	if c.IsRead {
		isRead = 1
	}
	query := `INSERT INTO ai_conversations (title, source, proactive_level, proactive_type, proactive_context, is_read) VALUES (?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, c.Title, c.Source, c.ProactiveLevel, c.ProactiveType, c.ProactiveContext, isRead)
	if err != nil {
		return err
	}
	c.ID, _ = result.LastInsertId()
	fresh, err := d.GetAIConversation(ctx, c.ID)
	if err == nil {
		*c = *fresh
	}
	return nil
}

func (d *DB) GetAIConversation(ctx context.Context, id int64) (*AIConversation, error) {
	var c AIConversation
	err := d.GetContext(ctx, &c, "SELECT * FROM ai_conversations WHERE id = ?", id)
	return &c, err
}

func (d *DB) ListAIConversations(ctx context.Context) ([]AIConversation, error) {
	var convs []AIConversation
	err := d.SelectContext(ctx, &convs, "SELECT * FROM ai_conversations ORDER BY updated_at DESC")
	return convs, err
}

func (d *DB) UpdateAIConversationTitle(ctx context.Context, id int64, title string) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_conversations SET title=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", title, id)
	return err
}

func (d *DB) TouchAIConversation(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_conversations SET updated_at=CURRENT_TIMESTAMP WHERE id=?", id)
	return err
}

func (d *DB) DeleteAIConversation(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM ai_conversations WHERE id = ?", id)
	return err
}

func (d *DB) DeleteAllAIConversations(ctx context.Context) error {
	_, err := d.ExecContext(ctx, "DELETE FROM ai_conversations")
	return err
}

// PruneOldAIConversations keeps only the most recent N conversations.
func (d *DB) PruneOldAIConversations(ctx context.Context, keep int) error {
	_, err := d.ExecContext(ctx, `DELETE FROM ai_conversations WHERE id NOT IN (
		SELECT id FROM ai_conversations ORDER BY updated_at DESC LIMIT ?
	)`, keep)
	return err
}

// AI Message operations
func (d *DB) CreateAIMessage(ctx context.Context, m *AIMessage) error {
	query := `INSERT INTO ai_messages (conversation_id, role, content, tool_calls) VALUES (?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, m.ConversationID, m.Role, m.Content, m.ToolCalls)
	if err != nil {
		return err
	}
	m.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) ListAIMessages(ctx context.Context, conversationID int64) ([]AIMessage, error) {
	var msgs []AIMessage
	err := d.SelectContext(ctx, &msgs, "SELECT * FROM ai_messages WHERE conversation_id = ? ORDER BY created_at ASC", conversationID)
	return msgs, err
}

// Temp Document operations
func (d *DB) CreateTempDocument(ctx context.Context, doc *TempDocument) error {
	query := `INSERT INTO temp_documents (id, title, content) VALUES (?, ?, ?)`
	_, err := d.ExecContext(ctx, query, doc.ID, doc.Title, doc.Content)
	return err
}

func (d *DB) GetTempDocument(ctx context.Context, id string) (*TempDocument, error) {
	var doc TempDocument
	err := d.GetContext(ctx, &doc, "SELECT * FROM temp_documents WHERE id = ?", id)
	return &doc, err
}

// Project Task operations
func (d *DB) CreateTask(ctx context.Context, t *ProjectTask) error {
	query := `INSERT INTO project_tasks (project_id, parent_id, title, description, status, priority, due_date, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, t.ProjectID, t.ParentID, t.Title, t.Description, t.Status, t.Priority, t.DueDate, t.SortOrder)
	if err != nil {
		return err
	}
	t.ID, _ = result.LastInsertId()
	fresh, err := d.GetTask(ctx, t.ID)
	if err == nil {
		*t = *fresh
	}
	return nil
}

func (d *DB) GetTask(ctx context.Context, id int64) (*ProjectTask, error) {
	var t ProjectTask
	err := d.GetContext(ctx, &t, "SELECT * FROM project_tasks WHERE id = ?", id)
	return &t, err
}

func (d *DB) ListTasksByProject(ctx context.Context, projectID int64) ([]ProjectTask, error) {
	var tasks []ProjectTask
	err := d.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE project_id = ? ORDER BY sort_order, created_at", projectID)
	return tasks, err
}

func (d *DB) UpdateTask(ctx context.Context, t *ProjectTask) error {
	query := `UPDATE project_tasks SET title=?, description=?, status=?, priority=?, due_date=?, sort_order=?, due_notified=?, parent_id=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, t.Title, t.Description, t.Status, t.Priority, t.DueDate, t.SortOrder, t.DueNotified, t.ParentID, time.Now(), t.ID)
	if err != nil {
		return err
	}
	fresh, err := d.GetTask(ctx, t.ID)
	if err == nil {
		*t = *fresh
	}
	return nil
}

func (d *DB) UpdateTaskStatus(ctx context.Context, id int64, status string) error {
	_, err := d.ExecContext(ctx, "UPDATE project_tasks SET status=?, updated_at=? WHERE id=?", status, time.Now(), id)
	return err
}

func (d *DB) DeleteTask(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM project_tasks WHERE id = ?", id)
	return err
}

func (d *DB) DuplicateTask(ctx context.Context, id int64) (*ProjectTask, error) {
	original, err := d.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}

	clone := &ProjectTask{
		ProjectID:   original.ProjectID,
		ParentID:    original.ParentID,
		Title:       original.Title + " (Copy)",
		Description: original.Description,
		Status:      "todo",
		Priority:    original.Priority,
		DueDate:     original.DueDate,
		SortOrder:   original.SortOrder,
	}
	if err := d.CreateTask(ctx, clone); err != nil {
		return nil, err
	}

	// Duplicate subtasks
	var children []ProjectTask
	if err := d.SelectContext(ctx, &children, "SELECT * FROM project_tasks WHERE parent_id = ?", id); err == nil {
		for _, child := range children {
			subClone := &ProjectTask{
				ProjectID:   child.ProjectID,
				ParentID:    sql.NullInt64{Int64: clone.ID, Valid: true},
				Title:       child.Title + " (Copy)",
				Description: child.Description,
				Status:      "todo",
				Priority:    child.Priority,
				DueDate:     child.DueDate,
				SortOrder:   child.SortOrder,
			}
			d.CreateTask(ctx, subClone)
		}
	}

	return clone, nil
}

func (d *DB) ListOverdueTasks(ctx context.Context) ([]ProjectTask, error) {
	var tasks []ProjectTask
	err := d.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE due_date < CURRENT_TIMESTAMP AND status != 'done' AND due_notified = 0 ORDER BY due_date")
	return tasks, err
}

func (d *DB) ListUpcomingTasks(ctx context.Context, within time.Duration) ([]ProjectTask, error) {
	var tasks []ProjectTask
	deadline := time.Now().Add(within)
	err := d.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE due_date > CURRENT_TIMESTAMP AND due_date <= ? AND status != 'done' AND due_notified = 0 ORDER BY due_date", deadline)
	return tasks, err
}

func (d *DB) MarkTaskDueNotified(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE project_tasks SET due_notified = 1 WHERE id = ?", id)
	return err
}

func (d *DB) GetTaskSummaryByProject(ctx context.Context, projectID int64) (map[string]int, error) {
	type statusCount struct {
		Status string `db:"status"`
		Count  int    `db:"count"`
	}
	var counts []statusCount
	err := d.SelectContext(ctx, &counts, "SELECT status, COUNT(*) as count FROM project_tasks WHERE project_id = ? GROUP BY status", projectID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	for _, c := range counts {
		result[c.Status] = c.Count
	}
	return result, nil
}

// SessionTask operations

func (d *DB) CreateSessionTask(ctx context.Context, st *SessionTask) error {
	query := `INSERT INTO session_tasks (session_id, task_id, role) VALUES (?, ?, ?)
	           ON CONFLICT(session_id, task_id, role) DO NOTHING`
	result, err := d.ExecContext(ctx, query, st.SessionID, st.TaskID, st.Role)
	if err != nil {
		return err
	}
	st.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetTaskForSession(ctx context.Context, sessionID string) (*ProjectTask, error) {
	var task ProjectTask
	err := d.GetContext(ctx, &task,
		`SELECT t.* FROM project_tasks t
		 INNER JOIN session_tasks st ON t.id = st.task_id
		 WHERE st.session_id = ? AND st.role IN ('created_from', 'registered_as')
		 ORDER BY st.created_at DESC LIMIT 1`, sessionID)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func (d *DB) GetSessionsForTask(ctx context.Context, taskID int64) ([]Session, error) {
	var sessions []Session
	err := d.SelectContext(ctx, &sessions,
		`SELECT s.* FROM sessions s
		 INNER JOIN session_tasks st ON s.id = st.session_id
		 WHERE st.task_id = ?
		 ORDER BY s.start_time DESC`, taskID)
	return sessions, err
}

func (d *DB) GetTaskSessionSummary(ctx context.Context, projectID int64) ([]struct {
	TaskID        int64  `db:"task_id" json:"task_id"`
	SessionCount  int    `db:"session_count" json:"session_count"`
	ActiveCount   int    `db:"active_count" json:"active_count"`
	LatestSession string `db:"latest_session" json:"latest_session"`
}, error) {
	type taskSessionInfo struct {
		TaskID        int64  `db:"task_id" json:"task_id"`
		SessionCount  int    `db:"session_count" json:"session_count"`
		ActiveCount   int    `db:"active_count" json:"active_count"`
		LatestSession string `db:"latest_session" json:"latest_session"`
	}
	var results []taskSessionInfo
	err := d.SelectContext(ctx, &results, `
		SELECT st.task_id,
		       COUNT(DISTINCT st.session_id) as session_count,
		       COUNT(DISTINCT CASE WHEN s.status IN ('running', 'starting') THEN st.session_id END) as active_count,
		       COALESCE(MAX(s.id), '') as latest_session
		FROM session_tasks st
		INNER JOIN sessions s ON s.id = st.session_id
		INNER JOIN project_tasks t ON t.id = st.task_id
		WHERE t.project_id = ?
		GROUP BY st.task_id`, projectID)
	if err != nil {
		return nil, err
	}
	// Convert to the return type
	out := make([]struct {
		TaskID        int64  `db:"task_id" json:"task_id"`
		SessionCount  int    `db:"session_count" json:"session_count"`
		ActiveCount   int    `db:"active_count" json:"active_count"`
		LatestSession string `db:"latest_session" json:"latest_session"`
	}, len(results))
	for i, r := range results {
		out[i].TaskID = r.TaskID
		out[i].SessionCount = r.SessionCount
		out[i].ActiveCount = r.ActiveCount
		out[i].LatestSession = r.LatestSession
	}
	return out, nil
}

// AISuggestion operations

func (d *DB) CreateAISuggestion(ctx context.Context, s *AISuggestion) error {
	if s.Level == "" {
		s.Level = "standard"
	}
	query := `INSERT INTO ai_suggestions (session_id, project_id, type, title, description, context_json, status, level, conversation_id)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, s.SessionID, s.ProjectID, s.Type, s.Title, s.Description, s.ContextJSON, s.Status, s.Level, s.ConversationID)
	if err != nil {
		return err
	}
	s.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetAISuggestion(ctx context.Context, id int64) (*AISuggestion, error) {
	var s AISuggestion
	err := d.GetContext(ctx, &s, "SELECT * FROM ai_suggestions WHERE id = ?", id)
	return &s, err
}

func (d *DB) UpdateAISuggestionStatus(ctx context.Context, id int64, status string) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_suggestions SET status = ? WHERE id = ?", status, id)
	return err
}

func (d *DB) ListPendingAISuggestions(ctx context.Context) ([]AISuggestion, error) {
	var suggestions []AISuggestion
	err := d.SelectContext(ctx, &suggestions,
		"SELECT * FROM ai_suggestions WHERE status = 'pending' ORDER BY created_at DESC")
	return suggestions, err
}

// UpdateAISuggestionConversation links a suggestion to a conversation.
func (d *DB) UpdateAISuggestionConversation(ctx context.Context, id int64, conversationID int64) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_suggestions SET conversation_id = ? WHERE id = ?", conversationID, id)
	return err
}

// CreateProactiveConversation creates an AI-initiated conversation with an initial assistant message.
func (d *DB) CreateProactiveConversation(ctx context.Context, title, level, pType, contextJSON, assistantMsg string) (*AIConversation, error) {
	conv := &AIConversation{
		Title:            title,
		Source:           "ai",
		ProactiveLevel:   level,
		ProactiveType:    pType,
		ProactiveContext: contextJSON,
	}
	if err := d.CreateAIConversation(ctx, conv); err != nil {
		return nil, err
	}

	msg := &AIMessage{
		ConversationID: conv.ID,
		Role:           "assistant",
		Content:        assistantMsg,
		ToolCalls:      "[]",
	}
	if err := d.CreateAIMessage(ctx, msg); err != nil {
		return nil, err
	}

	return conv, nil
}

// MarkConversationRead marks an AI conversation as read.
func (d *DB) MarkConversationRead(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_conversations SET is_read = 1 WHERE id = ?", id)
	return err
}

// CountUnreadProactive returns the number of unread AI-initiated conversations.
func (d *DB) CountUnreadProactive(ctx context.Context) (int, error) {
	var count int
	err := d.GetContext(ctx, &count, "SELECT COUNT(*) FROM ai_conversations WHERE source = 'ai' AND is_read = 0")
	return count, err
}

// Project Meta Document operations
func (d *DB) GetProjectMeta(ctx context.Context, projectID int64) (*ProjectMetaDocument, error) {
	var doc ProjectMetaDocument
	err := d.GetContext(ctx, &doc, "SELECT * FROM project_meta_documents WHERE project_id = ?", projectID)
	return &doc, err
}

// Token usage operations

// ClearTokenUsage deletes all token usage records.
func (d *DB) ClearTokenUsage(ctx context.Context) (int64, error) {
	result, err := d.ExecContext(ctx, `DELETE FROM token_usage`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (d *DB) CreateTokenUsage(ctx context.Context, t *TokenUsage) error {
	query := `INSERT INTO token_usage (source, subcategory, project_id, session_id, conversation_id, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query,
		t.Source, t.Subcategory, t.ProjectID, t.SessionID, t.ConversationID,
		t.Model, t.InputTokens, t.OutputTokens,
		t.CacheReadTokens, t.CacheCreationTokens, t.CostUSD,
	)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	t.ID = id
	return nil
}

// TokenUsageSummary holds aggregated token usage data.
type TokenUsageSummary struct {
	TotalInputTokens  int64   `db:"total_input_tokens" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output_tokens" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost_usd" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
}

// TokenUsageBySource holds per-source summary.
type TokenUsageBySource struct {
	Source            string  `db:"source" json:"source"`
	TotalInputTokens  int64  `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64  `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64  `db:"total_requests" json:"total_requests"`
}

// TokenUsageByModel holds per-model summary (aggregated across all sources).
type TokenUsageByModel struct {
	Model             string  `db:"model" json:"model"`
	TotalInputTokens  int64  `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64  `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64  `db:"total_requests" json:"total_requests"`
}

// TokenUsageDaily holds daily aggregated data.
type TokenUsageDaily struct {
	Date              string  `db:"date" json:"date"`
	TotalInputTokens  int64  `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64  `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64  `db:"total_requests" json:"total_requests"`
}

// TokenUsageByProject holds per-project summary.
type TokenUsageByProject struct {
	ProjectID         int64   `db:"project_id" json:"project_id"`
	ProjectName       string  `db:"project_name" json:"project_name"`
	TotalInputTokens  int64  `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64  `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64  `db:"total_requests" json:"total_requests"`
}

// TokenUsageByProjectModel holds per-project per-model breakdown.
type TokenUsageByProjectModel struct {
	ProjectID         int64   `db:"project_id" json:"project_id"`
	ProjectName       string  `db:"project_name" json:"project_name"`
	Model             string  `db:"model" json:"model"`
	TotalInputTokens  int64  `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64  `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64  `db:"total_requests" json:"total_requests"`
}

// GetTokenUsageSummary returns aggregated token usage, optionally filtered by project.
func (d *DB) GetTokenUsageSummary(ctx context.Context, since time.Time, projectID *int64) (*TokenUsageSummary, error) {
	var summary TokenUsageSummary
	query := `SELECT COALESCE(SUM(input_tokens),0) as total_input_tokens,
	                 COALESCE(SUM(output_tokens),0) as total_output_tokens,
	                 COALESCE(SUM(cost_usd),0) as total_cost_usd,
	                 COUNT(*) as total_requests
	          FROM token_usage WHERE created_at >= ?`
	args := []interface{}{since}
	if projectID != nil {
		query += ` AND project_id = ?`
		args = append(args, *projectID)
	}
	err := d.GetContext(ctx, &summary, query, args...)
	return &summary, err
}

// GetTokenUsageBySource returns per-source breakdown.
func (d *DB) GetTokenUsageBySource(ctx context.Context, since time.Time, projectID *int64) ([]TokenUsageBySource, error) {
	query := `SELECT source,
	                 COALESCE(SUM(input_tokens),0) as total_input,
	                 COALESCE(SUM(output_tokens),0) as total_output,
	                 COALESCE(SUM(cost_usd),0) as total_cost,
	                 COUNT(*) as total_requests
	          FROM token_usage WHERE created_at >= ?`
	args := []interface{}{since}
	if projectID != nil {
		query += ` AND project_id = ?`
		args = append(args, *projectID)
	}
	query += ` GROUP BY source`
	var results []TokenUsageBySource
	err := d.SelectContext(ctx, &results, query, args...)
	return results, err
}

// GetTokenUsageByModel returns per-model breakdown (aggregated across all sources).
func (d *DB) GetTokenUsageByModel(ctx context.Context, since time.Time, projectID *int64) ([]TokenUsageByModel, error) {
	query := `SELECT model,
	                 COALESCE(SUM(input_tokens),0) as total_input,
	                 COALESCE(SUM(output_tokens),0) as total_output,
	                 COALESCE(SUM(cost_usd),0) as total_cost,
	                 COUNT(*) as total_requests
	          FROM token_usage WHERE created_at >= ?`
	args := []interface{}{since}
	if projectID != nil {
		query += ` AND project_id = ?`
		args = append(args, *projectID)
	}
	query += ` GROUP BY model ORDER BY total_cost DESC`
	var results []TokenUsageByModel
	err := d.SelectContext(ctx, &results, query, args...)
	return results, err
}

// GetTokenUsageDaily returns daily aggregated data.
func (d *DB) GetTokenUsageDaily(ctx context.Context, since time.Time, projectID *int64) ([]TokenUsageDaily, error) {
	query := `SELECT DATE(created_at) as date,
	                 COALESCE(SUM(input_tokens),0) as total_input,
	                 COALESCE(SUM(output_tokens),0) as total_output,
	                 COALESCE(SUM(cost_usd),0) as total_cost,
	                 COUNT(*) as total_requests
	          FROM token_usage WHERE created_at >= ?`
	args := []interface{}{since}
	if projectID != nil {
		query += ` AND project_id = ?`
		args = append(args, *projectID)
	}
	query += ` GROUP BY DATE(created_at) ORDER BY date DESC`
	var results []TokenUsageDaily
	err := d.SelectContext(ctx, &results, query, args...)
	return results, err
}

// GetTokenUsageByProject returns per-project token usage summary.
func (d *DB) GetTokenUsageByProject(ctx context.Context, since time.Time) ([]TokenUsageByProject, error) {
	query := `SELECT t.project_id, COALESCE(p.name, 'Unknown') as project_name,
	                 COALESCE(SUM(t.input_tokens),0) as total_input,
	                 COALESCE(SUM(t.output_tokens),0) as total_output,
	                 COALESCE(SUM(t.cost_usd),0) as total_cost,
	                 COUNT(*) as total_requests
	          FROM token_usage t
	          LEFT JOIN projects p ON t.project_id = p.id
	          WHERE t.created_at >= ? AND t.project_id IS NOT NULL
	          GROUP BY t.project_id
	          ORDER BY total_cost DESC`
	var results []TokenUsageByProject
	err := d.SelectContext(ctx, &results, query, since)
	return results, err
}

// GetTokenUsageByProjectModel returns per-project per-model token usage breakdown.
func (d *DB) GetTokenUsageByProjectModel(ctx context.Context, since time.Time) ([]TokenUsageByProjectModel, error) {
	query := `SELECT t.project_id, COALESCE(p.name, 'Unknown') as project_name,
	                 t.model,
	                 COALESCE(SUM(t.input_tokens),0) as total_input,
	                 COALESCE(SUM(t.output_tokens),0) as total_output,
	                 COALESCE(SUM(t.cost_usd),0) as total_cost,
	                 COUNT(*) as total_requests
	          FROM token_usage t
	          LEFT JOIN projects p ON t.project_id = p.id
	          WHERE t.created_at >= ? AND t.project_id IS NOT NULL
	          GROUP BY t.project_id, t.model
	          ORDER BY t.project_id, total_cost DESC`
	var results []TokenUsageByProjectModel
	err := d.SelectContext(ctx, &results, query, since)
	return results, err
}

// TokenUsageBySubcategory holds per-subcategory summary for AI assistant.
type TokenUsageBySubcategory struct {
	Subcategory       string  `db:"subcategory" json:"subcategory"`
	Model             string  `db:"model" json:"model"`
	TotalInputTokens  int64   `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
}

// GetTokenUsageByAISubcategory returns AI assistant usage grouped by subcategory and model.
func (d *DB) GetTokenUsageByAISubcategory(ctx context.Context, since time.Time) ([]TokenUsageBySubcategory, error) {
	query := `SELECT subcategory, model,
	                 COALESCE(SUM(input_tokens),0) as total_input,
	                 COALESCE(SUM(output_tokens),0) as total_output,
	                 COALESCE(SUM(cost_usd),0) as total_cost,
	                 COUNT(*) as total_requests
	          FROM token_usage
	          WHERE created_at >= ? AND source = 'ai_assistant'
	          GROUP BY subcategory, model
	          ORDER BY total_cost DESC`
	var results []TokenUsageBySubcategory
	err := d.SelectContext(ctx, &results, query, since)
	return results, err
}

func (d *DB) UpsertProjectMeta(ctx context.Context, projectID int64, content, updatedBy string, summary string) (*ProjectMetaDocument, error) {
	query := `INSERT INTO project_meta_documents (project_id, content, version, last_updated_by, summary)
			  VALUES (?, ?, 1, ?, ?)
			  ON CONFLICT(project_id) DO UPDATE SET
			    content=excluded.content,
			    version=version+1,
			    last_updated_by=excluded.last_updated_by,
			    summary=excluded.summary,
			    updated_at=CURRENT_TIMESTAMP`
	_, err := d.ExecContext(ctx, query, projectID, content, updatedBy, sql.NullString{String: summary, Valid: summary != ""})
	if err != nil {
		return nil, err
	}
	return d.GetProjectMeta(ctx, projectID)
}
