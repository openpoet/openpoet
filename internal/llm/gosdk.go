package llm

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

const (
	// oneShotMaxRetries is the number of retry attempts for retryable one-shot errors.
	oneShotMaxRetries = 3
	// oneShotInitialBackoff is the initial wait time before the first retry.
	oneShotInitialBackoff = 2 * time.Second
	// oneShotMaxBackoff caps the exponential backoff.
	oneShotMaxBackoff = 10 * time.Second
)

// isRetryableOneShotError checks if a one-shot error is retryable.
// The SDK returns "unknown message type: <type>" for events it doesn't recognize
// (e.g., rate_limit_event). These are non-fatal from the API perspective but
// permanently kill the SDK's iterator, so we must retry the entire query.
func isRetryableOneShotError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "unknown message type")
}

// GoSDKProvider wraps the community Go Agent SDK (severity1/claude-agent-sdk-go)
// to provide AI interactions in two modes:
//
// 1. ONE-SHOT (ConversationID == 0): Uses claudecode.Query() for isolated tasks
//   - Spawns a CLI process, sends prompt, gets result, process exits
//   - No MCP tools, no session persistence, no budget control
//   - Used for: skill generation, validation, session evaluation
//
// 2. INTERACTIVE (ConversationID > 0): Uses persistent Client per conversation
//   - CLI subprocess stays alive across multiple messages
//   - Context maintained in-memory (no disk I/O between messages)
//   - MCP server with OpenPoet tools, budget control, session resume
//   - Used for: AI assistant chat panel
type GoSDKProvider struct {
	apiURL              string
	budgetUSD           float64 // max budget per interactive query (0 = unlimited)
	toolExecutor        ToolExecutor
	customToolsProvider CustomToolsProvider // provides custom project tools for a conversation
	extraEnv            map[string]string   // extra env vars for the Claude CLI subprocess
	providerName        string              // override Name() return value (e.g. "ollama-sdk")
	sessions            map[int64]*goSDKSession
	mu                  sync.RWMutex
}

// CustomToolsProvider returns custom tool definitions for a given conversation ID.
// Used by GoSDKProvider to inject project-specific tools into the MCP server.
type CustomToolsProvider interface {
	GetCustomToolsForConversation(conversationID int64) []ToolDefinition
}

// goSDKSession holds a persistent Client connection for an interactive conversation.
// The client stays alive across multiple messages, maintaining context in-memory.
type goSDKSession struct {
	client    claudecode.Client
	sessionID string             // Claude Code internal session ID (for resume after restart)
	cancel    context.CancelFunc // cancels the persistent context
	msgChan   <-chan claudecode.Message
}

// NewGoSDKProvider creates a new Go Agent SDK provider.
// If toolExecutor is nil, tools will not be available until SetToolExecutor is called.
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

// SetCustomToolsProvider sets the provider for custom project tools.
func (p *GoSDKProvider) SetCustomToolsProvider(provider CustomToolsProvider) {
	p.customToolsProvider = provider
}

// SetBudgetUSD sets the maximum budget in USD per interactive query.
func (p *GoSDKProvider) SetBudgetUSD(budget float64) {
	p.budgetUSD = budget
}

// SetExtraEnv sets extra environment variables for the Claude CLI subprocess.
// Used by ollama-sdk to inject ANTHROPIC_BASE_URL and model overrides.
func (p *GoSDKProvider) SetExtraEnv(env map[string]string) {
	p.extraEnv = env
}

// SetProviderName overrides the name returned by Name().
func (p *GoSDKProvider) SetProviderName(name string) {
	p.providerName = name
}

// Name returns the provider identifier.
func (p *GoSDKProvider) Name() string {
	if p.providerName != "" {
		return p.providerName
	}
	return "gosdk"
}

