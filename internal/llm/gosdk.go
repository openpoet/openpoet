package llm

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// GoSDKProvider wraps the community Go Agent SDK (severity1/claude-agent-sdk-go)
// to provide session-managed, tool-capable AI interactions.
// It uses WithClient for persistent connections and CreateSDKMcpServer for
// in-process DevManager tool execution.
type GoSDKProvider struct {
	apiURL       string
	toolExecutor ToolExecutor
	sessions     map[int64]*goSDKSession
	mu           sync.RWMutex
}

type goSDKSession struct {
	client    claudecode.Client
	sessionID string // Claude Code internal session ID
	cancel    context.CancelFunc
}

// NewGoSDKProvider creates a new Go Agent SDK provider.
// If toolExecutor is nil, tools will not be available.
func NewGoSDKProvider(apiURL string, toolExecutor ...ToolExecutor) *GoSDKProvider {
	p := &GoSDKProvider{
		apiURL:   apiURL,
		sessions: make(map[int64]*goSDKSession),
	}
	if len(toolExecutor) > 0 {
		p.toolExecutor = toolExecutor[0]
	}
	return p
}

// SetToolExecutor sets the tool executor after creation (for lazy wiring).
func (p *GoSDKProvider) SetToolExecutor(executor ToolExecutor) {
	p.toolExecutor = executor
}

// Name returns the provider identifier.
func (p *GoSDKProvider) Name() string {
	return "gosdk"
}

// buildMCPServer creates an in-process MCP server with all DevManager tools.
func (p *GoSDKProvider) buildMCPServer(convID int64) *claudecode.McpSdkServerConfig {
	if p.toolExecutor == nil {
		return nil
	}

	var tools []*claudecode.McpTool
	for _, td := range ChatTools() {
		toolName := td.Name
		tools = append(tools, claudecode.NewTool(
			toolName,
			td.Description,
			ConvertInputSchema(td.InputSchema),
			func(ctx context.Context, args map[string]any) (*claudecode.McpToolResult, error) {
				result, err := p.toolExecutor.ExecuteTool(ctx, toolName, args, convID)
				if err != nil {
					return &claudecode.McpToolResult{
						Content: []claudecode.McpContent{{Type: "text", Text: "Error: " + err.Error()}},
					}, nil
				}
				return &claudecode.McpToolResult{
					Content: []claudecode.McpContent{{Type: "text", Text: result}},
				}, nil
			},
		))
	}

	return claudecode.CreateSDKMcpServer("devmanager", "1.0.0", tools...)
}

// buildOptions creates the SDK options for a query.
func (p *GoSDKProvider) buildOptions(req *Request, convID int64) []claudecode.Option {
	opts := []claudecode.Option{
		claudecode.WithPermissionMode(claudecode.PermissionModeBypassPermissions),
		claudecode.WithMaxTurns(15),
	}

	// Model
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	opts = append(opts, claudecode.WithModel(model))

	// System prompt
	if req.System != "" {
		opts = append(opts, claudecode.WithSystemPrompt(req.System))
	}

	// MCP server with DevManager tools
	mcpServer := p.buildMCPServer(convID)
	if mcpServer != nil {
		opts = append(opts, claudecode.WithSdkMcpServer("devmanager", mcpServer))
		opts = append(opts, claudecode.WithAllowedTools("mcp__devmanager__*"))
	}

	return opts
}

