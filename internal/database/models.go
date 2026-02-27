package database

import (
	"database/sql"
	"time"
)

type Project struct {
	ID                         int64          `db:"id" json:"id"`
	Name                       string         `db:"name" json:"name"`
	Path                       string         `db:"path" json:"path"`
	Type                       string         `db:"type" json:"type"` // 'local' or 'remote'
	SSHHost                    sql.NullString `db:"ssh_host" json:"ssh_host,omitempty"`
	SSHPort                    sql.NullInt64  `db:"ssh_port" json:"ssh_port,omitempty"`
	SSHUser                    sql.NullString `db:"ssh_user" json:"ssh_user,omitempty"`
	SSHAuthType                sql.NullString `db:"ssh_auth_type" json:"ssh_auth_type,omitempty"` // 'password', 'key', 'key_passphrase'
	SSHCredentialEncrypted     sql.NullString `db:"ssh_credential_encrypted" json:"-"`
	SSHCredentialIV            sql.NullString `db:"ssh_credential_iv" json:"-"`
	HasCredential              bool           `db:"-" json:"has_credential"`
	ToolPolicy                 string         `db:"tool_policy" json:"tool_policy,omitempty"`   // JSON ToolPolicy
	SkillPolicy                string         `db:"skill_policy" json:"skill_policy,omitempty"` // '' = inherit global, 'custom' = per-project
	DangerouslySkipPermissions bool           `db:"dangerously_skip_permissions" json:"dangerously_skip_permissions"`
	Backend                    string         `db:"backend" json:"backend"`               // 'claude_code' or 'copilot'
	BackendConfig              string         `db:"backend_config" json:"backend_config"` // JSON blob for backend-specific settings
	ConfigSyncedAt             sql.NullTime   `db:"config_synced_at" json:"config_synced_at,omitempty"`
	CreatedAt                  time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt                  time.Time      `db:"updated_at" json:"updated_at"`
}

type ProjectInput struct {
	Name                       string `json:"name"`
	Path                       string `json:"path"`
	Type                       string `json:"type"`
	SSHHost                    string `json:"ssh_host,omitempty"`
	SSHPort                    int    `json:"ssh_port,omitempty"`
	SSHUser                    string `json:"ssh_user,omitempty"`
	SSHAuthType                string `json:"ssh_auth_type,omitempty"`
	SSHCredential              string `json:"ssh_credential,omitempty"`
	ToolPolicy                 string `json:"tool_policy,omitempty"`
	SkillPolicy                string `json:"skill_policy,omitempty"`
	DangerouslySkipPermissions bool   `json:"dangerously_skip_permissions"`
	Backend                    string `json:"backend,omitempty"`
	BackendConfig              string `json:"backend_config,omitempty"`
}

type Session struct {
	ID              string        `db:"id" json:"id"`
	ProjectID       int64         `db:"project_id" json:"project_id"`
	Status          string        `db:"status" json:"status"` // 'starting', 'running', 'stopped', 'error', 'completed'
	PID             sql.NullInt64 `db:"pid" json:"pid,omitempty"`
	Name            string        `db:"name" json:"name"`
	TaskID          sql.NullInt64 `db:"task_id" json:"task_id,omitempty"`
	StartTime       time.Time     `db:"start_time" json:"start_time"`
	EndTime         sql.NullTime  `db:"end_time" json:"end_time,omitempty"`
	LastActivityAt  sql.NullTime  `db:"last_activity_at" json:"last_activity_at,omitempty"`
	PlanContent     string        `db:"plan_content" json:"plan_content"`
	PlanUpdatedAt   sql.NullTime  `db:"plan_updated_at" json:"plan_updated_at,omitempty"`
	Backend         string        `db:"backend" json:"backend"` // 'claude_code' or 'copilot'
	SkipPermissions bool          `db:"skip_permissions" json:"skip_permissions"`
}