// buildMCPServer creates an in-process MCP server with all OpenPoet tools.
// Only used for interactive mode (requires control protocol).
func (p *GoSDKProvider) buildMCPServer(convID int64) *claudecode.McpSdkServerConfig {
	if p.toolExecutor == nil {
		return nil
	}

	var tools []*claudecode.McpTool

	// Add built-in chat tools
	allDefs := ChatTools()

	// Add custom project tools if provider is set
	if p.customToolsProvider != nil {
		allDefs = append(allDefs, p.customToolsProvider.GetCustomToolsForConversation(convID)...)
	}

	for _, td := range allDefs {
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

	return claudecode.CreateSDKMcpServer("openpoet", "1.0.0", tools...)
}

// buildOneShotOptions creates SDK options for one-shot queries (no MCP; a USD
// budget cap is applied when one is configured).
func (p *GoSDKProvider) buildOneShotOptions(req *Request) []claudecode.Option {
	opts := []claudecode.Option{
		claudecode.WithPermissionMode(claudecode.PermissionModeBypassPermissions),
		claudecode.WithMaxTurns(3),
		// Capture CLI stderr for diagnostics (rate_limit_event investigation)
		claudecode.WithStderrCallback(func(line string) {
			log.Printf("[GoSDK-stderr] %s", line)
		}),
	}

	// Inject extra env vars (e.g. ANTHROPIC_BASE_URL for Ollama)
	if len(p.extraEnv) > 0 {
		opts = append(opts, claudecode.WithEnv(p.extraEnv))
	}

	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	opts = append(opts, claudecode.WithModel(model))

	// Defense-in-depth: cap one-shot spend at the same USD ceiling as interactive
	// clients when a budget is configured. One-shot queries (e.g. the coordinator
	// brain consult) are bounded by MaxTokens, but a hard USD cap prevents a
	// runaway from ever exceeding the operator's budget.
	if p.budgetUSD > 0 {
		opts = append(opts, claudecode.WithMaxBudgetUSD(p.budgetUSD))
	}

	if req.System != "" {
		opts = append(opts, claudecode.WithSystemPrompt(req.System))
	}

	// Safety: disallow all built-in tools in one-shot mode.
	// One-shot queries are pure text generation — no tool use needed.
	opts = append(opts, claudecode.WithDisallowedTools(
		"Bash", "Read", "Write", "Edit", "Glob", "Grep",
		"WebFetch", "WebSearch", "Task", "NotebookEdit",
		"EnterPlanMode", "ExitPlanMode", "AskUserQuestion",
		"TodoRead", "TodoWrite", "Skill",
		"TaskCreate", "TaskGet", "TaskUpdate", "TaskList",
	))

	return opts
}

// buildInteractiveOptions creates SDK options for persistent interactive clients.
// Includes MCP server with OpenPoet tools, budget control, and tool whitelisting.
func (p *GoSDKProvider) buildInteractiveOptions(req *Request, convID int64) []claudecode.Option {
	opts := []claudecode.Option{
		claudecode.WithPermissionMode(claudecode.PermissionModeBypassPermissions),
		claudecode.WithMaxTurns(15),
	}

	// Inject extra env vars (e.g. ANTHROPIC_BASE_URL for Ollama)
	if len(p.extraEnv) > 0 {
		opts = append(opts, claudecode.WithEnv(p.extraEnv))
	}

	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	opts = append(opts, claudecode.WithModel(model))

	if p.budgetUSD > 0 {
		opts = append(opts, claudecode.WithMaxBudgetUSD(p.budgetUSD))
	}

	if req.System != "" {
		opts = append(opts, claudecode.WithSystemPrompt(req.System))
	}

	// MCP server with OpenPoet tools — ONLY allow these, disable built-in tools.
	// WithAllowedTools pre-approves MCP tools for auto-execution.
	// WithDisallowedTools blocks all built-in Claude Code tools (Bash, Read, Write, etc.)
	// so the AI assistant can only use OpenPoet's MCP tools.
	mcpServer := p.buildMCPServer(convID)
	if mcpServer != nil {
		opts = append(opts, claudecode.WithSdkMcpServer("openpoet", mcpServer))
		opts = append(opts, claudecode.WithAllowedTools("mcp__openpoet__*"))
		opts = append(opts, claudecode.WithDisallowedTools(
			"Bash", "Read", "Write", "Edit", "Glob", "Grep",
			"WebFetch", "WebSearch", "Task", "NotebookEdit",
			"EnterPlanMode", "ExitPlanMode", "AskUserQuestion",
			"TodoRead", "TodoWrite", "Skill",
			"TaskCreate", "TaskGet", "TaskUpdate", "TaskList",
		))
	}

	return opts
}

// StreamMessage sends a message via the Go Agent SDK.
//
// Mode selection is automatic based on ConversationID:
//   - ConversationID == 0: One-shot mode (isolated task, no session persistence)
//   - ConversationID > 0: Interactive mode (persistent client, MCP tools, context)
func (p *GoSDKProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	// Get the latest user message text
	var prompt string
	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]
		prompt = lastMsg.TextContent()
	}
	if prompt == "" {
		return nil, fmt.Errorf("no message to send")
	}

	if req.ConversationID == 0 {
		return p.streamOneShot(ctx, prompt, req, callback)
	}
	return p.streamInteractive(ctx, prompt, req, callback)
}