// StreamMessage sends a message via the Go Agent SDK.
// For conversations with active sessions, it resumes the session.
// For new conversations, it creates a new query.
func (p *GoSDKProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	convID := req.ConversationID

	// Get the latest user message text
	var prompt string
	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]
		prompt = lastMsg.TextContent()
	}
	if prompt == "" {
		return nil, fmt.Errorf("no message to send")
	}

	// Build options
	opts := p.buildOptions(req, convID)

	// Resume existing session if available
	p.mu.RLock()
	session, hasSession := p.sessions[convID]
	p.mu.RUnlock()

	if hasSession && session.sessionID != "" {
		opts = append(opts, claudecode.WithResume(session.sessionID))
	}

	// Use Query() for one-shot with auto-cleanup
	iter, err := claudecode.Query(ctx, prompt, opts...)
	if err != nil {
		return nil, fmt.Errorf("gosdk query error: %w", err)
	}
	defer iter.Close()

	// Emit content_block_start for the first text block
	blockStarted := false
	var fullText strings.Builder
	var response Response
	response.StopReason = "end_turn"

	for {
		msg, err := iter.Next(ctx)
		if err == claudecode.ErrNoMoreMessages {
			break
		}
		if err != nil {
			// Emit stop event before returning error
			if blockStarted {
				callback(StreamEvent{Type: "content_block_stop", Index: 0})
			}
			return nil, fmt.Errorf("gosdk stream error: %w", err)
		}

		switch m := msg.(type) {
		case *claudecode.AssistantMessage:
			// Check for errors
			if m.HasError() {
				errMsg := string(m.GetError())
				return nil, fmt.Errorf("assistant error: %s", errMsg)
			}

			// Process content blocks
			for _, block := range m.Content {
				switch b := block.(type) {
				case *claudecode.TextBlock:
					if !blockStarted {
						callback(StreamEvent{
							Type:         "content_block_start",
							Index:        0,
							ContentBlock: &ContentBlock{Type: "text", Text: ""},
						})
						blockStarted = true
					}
					callback(StreamEvent{
						Type:  "content_block_delta",
						Index: 0,
						Delta: &StreamDelta{Type: "text_delta", Text: b.Text},
					})
					fullText.WriteString(b.Text)

				case *claudecode.ToolUseBlock:
					// Tool execution — emit SSE events for frontend
					callback(StreamEvent{
						Type:         "content_block_start",
						Index:        0,
						ContentBlock: &ContentBlock{Type: "tool_use", ID: b.ToolUseID, Name: b.Name},
					})
				}
			}

			// Extract model
			if m.Model != "" {
				response.Model = m.Model
			}

		case *claudecode.SystemMessage:
			// Capture session ID from init message
			if m.Subtype == "init" {
				if sid, ok := m.Data["session_id"].(string); ok {
					p.mu.Lock()
					p.sessions[convID] = &goSDKSession{sessionID: sid}
					p.mu.Unlock()
					response.SessionID = sid
				}
			}

		case *claudecode.ResultMessage:
			// Final result — extract usage and cost
			if m.SessionID != "" {
				p.mu.Lock()
				p.sessions[convID] = &goSDKSession{sessionID: m.SessionID}
				p.mu.Unlock()
				response.SessionID = m.SessionID
			}

			if m.TotalCostUSD != nil {
				response.CostUSD = *m.TotalCostUSD
			}

			if m.Usage != nil {
				usage := *m.Usage
				if v, ok := usage["input_tokens"].(float64); ok {
					response.Usage.InputTokens = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					response.Usage.OutputTokens = int(v)
				}
				if v, ok := usage["cache_read_input_tokens"].(float64); ok {
					response.Usage.CacheReadTokens = int(v)
				}
				if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
					response.Usage.CacheCreationTokens = int(v)
				}
			}

			if m.IsError {
				errText := "unknown error"
				if m.Result != nil {
					errText = *m.Result
				}
				if blockStarted {
					callback(StreamEvent{Type: "content_block_stop", Index: 0})
				}
				return nil, fmt.Errorf("gosdk result error: %s", errText)
			}
		}
	}

	// Close the text block
	if blockStarted {
		callback(StreamEvent{Type: "content_block_stop", Index: 0})
	}

	// Emit message_delta with stop_reason
	callback(StreamEvent{
		Type:  "message_delta",
		Delta: &StreamDelta{StopReason: "end_turn"},
	})

	response.Content = []ContentBlock{{Type: "text", Text: fullText.String()}}
	return &response, nil
}

// HasActiveSession returns true if a session exists for the given conversation.
func (p *GoSDKProvider) HasActiveSession(conversationID int64) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, exists := p.sessions[conversationID]
	return exists
}

// CloseSession terminates the session for a conversation.
func (p *GoSDKProvider) CloseSession(conversationID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if session, ok := p.sessions[conversationID]; ok {
		if session.cancel != nil {
			session.cancel()
		}
		delete(p.sessions, conversationID)
	}
	return nil
}

// CloseAllSessions terminates all active sessions.
func (p *GoSDKProvider) CloseAllSessions() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, session := range p.sessions {
		if session.cancel != nil {
			session.cancel()
		}
		delete(p.sessions, id)
	}
	log.Printf("[GoSDK] Closed all sessions")
}

// IsClaudeCLIAvailable checks if the Claude Code CLI is installed.
func IsClaudeCLIAvailable() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}
