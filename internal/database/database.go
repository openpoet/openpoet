package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type DB struct {
	*sqlx.DB
}

const (
	DefaultCodexTranscriptRetentionDays = 14
	DefaultCodexTranscriptMaxEvents     = 10000
	DefaultMCPHTTPSessionRetentionDays  = 7
	DefaultMCPHTTPEventRetentionDays    = 14
	DefaultMCPHTTPMaxEventsPerSession   = 1000
)

type CodexTranscriptCleanupStats struct {
	ExpiredDeleted  int64
	OverflowDeleted int64
}

type MCPHTTPCleanupStats struct {
	SessionsDeleted int64
	ExpiredDeleted  int64
	OverflowDeleted int64
}

func New(path string) (*DB, error) {
	db, err := sqlx.Connect("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
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

// OpenExisting opens an already-migrated database without applying schema
// migrations. Offline dry-run/cutover tools use it so inspection itself cannot
// change production schema.
func OpenExisting(path string) (*DB, error) {
	db, err := sqlx.Connect("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, fmt.Errorf("failed to connect to existing database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &DB{DB: db}, nil
}

func (d *DB) migrate() error {
	return RunMigrations(d.DB)
}

// Project operations
func (d *DB) CreateProject(ctx context.Context, p *Project) error {
	p.TaskAutoApproveVerification = normalizeTaskAutoApproveVerification(p.TaskAutoApproveVerification)
	query := `INSERT INTO projects (name, path, type, ssh_host, ssh_port, ssh_user, ssh_auth_type, ssh_credential_encrypted, ssh_credential_iv, tool_policy, skill_policy, backend, backend_config, task_auto_approve_verification)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, p.Name, p.Path, p.Type, p.SSHHost, p.SSHPort, p.SSHUser, p.SSHAuthType, p.SSHCredentialEncrypted, p.SSHCredentialIV, p.ToolPolicy, p.SkillPolicy, p.Backend, p.BackendConfig, p.TaskAutoApproveVerification)
	if err != nil {
		return err
	}
	p.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetProject(ctx context.Context, id int64) (*Project, error) {
	var p Project
	err := d.GetContext(ctx, &p, "SELECT * FROM projects WHERE id = ?", id)
	p.HasCredential = p.SSHCredentialEncrypted.Valid && p.SSHCredentialEncrypted.String != ""
	return &p, err
}

func (d *DB) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	var p Project
	err := d.GetContext(ctx, &p, "SELECT * FROM projects WHERE name = ?", name)
	p.HasCredential = p.SSHCredentialEncrypted.Valid && p.SSHCredentialEncrypted.String != ""
	return &p, err
}

func (d *DB) ListProjects(ctx context.Context) ([]Project, error) {
	var projects []Project
	err := d.SelectContext(ctx, &projects, `
		SELECT p.* FROM projects p
		LEFT JOIN (
			SELECT project_id, MAX(COALESCE(last_activity_at, start_time)) AS latest_activity
			FROM sessions
			GROUP BY project_id
		) s ON s.project_id = p.id
		ORDER BY s.latest_activity IS NULL, s.latest_activity DESC, p.name`)
	for i := range projects {
		projects[i].HasCredential = projects[i].SSHCredentialEncrypted.Valid && projects[i].SSHCredentialEncrypted.String != ""
	}
	return projects, err
}

func (d *DB) UpdateProject(ctx context.Context, p *Project) error {
	p.TaskAutoApproveVerification = normalizeTaskAutoApproveVerification(p.TaskAutoApproveVerification)
	query := `UPDATE projects SET name=?, path=?, type=?, ssh_host=?, ssh_port=?, ssh_user=?, ssh_auth_type=?, ssh_credential_encrypted=?, ssh_credential_iv=?, tool_policy=?, skill_policy=?, dangerously_skip_permissions=?, backend=?, backend_config=?, task_auto_approve_verification=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, p.Name, p.Path, p.Type, p.SSHHost, p.SSHPort, p.SSHUser, p.SSHAuthType, p.SSHCredentialEncrypted, p.SSHCredentialIV, p.ToolPolicy, p.SkillPolicy, p.DangerouslySkipPermissions, p.Backend, p.BackendConfig, p.TaskAutoApproveVerification, time.Now(), p.ID)
	return err
}

func normalizeTaskAutoApproveVerification(value string) string {
	switch strings.TrimSpace(value) {
	case "enabled", "disabled":
		return strings.TrimSpace(value)
	default:
		return "inherit"
	}
}

func (d *DB) UpdateProjectConfigSyncedAt(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE projects SET config_synced_at=? WHERE id=?", time.Now(), id)
	return err
}

func (d *DB) DeleteProject(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM projects WHERE id = ?", id)
	return err
}

// Project share operations

func (d *DB) ListProjectShares(ctx context.Context, projectID int64) ([]ProjectShare, error) {
	var shares []ProjectShare
	err := d.SelectContext(ctx, &shares,
		"SELECT * FROM project_shares WHERE project_id = ? ORDER BY created_at", projectID)
	return shares, err
}

func (d *DB) ReplaceProjectShares(ctx context.Context, projectID int64, sharedProjectIDs []int64) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM project_shares WHERE project_id = ?", projectID); err != nil {
		return err
	}

	for _, sharedID := range sharedProjectIDs {
		if sharedID == projectID {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO project_shares (project_id, shared_project_id) VALUES (?, ?)",
			projectID, sharedID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (d *DB) HasProjectShare(ctx context.Context, projectID, sharedProjectID int64) (bool, error) {
	var count int
	err := d.GetContext(ctx, &count,
		"SELECT COUNT(*) FROM project_shares WHERE project_id = ? AND shared_project_id = ?",
		projectID, sharedProjectID)
	return count > 0, err
}

// Tag operations (global)

func (d *DB) ListTags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	err := d.SelectContext(ctx, &tags, "SELECT * FROM tags ORDER BY name")
	return tags, err
}

func (d *DB) GetTag(ctx context.Context, id int64) (*Tag, error) {
	var tag Tag
	err := d.GetContext(ctx, &tag, "SELECT * FROM tags WHERE id = ?", id)
	return &tag, err
}

func (d *DB) CreateTag(ctx context.Context, tag *Tag) error {
	result, err := d.ExecContext(ctx,
		"INSERT INTO tags (name, color) VALUES (?, ?)", tag.Name, tag.Color)
	if err != nil {
		return err
	}
	tag.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) UpdateTag(ctx context.Context, tag *Tag) error {
	_, err := d.ExecContext(ctx,
		"UPDATE tags SET name = ?, color = ? WHERE id = ?", tag.Name, tag.Color, tag.ID)
	return err
}

func (d *DB) DeleteTag(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM tags WHERE id = ?", id)
	return err
}

// Project tag assignment operations

type ProjectTagWithDetails struct {
	TagID int64  `db:"tag_id" json:"tag_id"`
	Name  string `db:"name" json:"name"`
	Color string `db:"color" json:"color"`
}

func (d *DB) ListProjectTagDetails(ctx context.Context, projectID int64) ([]ProjectTagWithDetails, error) {
	var tags []ProjectTagWithDetails
	err := d.SelectContext(ctx, &tags,
		`SELECT t.id as tag_id, t.name, t.color FROM project_tags pt
		 JOIN tags t ON pt.tag_id = t.id
		 WHERE pt.project_id = ? ORDER BY t.name`, projectID)
	return tags, err
}

func (d *DB) ListAllProjectTagDetails(ctx context.Context) ([]struct {
	ProjectID int64  `db:"project_id"`
	TagID     int64  `db:"tag_id"`
	Name      string `db:"name"`
	Color     string `db:"color"`
}, error) {
	var tags []struct {
		ProjectID int64  `db:"project_id"`
		TagID     int64  `db:"tag_id"`
		Name      string `db:"name"`
		Color     string `db:"color"`
	}
	err := d.SelectContext(ctx, &tags,
		`SELECT pt.project_id, t.id as tag_id, t.name, t.color FROM project_tags pt
		 JOIN tags t ON pt.tag_id = t.id ORDER BY t.name`)
	return tags, err
}

func (d *DB) ReplaceProjectTagIDs(ctx context.Context, projectID int64, tagIDs []int64) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM project_tags WHERE project_id = ?", projectID); err != nil {
		return err
	}

	for _, tagID := range tagIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO project_tags (project_id, tag_id) VALUES (?, ?)",
			projectID, tagID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// Session operations
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	query := `INSERT INTO sessions (id, project_id, status, pid, name, task_id, start_time, backend, skip_permissions, model, requested_model, effort, harness) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.ExecContext(ctx, query, s.ID, s.ProjectID, s.Status, s.PID, s.Name, s.TaskID, s.StartTime, s.Backend, s.SkipPermissions, s.Model, s.RequestedModel, s.Effort, s.Harness)
	return err
}

func (d *DB) UpdateSessionRuntimeMetadata(ctx context.Context, id, model, effort, harness string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET model=?, effort=?, harness=? WHERE id=?", model, effort, harness, id)
	return err
}

func (d *DB) UpdateSessionRequestedModel(ctx context.Context, id, requestedModel string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET requested_model=? WHERE id=?", requestedModel, id)
	return err
}

func (d *DB) UpdateSessionEffectiveModel(ctx context.Context, id, effectiveModel string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET model=? WHERE id=?", effectiveModel, id)
	return err
}

func (d *DB) UpdateSessionSkipPermissions(ctx context.Context, id string, skipPermissions bool) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET skip_permissions=? WHERE id=?", skipPermissions, id)
	return err
}

func (d *DB) UpdateSessionProviderSessionID(ctx context.Context, id string, providerSessionID string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET provider_session_id=? WHERE id=?", providerSessionID, id)
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
	// Per-session credentials die with the session; Reopen mints fresh ones.
	_, err := d.ExecContext(ctx, "UPDATE sessions SET status=?, end_time=?, mcp_token_hash=NULL, hook_token_hash=NULL WHERE id=?", status, time.Now(), id)
	return err
}

// UpdateSessionTokenHashes stores the SHA-256 hex digests of a session's
// MCP/REST bearer and hook bridge tokens. Empty strings clear a hash.
func (d *DB) UpdateSessionTokenHashes(ctx context.Context, id, mcpTokenHash, hookTokenHash string) error {
	_, err := d.ExecContext(ctx,
		"UPDATE sessions SET mcp_token_hash=NULLIF(?, ''), hook_token_hash=NULLIF(?, '') WHERE id=?",
		mcpTokenHash, hookTokenHash, id)
	return err
}

// GetSessionTokenHashes returns the stored MCP/REST and hook token hex digests
// for a session. Empty strings mean the credential is unset (legacy session);
// sql.ErrNoRows means the session id is unknown.
func (d *DB) GetSessionTokenHashes(ctx context.Context, id string) (mcpTokenHash, hookTokenHash string, err error) {
	var row struct {
		Mcp  sql.NullString `db:"mcp_token_hash"`
		Hook sql.NullString `db:"hook_token_hash"`
	}
	err = d.GetContext(ctx, &row, "SELECT mcp_token_hash, hook_token_hash FROM sessions WHERE id = ?", id)
	if err != nil {
		return "", "", err
	}
	return row.Mcp.String, row.Hook.String, nil
}

func (d *DB) TouchSessionActivity(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET last_activity_at=? WHERE id=?", time.Now(), id)
	return err
}

func (d *DB) UpdateSessionPlan(ctx context.Context, id string, planContent string) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET plan_content=?, plan_updated_at=? WHERE id=?", planContent, time.Now(), id)
	return err
}

func (d *DB) GetSessionPlan(ctx context.Context, id string) (string, *time.Time, error) {
	var result struct {
		PlanContent   string       `db:"plan_content"`
		PlanUpdatedAt sql.NullTime `db:"plan_updated_at"`
	}
	err := d.GetContext(ctx, &result, "SELECT plan_content, plan_updated_at FROM sessions WHERE id = ?", id)
	if err != nil {
		return "", nil, err
	}
	var updatedAt *time.Time
	if result.PlanUpdatedAt.Valid {
		updatedAt = &result.PlanUpdatedAt.Time
	}
	return result.PlanContent, updatedAt, nil
}

func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	return err
}

func (d *DB) UpsertMCPHTTPSession(ctx context.Context, s *MCPHTTPSession) error {
	if s == nil || strings.TrimSpace(s.ID) == "" {
		return nil
	}
	if s.Context == "" {
		s.Context = "http"
	}
	if s.Status == "" {
		s.Status = "active"
	}
	now := time.Now()
	_, err := d.ExecContext(ctx, `
		INSERT INTO mcp_http_sessions
			(id, openpoet_session_id, context, status, initialized_at, last_used_at, request_count, last_method)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			openpoet_session_id = excluded.openpoet_session_id,
			context = excluded.context,
			status = excluded.status,
			last_used_at = excluded.last_used_at,
			closed_at = CASE WHEN excluded.status = 'active' THEN NULL ELSE mcp_http_sessions.closed_at END,
			last_method = excluded.last_method`,
		s.ID, s.OpenPoetSessionID, s.Context, s.Status, now, now, s.RequestCount, s.LastMethod)
	return err
}

func (d *DB) GetMCPHTTPSession(ctx context.Context, id string) (*MCPHTTPSession, error) {
	var s MCPHTTPSession
	err := d.GetContext(ctx, &s, "SELECT * FROM mcp_http_sessions WHERE id = ?", id)
	return &s, err
}

func (d *DB) TouchMCPHTTPSession(ctx context.Context, id, method string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE mcp_http_sessions
		 SET last_used_at = ?, request_count = request_count + 1, last_method = ?
		 WHERE id = ?`,
		time.Now(), method, id)
	return err
}