// ---------- ONE-SHOT MODE ----------

// oneShotResult holds the output of a single one-shot attempt.
type oneShotResult struct {
	response     Response
	fullText     string
	blockStarted bool
}

// streamOneShot handles isolated queries using claudecode.Query() with retry logic.
// If the SDK encounters a retryable error (e.g., "unknown message type: rate_limit_event"),
// the entire query is retried with exponential backoff since the SDK iterator is permanently
// killed by such errors and cannot be resumed.
func (p *GoSDKProvider) streamOneShot(ctx context.Context, prompt string, req *Request, callback StreamCallback) (*Response, error) {
	var lastErr error
	backoff := oneShotInitialBackoff

	for attempt := 0; attempt <= oneShotMaxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[GoSDK] One-shot retry %d/%d after %v (previous error: %v)", attempt, oneShotMaxRetries, backoff, lastErr)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("gosdk one-shot cancelled during retry: %w", ctx.Err())
			case <-time.After(backoff):
			}

			backoff = backoff * 2
			if backoff > oneShotMaxBackoff {
				backoff = oneShotMaxBackoff
			}
		}

		log.Printf("[GoSDK] One-shot attempt %d/%d: model=%s system_len=%d prompt_len=%d",
			attempt+1, oneShotMaxRetries+1, req.Model, len(req.System), len(prompt))

		result, err := p.executeOneShot(ctx, prompt, req, callback)
		if err == nil {
			// Success — finalize stream events
			if result.blockStarted {
				callback(StreamEvent{Type: "content_block_stop", Index: 0})
			}
			callback(StreamEvent{
				Type:  "message_delta",
				Delta: &StreamDelta{StopReason: "end_turn"},
			})
			result.response.Content = []ContentBlock{{Type: "text", Text: result.fullText}}
			log.Printf("[GoSDK] One-shot completed: model=%s text_len=%d attempt=%d",
				result.response.Model, len(result.fullText), attempt+1)
			return &result.response, nil
		}

		// Check if retryable
		if !isRetryableOneShotError(err) {
			// Non-retryable: clean up and fail immediately
			if result != nil && result.blockStarted {
				callback(StreamEvent{Type: "content_block_stop", Index: 0})
			}
			log.Printf("[GoSDK] One-shot non-retryable error on attempt %d: %v", attempt+1, err)
			return nil, err
		}

		// Retryable error — log and continue to next attempt
		log.Printf("[GoSDK] One-shot retryable error on attempt %d: %v", attempt+1, err)
		lastErr = err

		// Close any partial stream before retrying
		if result != nil && result.blockStarted {
			callback(StreamEvent{Type: "content_block_stop", Index: 0})
		}
	}

	// All retries exhausted
	return nil, fmt.Errorf("gosdk one-shot failed after %d attempts: %w", oneShotMaxRetries+1, lastErr)
}