type Skill struct {
	ID        int64     `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Content   string    `db:"content" json:"content"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	Category  string    `db:"category" json:"category"`
	SortOrder int       `db:"sort_order" json:"sort_order"`
	SyncCount int       `db:"sync_count" json:"sync_count"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type SyncedSkillFile struct {
	ID        int64         `db:"id" json:"id"`
	ProjectID int64         `db:"project_id" json:"project_id"`
	SkillID   sql.NullInt64 `db:"skill_id" json:"skill_id,omitempty"`
	FileName  string        `db:"file_name" json:"file_name"`
	SyncedAt  time.Time     `db:"synced_at" json:"synced_at"`
}

type SkillVersion struct {
	ID        int64     `db:"id" json:"id"`
	SkillID   int64     `db:"skill_id" json:"skill_id"`
	Name      string    `db:"name" json:"name"`
	Content   string    `db:"content" json:"content"`
	Version   int       `db:"version" json:"version"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type ProjectSkillConfig struct {
	ID        int64 `db:"id" json:"id"`
	ProjectID int64 `db:"project_id" json:"project_id"`
	SkillID   int64 `db:"skill_id" json:"skill_id"`
	Enabled   bool  `db:"enabled" json:"enabled"`
}

type ProjectSkill struct {
	ID        int64     `db:"id" json:"id"`
	ProjectID int64     `db:"project_id" json:"project_id"`
	Name      string    `db:"name" json:"name"`
	Content   string    `db:"content" json:"content"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	Category  string    `db:"category" json:"category"`
	SortOrder int       `db:"sort_order" json:"sort_order"`
	SyncCount int       `db:"sync_count" json:"sync_count"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type ProjectShare struct {
	ID              int64     `db:"id" json:"id"`
	ProjectID       int64     `db:"project_id" json:"project_id"`
	SharedProjectID int64     `db:"shared_project_id" json:"shared_project_id"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

type MCPServer struct {
	ID        int64     `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Command   string    `db:"command" json:"command"`
	Args      string    `db:"args" json:"args"` // JSON array
	Env       string    `db:"env" json:"env"`   // JSON object
	Enabled   bool      `db:"enabled" json:"enabled"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type ProjectMCPServer struct {
	ID        int64     `db:"id" json:"id"`
	ProjectID int64     `db:"project_id" json:"project_id"`
	Name      string    `db:"name" json:"name"`
	Command   string    `db:"command" json:"command"`
	Args      string    `db:"args" json:"args"` // JSON array
	Env       string    `db:"env" json:"env"`   // JSON object
	Enabled   bool      `db:"enabled" json:"enabled"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type Setting struct {
	Key   string `db:"key" json:"key"`
	Value string `db:"value" json:"value"` // JSON
}

type PushSubscription struct {
	ID        int64     `db:"id" json:"id"`
	Endpoint  string    `db:"endpoint" json:"endpoint"`
	P256dh    string    `db:"p256dh" json:"p256dh"`
	Auth      string    `db:"auth" json:"auth"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type Notification struct {
	ID        int64     `db:"id" json:"id"`
	SessionID string    `db:"session_id" json:"session_id"`
	Type      string    `db:"type" json:"type"` // 'info', 'warning', 'error', 'question'
	Title     string    `db:"title" json:"title"`
	Body      string    `db:"body" json:"body"`
	Link      string    `db:"link" json:"link"`
	Read      bool      `db:"read" json:"read"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type AIConversation struct {
	ID               int64     `db:"id" json:"id"`
	Title            string    `db:"title" json:"title"`
	Source           string    `db:"source" json:"source"`                       // 'user' or 'ai'
	ProactiveLevel   string    `db:"proactive_level" json:"proactive_level"`     // 'critical', 'standard', 'subtle', or ''
	ProactiveType    string    `db:"proactive_type" json:"proactive_type"`       // 'task_suggestion', 'memory_doc_update', 'insight', 'alert', or ''
	ProactiveContext string    `db:"proactive_context" json:"proactive_context"` // JSON context for system prompt
	IsRead           bool      `db:"is_read" json:"is_read"`
	SessionID        string    `db:"session_id" json:"session_id"` // Claude Code session ID for resume across restarts
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time `db:"updated_at" json:"updated_at"`
}

type AIMessage struct {
	ID             int64     `db:"id" json:"id"`
	ConversationID int64     `db:"conversation_id" json:"conversation_id"`
	Role           string    `db:"role" json:"role"` // 'user', 'assistant'
	Content        string    `db:"content" json:"content"`
	ToolCalls      string    `db:"tool_calls" json:"tool_calls"` // JSON array
	Status         string    `db:"status" json:"status"`         // 'streaming', 'completed', 'error'
	ErrorInfo      string    `db:"error_info" json:"error_info"` // error description when status='error'
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

type TempDocument struct {
	ID             string        `db:"id" json:"id"`
	Title          string        `db:"title" json:"title"`
	Content        string        `db:"content" json:"content"`
	ConversationID sql.NullInt64 `db:"conversation_id" json:"conversation_id,omitempty"`
	TaskID         sql.NullInt64 `db:"task_id" json:"task_id,omitempty"`
	SessionID      string        `db:"session_id" json:"session_id,omitempty"`
	Summary        string        `db:"summary" json:"summary"`
	Status         string        `db:"status" json:"status"`
	MessageID      int64         `db:"message_id" json:"message_id"`
	FeedbackAck    bool          `db:"feedback_ack" json:"feedback_ack"` // true if AI has acknowledged the approval/rejection
	CreatedAt      time.Time     `db:"created_at" json:"created_at"`
}

type ProjectTask struct {
	ID                int64         `db:"id" json:"id"`
	ProjectID         int64         `db:"project_id" json:"project_id"`
	ParentID          sql.NullInt64 `db:"parent_id" json:"parent_id,omitempty"`
	Title             string        `db:"title" json:"title"`
	Description       string        `db:"description" json:"description"`
	Status            string        `db:"status" json:"status"`     // 'todo', 'in_progress', 'awaiting_approval', 'done'
	Priority          string        `db:"priority" json:"priority"` // 'low', 'medium', 'high', 'urgent'
	DueDate           sql.NullTime  `db:"due_date" json:"due_date,omitempty"`
	SortOrder         int           `db:"sort_order" json:"sort_order"`
	GlobalSortOrder   int           `db:"global_sort_order" json:"global_sort_order"`
	DueNotified       bool          `db:"due_notified" json:"due_notified"`
	VerificationDocID string        `db:"verification_doc_id" json:"verification_doc_id"`
	CreatedAt         time.Time     `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time     `db:"updated_at" json:"updated_at"`
}

type TaskHistory struct {
	ID        int64          `db:"id" json:"id"`
	TaskID    int64          `db:"task_id" json:"task_id"`
	ProjectID int64          `db:"project_id" json:"project_id"`
	EventType string         `db:"event_type" json:"event_type"`
	Details   string         `db:"details" json:"details"`
	Actor     string         `db:"actor" json:"actor"`
	SessionID sql.NullString `db:"session_id" json:"session_id,omitempty"`
	CreatedAt time.Time      `db:"created_at" json:"created_at"`
}

type AISuggestion struct {
	ID             int64         `db:"id" json:"id"`
	SessionID      string        `db:"session_id" json:"session_id"`
	ProjectID      int64         `db:"project_id" json:"project_id"`
	Type           string        `db:"type" json:"type"` // 'link_task', 'create_task', 'update_task', 'complete_task', 'unlink_task'
	Title          string        `db:"title" json:"title"`
	Description    string        `db:"description" json:"description"`
	ContextJSON    string        `db:"context_json" json:"context"`
	Status         string        `db:"status" json:"status"` // 'pending', 'accepted', 'dismissed'
	Level          string        `db:"level" json:"level"`   // 'critical', 'standard', 'subtle'
	ConversationID sql.NullInt64 `db:"conversation_id" json:"conversation_id"`
	CreatedAt      time.Time     `db:"created_at" json:"created_at"`
}

type TokenUsage struct {
	ID                  int64          `db:"id" json:"id"`
	Source              string         `db:"source" json:"source"`           // 'ai_assistant', 'claude_code'
	Subcategory         string         `db:"subcategory" json:"subcategory"` // 'chat', 'skill_generate', 'skill_validate', 'meta_eval', 'session_eval'
	ProjectID           sql.NullInt64  `db:"project_id" json:"project_id,omitempty"`
	SessionID           sql.NullString `db:"session_id" json:"session_id,omitempty"`
	ConversationID      sql.NullInt64  `db:"conversation_id" json:"conversation_id,omitempty"`
	Model               string         `db:"model" json:"model"`
	InputTokens         int            `db:"input_tokens" json:"input_tokens"`
	OutputTokens        int            `db:"output_tokens" json:"output_tokens"`
	CacheReadTokens     int            `db:"cache_read_tokens" json:"cache_read_tokens"`
	CacheCreationTokens int            `db:"cache_creation_tokens" json:"cache_creation_tokens"`
	CostUSD             float64        `db:"cost_usd" json:"cost_usd"`
	CreatedAt           time.Time      `db:"created_at" json:"created_at"`
}

type AIConfig struct {
	ID              int64     `db:"id" json:"id"`
	Name            string    `db:"name" json:"name"`
	ProviderType    string    `db:"provider_type" json:"provider_type"`
	APIKeyEncrypted string    `db:"api_key_encrypted" json:"-"`
	APIKeyIV        string    `db:"api_key_iv" json:"-"`
	APIKeyPreview   string    `db:"api_key_preview" json:"api_key_preview"`
	Model           string    `db:"model" json:"model"`
	BaseURL         string    `db:"base_url" json:"base_url"`
	ExtraJSON       string    `db:"extra_json" json:"extra_json"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

type AIConfigAssignment struct {
	Slot     string        `db:"slot" json:"slot"`
	ConfigID sql.NullInt64 `db:"config_id" json:"config_id"`
}

type MemoryDoc struct {
	ID            int64          `db:"id" json:"id"`
	ProjectID     int64          `db:"project_id" json:"project_id"`
	Content       string         `db:"content" json:"content"`
	Version       int            `db:"version" json:"version"`
	LastUpdatedBy string         `db:"last_updated_by" json:"last_updated_by"`
	Summary       sql.NullString `db:"summary" json:"summary,omitempty"`
	CreatedAt     time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time      `db:"updated_at" json:"updated_at"`
}

type PairedDevice struct {
	ID              string    `db:"id" json:"id"`
	DeviceName      string    `db:"device_name" json:"device_name"`
	UserAgent       string    `db:"user_agent" json:"user_agent"`
	EncryptionKey   string    `db:"encryption_key" json:"-"`
	EncryptionKeyIV string    `db:"encryption_key_iv" json:"-"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	LastSeenAt      time.Time `db:"last_seen_at" json:"last_seen_at"`
	Revoked         bool      `db:"revoked" json:"revoked"`
}
