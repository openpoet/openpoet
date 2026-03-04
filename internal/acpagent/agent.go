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
	"regexp"
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

	// stdinMu protects stdin writes. Permission goroutines may write concurrently
	// with the readLoop (which dispatches incoming requests), so all stdin.Write
	// calls must be serialized.
	stdinMu sync.Mutex

	sessionID string
	workDir   string

	hookURL     string
	hookSessID  string
	autoApprove bool

	// Thinking state — accessed only from readLoop goroutine.
	// Uses single-line overwrite (\r + erase line) to avoid stacking.
	isThinking    bool
	thinkingLines int // total lines of thought accumulated

	// Tool tracking for PreToolUse/PostToolUse hook events.
	lastToolName string

	// injectCh allows goroutines (e.g., plan approval) to inject messages into the REPL.
	injectCh chan string

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
		injectCh:    make(chan string, 1),
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
		agent.postHookEvent("mode_changed", map[string]interface{}{"mode": "idle"})
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
	agent.postHookEvent("mode_changed", map[string]interface{}{"mode": "idle"})

	// REPL — reads from stdin and also accepts injected messages (e.g., after plan approval).
	// Custom split to handle both \r and \n as line terminators (mobile sends \r).
	stdinCh := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		scanner.Split(scanCRorLF)
		for scanner.Scan() {
			stdinCh <- scanner.Text()
		}
		close(stdinCh)
	}()

	firstMessage := true

	for {
		fmt.Printf("%s❯%s ", colorGreen, colorReset)

		var input string
		var ok bool
		select {
		case input, ok = <-stdinCh:
			if !ok {
				goto done
			}
		case input = <-agent.injectCh:
			// Injected message (e.g., plan approval follow-up)
			fmt.Printf("%s%s%s\r\n", colorCyan, input, colorReset)
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		if firstMessage && *systemPrompt != "" {
			input = *systemPrompt + "\n\n" + input
			firstMessage = false
		}

		agent.postHookEvent("mode_changed", map[string]interface{}{"mode": "executing"})
		agent.postHookEvent("userPromptSubmitted", map[string]interface{}{})

		result, err := agent.prompt(ctx, input)
		// Flush any trailing tool tracking (last tool before prompt completed)
		agent.emitPostToolUse()
		if err != nil {
			fmt.Printf("\r\n%sError: %s%s\r\n\r\n", colorRed, err, colorReset)
		} else if result != nil && result.StopReason == "cancelled" {
			fmt.Printf("\r\n%s(cancelled)%s\r\n\r\n", colorYellow, colorReset)
		} else {
			fmt.Printf("\r\n")
		}

		agent.postHookEvent("mode_changed", map[string]interface{}{"mode": "idle"})
	}

done:
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
	a.stdinMu.Lock()
	_, writeErr := a.stdin.Write(data)
	a.stdinMu.Unlock()
	if writeErr != nil {
		return nil, fmt.Errorf("write to copilot stdin: %w", writeErr)
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
	a.stdinMu.Lock()
	_, err = a.stdin.Write(data)
	a.stdinMu.Unlock()
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
	a.stdinMu.Lock()
	_, err = a.stdin.Write(data)
	a.stdinMu.Unlock()
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
			a.stdinMu.Lock()
			a.stdin.Write(data)
			a.stdinMu.Unlock()
		}
	}
}