// executeOneShot runs a single one-shot query attempt.
// Returns the result on success, or a partial result + error on failure.
func (p *GoSDKProvider) executeOneShot(ctx context.Context, prompt string, req *Request, callback StreamCallback) (*oneShotResult, error) {
	opts := p.buildOneShotOptions(req)

	startTime := time.Now()
	iter, err := claudecode.Query(ctx, prompt, opts...)
	if err != nil {
		return nil, fmt.Errorf("gosdk one-shot error: %w", err)
	}
	defer iter.Close()

	var response Response
	response.StopReason = "end_turn"
	var fullText strings.Builder
	blockStarted := false
	msgCount := 0

	for {
		msg, err := iter.Next(ctx)
		if err != nil {
			if err == claudecode.ErrNoMoreMessages {
				log.Printf("[GoSDK] Iterator exhausted: %d messages received in %v", msgCount, time.Since(startTime).Round(time.Millisecond))
				break
			}
			log.Printf("[GoSDK] Iterator error after %d messages (fullText=%d bytes, elapsed=%v): %v",
				msgCount, fullText.Len(), time.Since(startTime).Round(time.Millisecond), err)

			// The SDK's queryIterator treats ALL errors as fatal and closes the iterator.
			// When the Claude CLI sends message types the SDK doesn't recognize
			// (e.g. "rate_limit_event"), the parser returns a MessageParseError which
			// permanently kills the iterator. If we already received the assistant's
			// response text, treat this as a graceful completion — we only lose the
			// ResultMessage metadata (usage/cost).
			if fullText.Len() > 0 && strings.Contains(err.Error(), "unknown message type") {
				log.Printf("[GoSDK] Ignoring non-fatal parse error (response already received): %v", err)
				break
			}
			return &oneShotResult{
				response:     response,
				fullText:     fullText.String(),
				blockStarted: blockStarted,
			}, fmt.Errorf("gosdk one-shot read error: %w", err)
		}

		msgCount++

		switch m := msg.(type) {
		case *claudecode.AssistantMessage:
			log.Printf("[GoSDK] Message #%d: AssistantMessage (model=%s, blocks=%d, hasError=%v)",
				msgCount, m.Model, len(m.Content), m.HasError())
			if m.HasError() {
				if blockStarted {
					callback(StreamEvent{Type: "content_block_stop", Index: 0})
				}
				return nil, fmt.Errorf("assistant error: %s", string(m.GetError()))
			}
			for _, block := range m.Content {
				if b, ok := block.(*claudecode.TextBlock); ok {
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
				}
			}
			if m.Model != "" {
				response.Model = m.Model
			}

		case *claudecode.ResultMessage:
			log.Printf("[GoSDK] Message #%d: ResultMessage (isError=%v, sessionID=%s, costUSD=%v)",
				msgCount, m.IsError, m.SessionID, m.TotalCostUSD)
			if m.TotalCostUSD != nil {
				response.CostUSD = *m.TotalCostUSD
			}
			p.extractUsage(m, &response)
			if m.IsError {
				errText := "unknown error"
				if m.Result != nil {
					errText = *m.Result
				}
				if blockStarted {
					callback(StreamEvent{Type: "content_block_stop", Index: 0})
				}
				return nil, fmt.Errorf("gosdk one-shot result error: %s", errText)
			}
			// Fallback: if no AssistantMessage text was received, use ResultMessage.Result
			if m.Result != nil && *m.Result != "" && fullText.Len() == 0 {
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
					Delta: &StreamDelta{Type: "text_delta", Text: *m.Result},
				})
				fullText.WriteString(*m.Result)
			}

		default:
			log.Printf("[GoSDK] Message #%d: unexpected type %T", msgCount, msg)
		}
	}

	return &oneShotResult{
		response:     response,
		fullText:     fullText.String(),
		blockStarted: blockStarted,
	}, nil
}