func (d *DB) CloseMCPHTTPSession(ctx context.Context, id, status string) error {
	if status == "" {
		status = "closed"
	}
	_, err := d.ExecContext(ctx,
		`UPDATE mcp_http_sessions
		 SET status = ?, closed_at = ?, last_used_at = ?
		 WHERE id = ?`,
		status, time.Now(), time.Now(), id)
	return err
}

func (d *DB) ExpireStaleMCPHTTPSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE mcp_http_sessions
		 SET status = 'expired', closed_at = COALESCE(closed_at, ?)
		 WHERE status = 'active' AND last_used_at < ?`,
		time.Now(), before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) CleanupMCPHTTPSessions(ctx context.Context, closedBefore time.Time, eventBefore time.Time, maxEventsPerSession int) (MCPHTTPCleanupStats, error) {
	var stats MCPHTTPCleanupStats
	if maxEventsPerSession <= 0 {
		maxEventsPerSession = DefaultMCPHTTPMaxEventsPerSession
	}

	res, err := d.ExecContext(ctx,
		`DELETE FROM mcp_http_session_events
		 WHERE created_at < ?`,
		eventBefore)
	if err != nil {
		return stats, err
	}
	stats.ExpiredDeleted, _ = res.RowsAffected()

	res, err = d.ExecContext(ctx, `
		DELETE FROM mcp_http_session_events
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
				       ROW_NUMBER() OVER (PARTITION BY mcp_session_id ORDER BY id DESC) AS rn
				FROM mcp_http_session_events
			)
			WHERE rn > ?
		)`, maxEventsPerSession)
	if err != nil {
		return stats, err
	}
	stats.OverflowDeleted, _ = res.RowsAffected()

	res, err = d.ExecContext(ctx,
		`DELETE FROM mcp_http_sessions
		 WHERE status IN ('closed', 'expired')
		   AND COALESCE(closed_at, last_used_at, initialized_at) < ?`,
		closedBefore)
	if err != nil {
		return stats, err
	}
	stats.SessionsDeleted, _ = res.RowsAffected()
	return stats, nil
}

func (d *DB) CreateMCPHTTPSessionEvent(ctx context.Context, e *MCPHTTPSessionEvent) error {
	if e == nil || strings.TrimSpace(e.MCPSessionID) == "" {
		return nil
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO mcp_http_session_events
			(mcp_session_id, openpoet_session_id, method, event_type, status, error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.MCPSessionID, e.OpenPoetSessionID, e.Method, e.EventType, e.Status, e.Error)
	return err
}

func (d *DB) ListMCPHTTPSessionEvents(ctx context.Context, sessionID string, limit int) ([]MCPHTTPSessionEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var events []MCPHTTPSessionEvent
	err := d.SelectContext(ctx, &events,
		`SELECT * FROM mcp_http_session_events
		 WHERE mcp_session_id = ?
		 ORDER BY id DESC LIMIT ?`,
		sessionID, limit)
	return events, err
}

func (d *DB) InsertCodexTranscriptEvent(ctx context.Context, event *CodexTranscriptEvent) error {
	if event == nil || event.SessionID == "" || event.EventID <= 0 {
		return nil
	}
	if event.Kind == "" {
		event.Kind = "status"
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO codex_transcript_events
			(session_id, event_id, kind, text, title, command, status, append, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.SessionID,
		event.EventID,
		event.Kind,
		event.Text,
		event.Title,
		event.Command,
		event.Status,
		event.Append,
		event.CreatedAt,
	)
	return err
}

func (d *DB) ClearCodexTranscriptEvents(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := d.ExecContext(ctx, "DELETE FROM codex_transcript_events WHERE session_id = ?", sessionID)
	return err
}

func (d *DB) ListCodexTranscriptEvents(ctx context.Context, sessionID string, limit int) ([]CodexTranscriptEvent, error) {
	if limit <= 0 {
		limit = 4000
	}
	var events []CodexTranscriptEvent
	err := d.SelectContext(ctx, &events, `
		SELECT * FROM (
			SELECT * FROM codex_transcript_events
			WHERE session_id = ?
			ORDER BY id DESC
			LIMIT ?
	) ORDER BY id ASC`, sessionID, limit)
	return events, err
}

func (d *DB) CleanupCodexTranscriptEvents(ctx context.Context, retentionDays, maxEventsPerSession int) (CodexTranscriptCleanupStats, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultCodexTranscriptRetentionDays
	}
	if maxEventsPerSession <= 0 {
		maxEventsPerSession = DefaultCodexTranscriptMaxEvents
	}

	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	expiredResult, err := d.ExecContext(ctx, `
		DELETE FROM codex_transcript_events
		WHERE session_id IN (
			SELECT id FROM sessions
			WHERE status IN ('stopped', 'completed', 'error')
			  AND COALESCE(end_time, start_time) < ?
		)`, cutoff)
	if err != nil {
		return CodexTranscriptCleanupStats{}, err
	}
	expiredDeleted, _ := expiredResult.RowsAffected()

	overflowResult, err := d.ExecContext(ctx, `
		DELETE FROM codex_transcript_events
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
				       ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY id DESC) AS rn
				FROM codex_transcript_events
			)
			WHERE rn > ?
		)`, maxEventsPerSession)
	if err != nil {
		return CodexTranscriptCleanupStats{}, err
	}
	overflowDeleted, _ := overflowResult.RowsAffected()

	return CodexTranscriptCleanupStats{
		ExpiredDeleted:  expiredDeleted,
		OverflowDeleted: overflowDeleted,
	}, nil
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

// ProjectSkillConfig operations (per-project overrides for global skills)
func (d *DB) ListProjectSkillConfigs(ctx context.Context, projectID int64) ([]ProjectSkillConfig, error) {
	var configs []ProjectSkillConfig
	err := d.SelectContext(ctx, &configs, "SELECT * FROM project_skill_config WHERE project_id = ?", projectID)
	return configs, err
}

func (d *DB) UpsertProjectSkillConfig(ctx context.Context, projectID, skillID int64, enabled bool) error {
	query := `INSERT INTO project_skill_config (project_id, skill_id, enabled) VALUES (?, ?, ?)
			  ON CONFLICT(project_id, skill_id) DO UPDATE SET enabled = excluded.enabled`
	_, err := d.ExecContext(ctx, query, projectID, skillID, enabled)
	return err
}

func (d *DB) DeleteProjectSkillConfigs(ctx context.Context, projectID int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM project_skill_config WHERE project_id = ?", projectID)
	return err
}

// ListEnabledSkillsForProject returns global skills that are enabled for a specific project (custom policy).
// Uses project_skill_config overrides: if a config row exists, use its enabled value; otherwise the skill is excluded.
func (d *DB) ListEnabledSkillsForProject(ctx context.Context, projectID int64) ([]Skill, error) {
	var skills []Skill
	query := `SELECT s.* FROM skills s
			  INNER JOIN project_skill_config psc ON psc.skill_id = s.id AND psc.project_id = ?
			  WHERE psc.enabled = 1
			  ORDER BY s.sort_order, s.name`
	err := d.SelectContext(ctx, &skills, query, projectID)
	return skills, err
}

// ProjectSkill operations (project-specific skills)
func (d *DB) CreateProjectSkill(ctx context.Context, ps *ProjectSkill) error {
	query := `INSERT INTO project_skills (project_id, name, content, enabled, category, sort_order) VALUES (?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, ps.ProjectID, ps.Name, ps.Content, ps.Enabled, ps.Category, ps.SortOrder)
	if err != nil {
		return err
	}
	ps.ID, _ = result.LastInsertId()
	fresh, err := d.GetProjectSkill(ctx, ps.ID)
	if err == nil {
		*ps = *fresh
	}
	return nil
}