// handlePermissionRequest responds to tool permission requests from copilot.
// When hookURL is available, routes the request through OpenPoet's UI for
// interactive approval. Otherwise falls back to auto-approve.
func (a *CopilotAgent) handlePermissionRequest(msg *jsonrpcMessage) {
	// Copilot ACP permission format:
	// { "sessionId", "toolCall": { "toolCallId", "title", "kind", "status", "rawInput": {...} }, "options": [...] }
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			Status     string          `json:"status"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID    string `json:"optionId"`
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	}
	json.Unmarshal(msg.Params, &params)

	// Extract a display-friendly tool name from the toolCall
	toolName := params.ToolCall.Kind // e.g., "execute", "edit", "read"
	if toolName == "" {
		toolName = params.ToolCall.ToolCallID // fallback
	}
	title := params.ToolCall.Title // e.g., "Write hello to /tmp/test.txt"

	// Build the allow/deny response payloads using optionId from the options array.
	// Default to "allow_once" / "reject_once" if no options provided.
	allowOptionID := "allow_once"
	denyOptionID := "reject_once"
	for _, opt := range params.Options {
		switch opt.OptionID {
		case "allow_once", "allow":
			allowOptionID = opt.OptionID
		case "reject_once", "deny", "deny_once":
			denyOptionID = opt.OptionID
		}
	}

	// permissionOutcome builds the ACP permission response.
	// ACP format: {outcome: {outcome: "selected", optionId: "<id>"}} for grant
	//             {outcome: {outcome: "cancelled"}} for cancel/deny
	permissionOutcome := func(optID string, granted bool) interface{} {
		if !granted {
			return map[string]interface{}{
				"outcome": map[string]string{"outcome": "cancelled"},
			}
		}
		return map[string]interface{}{
			"outcome": map[string]interface{}{
				"outcome":  "selected",
				"optionId": optID,
			},
		}
	}

	// Auto-approve mode: approve silently
	if a.autoApprove {
		fmt.Printf("%s%s(auto-approved: %s — %s)%s\r\n", colorDim, colorGray, toolName, title, colorReset)
		if msg.ID != nil {
			a.sendResponse(*msg.ID, permissionOutcome(allowOptionID, true))
		}
		return
	}

	// No hook URL: fall back to local auto-approve (standalone mode)
	if a.hookURL == "" || a.hookSessID == "" {
		fmt.Printf("\r\n%s%s🔒 Permission requested:%s\r\n", colorBold, colorYellow, colorReset)
		if title != "" {
			fmt.Printf("%s   %s%s\r\n", colorYellow, title, colorReset)
		}
		if toolName != "" {
			fmt.Printf("%s   Kind: %s%s\r\n", colorGray, toolName, colorReset)
		}
		fmt.Printf("%s   (granted — no hook URL, use --auto-approve to suppress)%s\r\n\r\n", colorGray, colorReset)
		if msg.ID != nil {
			a.sendResponse(*msg.ID, permissionOutcome(allowOptionID, true))
		}
		return
	}

	// Route through OpenPoet's hook system for UI-based approval.
	// Run in a goroutine so readLoop can continue processing session/update notifications.
	msgID := 0
	if msg.ID != nil {
		msgID = *msg.ID
	}
	go func() {
		// Detect Copilot plan file writes and route through OpenPoet's plan approval.
		// Copilot writes plans to ~/.copilot/session-state/{session-id}/plan.md
		// using a regular "edit" tool. We intercept this, auto-approve the write,
		// read the file content, and route it through OpenPoet's ExitPlanMode pipeline.
		if toolName == "edit" {
			planPath := detectPlanFilePath(params.ToolCall.RawInput, title)
			if planPath != "" {
				log.Printf("[acp] plan file detected: %s", planPath)
				fmt.Printf("\r\n%s%s   📋 Plan file detected — routing to plan approval%s\r\n", colorBold, colorCyan, colorReset)

				// Auto-approve the edit so Copilot writes the file
				if err := a.sendResponse(msgID, permissionOutcome(allowOptionID, true)); err != nil {
					log.Printf("[acp] plan auto-approve error: %v", err)
				}
				a.postHookEvent("PostToolUse", map[string]interface{}{"tool_name": toolName})

				// Extract plan content: prefer reading from disk (after write completes),
				// fall back to extracting from the diff in rawInput.
				time.Sleep(500 * time.Millisecond)
				planContent, err := os.ReadFile(planPath)
				if err != nil {
					log.Printf("[acp] failed to read plan file, extracting from diff: %v", err)
					planContent = []byte(extractContentFromDiff(params.ToolCall.RawInput))
				}
				if len(planContent) == 0 {
					log.Printf("[acp] plan file empty or unreadable, skipping plan approval")
					return
				}

				// Route through OpenPoet's plan approval pipeline
				a.postPlanApproval(string(planContent))
				return
			}
		}

		fmt.Printf("\r\n%s%s🔒 Permission requested: %s%s\r\n", colorBold, colorYellow, title, colorReset)
		if toolName != "" {
			fmt.Printf("%s   Kind: %s%s\r\n", colorGray, toolName, colorReset)
		}
		fmt.Printf("%s   (waiting for approval in OpenPoet UI...)%s\r\n", colorGray, colorReset)

		// Emit PreToolUse so the tool activity panel shows the pending tool
		toolInput := map[string]interface{}{}
		if len(params.ToolCall.RawInput) > 0 {
			json.Unmarshal(params.ToolCall.RawInput, &toolInput)
		}
		if title != "" {
			toolInput["description"] = title
		}
		a.postHookEvent("PreToolUse", map[string]interface{}{
			"tool_name":  toolName,
			"tool_input": toolInput,
		})

		granted := a.postHookPermission(toolName, title, params.ToolCall.RawInput)

		if granted {
			fmt.Printf("%s   ✓ approved%s\r\n", colorGreen, colorReset)
			a.postHookEvent("PostToolUse", map[string]interface{}{
				"tool_name": toolName,
			})
		} else {
			fmt.Printf("%s   ✗ denied%s\r\n", colorRed, colorReset)
			a.postHookEvent("PostToolUseFailure", map[string]interface{}{
				"tool_name": toolName,
			})
		}

		optID := allowOptionID
		if !granted {
			optID = denyOptionID
		}
		if err := a.sendResponse(msgID, permissionOutcome(optID, granted)); err != nil {
			log.Printf("[acp] permission sendResponse error: %v", err)
		}
	}()
}

// postHookPermission sends a blocking permission request to OpenPoet's hook endpoint.
// Maps ACP permission fields to the hook format that OpenPoet's UI understands.
// Returns true if granted, false if denied or on error.
func (a *CopilotAgent) postHookPermission(toolName, title string, rawInput json.RawMessage) bool {
	// Build tool_input: merge rawInput (command, path, etc.) with title as description
	toolInput := map[string]interface{}{}
	if len(rawInput) > 0 {
		json.Unmarshal(rawInput, &toolInput)
	}
	if title != "" {
		toolInput["description"] = title
	}

	payload := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"session_id":      a.hookSessID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[acp] permission marshal error: %v", err)
		return true // fail-open
	}

	url := strings.TrimRight(a.hookURL, "/") + "/api/hooks/permission"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[acp] permission request error: %v", err)
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", a.hookSessID)
	req.Header.Set("X-Backend", "acp")

	// Use a long timeout — OpenPoet blocks up to 590s waiting for user response
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[acp] permission request failed: %v", err)
		return true // fail-open on network error
	}
	defer resp.Body.Close()

	// 204 No Content = passthrough (user dismissed dialog), treat as allow
	if resp.StatusCode == http.StatusNoContent {
		return true
	}

	// Parse OpenPoet's response envelope
	var output struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&output); err != nil {
		log.Printf("[acp] permission response parse error: %v", err)
		return true // fail-open
	}

	return output.HookSpecificOutput.Decision.Behavior == "allow"
}

// planPathRe matches absolute paths containing .copilot/session-state/*/plan.md
var planPathRe = regexp.MustCompile(`(/[^\s"']+/\.copilot/session-state/[^/]+/plan\.md)`)

// detectPlanFilePath checks if a Copilot edit permission targets a plan.md file.
// It tries multiple sources: rawInput fields (path, file, file_path, filename) and title text.
func detectPlanFilePath(rawInput json.RawMessage, title string) string {
	// Try common field names in rawInput
	if len(rawInput) > 0 {
		var fields map[string]interface{}
		if json.Unmarshal(rawInput, &fields) == nil {
			for _, key := range []string{"path", "file", "fileName", "file_path", "filename", "filePath"} {
				if v, ok := fields[key]; ok {
					if s, ok := v.(string); ok && isPlanPath(s) {
						return s
					}
				}
			}
		}
	}
	// Fall back to extracting path from the title string
	if m := planPathRe.FindString(title); m != "" {
		return m
	}
	// Also check rawInput as a string dump for the path pattern
	if len(rawInput) > 0 {
		if m := planPathRe.FindString(string(rawInput)); m != "" {
			return m
		}
	}
	return ""
}

// extractContentFromDiff extracts file content from a Copilot edit diff.
// The diff field contains unified diff format with +lines being the new content.
func extractContentFromDiff(rawInput json.RawMessage) string {
	var fields struct {
		Diff string `json:"diff"`
	}
	if json.Unmarshal(rawInput, &fields) != nil || fields.Diff == "" {
		return ""
	}
	var lines []string
	inContent := false
	for _, line := range strings.Split(fields.Diff, "\n") {
		if strings.HasPrefix(line, "@@") {
			inContent = true
			continue
		}
		if inContent && strings.HasPrefix(line, "+") {
			lines = append(lines, line[1:])
		}
	}
	return strings.Join(lines, "\n")
}

func isPlanPath(p string) bool {
	return strings.HasSuffix(p, "/plan.md") && strings.Contains(p, ".copilot/session-state/")
}

// postPlanApproval sends a synthetic ExitPlanMode permission request to OpenPoet's
// hook endpoint. This routes Copilot's plan.md file writes through OpenPoet's plan
// approval UI (dialog, DB persistence, task history).
func (a *CopilotAgent) postPlanApproval(planContent string) {
	// Emit plan_captured event first so the hook system has the plan content
	a.postHookEvent("plan_captured", map[string]interface{}{
		"plan": planContent,
	})

	toolInput := map[string]interface{}{
		"plan": planContent,
	}
	payload := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "ExitPlanMode",
		"tool_input":      toolInput,
		"session_id":      a.hookSessID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[acp] plan approval marshal error: %v", err)
		return
	}

	url := strings.TrimRight(a.hookURL, "/") + "/api/hooks/permission"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[acp] plan approval request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", a.hookSessID)
	req.Header.Set("X-Backend", "acp")

	fmt.Printf("%s   (waiting for plan approval in OpenPoet UI...)%s\r\n", colorGray, colorReset)

	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[acp] plan approval request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	// 204 No Content = dismissed, treat as approved
	if resp.StatusCode == http.StatusNoContent {
		fmt.Printf("%s   ✓ plan approved%s\r\n", colorGreen, colorReset)
		return
	}

	var output struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&output); err != nil {
		log.Printf("[acp] plan approval response parse error: %v", err)
		return
	}

	if output.HookSpecificOutput.Decision.Behavior == "allow" {
		fmt.Printf("\r\n%s   ✓ plan approved — sending implementation prompt%s\r\n", colorGreen, colorReset)
		// Write follow-up prompt to PTY stdin so the REPL picks it up
		a.writePTYInput("Plan approved. Proceed with the implementation.\n")
	} else {
		feedback := output.HookSpecificOutput.Decision.Message
		if feedback != "" {
			fmt.Printf("\r\n%s   ✗ plan denied — feedback: %s%s\r\n", colorRed, feedback, colorReset)
			a.writePTYInput("Plan denied. User feedback: " + feedback + "\n")
		} else {
			fmt.Printf("\r\n%s   ✗ plan denied%s\r\n", colorRed, colorReset)
			a.writePTYInput("Plan denied. Please revise the plan based on user feedback.\n")
		}
	}
}

// writePTYInput writes text to the PTY's stdin pipe (os.Stdin of the acp-agent process).
// This simulates user input so the REPL loop picks it up as the next message.
func (a *CopilotAgent) writePTYInput(text string) {
	// The acp-agent runs inside a PTY. Writing to /dev/stdin or using the PTY master
	// would be complex. Instead, we use a channel to inject messages into the REPL.
	if a.injectCh != nil {
		a.injectCh <- text
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
		// If we were tracking a tool, it's done now
		a.emitPostToolUse()
		a.collapseThinking()
		a.renderContentChunks(params.Update.Content)

	case "agent_thought_chunk":
		text := extractText(params.Update.Content)
		if text != "" {
			a.appendThinking(text)
		}

	case "tool_call":
		// If a previous tool was tracked, emit PostToolUse for it
		a.emitPostToolUse()
		a.collapseThinking()
		name := ""
		if params.Update.Function != nil {
			name = params.Update.Function.Name
		}
		if name != "" {
			fmt.Printf("\r\n%s%s⚙ Tool: %s%s\r\n", colorDim, colorBlue, name, colorReset)
			// Post PreToolUse hook event
			a.lastToolName = name
			a.postHookEvent("PreToolUse", map[string]interface{}{
				"tool_name": name,
			})
		}

	case "tool_call_update":
		if params.Update.Output != "" {
			fmt.Printf("%s%s%s", colorDim, params.Update.Output, colorReset)
		}

	case "plan":
		a.emitPostToolUse()
		a.collapseThinking()
		if len(params.Update.Steps) > 0 {
			fmt.Printf("\r\n%s%s📋 Plan:%s\r\n", colorBold, colorCyan, colorReset)
			for i, step := range params.Update.Steps {
				fmt.Printf("%s  %d. %s%s\r\n", colorCyan, i+1, step, colorReset)
			}
			fmt.Printf("\r\n")
			a.postHookEvent("plan_captured", map[string]interface{}{
				"plan": strings.Join(params.Update.Steps, "\n"),
			})
		}

	default:
		a.emitPostToolUse()
		a.collapseThinking()
		a.renderContentChunks(params.Update.Content)
	}
}

// emitPostToolUse posts a PostToolUse hook event if a tool was being tracked.
func (a *CopilotAgent) emitPostToolUse() {
	if a.lastToolName == "" {
		return
	}
	name := a.lastToolName
	a.lastToolName = ""
	a.postHookEvent("PostToolUse", map[string]interface{}{
		"tool_name": name,
	})
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
func (a *CopilotAgent) postHookEvent(eventName string, data map[string]interface{}) {
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