// ---------- INTERACTIVE MODE ----------

// getOrCreateSession returns the existing persistent client for a conversation,
// or creates a new one. If a previous session ID exists (e.g., after server restart),
// it uses WithResume to restore the conversation context.
func (p *GoSDKProvider) getOrCreateSession(ctx context.Context, req *Request, convID int64) (*goSDKSession, error) {
	p.mu.RLock()
	session, exists := p.sessions[convID]
	p.mu.RUnlock()

	if exists && session.client != nil {
		return session, nil
	}

	// Build options for the new interactive client
	opts := p.buildInteractiveOptions(req, convID)

	// Resume previous session if we have a session ID (e.g., after server restart)
	if exists && session != nil && session.sessionID != "" {
		opts = append(opts, claudecode.WithResume(session.sessionID))
		log.Printf("[GoSDK] Resuming session %s for conversation %d", session.sessionID, convID)
	} else if !exists && req.SessionID != "" {
		// Fallback: resume using session ID persisted in the database (survives server restarts)
		opts = append(opts, claudecode.WithResume(req.SessionID))
		log.Printf("[GoSDK] Resuming session from DB: %s for conversation %d", req.SessionID, convID)
	}

	// Create a persistent context for this conversation's client
	clientCtx, cancel := context.WithCancel(context.Background())

	// Create and connect the client
	client := claudecode.NewClient(opts...)
	if err := client.Connect(clientCtx); err != nil {
		cancel()
		return nil, fmt.Errorf("gosdk connect error: %w", err)
	}

	// Get the message channel (shared across all queries on this client)
	msgChan := client.ReceiveMessages(clientCtx)

	newSession := &goSDKSession{
		client:  client,
		cancel:  cancel,
		msgChan: msgChan,
	}

	// Preserve session ID if we had one
	if exists && session != nil {
		newSession.sessionID = session.sessionID
	}

	p.mu.Lock()
	p.sessions[convID] = newSession
	p.mu.Unlock()

	log.Printf("[GoSDK] Created persistent client for conversation %d", convID)
	return newSession, nil
}

