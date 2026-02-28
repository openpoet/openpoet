package acpagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// JSON-RPC 2.0 types

type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ACP protocol types

type initializeParams struct {
	ProtocolVersion int                `json:"protocolVersion"`
	ClientInfo      clientInfo         `json:"clientInfo"`
	Capabilities    clientCapabilities `json:"capabilities"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type clientCapabilities struct {
	Streaming bool `json:"streaming,omitempty"`
}

type initializeResult struct {
	ProtocolVersion   int             `json:"protocolVersion"`
	AgentCapabilities json.RawMessage `json:"agentCapabilities,omitempty"`
	AgentInfo         struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"agentInfo"`
	AuthMethods []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"authMethods,omitempty"`
}

type sessionNewParams struct {
	CWD        string        `json:"cwd"`
	MCPServers []interface{} `json:"mcpServers"`
}

type sessionNewResult struct {
	SessionID string `json:"sessionId"`
}

type sessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type sessionPromptResult struct {
	Content    []contentBlock `json:"content,omitempty"`
	StopReason string         `json:"stopReason,omitempty"`
}

type sessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    sessionUpdate `json:"update"`
}

type sessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content,omitempty"`
	// For tool_call
	ToolCallID string        `json:"toolCallId,omitempty"`
	Function   *toolFunction `json:"function,omitempty"`
	// For plan
	Steps []string `json:"steps,omitempty"`
	// For tool_call_update
	Output string `json:"output,omitempty"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ANSI colors
const (
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorBlue   = "\033[34m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

// CopilotAgent manages the copilot --acp subprocess and JSON-RPC communication.
type CopilotAgent struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu      sync.Mutex
	nextID  int
	pending map[int]chan *jsonrpcMessage

	sessionID string
	workDir   string

	hookURL     string
	hookSessID  string
	autoApprove bool

	// Thinking state — accessed only from readLoop goroutine.
	// Uses single-line overwrite (\r + erase line) to avoid stacking.
	isThinking    bool
	thinkingLines int // total lines of thought accumulated

	done chan struct{}
}

// Run is the entry point for the acp-agent subcommand.
func Run(args []string) {
	fs := flag.NewFlagSet("acp-agent", flag.ExitOnError)
	sessionID := fs.String("session-id", "", "OpenPoet session ID for hook tracking")
	resume := fs.String("resume", "", "Resume existing session (alias for --session-id)")
	systemPrompt := fs.String("system-prompt", "", "System prompt prepended to first message")
	autoApprove := fs.Bool("auto-approve", false, "Auto-approve permission requests")
	copilotPath := fs.String("copilot-path", "", "Override copilot binary path")
	// Legacy flags from generic ACP mode — accepted but ignored
	_ = fs.String("agent-url", "", "Deprecated: ignored")
	_ = fs.String("agent-name", "", "Deprecated: ignored")
	_ = fs.String("mode", "", "Deprecated: ignored")
	_ = fs.String("mcp-config", "", "MCP configuration file path (reserved)")
	fs.Parse(args)

	sid := *sessionID
	if *resume != "" {
		sid = *resume
	}

	hookURL := os.Getenv("OPENPOET_HOOK_URL")
	hookSessID := os.Getenv("OPENPOET_SESSION_ID")
	if hookSessID == "" {
		hookSessID = sid
	}

	// Resolve copilot binary
	binPath := "copilot"
	if *copilotPath != "" {
		binPath = *copilotPath
	}
	if envPath := os.Getenv("COPILOT_CLI_PATH"); envPath != "" {
		binPath = envPath
	}
	resolvedPath, err := exec.LookPath(binPath)
	if err != nil {
		fmt.Printf("\r\n%sError: GitHub Copilot CLI (%s) not found in PATH.%s\r\n", colorRed, binPath, colorReset)
		fmt.Printf("%sInstall: https://docs.github.com/en/copilot/how-tos/copilot-cli/use-copilot-cli%s\r\n", colorGray, colorReset)
		os.Exit(1)
	}

	workDir, _ := os.Getwd()

	// Banner
	fmt.Printf("\r\n%s%sCopilot ACP Agent%s\r\n", colorBold, colorCyan, colorReset)
	fmt.Printf("%sCopilot: %s%s\r\n", colorGray, resolvedPath, colorReset)
	fmt.Printf("%sWorkDir: %s%s\r\n", colorGray, workDir, colorReset)
	fmt.Printf("%sStarting copilot --acp ...%s\r\n\r\n", colorGray, colorReset)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &CopilotAgent{
		workDir:     workDir,
		hookURL:     hookURL,
		hookSessID:  hookSessID,
		autoApprove: *autoApprove,
		pending:     make(map[int]chan *jsonrpcMessage),
		done:        make(chan struct{}),
	}

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Printf("\r\n%sSession terminated.%s\r\n", colorYellow, colorReset)
		if agent.sessionID != "" {
			agent.cancelSession()
		}
		agent.postHookEvent("mode_changed", map[string]string{"mode": "idle"})
		agent.stop()
		cancel()
		os.Exit(0)
	}()

	// Start copilot --acp subprocess
	if err := agent.start(ctx, resolvedPath); err != nil {
		fmt.Printf("%sFailed to start copilot: %s%s\r\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	defer agent.stop()

	// ACP initialization handshake
	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := agent.initialize(initCtx); err != nil {
		initCancel()
		fmt.Printf("%sACP initialization failed: %s%s\r\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	initCancel()

	// Create or load session
	sessCtx, sessCancel := context.WithTimeout(ctx, 30*time.Second)
	if *resume != "" {
		// Resuming — try to load the previous copilot session
		copilotSID := readCopilotSessionID(sid)
		if copilotSID != "" {
			if err := agent.loadSession(sessCtx, copilotSID); err != nil {
				fmt.Printf("%sSession load failed (%v), starting fresh session%s\r\n", colorYellow, err, colorReset)
				if err := agent.newSession(sessCtx); err != nil {
					sessCancel()
					fmt.Printf("%sSession creation failed: %s%s\r\n", colorRed, err, colorReset)
					os.Exit(1)
				}
				saveCopilotSessionID(sid, agent.sessionID)
			}
		} else {
			// No mapping found — create new session
			if err := agent.newSession(sessCtx); err != nil {
				sessCancel()
				fmt.Printf("%sSession creation failed: %s%s\r\n", colorRed, err, colorReset)
				os.Exit(1)
			}
			saveCopilotSessionID(sid, agent.sessionID)
		}
	} else {
		// New session
		if err := agent.newSession(sessCtx); err != nil {
			sessCancel()
			fmt.Printf("%sSession creation failed: %s%s\r\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		saveCopilotSessionID(sid, agent.sessionID)
	}
	sessCancel()

	fmt.Printf("%sSession ready. Type your message and press Enter. Ctrl+C to exit.%s\r\n\r\n", colorGreen, colorReset)
	agent.postHookEvent("mode_changed", map[string]string{"mode": "idle"})

	// REPL — custom split to handle both \r and \n as line terminators.
	// Mobile input sends \r which the PTY may not always translate to \n.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	scanner.Split(scanCRorLF)
	firstMessage := true

	for {
		fmt.Printf("%s❯%s ", colorGreen, colorReset)

		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		if firstMessage && *systemPrompt != "" {
			input = *systemPrompt + "\n\n" + input
			firstMessage = false
		}

		agent.postHookEvent("mode_changed", map[string]string{"mode": "executing"})

		result, err := agent.prompt(ctx, input)
		if err != nil {
			fmt.Printf("\r\n%sError: %s%s\r\n\r\n", colorRed, err, colorReset)
		} else if result != nil && result.StopReason == "cancelled" {
			fmt.Printf("\r\n%s(cancelled)%s\r\n\r\n", colorYellow, colorReset)
		} else {
			fmt.Printf("\r\n")
		}

		agent.postHookEvent("mode_changed", map[string]string{"mode": "idle"})
	}

	fmt.Printf("\r\n%sSession ended.%s\r\n", colorGray, colorReset)
}

// start launches the copilot --acp subprocess with piped stdio.
func (a *CopilotAgent) start(ctx context.Context, binPath string) error {
	a.cmd = exec.CommandContext(ctx, binPath, "--acp")
	a.cmd.Dir = a.workDir
	a.cmd.Stderr = os.Stderr

	// Inherit essential env vars
	a.cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
	)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		a.cmd.Env = append(a.cmd.Env, "GITHUB_TOKEN="+token)
	}

	var err error
	a.stdin, err = a.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	a.stdout, err = a.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := a.cmd.Start(); err != nil {
		return fmt.Errorf("start copilot --acp: %w", err)
	}

	go a.readLoop()
	return nil
}

// stop terminates the copilot subprocess gracefully.
func (a *CopilotAgent) stop() {
	select {
	case <-a.done:
		return // already stopped
	default:
		close(a.done)
	}

	if a.stdin != nil {
		a.stdin.Close()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			time.Sleep(3 * time.Second)
			a.mu.Lock()
			if a.cmd != nil && a.cmd.Process != nil {
				a.cmd.Process.Kill()
			}
			a.mu.Unlock()
		}()
		a.cmd.Wait()
	}
}

// sendRequest sends a JSON-RPC request and waits for the response.
func (a *CopilotAgent) sendRequest(ctx context.Context, method string, params interface{}) (*jsonrpcMessage, error) {
	a.mu.Lock()
	id := a.nextID
	a.nextID++
	ch := make(chan *jsonrpcMessage, 1)
	a.pending[id] = ch
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	msg := jsonrpcMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  paramsJSON,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}

	data = append(data, '\n')
	if _, err := a.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write to copilot stdin: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			detail := ""
			if len(resp.Error.Data) > 0 {
				detail = " (" + string(resp.Error.Data) + ")"
			}
			return resp, fmt.Errorf("RPC error %d: %s%s", resp.Error.Code, resp.Error.Message, detail)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// sendNotification sends a JSON-RPC notification (no response expected).
func (a *CopilotAgent) sendNotification(method string, params interface{}) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return err
	}

	msg := jsonrpcMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsJSON,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	data = append(data, '\n')
	_, err = a.stdin.Write(data)
	return err
}

// sendResponse sends a JSON-RPC response to an incoming request from copilot.
func (a *CopilotAgent) sendResponse(id int, result interface{}) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}

	msg := jsonrpcMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Result:  resultJSON,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	data = append(data, '\n')
	_, err = a.stdin.Write(data)
	return err
}

// readLoop reads NDJSON messages from copilot's stdout and dispatches them.
func (a *CopilotAgent) readLoop() {
	scanner := bufio.NewScanner(a.stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("[acp] JSON parse error: %v (line: %.200s)", err, line)
			continue
		}

		if msg.Method != "" {
			if msg.ID != nil {
				// Incoming request from copilot (e.g., permission request)
				a.handleIncomingRequest(&msg)
			} else {
				// Notification from copilot (e.g., session/update)
				a.handleNotification(&msg)
			}
		} else if msg.ID != nil {
			// Response to one of our requests
			a.mu.Lock()
			ch, ok := a.pending[*msg.ID]
			a.mu.Unlock()
			if ok {
				ch <- &msg
			}
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case <-a.done:
			// Normal shutdown
		default:
			fmt.Printf("\r\n%sCopilot stream error: %s%s\r\n", colorRed, err, colorReset)
		}
	}
}

// handleIncomingRequest handles JSON-RPC requests from copilot to us.
func (a *CopilotAgent) handleIncomingRequest(msg *jsonrpcMessage) {
	switch msg.Method {
	case "session/request_permission":
		a.handlePermissionRequest(msg)
	default:
		log.Printf("[acp] unknown incoming request: %s", msg.Method)
		// Respond with method not found
		if msg.ID != nil {
			errResp := jsonrpcMessage{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &jsonrpcError{Code: -32601, Message: "method not found"},
			}
			data, _ := json.Marshal(errResp)
			data = append(data, '\n')
			a.stdin.Write(data)
		}
	}
}

// handlePermissionRequest responds to tool permission requests from copilot.
func (a *CopilotAgent) handlePermissionRequest(msg *jsonrpcMessage) {
	var params struct {
		SessionID   string `json:"sessionId"`
		Resource    string `json:"resource"`
		Action      string `json:"action"`
		Description string `json:"description"`
	}
	json.Unmarshal(msg.Params, &params)

	granted := a.autoApprove
	if granted {
		fmt.Printf("%s%s(auto-approved: %s — %s)%s\r\n", colorDim, colorGray, params.Action, params.Description, colorReset)
	} else {
		// Display permission request in terminal
		fmt.Printf("\r\n%s%s🔒 Permission requested:%s\r\n", colorBold, colorYellow, colorReset)
		if params.Action != "" {
			fmt.Printf("%s   Action:      %s%s\r\n", colorYellow, params.Action, colorReset)
		}
		if params.Resource != "" {
			fmt.Printf("%s   Resource:    %s%s\r\n", colorYellow, params.Resource, colorReset)
		}
		if params.Description != "" {
			fmt.Printf("%s   Description: %s%s\r\n", colorYellow, params.Description, colorReset)
		}
		// Auto-approve since we can't easily read stdin during a prompt
		// (stdin is being read by the REPL scanner)
		granted = true
		fmt.Printf("%s   (granted — use --auto-approve to suppress)%s\r\n\r\n", colorGray, colorReset)
	}

	a.postHookEvent("permission_request", map[string]string{
		"action":      params.Action,
		"resource":    params.Resource,
		"description": params.Description,
		"granted":     fmt.Sprintf("%t", granted),
	})

	if msg.ID != nil {
		a.sendResponse(*msg.ID, map[string]bool{"granted": granted})
	}
}

// handleNotification handles JSON-RPC notifications from copilot.
func (a *CopilotAgent) handleNotification(msg *jsonrpcMessage) {
	switch msg.Method {
	case "session/update":
		var params sessionUpdateParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			log.Printf("[acp] session/update parse error: %v", err)
			return
		}
		a.handleSessionUpdate(&params)
	default:
		log.Printf("[acp] unknown notification: %s", msg.Method)
	}
}

// handleSessionUpdate processes streaming events from the copilot agent.
func (a *CopilotAgent) handleSessionUpdate(params *sessionUpdateParams) {
	switch params.Update.SessionUpdate {
	case "agent_message_chunk":
		a.collapseThinking()
		a.renderContentChunks(params.Update.Content)

	case "agent_thought_chunk":
		text := extractText(params.Update.Content)
		if text != "" {
			a.appendThinking(text)
		}

	case "tool_call":
		a.collapseThinking()
		name := ""
		if params.Update.Function != nil {
			name = params.Update.Function.Name
		}
		if name != "" {
			fmt.Printf("\r\n%s%s⚙ Tool: %s%s\r\n", colorDim, colorBlue, name, colorReset)
		}

	case "tool_call_update":
		if params.Update.Output != "" {
			fmt.Printf("%s%s%s", colorDim, params.Update.Output, colorReset)
		}

	case "plan":
		a.collapseThinking()
		if len(params.Update.Steps) > 0 {
			fmt.Printf("\r\n%s%s📋 Plan:%s\r\n", colorBold, colorCyan, colorReset)
			for i, step := range params.Update.Steps {
				fmt.Printf("%s  %d. %s%s\r\n", colorCyan, i+1, step, colorReset)
			}
			fmt.Printf("\r\n")
			a.postHookEvent("plan_captured", map[string]string{
				"plan": strings.Join(params.Update.Steps, "\n"),
			})
		}

	default:
		a.collapseThinking()
		a.renderContentChunks(params.Update.Content)
	}
}

// appendThinking overwrites a single line with the latest thinking snippet.
// Uses \r + erase-line to keep thinking to exactly ONE terminal line.
func (a *CopilotAgent) appendThinking(text string) {
	a.isThinking = true
	a.thinkingLines += strings.Count(text, "\n") + 1

	// Take the last non-empty line fragment as the preview
	preview := text
	if idx := strings.LastIndex(strings.TrimRight(text, "\n"), "\n"); idx >= 0 {
		preview = text[idx+1:]
	}
	preview = strings.TrimSpace(preview)
	if preview == "" {
		return
	}

	// Truncate to fit in ~70 chars alongside the prefix
	if len(preview) > 60 {
		preview = preview[:57] + "..."
	}

	// \r = move to start of line, \033[2K = erase entire line
	fmt.Printf("\r\033[2K%s%s💭 %s%s", colorDim, colorGray, preview, colorReset)
}

// collapseThinking finishes the thinking line with a summary and moves to the next line.
func (a *CopilotAgent) collapseThinking() {
	if !a.isThinking {
		return
	}
	a.isThinking = false

	// Overwrite the thinking line with a collapsed summary, then newline
	fmt.Printf("\r\033[2K%s%s💭 thinking (%d lines)%s\r\n", colorDim, colorGray, a.thinkingLines, colorReset)
	a.thinkingLines = 0
}

// extractText pulls text from a content JSON payload (used for thought chunks).
// Handles three formats:
//   - Single object: {"type":"text","text":"..."}
//   - Array: [{"type":"text","text":"..."}]
//   - Wrapped: {"content":[{"type":"text","text":"..."}]}
func extractText(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	// Try as single content block (most common for thought chunks)
	var single contentBlock
	if err := json.Unmarshal(raw, &single); err == nil && single.Text != "" {
		return single.Text
	}
	// Try as array of content blocks
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	// Try as wrapped object with content array
	var obj struct {
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		var sb strings.Builder
		for _, b := range obj.Content {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return ""
}

// renderContentChunks extracts and prints text from a content JSON payload.
func (a *CopilotAgent) renderContentChunks(raw json.RawMessage) {
	if raw == nil {
		return
	}
	// Try as array of content blocks
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Text != "" {
				fmt.Print(b.Text)
			}
		}
		return
	}
	// Try as object with nested content array
	var obj struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj.Content) > 0 {
		for _, b := range obj.Content {
			if b.Text != "" {
				fmt.Print(b.Text)
			}
		}
		return
	}
	// Try as single content block
	var single contentBlock
	if err := json.Unmarshal(raw, &single); err == nil && single.Text != "" {
		fmt.Print(single.Text)
	}
}

// initialize performs the ACP initialization handshake.
func (a *CopilotAgent) initialize(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: 1,
		ClientInfo: clientInfo{
			Name:    "openpoet-acp",
			Version: "1.0.0",
		},
		Capabilities: clientCapabilities{
			Streaming: true,
		},
	}

	resp, err := a.sendRequest(ctx, "initialize", params)
	if err != nil {
		return err
	}

	var result initializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse initialize result: %w", err)
	}

	name := result.AgentInfo.Title
	if name == "" {
		name = result.AgentInfo.Name
	}
	if name == "" {
		name = "Copilot"
	}
	version := result.AgentInfo.Version
	if version == "" {
		version = "unknown"
	}
	fmt.Printf("%sConnected: %s v%s (protocol v%d)%s\r\n", colorGray, name, version, result.ProtocolVersion, colorReset)

	// Show auth hint if login is needed
	if len(result.AuthMethods) > 0 {
		for _, m := range result.AuthMethods {
			fmt.Printf("%sAuth available: %s — %s%s\r\n", colorGray, m.Name, m.Description, colorReset)
		}
	}

	return nil
}

// newSession creates a new ACP session.
func (a *CopilotAgent) newSession(ctx context.Context) error {
	params := sessionNewParams{
		CWD:        a.workDir,
		MCPServers: []interface{}{},
	}

	resp, err := a.sendRequest(ctx, "session/new", params)
	if err != nil {
		return err
	}

	var result sessionNewResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse session/new result: %w", err)
	}

	a.sessionID = result.SessionID
	fmt.Printf("%sSession: %s%s\r\n", colorGray, a.sessionID, colorReset)
	return nil
}

// sessionLoadParams extends sessionNewParams with a session ID for loading.
type sessionLoadParams struct {
	SessionID  string        `json:"sessionId"`
	CWD        string        `json:"cwd"`
	MCPServers []interface{} `json:"mcpServers"`
}

// loadSession loads a previous ACP session by its copilot session ID.
// Copilot replays the full conversation history via session/update notifications.
func (a *CopilotAgent) loadSession(ctx context.Context, copilotSessionID string) error {
	params := sessionLoadParams{
		SessionID:  copilotSessionID,
		CWD:        a.workDir,
		MCPServers: []interface{}{},
	}

	_, err := a.sendRequest(ctx, "session/load", params)
	if err != nil {
		return err
	}

	a.sessionID = copilotSessionID
	fmt.Printf("%sSession loaded: %s%s\r\n", colorGray, a.sessionID, colorReset)
	return nil
}

// sessionMappingPath returns the path where the copilot session ID is stored
// for a given OpenPoet session ID.
func sessionMappingPath(openpoetSID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openpoet", "acp-sessions", openpoetSID)
}

// saveCopilotSessionID persists the copilot-generated session ID so it can
// be used for session/load on reopen.
func saveCopilotSessionID(openpoetSID, copilotSID string) {
	path := sessionMappingPath(openpoetSID)
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte(copilotSID), 0644)
}

// readCopilotSessionID reads the previously stored copilot session ID.
func readCopilotSessionID(openpoetSID string) string {
	data, err := os.ReadFile(sessionMappingPath(openpoetSID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// prompt sends a user prompt and blocks until the agent finishes.
// Streaming updates arrive via session/update notifications handled by readLoop.
func (a *CopilotAgent) prompt(ctx context.Context, text string) (*sessionPromptResult, error) {
	params := sessionPromptParams{
		SessionID: a.sessionID,
		Prompt: []contentBlock{
			{Type: "text", Text: text},
		},
	}

	resp, err := a.sendRequest(ctx, "session/prompt", params)
	if err != nil {
		return nil, err
	}

	var result sessionPromptResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse prompt result: %w", err)
	}

	// Print any final content from the response (non-streamed summary)
	for _, block := range result.Content {
		if block.Text != "" {
			fmt.Print(block.Text)
		}
	}

	return &result, nil
}

// cancelSession sends a session/cancel notification to abort the current operation.
func (a *CopilotAgent) cancelSession() {
	if a.sessionID == "" {
		return
	}
	a.sendNotification("session/cancel", map[string]string{
		"sessionId": a.sessionID,
	})
}

// postHookEvent sends an event to the OpenPoet hook endpoint.
func (a *CopilotAgent) postHookEvent(eventName string, data map[string]string) {
	if a.hookURL == "" || a.hookSessID == "" {
		return
	}

	payload := map[string]interface{}{
		"hook_event_name": eventName,
		"session_id":      a.hookSessID,
	}
	for k, v := range data {
		payload[k] = v
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	url := strings.TrimRight(a.hookURL, "/") + "/api/hooks/event"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", a.hookSessID)
	req.Header.Set("X-Backend", "acp")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// scanCRorLF is a bufio.SplitFunc that splits on \r, \n, or \r\n.
// This ensures the mobile input's \r (which the PTY may not translate to \n)
// is correctly treated as a line terminator.
func scanCRorLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			// \r\n counts as one terminator
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
