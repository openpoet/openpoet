package llm

import "context"

// Provider abstracts different LLM backends.
type Provider interface {
	// StreamMessage sends a streaming request, calling callback for each event.
	StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error)
	// Name returns the provider identifier ("apikey", "gosdk", or "nodesdk").
	Name() string
}

// SessionProvider is an optional interface for providers that manage their own
// conversation state and tool execution (e.g. Agent SDK providers).
// When a provider implements this, ai.go will:
//   - Send only the latest user message (not full history) for active sessions
//   - Skip the tool loop (provider handles tools internally via MCP)
//   - Not send tool definitions in the request
type SessionProvider interface {
	Provider
	// HasActiveSession returns true if a session exists for the given conversation.
	HasActiveSession(conversationID int64) bool
	// CloseSession terminates the session for a conversation.
	CloseSession(conversationID int64) error
	// CloseAllSessions terminates all active sessions (used during shutdown).
	CloseAllSessions()
}

// ToolExecutor executes OpenPoet tools. Shared between ai.go tool loop
// and SDK providers' in-process MCP servers.
type ToolExecutor interface {
	ExecuteTool(ctx context.Context, name string, input map[string]any, conversationID int64) (string, error)
}