func (d *DB) GetProjectSkill(ctx context.Context, id int64) (*ProjectSkill, error) {
	var ps ProjectSkill
	err := d.GetContext(ctx, &ps, "SELECT * FROM project_skills WHERE id = ?", id)
	return &ps, err
}

func (d *DB) ListProjectSkills(ctx context.Context, projectID int64) ([]ProjectSkill, error) {
	var skills []ProjectSkill
	err := d.SelectContext(ctx, &skills, "SELECT * FROM project_skills WHERE project_id = ? ORDER BY sort_order, name", projectID)
	return skills, err
}

func (d *DB) ListEnabledProjectSkills(ctx context.Context, projectID int64) ([]ProjectSkill, error) {
	var skills []ProjectSkill
	err := d.SelectContext(ctx, &skills, "SELECT * FROM project_skills WHERE project_id = ? AND enabled = 1 ORDER BY sort_order, name", projectID)
	return skills, err
}

func (d *DB) UpdateProjectSkill(ctx context.Context, ps *ProjectSkill) error {
	query := `UPDATE project_skills SET name=?, content=?, enabled=?, category=?, sort_order=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, ps.Name, ps.Content, ps.Enabled, ps.Category, ps.SortOrder, time.Now(), ps.ID)
	if err != nil {
		return err
	}
	fresh, err := d.GetProjectSkill(ctx, ps.ID)
	if err == nil {
		*ps = *fresh
	}
	return nil
}

func (d *DB) DeleteProjectSkill(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM project_skills WHERE id = ?", id)
	return err
}

func (d *DB) IncrementProjectSkillSyncCount(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "UPDATE project_skills SET sync_count = sync_count + 1 WHERE id = ?", id)
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

// Project MCP Server operations

func (d *DB) CreateProjectMCPServer(ctx context.Context, m *ProjectMCPServer) error {
	query := `INSERT INTO project_mcp_servers (project_id, name, command, args, env, enabled) VALUES (?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, m.ProjectID, m.Name, m.Command, m.Args, m.Env, m.Enabled)
	if err != nil {
		return err
	}
	m.ID, _ = result.LastInsertId()
	fresh, err := d.GetProjectMCPServer(ctx, m.ID)
	if err == nil {
		*m = *fresh
	}
	return nil
}

func (d *DB) GetProjectMCPServer(ctx context.Context, id int64) (*ProjectMCPServer, error) {
	var m ProjectMCPServer
	err := d.GetContext(ctx, &m, "SELECT * FROM project_mcp_servers WHERE id = ?", id)
	return &m, err
}

func (d *DB) GetProjectMCPServerByName(ctx context.Context, projectID int64, name string) (*ProjectMCPServer, error) {
	var m ProjectMCPServer
	err := d.GetContext(ctx, &m, "SELECT * FROM project_mcp_servers WHERE project_id = ? AND name = ?", projectID, name)
	return &m, err
}

func (d *DB) ListProjectMCPServers(ctx context.Context, projectID int64) ([]ProjectMCPServer, error) {
	var servers []ProjectMCPServer
	err := d.SelectContext(ctx, &servers, "SELECT * FROM project_mcp_servers WHERE project_id = ? ORDER BY name", projectID)
	return servers, err
}

