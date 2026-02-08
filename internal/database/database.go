package database

import (
	"context"
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
	query := `INSERT INTO sessions (id, project_id, status, pid, start_time) VALUES (?, ?, ?, ?, ?)`
	_, err := d.ExecContext(ctx, query, s.ID, s.ProjectID, s.Status, s.PID, s.StartTime)
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
	query := `INSERT INTO skills (name, content, enabled) VALUES (?, ?, ?)`
	result, err := d.ExecContext(ctx, query, s.Name, s.Content, s.Enabled)
	if err != nil {
		return err
	}
	s.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetSkill(ctx context.Context, id int64) (*Skill, error) {
	var s Skill
	err := d.GetContext(ctx, &s, "SELECT * FROM skills WHERE id = ?", id)
	return &s, err
}

func (d *DB) ListSkills(ctx context.Context) ([]Skill, error) {
	var skills []Skill
	err := d.SelectContext(ctx, &skills, "SELECT * FROM skills ORDER BY name")
	return skills, err
}

func (d *DB) ListEnabledSkills(ctx context.Context) ([]Skill, error) {
	var skills []Skill
	err := d.SelectContext(ctx, &skills, "SELECT * FROM skills WHERE enabled = 1 ORDER BY name")
	return skills, err
}

func (d *DB) UpdateSkill(ctx context.Context, s *Skill) error {
	query := `UPDATE skills SET name=?, content=?, enabled=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, s.Name, s.Content, s.Enabled, time.Now(), s.ID)
	return err
}

func (d *DB) DeleteSkill(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM skills WHERE id = ?", id)
	return err
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
