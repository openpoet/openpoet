package jsonlview

import "time"

// SessionEvent is a parsed JSONL event sent to the frontend.
type SessionEvent struct {
	Type       string        `json:"type"` // "user", "assistant", "progress"
	UUID       string        `json:"uuid"`
	ParentUUID string        `json:"parent_uuid,omitempty"`
	Timestamp  time.Time     `json:"timestamp"`
	SessionID  string        `json:"session_id"`
	Message    *EventMessage `json:"message,omitempty"`
	Progress   *ProgressData `json:"progress,omitempty"`
}

// EventMessage represents a parsed message from the JSONL.
type EventMessage struct {
	Role          string         `json:"role"` // "user", "assistant"
	Model         string         `json:"model,omitempty"`
	MessageID     string         `json:"message_id,omitempty"`
	ContentBlocks []ContentBlock `json:"content_blocks"`
	Usage         *TokenUsage    `json:"usage,omitempty"`
	StopReason    string         `json:"stop_reason,omitempty"`
}

// ContentBlock is a single block within a message.
type ContentBlock struct {
	Type      string `json:"type"` // "text", "tool_use", "tool_result", "thinking"
	Text      string `json:"text,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolID    string `json:"tool_id,omitempty"`
	ToolInput any    `json:"tool_input,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// TokenUsage holds token counts from a message.
type TokenUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

// ProgressData holds progress event data.
type ProgressData struct {
	Type      string `json:"type,omitempty"`
	HookEvent string `json:"hook_event,omitempty"`
	HookName  string `json:"hook_name,omitempty"`
	Command   string `json:"command,omitempty"`
}