func (d *DB) ListEnabledProjectMCPServers(ctx context.Context, projectID int64) ([]ProjectMCPServer, error) {
	var servers []ProjectMCPServer
	err := d.SelectContext(ctx, &servers, "SELECT * FROM project_mcp_servers WHERE project_id = ? AND enabled = 1 ORDER BY name", projectID)
	return servers, err
}

func (d *DB) UpdateProjectMCPServer(ctx context.Context, m *ProjectMCPServer) error {
	query := `UPDATE project_mcp_servers SET name=?, command=?, args=?, env=?, enabled=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, m.Name, m.Command, m.Args, m.Env, m.Enabled, time.Now(), m.ID)
	if err != nil {
		return err
	}
	fresh, err := d.GetProjectMCPServer(ctx, m.ID)
	if err == nil {
		*m = *fresh
	}
	return nil
}

func (d *DB) DeleteProjectMCPServer(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM project_mcp_servers WHERE id = ?", id)
	return err
}

// Project Tool operations

func (d *DB) CreateProjectTool(ctx context.Context, t *ProjectTool) error {
	query := `INSERT INTO project_tools (project_id, name, description, command, parameters, confirm, working_dir, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, t.ProjectID, t.Name, t.Description, t.Command, t.Parameters, t.Confirm, t.WorkingDir, t.Enabled)
	if err != nil {
		return err
	}
	t.ID, _ = result.LastInsertId()
	fresh, err := d.GetProjectTool(ctx, t.ID)
	if err == nil {
		*t = *fresh
	}
	return nil
}

func (d *DB) GetProjectTool(ctx context.Context, id int64) (*ProjectTool, error) {
	var t ProjectTool
	err := d.GetContext(ctx, &t, "SELECT * FROM project_tools WHERE id = ?", id)
	return &t, err
}

func (d *DB) GetProjectToolByName(ctx context.Context, projectID int64, name string) (*ProjectTool, error) {
	var t ProjectTool
	err := d.GetContext(ctx, &t, "SELECT * FROM project_tools WHERE project_id = ? AND name = ?", projectID, name)
	return &t, err
}

func (d *DB) ListProjectTools(ctx context.Context, projectID int64) ([]ProjectTool, error) {
	var tools []ProjectTool
	err := d.SelectContext(ctx, &tools, "SELECT * FROM project_tools WHERE project_id = ? ORDER BY name", projectID)
	return tools, err
}

func (d *DB) ListEnabledProjectTools(ctx context.Context, projectID int64) ([]ProjectTool, error) {
	var tools []ProjectTool
	err := d.SelectContext(ctx, &tools, "SELECT * FROM project_tools WHERE project_id = ? AND enabled = 1 ORDER BY name", projectID)
	return tools, err
}