// streamInteractive handles chat messages using a persistent Client.
//
// The client stays connected across messages in the same conversation:
//   - Context is maintained in-memory by the CLI subprocess
//   - No process startup/teardown overhead between messages
//   - MCP tools are registered once and stay available
//   - Claude Code handles context compaction automatically
//
// If the persistent client has disconnected (CLI crash, etc.), a new one is created
// with WithResume to restore the conversation from disk.
func (p *GoSDKProvider) streamInteractive(ctx context.Context, prompt string, req *Request, callback StreamCallback) (*Response, error) {
	convID := req.ConversationID

	// Get or create persistent client for this conversation
	session, err := p.getOrCreateSession(ctx, req, convID)
	if err != nil {
		return nil, err
	}

	// Send the prompt via the persistent connection
	if err := session.client.Query(ctx, prompt); err != nil {
		// Client may have disconnected — try to recreate
		log.Printf("[GoSDK] Query failed, recreating client for conversation %d: %v", convID, err)
		p.disconnectSession(convID)
		session, err = p.getOrCreateSession(ctx, req, convID)
		if err != nil {
			return nil, fmt.Errorf("gosdk reconnect error: %w", err)
		}
		if err := session.client.Query(ctx, prompt); err != nil {
			return nil, fmt.Errorf("gosdk query error after reconnect: %w", err)
		}
	}

	// Process response messages until ResultMessage.
	// CRITICAL: Use select with ctx.Done() to break out if context is cancelled
	// (e.g., user stops the stream, timeout). Without this, the for-range loop
	// blocks indefinitely if the Claude Code subprocess stalls.
	blockStarted := false
	var fullText strings.Builder
	var response Response
	response.StopReason = "end_turn"

	for {
		select {
		case <-ctx.Done():
			// Context cancelled (user stopped stream or timeout).
			// Disconnect the session to kill the underlying claude subprocess.
			if blockStarted {
				callback(StreamEvent{Type: "content_block_stop", Index: 0})
			}
			p.disconnectSession(convID)
			return nil, fmt.Errorf("gosdk: stream cancelled: %w", ctx.Err())

		case msg, ok := <-session.msgChan:
			if !ok {
				// Channel closed — client disconnected
				if blockStarted {
					callback(StreamEvent{Type: "content_block_stop", Index: 0})
				}
				p.disconnectSession(convID)
				return nil, fmt.Errorf("gosdk: client disconnected during response")
			}
			switch m := msg.(type) {
			case *claudecode.AssistantMessage:
				if m.HasError() {
					errMsg := string(m.GetError())
					if blockStarted {
						callback(StreamEvent{Type: "content_block_stop", Index: 0})
					}
					return nil, fmt.Errorf("assistant error: %s", errMsg)
				}

				// Log all content blocks for debugging tool duplication
				for i, block := range m.Content {
					switch b := block.(type) {
					case *claudecode.TextBlock:
						snippet := b.Text
						if len(snippet) > 80 {
							snippet = snippet[:80] + "..."
						}
						log.Printf("[GoSDK] AssistantMessage block[%d]: TextBlock len=%d snippet=%q", i, len(b.Text), snippet)
					case *claudecode.ToolUseBlock:
						log.Printf("[GoSDK] AssistantMessage block[%d]: ToolUseBlock id=%s name=%s", i, b.ToolUseID, b.Name)
					default:
						log.Printf("[GoSDK] AssistantMessage block[%d]: %T", i, block)
					}
				}

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
						callback(StreamEvent{
							Type:         "content_block_start",
							Index:        0,
							ContentBlock: &ContentBlock{Type: "tool_use", ID: b.ToolUseID, Name: b.Name},
						})
					}
				}

				if m.Model != "" {
					response.Model = m.Model
				}

			case *claudecode.SystemMessage:
				if m.Subtype == "init" {
					if sid, ok := m.Data["session_id"].(string); ok {
						p.mu.Lock()
						if s, ok := p.sessions[convID]; ok {
							s.sessionID = sid
						}
						p.mu.Unlock()
						response.SessionID = sid
					}
				}

			case *claudecode.ResultMessage:
				// Final result for this query — extract data and STOP reading
				if m.SessionID != "" {
					p.mu.Lock()
					if s, ok := p.sessions[convID]; ok {
						s.sessionID = m.SessionID
					}
					p.mu.Unlock()
					response.SessionID = m.SessionID
				}

				if m.TotalCostUSD != nil {
					response.CostUSD = *m.TotalCostUSD
				}
				p.extractUsage(m, &response)

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

				// ResultMessage marks the end of this query's response.
				// STOP reading — the client stays alive for the next message.
				goto done
			}
		}
	}

done:
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

// ---------- SHARED HELPERS ----------

// extractUsage populates response usage fields from a ResultMessage.
func (p *GoSDKProvider) extractUsage(m *claudecode.ResultMessage, response *Response) {
	if m.Usage == nil {
		return
	}
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

// ---------- SESSION MANAGEMENT ----------

// disconnectSession gracefully disconnects a client and removes it from sessions.
// The session ID is preserved for potential resume via WithResume.
func (p *GoSDKProvider) disconnectSession(conversationID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if session, ok := p.sessions[conversationID]; ok {
		if session.client != nil {
			session.client.Disconnect()
		}
		if session.cancel != nil {
			session.cancel()
		}
		// Keep the session ID for potential resume, but clear the client
		p.sessions[conversationID] = &goSDKSession{sessionID: session.sessionID}
	}
}

// DisconnectSession disconnects the client but preserves the session ID for resume.
// Forces the MCP server to be rebuilt on the next query.
func (p *GoSDKProvider) DisconnectSession(conversationID int64) {
	p.disconnectSession(conversationID)
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
		if session.client != nil {
			session.client.Disconnect()
		}
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
		if session.client != nil {
			session.client.Disconnect()
		}
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
