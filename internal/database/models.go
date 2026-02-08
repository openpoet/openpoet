package database

import (
	"database/sql"
	"time"
)

type Project struct {
	ID                     int64          `db:"id" json:"id"`
	Name                   string         `db:"name" json:"name"`
	Path                   string         `db:"path" json:"path"`
	Type                   string         `db:"type" json:"type"` // 'local' or 'remote'
	SSHHost                sql.NullString `db:"ssh_host" json:"ssh_host,omitempty"`
	SSHPort                sql.NullInt64  `db:"ssh_port" json:"ssh_port,omitempty"`
	SSHUser                sql.NullString `db:"ssh_user" json:"ssh_user,omitempty"`
	SSHAuthType            sql.NullString `db:"ssh_auth_type" json:"ssh_auth_type,omitempty"` // 'password', 'key', 'key_passphrase'
	SSHCredentialEncrypted sql.NullString `db:"ssh_credential_encrypted" json:"-"`
	SSHCredentialIV        sql.NullString `db:"ssh_credential_iv" json:"-"`
	ConfigSyncedAt         sql.NullTime   `db:"config_synced_at" json:"config_synced_at,omitempty"`
	CreatedAt              time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt              time.Time      `db:"updated_at" json:"updated_at"`
}

type ProjectInput struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	SSHHost       string `json:"ssh_host,omitempty"`
	SSHPort       int    `json:"ssh_port,omitempty"`
	SSHUser       string `json:"ssh_user,omitempty"`
	SSHAuthType   string `json:"ssh_auth_type,omitempty"`
	SSHCredential string `json:"ssh_credential,omitempty"`
}

type Session struct {
	ID        string       `db:"id" json:"id"`
	ProjectID int64        `db:"project_id" json:"project_id"`
	Status    string       `db:"status" json:"status"` // 'starting', 'running', 'stopped', 'error', 'completed'
	PID       sql.NullInt64 `db:"pid" json:"pid,omitempty"`
	StartTime time.Time    `db:"start_time" json:"start_time"`
	EndTime   sql.NullTime `db:"end_time" json:"end_time,omitempty"`
}

type Macro struct {
	ID          int64  `db:"id" json:"id"`
	Name        string `db:"name" json:"name"`
	Description string `db:"description" json:"description"`
	Script      string `db:"script" json:"script"`
	TargetType  string `db:"target_type" json:"target_type"` // 'local', 'remote', 'any'
	IsBuiltin   bool   `db:"is_builtin" json:"is_builtin"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
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
	ID        int64        `db:"id" json:"id"`
	ProjectID int64        `db:"project_id" json:"project_id"`
	SkillID   sql.NullInt64 `db:"skill_id" json:"skill_id,omitempty"`
	FileName  string       `db:"file_name" json:"file_name"`
	SyncedAt  time.Time    `db:"synced_at" json:"synced_at"`
}

type SkillVersion struct {
	ID        int64     `db:"id" json:"id"`
	SkillID   int64     `db:"skill_id" json:"skill_id"`
	Name      string    `db:"name" json:"name"`
	Content   string    `db:"content" json:"content"`
	Version   int       `db:"version" json:"version"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
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
	Read      bool      `db:"read" json:"read"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type AIConversation struct {
	ID        int64     `db:"id" json:"id"`
	Title     string    `db:"title" json:"title"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type AIMessage struct {
	ID             int64     `db:"id" json:"id"`
	ConversationID int64     `db:"conversation_id" json:"conversation_id"`
	Role           string    `db:"role" json:"role"` // 'user', 'assistant'
	Content        string    `db:"content" json:"content"`
	ToolCalls      string    `db:"tool_calls" json:"tool_calls"` // JSON array
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}