func (d *DB) UpdateProjectTool(ctx context.Context, t *ProjectTool) error {
	query := `UPDATE project_tools SET name=?, description=?, command=?, parameters=?, confirm=?, working_dir=?, enabled=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, t.Name, t.Description, t.Command, t.Parameters, t.Confirm, t.WorkingDir, t.Enabled, time.Now(), t.ID)
	if err != nil {
		return err
	}
	fresh, err := d.GetProjectTool(ctx, t.ID)
	if err == nil {
		*t = *fresh
	}
	return nil
}

func (d *DB) DeleteProjectTool(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM project_tools WHERE id = ?", id)
	return err
}

// AI Config operations

func (d *DB) CreateAIConfig(ctx context.Context, c *AIConfig) error {
	query := `INSERT INTO ai_configs (name, provider_type, api_key_encrypted, api_key_iv, api_key_preview, model, base_url, extra_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, c.Name, c.ProviderType, c.APIKeyEncrypted, c.APIKeyIV, c.APIKeyPreview, c.Model, c.BaseURL, c.ExtraJSON)
	if err != nil {
		return err
	}
	c.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetAIConfig(ctx context.Context, id int64) (*AIConfig, error) {
	var c AIConfig
	err := d.GetContext(ctx, &c, "SELECT * FROM ai_configs WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListAIConfigs(ctx context.Context) ([]AIConfig, error) {
	var configs []AIConfig
	err := d.SelectContext(ctx, &configs, "SELECT * FROM ai_configs ORDER BY name")
	return configs, err
}

func (d *DB) UpdateAIConfig(ctx context.Context, c *AIConfig) error {
	query := `UPDATE ai_configs SET name=?, provider_type=?, api_key_encrypted=?, api_key_iv=?, api_key_preview=?, model=?, base_url=?, extra_json=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, c.Name, c.ProviderType, c.APIKeyEncrypted, c.APIKeyIV, c.APIKeyPreview, c.Model, c.BaseURL, c.ExtraJSON, time.Now(), c.ID)
	return err
}

func (d *DB) DeleteAIConfig(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM ai_configs WHERE id = ?", id)
	return err
}

// AI Config Assignment operations

func (d *DB) GetAIConfigAssignments(ctx context.Context) ([]AIConfigAssignment, error) {
	var assignments []AIConfigAssignment
	err := d.SelectContext(ctx, &assignments, "SELECT slot, config_id FROM ai_config_assignments ORDER BY slot")
	return assignments, err
}

func (d *DB) SetAIConfigAssignment(ctx context.Context, slot string, configID *int64) error {
	if configID == nil {
		_, err := d.ExecContext(ctx, "UPDATE ai_config_assignments SET config_id = NULL WHERE slot = ?", slot)
		return err
	}
	_, err := d.ExecContext(ctx, "UPDATE ai_config_assignments SET config_id = ? WHERE slot = ?", *configID, slot)
	return err
}

func (d *DB) GetAIConfigForSlot(ctx context.Context, slot string) (*AIConfig, error) {
	var c AIConfig
	err := d.GetContext(ctx, &c, `
		SELECT c.* FROM ai_configs c
		INNER JOIN ai_config_assignments a ON a.config_id = c.id
		WHERE a.slot = ?`, slot)
	if err != nil {
		return nil, err
	}
	return &c, nil
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
	query := `INSERT INTO notifications (session_id, type, title, body, link) VALUES (?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, n.SessionID, n.Type, n.Title, n.Body, n.Link)
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

func (d *DB) MarkSessionNotificationsRead(ctx context.Context, sessionID string) (int64, error) {
	result, err := d.ExecContext(ctx, "UPDATE notifications SET read = 1 WHERE session_id = ? AND read = 0", sessionID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (d *DB) CountUnreadNotifications(ctx context.Context) (int, error) {
	var count int
	err := d.GetContext(ctx, &count, `
		SELECT COUNT(*) FROM notifications n
		INNER JOIN sessions s ON s.id = n.session_id
		WHERE n.read = 0 AND s.status IN ('starting', 'running')`)
	return count, err
}

// ListActiveNotifications returns unread notifications only from active sessions.
func (d *DB) ListActiveNotifications(ctx context.Context) ([]Notification, error) {
	var notifications []Notification
	err := d.SelectContext(ctx, &notifications, `
		SELECT n.* FROM notifications n
		INNER JOIN sessions s ON s.id = n.session_id
		WHERE n.read = 0 AND s.status IN ('starting', 'running')
		ORDER BY n.created_at DESC`)
	return notifications, err
}

// AI Agent operations

func (d *DB) CreateAIAgent(ctx context.Context, a *AIAgent) error {
	query := `INSERT INTO ai_agents (name, system_prompt, tool_policy, project_filter, is_default, enabled) VALUES (?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, a.Name, a.SystemPrompt, a.ToolPolicy, a.ProjectFilter, a.IsDefault, a.Enabled)
	if err != nil {
		return err
	}
	a.ID, _ = result.LastInsertId()
	return nil
}

func (d *DB) GetAIAgent(ctx context.Context, id int64) (*AIAgent, error) {
	var a AIAgent
	err := d.GetContext(ctx, &a, "SELECT * FROM ai_agents WHERE id = ?", id)
	return &a, err
}

func (d *DB) GetDefaultAIAgent(ctx context.Context) (*AIAgent, error) {
	var a AIAgent
	err := d.GetContext(ctx, &a, "SELECT * FROM ai_agents WHERE is_default = 1 LIMIT 1")
	return &a, err
}

func (d *DB) ListAIAgents(ctx context.Context) ([]AIAgent, error) {
	var agents []AIAgent
	err := d.SelectContext(ctx, &agents, "SELECT * FROM ai_agents ORDER BY is_default DESC, name")
	return agents, err
}

func (d *DB) ListEnabledAIAgents(ctx context.Context) ([]AIAgent, error) {
	var agents []AIAgent
	err := d.SelectContext(ctx, &agents, "SELECT * FROM ai_agents WHERE enabled = 1 ORDER BY is_default DESC, name")
	return agents, err
}

func (d *DB) UpdateAIAgent(ctx context.Context, a *AIAgent) error {
	query := `UPDATE ai_agents SET name=?, system_prompt=?, tool_policy=?, project_filter=?, enabled=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, a.Name, a.SystemPrompt, a.ToolPolicy, a.ProjectFilter, a.Enabled, time.Now(), a.ID)
	return err
}

func (d *DB) DeleteAIAgent(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM ai_agents WHERE id = ? AND is_default = 0", id)
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
	query := `INSERT INTO ai_conversations (title, source, proactive_level, proactive_type, proactive_context, is_read, agent_id) VALUES (?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, c.Title, c.Source, c.ProactiveLevel, c.ProactiveType, c.ProactiveContext, isRead, c.AgentID)
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

// UpdateAIConversationSessionID persists the Claude Code session ID for resume across restarts.
func (d *DB) UpdateAIConversationSessionID(ctx context.Context, id int64, sessionID string) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_conversations SET session_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", sessionID, id)
	return err
}

func (d *DB) DeleteAIConversation(ctx context.Context, id int64) error {
	d.DeleteTempDocumentsByConversation(ctx, id)
	_, err := d.ExecContext(ctx, "DELETE FROM ai_conversations WHERE id = ?", id)
	return err
}

func (d *DB) DeleteAllAIConversations(ctx context.Context) error {
	d.ExecContext(ctx, "DELETE FROM temp_documents WHERE conversation_id IS NOT NULL")
	_, err := d.ExecContext(ctx, "DELETE FROM ai_conversations")
	return err
}

// PruneOldAIConversations keeps only the most recent N conversations.
func (d *DB) PruneOldAIConversations(ctx context.Context, keep int) error {
	// Clean up temp documents for conversations being pruned
	d.ExecContext(ctx, `DELETE FROM temp_documents WHERE conversation_id IN (
		SELECT id FROM ai_conversations WHERE id NOT IN (
			SELECT id FROM ai_conversations ORDER BY updated_at DESC LIMIT ?
		)
	)`, keep)
	_, err := d.ExecContext(ctx, `DELETE FROM ai_conversations WHERE id NOT IN (
		SELECT id FROM ai_conversations ORDER BY updated_at DESC LIMIT ?
	)`, keep)
	return err
}

// AI Message operations
func (d *DB) CreateAIMessage(ctx context.Context, m *AIMessage) error {
	if m.Status == "" {
		m.Status = "completed"
	}
	query := `INSERT INTO ai_messages (conversation_id, role, content, tool_calls, status) VALUES (?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, m.ConversationID, m.Role, m.Content, m.ToolCalls, m.Status)
	if err != nil {
		return err
	}
	m.ID, _ = result.LastInsertId()
	m.CreatedAt = time.Now()
	return nil
}

func (d *DB) ListAIMessages(ctx context.Context, conversationID int64) ([]AIMessage, error) {
	var msgs []AIMessage
	err := d.SelectContext(ctx, &msgs, "SELECT * FROM ai_messages WHERE conversation_id = ? ORDER BY created_at ASC", conversationID)
	return msgs, err
}

// UpdateAIMessageContent updates the content and tool_calls of a streaming message (debounced flush).
func (d *DB) UpdateAIMessageContent(ctx context.Context, id int64, content string, toolCalls string) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_messages SET content=?, tool_calls=? WHERE id=?", content, toolCalls, id)
	return err
}

// UpdateAIMessageStatus sets the final status of a message (completed or error).
func (d *DB) UpdateAIMessageStatus(ctx context.Context, id int64, status string, content string, toolCalls string, errorInfo string) error {
	_, err := d.ExecContext(ctx, "UPDATE ai_messages SET status=?, content=?, tool_calls=?, error_info=? WHERE id=?",
		status, content, toolCalls, errorInfo, id)
	return err
}

// FixStaleStreamingMessages marks any leftover 'streaming' messages as 'error' on startup.
func (d *DB) FixStaleStreamingMessages(ctx context.Context) error {
	_, err := d.ExecContext(ctx,
		"UPDATE ai_messages SET status='error', error_info='Server restarted during streaming' WHERE status='streaming'")
	return err
}

// Temp Document operations
func (d *DB) CreateTempDocument(ctx context.Context, doc *TempDocument) error {
	if doc.Status == "" {
		doc.Status = "pending"
	}
	query := `INSERT INTO temp_documents (id, title, content, conversation_id, task_id, session_id, summary, status, message_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.ExecContext(ctx, query, doc.ID, doc.Title, doc.Content, doc.ConversationID, doc.TaskID, doc.SessionID, doc.Summary, doc.Status, doc.MessageID)
	return err
}

func (d *DB) ListDocumentsByTask(ctx context.Context, taskID int64) ([]TempDocument, error) {
	var docs []TempDocument
	err := d.SelectContext(ctx, &docs,
		"SELECT id, title, status, task_id, created_at FROM temp_documents WHERE task_id = ? ORDER BY created_at DESC", taskID)
	return docs, err
}

func (d *DB) ListDocumentsBySession(ctx context.Context, sessionID string) ([]TempDocument, error) {
	var docs []TempDocument
	err := d.SelectContext(ctx, &docs,
		"SELECT id, title, status, session_id, created_at FROM temp_documents WHERE session_id = ? ORDER BY created_at DESC", sessionID)
	return docs, err
}

func (d *DB) UpdateTempDocumentStatus(ctx context.Context, id string, status string) error {
	_, err := d.ExecContext(ctx, "UPDATE temp_documents SET status = ? WHERE id = ?", status, id)
	return err
}

func (d *DB) ListTempDocumentsByConversation(ctx context.Context, conversationID int64) ([]TempDocument, error) {
	var docs []TempDocument
	err := d.SelectContext(ctx, &docs, "SELECT * FROM temp_documents WHERE conversation_id = ? ORDER BY created_at ASC", conversationID)
	return docs, err
}

func (d *DB) DeleteTempDocumentsByConversation(ctx context.Context, conversationID int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM temp_documents WHERE conversation_id = ?", conversationID)
	return err
}

func (d *DB) GetTempDocument(ctx context.Context, id string) (*TempDocument, error) {
	var doc TempDocument
	err := d.GetContext(ctx, &doc, "SELECT * FROM temp_documents WHERE id = ?", id)
	return &doc, err
}

// ListUnacknowledgedFeedback returns temp documents that have been approved/rejected
// but not yet acknowledged by the AI assistant in a conversation.
func (d *DB) ListUnacknowledgedFeedback(ctx context.Context, conversationID int64) ([]TempDocument, error) {
	var docs []TempDocument
	err := d.SelectContext(ctx, &docs,
		`SELECT * FROM temp_documents WHERE conversation_id = ? AND status IN ('approved','rejected') AND feedback_ack = 0 ORDER BY created_at ASC`,
		conversationID)
	return docs, err
}

// AcknowledgeFeedback marks temp documents as acknowledged by the AI assistant.
func (d *DB) AcknowledgeFeedback(ctx context.Context, docIDs []string) error {
	if len(docIDs) == 0 {
		return nil
	}
	query, args, err := sqlx.In(`UPDATE temp_documents SET feedback_ack = 1 WHERE id IN (?)`, docIDs)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, d.Rebind(query), args...)
	return err
}

// Project Task operations
func (d *DB) CreateTask(ctx context.Context, t *ProjectTask) error {
	// Auto-assign sort_order = MAX(sort_order) + 1 among siblings
	if t.SortOrder == 0 && t.Status != "done" {
		var maxOrder sql.NullInt64
		if t.ParentID.Valid {
			d.GetContext(ctx, &maxOrder, "SELECT MAX(sort_order) FROM project_tasks WHERE project_id=? AND parent_id=?", t.ProjectID, t.ParentID.Int64)
		} else {
			d.GetContext(ctx, &maxOrder, "SELECT MAX(sort_order) FROM project_tasks WHERE project_id=? AND parent_id IS NULL", t.ProjectID)
		}
		if maxOrder.Valid {
			t.SortOrder = int(maxOrder.Int64) + 1
		} else {
			t.SortOrder = 1
		}
	}
	// Auto-assign global_sort_order = MAX(global_sort_order) + 1 across all tasks
	if t.GlobalSortOrder == 0 && t.Status != "done" {
		var maxGlobal sql.NullInt64
		d.GetContext(ctx, &maxGlobal, "SELECT MAX(global_sort_order) FROM project_tasks")
		if maxGlobal.Valid {
			t.GlobalSortOrder = int(maxGlobal.Int64) + 1
		} else {
			t.GlobalSortOrder = 1
		}
	}
	query := `INSERT INTO project_tasks (project_id, parent_id, title, description, status, priority, due_date, sort_order, global_sort_order, verification_doc_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, t.ProjectID, t.ParentID, t.Title, t.Description, t.Status, t.Priority, t.DueDate, t.SortOrder, t.GlobalSortOrder, t.VerificationDocID)
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

// ApplyUmbrellaStatus overwrites the Status field of umbrella tasks (parents)
// with a value computed from their children's statuses:
//   - all children todo   → todo
//   - all children done   → done
//   - otherwise           → in_progress
func ApplyUmbrellaStatus(tasks []ProjectTask) {
	childrenByParent := make(map[int64][]ProjectTask)
	for _, t := range tasks {
		if t.ParentID.Valid {
			childrenByParent[t.ParentID.Int64] = append(childrenByParent[t.ParentID.Int64], t)
		}
	}
	for i := range tasks {
		kids, isUmbrella := childrenByParent[tasks[i].ID]
		if !isUmbrella {
			continue
		}
		allTodo, allDone := true, true
		for _, k := range kids {
			if k.Status != "todo" {
				allTodo = false
			}
			if k.Status != "done" {
				allDone = false
			}
		}
		switch {
		case allDone:
			tasks[i].Status = "done"
		case allTodo:
			tasks[i].Status = "todo"
		default:
			tasks[i].Status = "in_progress"
		}
	}
}

func (d *DB) ListTasksByProject(ctx context.Context, projectID int64) ([]ProjectTask, error) {
	var tasks []ProjectTask
	err := d.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE project_id = ? ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END, CASE WHEN status = 'done' THEN NULL ELSE sort_order END, CASE WHEN status = 'done' THEN updated_at END DESC, created_at", projectID)
	return tasks, err
}

func (d *DB) UpdateTask(ctx context.Context, t *ProjectTask) error {
	query := `UPDATE project_tasks SET title=?, description=?, status=?, priority=?, due_date=?, sort_order=?, due_notified=?, parent_id=?, verification_doc_id=?, updated_at=? WHERE id=?`
	_, err := d.ExecContext(ctx, query, t.Title, t.Description, t.Status, t.Priority, t.DueDate, t.SortOrder, t.DueNotified, t.ParentID, t.VerificationDocID, time.Now(), t.ID)
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
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := changeTaskStatusInTx(ctx, tx, id, status); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) DeleteTask(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM project_tasks WHERE id = ?", id)
	return err
}

// ReorderTasks batch-updates sort_order and parent_id for multiple tasks in a single transaction.
// Also syncs global_sort_order to maintain consistent relative ordering across views.
func (d *DB) ReorderTasks(ctx context.Context, projectID int64, items []ReorderItem) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, "UPDATE project_tasks SET sort_order=?, parent_id=?, updated_at=? WHERE id=? AND project_id=?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now()
	taskIDs := make([]int64, 0, len(items))
	for _, item := range items {
		var parentID sql.NullInt64
		if item.ParentID != nil && *item.ParentID > 0 {
			parentID = sql.NullInt64{Int64: *item.ParentID, Valid: true}
		}
		if _, err := stmt.ExecContext(ctx, item.SortOrder, parentID, now, item.ID, projectID); err != nil {
			return err
		}
		taskIDs = append(taskIDs, item.ID)
	}
	stmt.Close()

	// Sync global_sort_order: redistribute existing global values to match new sort_order
	if len(taskIDs) > 0 {
		// Build IN clause
		placeholders := make([]string, len(taskIDs))
		args := make([]interface{}, 0, len(taskIDs)+1)
		args = append(args, projectID)
		for i, id := range taskIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		inClause := strings.Join(placeholders, ",")

		// Get tasks ordered by NEW sort_order, with their current global_sort_order
		var ordered []struct {
			ID              int64 `db:"id"`
			GlobalSortOrder int   `db:"global_sort_order"`
		}
		query := fmt.Sprintf("SELECT id, global_sort_order FROM project_tasks WHERE project_id=? AND id IN (%s) ORDER BY sort_order, created_at", inClause)
		tx.SelectContext(ctx, &ordered, query, args...)

		// Collect and sort global values
		globalValues := make([]int, len(ordered))
		for i, o := range ordered {
			globalValues[i] = o.GlobalSortOrder
		}
		sort.Ints(globalValues)

		// Re-assign sorted global values in new sort_order
		for i, o := range ordered {
			tx.ExecContext(ctx, "UPDATE project_tasks SET global_sort_order=? WHERE id=?", globalValues[i], o.ID)
		}
	}

	return tx.Commit()
}

// ReorderItem represents a single item in a batch reorder operation.
type ReorderItem struct {
	ID        int64  `json:"id"`
	SortOrder int    `json:"sort_order"`
	ParentID  *int64 `json:"parent_id"`
}

// GlobalReorderItem represents a single item in a global reorder operation.
type GlobalReorderItem struct {
	ID              int64  `json:"id"`
	GlobalSortOrder int    `json:"global_sort_order"`
	ParentID        *int64 `json:"parent_id"`
}

// ReorderTasksGlobal updates global_sort_order for tasks and syncs sort_order per project.
func (d *DB) ReorderTasksGlobal(ctx context.Context, items []GlobalReorderItem) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now()

	// 1. Update global_sort_order and parent_id for all items
	stmt, err := tx.PrepareContext(ctx, "UPDATE project_tasks SET global_sort_order=?, parent_id=?, updated_at=? WHERE id=?")
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err := stmt.ExecContext(ctx, item.GlobalSortOrder, item.ParentID, now, item.ID); err != nil {
			stmt.Close()
			return err
		}
	}
	stmt.Close()

	// 2. Sync sort_order: for each affected project, re-derive from global order
	// Collect task IDs
	taskIDs := make([]interface{}, len(items))
	placeholders := make([]string, len(items))
	for i, item := range items {
		taskIDs[i] = item.ID
		placeholders[i] = "?"
	}
	inClause := strings.Join(placeholders, ",")

	// Get distinct project IDs
	var projectIDs []int64
	query := fmt.Sprintf("SELECT DISTINCT project_id FROM project_tasks WHERE id IN (%s)", inClause)
	tx.SelectContext(ctx, &projectIDs, query, taskIDs...)

	for _, pid := range projectIDs {
		// Re-derive top-level sort_order from global order
		var tasks []struct {
			ID int64 `db:"id"`
		}
		tx.SelectContext(ctx, &tasks, "SELECT id FROM project_tasks WHERE project_id=? AND parent_id IS NULL AND status != 'done' ORDER BY global_sort_order, created_at", pid)
		for i, t := range tasks {
			tx.ExecContext(ctx, "UPDATE project_tasks SET sort_order=? WHERE id=?", i+1, t.ID)
		}

		// Re-derive subtask sort_order per parent
		var parents []struct {
			ID int64 `db:"id"`
		}
		tx.SelectContext(ctx, &parents, "SELECT DISTINCT parent_id AS id FROM project_tasks WHERE project_id=? AND parent_id IS NOT NULL", pid)
		for _, p := range parents {
			var children []struct {
				ID int64 `db:"id"`
			}
			tx.SelectContext(ctx, &children, "SELECT id FROM project_tasks WHERE parent_id=? AND status != 'done' ORDER BY global_sort_order, created_at", p.ID)
			for i, c := range children {
				tx.ExecContext(ctx, "UPDATE project_tasks SET sort_order=? WHERE id=?", i+1, c.ID)
			}
		}
	}

	return tx.Commit()
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

func (d *DB) SetTaskVerificationDoc(ctx context.Context, taskID int64, docID string) error {
	_, err := d.ExecContext(ctx, "UPDATE project_tasks SET verification_doc_id = ?, updated_at = ? WHERE id = ?", docID, time.Now(), taskID)
	return err
}

func (d *DB) ListOverdueTasks(ctx context.Context) ([]ProjectTask, error) {
	var tasks []ProjectTask
	err := d.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE due_date < CURRENT_TIMESTAMP AND status NOT IN ('done','awaiting_approval') AND due_notified = 0 ORDER BY due_date")
	return tasks, err
}

func (d *DB) ListUpcomingTasks(ctx context.Context, within time.Duration) ([]ProjectTask, error) {
	var tasks []ProjectTask
	deadline := time.Now().Add(within)
	err := d.SelectContext(ctx, &tasks, "SELECT * FROM project_tasks WHERE due_date > CURRENT_TIMESTAMP AND due_date <= ? AND status NOT IN ('done','awaiting_approval') AND due_notified = 0 ORDER BY due_date", deadline)
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
	err := d.SelectContext(ctx, &counts, "SELECT status, COUNT(*) as count FROM project_tasks WHERE project_id = ? AND id NOT IN (SELECT DISTINCT parent_id FROM project_tasks WHERE parent_id IS NOT NULL) GROUP BY status", projectID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	for _, c := range counts {
		result[c.Status] = c.Count
	}
	return result, nil
}

// TaskFilter holds optional filtering parameters for ListAllTasks.
type TaskFilter struct {
	Status    string
	Priority  string
	ProjectID *int64
	DueBefore *time.Time
	DueAfter  *time.Time
	Search    string
}

func (d *DB) ListAllTasks(ctx context.Context, f TaskFilter) ([]ProjectTask, error) {
	query := "SELECT * FROM project_tasks WHERE 1=1"
	args := []interface{}{}

	if f.Status != "" {
		query += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Priority != "" {
		query += " AND priority = ?"
		args = append(args, f.Priority)
	}
	if f.ProjectID != nil {
		query += " AND project_id = ?"
		args = append(args, *f.ProjectID)
	}
	if f.DueBefore != nil {
		query += " AND due_date <= ?"
		args = append(args, *f.DueBefore)
	}
	if f.DueAfter != nil {
		query += " AND due_date >= ?"
		args = append(args, *f.DueAfter)
	}
	if f.Search != "" {
		query += " AND (title LIKE ? OR description LIKE ?)"
		search := "%" + f.Search + "%"
		args = append(args, search, search)
	}

	query += " ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END, CASE WHEN status = 'done' THEN NULL ELSE global_sort_order END, CASE WHEN status = 'done' THEN updated_at END DESC, created_at"

	var tasks []ProjectTask
	err := d.SelectContext(ctx, &tasks, query, args...)
	return tasks, err
}

func (d *DB) GetAllTasksSummary(ctx context.Context) (map[string]int, error) {
	type statusCount struct {
		Status string `db:"status"`
		Count  int    `db:"count"`
	}
	var counts []statusCount
	err := d.SelectContext(ctx, &counts, "SELECT status, COUNT(*) as count FROM project_tasks WHERE id NOT IN (SELECT DISTINCT parent_id FROM project_tasks WHERE parent_id IS NOT NULL) GROUP BY status")
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	for _, c := range counts {
		result[c.Status] = c.Count
	}
	return result, nil
}

// Session-Task linking operations (uses sessions.task_id directly — 1:1 relationship)

// LinkSessionToTask sets the task_id on a session.
func (d *DB) LinkSessionToTask(ctx context.Context, sessionID string, taskID int64) error {
	_, err := d.ExecContext(ctx, "UPDATE sessions SET task_id = ?, last_activity_at = ? WHERE id = ?", taskID, time.Now(), sessionID)
	return err
}

// UnlinkSessionFromTask clears the task_id on a session and returns the old task ID.
func (d *DB) UnlinkSessionFromTask(ctx context.Context, sessionID string) (int64, error) {
	var taskID sql.NullInt64
	err := d.GetContext(ctx, &taskID, "SELECT task_id FROM sessions WHERE id = ?", sessionID)
	if err != nil {
		return 0, err
	}
	if !taskID.Valid {
		return 0, nil
	}
	_, err = d.ExecContext(ctx, "UPDATE sessions SET task_id = NULL WHERE id = ?", sessionID)
	if err != nil {
		return 0, err
	}
	return taskID.Int64, nil
}

func (d *DB) GetTaskForSession(ctx context.Context, sessionID string) (*ProjectTask, error) {
	var task ProjectTask
	err := d.GetContext(ctx, &task,
		`SELECT t.* FROM project_tasks t
		 INNER JOIN sessions s ON s.task_id = t.id
		 WHERE s.id = ?`, sessionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

func (d *DB) GetSessionsForTask(ctx context.Context, taskID int64) ([]Session, error) {
	var sessions []Session
	err := d.SelectContext(ctx, &sessions,
		`SELECT * FROM sessions WHERE task_id = ? ORDER BY COALESCE(last_activity_at, end_time, start_time) DESC`, taskID)
	return sessions, err
}

func (d *DB) GetTaskSessionSummary(ctx context.Context, projectID int64) ([]struct {
	TaskID               int64  `db:"task_id" json:"task_id"`
	SessionCount         int    `db:"session_count" json:"session_count"`
	ActiveCount          int    `db:"active_count" json:"active_count"`
	StoppedCount         int    `db:"stopped_count" json:"stopped_count"`
	LatestSession        string `db:"latest_session" json:"latest_session"`
	LatestStoppedSession string `db:"latest_stopped_session" json:"latest_stopped_session"`
}, error) {
	type taskSessionInfo struct {
		TaskID               int64  `db:"task_id" json:"task_id"`
		SessionCount         int    `db:"session_count" json:"session_count"`
		ActiveCount          int    `db:"active_count" json:"active_count"`
		StoppedCount         int    `db:"stopped_count" json:"stopped_count"`
		LatestSession        string `db:"latest_session" json:"latest_session"`
		LatestStoppedSession string `db:"latest_stopped_session" json:"latest_stopped_session"`
	}
	var results []taskSessionInfo
	err := d.SelectContext(ctx, &results, `
		SELECT s.task_id,
		       COUNT(*) as session_count,
		       COUNT(CASE WHEN s.status IN ('running', 'starting') THEN 1 END) as active_count,
		       COUNT(CASE WHEN s.status IN ('stopped', 'completed') THEN 1 END) as stopped_count,
		       COALESCE((SELECT s2.id FROM sessions s2 WHERE s2.task_id = s.task_id AND s2.status IN ('running', 'starting') ORDER BY COALESCE(s2.last_activity_at, s2.end_time, s2.start_time) DESC LIMIT 1), '') as latest_session,
		       COALESCE((SELECT s2.id FROM sessions s2 WHERE s2.task_id = s.task_id AND s2.status IN ('stopped', 'completed') ORDER BY COALESCE(s2.last_activity_at, s2.end_time, s2.start_time) DESC LIMIT 1), '') as latest_stopped_session
		FROM sessions s
		INNER JOIN project_tasks t ON t.id = s.task_id
		WHERE t.project_id = ? AND s.task_id IS NOT NULL
		GROUP BY s.task_id`, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		TaskID               int64  `db:"task_id" json:"task_id"`
		SessionCount         int    `db:"session_count" json:"session_count"`
		ActiveCount          int    `db:"active_count" json:"active_count"`
		StoppedCount         int    `db:"stopped_count" json:"stopped_count"`
		LatestSession        string `db:"latest_session" json:"latest_session"`
		LatestStoppedSession string `db:"latest_stopped_session" json:"latest_stopped_session"`
	}, len(results))
	for i, r := range results {
		out[i].TaskID = r.TaskID
		out[i].SessionCount = r.SessionCount
		out[i].ActiveCount = r.ActiveCount
		out[i].StoppedCount = r.StoppedCount
		out[i].LatestSession = r.LatestSession
		out[i].LatestStoppedSession = r.LatestStoppedSession
	}
	return out, nil
}

func (d *DB) GetAllTaskSessionSummary(ctx context.Context) ([]struct {
	TaskID               int64  `db:"task_id" json:"task_id"`
	SessionCount         int    `db:"session_count" json:"session_count"`
	ActiveCount          int    `db:"active_count" json:"active_count"`
	StoppedCount         int    `db:"stopped_count" json:"stopped_count"`
	LatestSession        string `db:"latest_session" json:"latest_session"`
	LatestStoppedSession string `db:"latest_stopped_session" json:"latest_stopped_session"`
}, error) {
	type taskSessionInfo struct {
		TaskID               int64  `db:"task_id" json:"task_id"`
		SessionCount         int    `db:"session_count" json:"session_count"`
		ActiveCount          int    `db:"active_count" json:"active_count"`
		StoppedCount         int    `db:"stopped_count" json:"stopped_count"`
		LatestSession        string `db:"latest_session" json:"latest_session"`
		LatestStoppedSession string `db:"latest_stopped_session" json:"latest_stopped_session"`
	}
	var results []taskSessionInfo
	err := d.SelectContext(ctx, &results, `
		SELECT s.task_id,
		       COUNT(*) as session_count,
		       COUNT(CASE WHEN s.status IN ('running', 'starting') THEN 1 END) as active_count,
		       COUNT(CASE WHEN s.status IN ('stopped', 'completed') THEN 1 END) as stopped_count,
		       COALESCE((SELECT s2.id FROM sessions s2 WHERE s2.task_id = s.task_id AND s2.status IN ('running', 'starting') ORDER BY COALESCE(s2.last_activity_at, s2.end_time, s2.start_time) DESC LIMIT 1), '') as latest_session,
		       COALESCE((SELECT s2.id FROM sessions s2 WHERE s2.task_id = s.task_id AND s2.status IN ('stopped', 'completed') ORDER BY COALESCE(s2.last_activity_at, s2.end_time, s2.start_time) DESC LIMIT 1), '') as latest_stopped_session
		FROM sessions s
		WHERE s.task_id IS NOT NULL
		GROUP BY s.task_id`)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		TaskID               int64  `db:"task_id" json:"task_id"`
		SessionCount         int    `db:"session_count" json:"session_count"`
		ActiveCount          int    `db:"active_count" json:"active_count"`
		StoppedCount         int    `db:"stopped_count" json:"stopped_count"`
		LatestSession        string `db:"latest_session" json:"latest_session"`
		LatestStoppedSession string `db:"latest_stopped_session" json:"latest_stopped_session"`
	}, len(results))
	for i, r := range results {
		out[i].TaskID = r.TaskID
		out[i].SessionCount = r.SessionCount
		out[i].ActiveCount = r.ActiveCount
		out[i].StoppedCount = r.StoppedCount
		out[i].LatestSession = r.LatestSession
		out[i].LatestStoppedSession = r.LatestStoppedSession
	}
	return out, nil
}

// Task History operations

func (d *DB) CreateTaskHistory(ctx context.Context, h *TaskHistory) error {
	if h.Actor == "" {
		h.Actor = "system"
	}
	if h.Details == "" {
		h.Details = "{}"
	}
	query := `INSERT INTO task_history (task_id, project_id, event_type, details, actor, session_id)
	           VALUES (?, ?, ?, ?, ?, ?)`
	result, err := d.ExecContext(ctx, query, h.TaskID, h.ProjectID, h.EventType, h.Details, h.Actor, h.SessionID)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	h.ID = id
	h.CreatedAt = time.Now()
	return nil
}

func (d *DB) ListTaskHistory(ctx context.Context, taskID int64, limit int) ([]TaskHistory, error) {
	if limit <= 0 {
		limit = 50
	}
	var history []TaskHistory
	err := d.SelectContext(ctx, &history,
		`SELECT * FROM task_history WHERE task_id = ? ORDER BY created_at DESC LIMIT ?`, taskID, limit)
	return history, err
}

func (d *DB) ReopenSession(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx,
		"UPDATE sessions SET status='starting', end_time=NULL, start_time=?, pid=NULL WHERE id=? AND status IN ('stopped', 'completed')",
		time.Now(), id)
	return err
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

// HasRecentAISuggestions checks if a session has any AI suggestions (any status) created after the given time.
func (d *DB) HasRecentAISuggestions(ctx context.Context, sessionID string, since time.Time) (bool, error) {
	var count int
	err := d.GetContext(ctx, &count,
		"SELECT COUNT(*) FROM ai_suggestions WHERE session_id = ? AND created_at > ?",
		sessionID, since)
	return count > 0, err
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

// Memory Doc operations
func (d *DB) GetMemoryDoc(ctx context.Context, projectID int64) (*MemoryDoc, error) {
	var doc MemoryDoc
	err := d.GetContext(ctx, &doc, "SELECT * FROM memory_docs WHERE project_id = ?", projectID)
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
	TotalInputTokens  int64   `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
}

// TokenUsageByModel holds per-model summary (aggregated across all sources).
type TokenUsageByModel struct {
	Model             string  `db:"model" json:"model"`
	TotalInputTokens  int64   `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
}

// TokenUsageDaily holds daily aggregated data.
type TokenUsageDaily struct {
	Date              string  `db:"date" json:"date"`
	TotalInputTokens  int64   `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
}

// TokenUsageByProject holds per-project summary.
type TokenUsageByProject struct {
	ProjectID         int64   `db:"project_id" json:"project_id"`
	ProjectName       string  `db:"project_name" json:"project_name"`
	TotalInputTokens  int64   `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
}

// TokenUsageByProjectModel holds per-project per-model breakdown.
type TokenUsageByProjectModel struct {
	ProjectID         int64   `db:"project_id" json:"project_id"`
	ProjectName       string  `db:"project_name" json:"project_name"`
	Model             string  `db:"model" json:"model"`
	TotalInputTokens  int64   `db:"total_input" json:"total_input_tokens"`
	TotalOutputTokens int64   `db:"total_output" json:"total_output_tokens"`
	TotalCostUSD      float64 `db:"total_cost" json:"total_cost_usd"`
	TotalRequests     int64   `db:"total_requests" json:"total_requests"`
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

func (d *DB) UpsertMemoryDoc(ctx context.Context, projectID int64, content, updatedBy string, summary string) (*MemoryDoc, error) {
	query := `INSERT INTO memory_docs (project_id, content, version, last_updated_by, summary)
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
	return d.GetMemoryDoc(ctx, projectID)
}

// Paired device operations

func (d *DB) CreatePairedDevice(ctx context.Context, dev *PairedDevice) error {
	query := `INSERT INTO paired_devices (id, device_name, user_agent, encryption_key, encryption_key_iv)
			  VALUES (?, ?, ?, ?, ?)`
	_, err := d.ExecContext(ctx, query, dev.ID, dev.DeviceName, dev.UserAgent, dev.EncryptionKey, dev.EncryptionKeyIV)
	return err
}

func (d *DB) GetPairedDevice(ctx context.Context, id string) (*PairedDevice, error) {
	var dev PairedDevice
	err := d.GetContext(ctx, &dev, "SELECT * FROM paired_devices WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &dev, nil
}

func (d *DB) ListPairedDevices(ctx context.Context) ([]PairedDevice, error) {
	var devices []PairedDevice
	err := d.SelectContext(ctx, &devices, "SELECT * FROM paired_devices ORDER BY created_at DESC")
	return devices, err
}

func (d *DB) UpdatePairedDeviceLastSeen(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "UPDATE paired_devices SET last_seen_at = CURRENT_TIMESTAMP WHERE id = ?", id)
	return err
}

func (d *DB) RevokePairedDevice(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "UPDATE paired_devices SET revoked = 1 WHERE id = ?", id)
	return err
}

func (d *DB) DeletePairedDevice(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM paired_devices WHERE id = ?", id)
	return err
}
