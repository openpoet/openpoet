package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"openpoet/internal/database"
	"openpoet/internal/websocket"
)

// CodexRunner drives Codex through `codex app-server` and exposes it through
// OpenPoet's existing terminal-session runner interface.
const (
	codexPromptANSI     = "\x1b[36mOpenPoet Codex>\x1b[0m "
	codexClearInputLine = "\r\x1b[2K"
)

type CodexRunner struct {
	workDir       string
	envVars       map[string]string
	outputHandler func([]byte)
	cfg           *SessionConfig
	db            *database.DB
	hub           *websocket.Hub

	terminalMu sync.Mutex

	mu                  sync.Mutex
	cmd                 *exec.Cmd
	stdin               io.WriteCloser
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	nextID              int
	pending             map[int]chan codexRPCResponse
	inputBuffer         []rune
	inputLineVisible    bool
	providerThreadID    string
	activeTurnID        string
	initialized         bool
	shuttingDown        bool
	allowForSession     map[string]bool
	interruptedTurns    map[string]bool
	commandProcesses    map[string]map[string]codexCommandProcess
	modelOverride       string
	modelOverrideSet    bool
	reasoningOverride   string
	reasoningSet        bool
	serviceTierOverride string
	serviceTierSet      bool
	approvalOverride    string
	sandboxOverride     string
	lastAssistantChunk  bool
	lastReasoningChunk  bool
	lastCommandOutChunk bool
	stderrTail          []string
	agentPhase          string
	agentDetail         string
	lastInputTokens     float64
	lastOutputTokens    float64
	transcriptSeq       int
	transcript          []codexTranscriptEvent
	transcriptStreams   map[string]int
	modelDefaults       codexModelDefaults
	modelDefaultsByID   map[string]codexModelDefaults
}

type codexCommandProcess struct {
	ID        string
	Command   string
	StartedAt time.Time
}

type codexModelDefaults struct {
	Model           string
	ReasoningEffort string
	ServiceTier     string
}

type codexTranscriptEvent struct {
	ID        int       `json:"id"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text,omitempty"`
	Title     string    `json:"title,omitempty"`
	Command   string    `json:"command,omitempty"`
	Status    string    `json:"status,omitempty"`
	Append    bool      `json:"append,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type codexThreadResumeResponse struct {
	Thread *codexThreadResumeThread `json:"thread"`
}

type codexThreadResumeThread struct {
	Turns []codexThreadResumeTurn `json:"turns"`
}

type codexThreadResumeTurn struct {
	ID        string                  `json:"id"`
	StartedAt *int64                  `json:"startedAt"`
	Status    string                  `json:"status"`
	Error     *codexThreadResumeError `json:"error"`
	Items     []codexThreadResumeItem `json:"items"`
}

type codexThreadResumeError struct {
	Message string `json:"message"`
}

type codexThreadResumeItem struct {
	ID               string                  `json:"id"`
	Type             string                  `json:"type"`
	Text             string                  `json:"text"`
	Phase            string                  `json:"phase"`
	Content          json.RawMessage         `json:"content"`
	Summary          []string                `json:"summary"`
	Command          string                  `json:"command"`
	AggregatedOutput *string                 `json:"aggregatedOutput"`
	Status           string                  `json:"status"`
	Changes          []codexThreadFileChange `json:"changes"`
	Error            *codexThreadResumeError `json:"error"`
	Server           string                  `json:"server"`
	Tool             string                  `json:"tool"`
	Namespace        string                  `json:"namespace"`
	Result           interface{}             `json:"result"`
	Arguments        interface{}             `json:"arguments"`
	Query            string                  `json:"query"`
	Path             string                  `json:"path"`
	Review           string                  `json:"review"`
}

type codexThreadUserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URL  string `json:"url"`
	Path string `json:"path"`
	Name string `json:"name"`
}

type codexThreadFileChange struct {
	Path string `json:"path"`
}

type codexRPCResponse struct {
	Result json.RawMessage
	Error  *codexRPCError
}

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *codexRPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("codex app-server error %d", e.Code)
}

type codexWireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

func NewCodexRunner(workDir string, envVars map[string]string, outputHandler func([]byte), cfg *SessionConfig, db *database.DB, hub *websocket.Hub) (*CodexRunner, error) {
	return newCodexRunner(workDir, envVars, outputHandler, cfg, db, hub, true)
}

func newCodexRunner(workDir string, envVars map[string]string, outputHandler func([]byte), cfg *SessionConfig, db *database.DB, hub *websocket.Hub, validateWorkDir bool) (*CodexRunner, error) {
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		if validateWorkDir {
			return nil, fmt.Errorf("work directory does not exist: %s", workDir)
		}
	}
	return &CodexRunner{
		workDir:           workDir,
		envVars:           envVars,
		outputHandler:     outputHandler,
		cfg:               cfg,
		db:                db,
		hub:               hub,
		done:              make(chan struct{}),
		pending:           make(map[int]chan codexRPCResponse),
		allowForSession:   make(map[string]bool),
		interruptedTurns:  make(map[string]bool),
		commandProcesses:  make(map[string]map[string]codexCommandProcess),
		modelDefaultsByID: make(map[string]codexModelDefaults),
	}, nil
}

func (r *CodexRunner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.initialized {
		r.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.ctx = runCtx
	r.cancel = cancel
	r.mu.Unlock()

	binaryPath, err := r.resolveBinaryPath()
	if err != nil {
		r.write([]byte((&CodexBackend{}).NotFoundMessage()))
		close(r.done)
		return err
	}

	r.write([]byte((&CodexBackend{}).StartupMessage(binaryPath, r.workDir)))
	r.loadPersistedCodexTranscript(ctx)

	cmd := exec.CommandContext(runCtx, binaryPath, "app-server")
	cmd.Dir = r.workDir
	cmd.Env = r.buildEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		close(r.done)
		return fmt.Errorf("codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		close(r.done)
		return fmt.Errorf("codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		close(r.done)
		return fmt.Errorf("codex stderr: %w", err)
	}

	r.mu.Lock()
	r.cmd = cmd
	r.stdin = stdin
	r.mu.Unlock()

	if err := cmd.Start(); err != nil {
		close(r.done)
		return fmt.Errorf("start codex app-server: %w", err)
	}

	go r.readStdout(stdout)
	go r.readStderr(stderr)
	go func() {
		err := cmd.Wait()
		exitMessage := "codex app-server exited"
		if detail := r.stderrSummary(); detail != "" {
			exitMessage += ": " + detail
		}
		r.failPending(&codexRPCError{Code: -32000, Message: exitMessage})
		if err != nil && !r.isShuttingDown() {
			if detail := r.stderrSummary(); detail != "" {
				r.write([]byte(fmt.Sprintf("\r\n\x1b[31mCodex app-server exited: %v\r\n%s\x1b[0m\r\n", err, detail)))
			} else {
				r.write([]byte(fmt.Sprintf("\r\n\x1b[31mCodex app-server exited: %v\x1b[0m\r\n", err)))
			}
		}
		close(r.done)
	}()

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := r.initialize(initCtx); err != nil {
		_ = r.Stop()
		return err
	}
	r.refreshCodexModelDefaults(initCtx)
	if err := r.openThread(initCtx); err != nil {
		_ = r.Stop()
		return err
	}

	r.mu.Lock()
	r.initialized = true
	r.mu.Unlock()

	r.writePrompt()
	r.setCodexPhase("idle", "Waiting for instructions")
	return nil
}

func (r *CodexRunner) refreshCodexModelDefaults(ctx context.Context) {
	raw, err := r.request(ctx, "model/list", map[string]interface{}{
		"limit":         100,
		"includeHidden": false,
	})
	if err != nil {
		log.Printf("[Codex] model/list failed while resolving defaults: %v", err)
		return
	}

	var resp struct {
		Data []struct {
			ID                     string `json:"id"`
			Model                  string `json:"model"`
			IsDefault              bool   `json:"isDefault"`
			DefaultReasoningEffort string `json:"defaultReasoningEffort"`
			DefaultServiceTier     string `json:"defaultServiceTier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		log.Printf("[Codex] invalid model/list response while resolving defaults: %v", err)
		return
	}

	defaultsByID := make(map[string]codexModelDefaults, len(resp.Data))
	var defaultModel codexModelDefaults
	for _, item := range resp.Data {
		model := strings.TrimSpace(item.Model)
		if model == "" {
			model = strings.TrimSpace(item.ID)
		}
		if model == "" {
			continue
		}
		defaults := codexModelDefaults{
			Model:           model,
			ReasoningEffort: strings.TrimSpace(item.DefaultReasoningEffort),
			ServiceTier:     strings.TrimSpace(item.DefaultServiceTier),
		}
		defaultsByID[model] = defaults
		if strings.TrimSpace(item.ID) != "" {
			defaultsByID[strings.TrimSpace(item.ID)] = defaults
		}
		if item.IsDefault && defaultModel.Model == "" {
			defaultModel = defaults
		}
		if defaultModel.Model == "" {
			defaultModel = defaults
		}
	}
	if defaultModel.Model == "" {
		return
	}

	r.mu.Lock()
	r.modelDefaults = defaultModel
	r.modelDefaultsByID = defaultsByID
	r.mu.Unlock()
}

func (r *CodexRunner) Stop() error {
	r.mu.Lock()
	if r.shuttingDown {
		r.mu.Unlock()
		return nil
	}
	r.shuttingDown = true
	cancel := r.cancel
	cmd := r.cmd
	stdin := r.stdin
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-r.done:
			return nil
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
	return nil
}

func (r *CodexRunner) Write(data []byte) (int, error) {
	for i := 0; i < len(data); {
		b := data[i]
		var text string
		var interrupt bool
		var typed []byte
		var typedRune rune

		if b == 0x1b {
			next, bareEscape := consumeCodexTerminalEscape(data, i)
			i = next
			if bareEscape {
				r.terminalMu.Lock()
				r.mu.Lock()
				r.inputBuffer = nil
				r.inputLineVisible = false
				r.emit([]byte("^ESC\r\n"))
				r.emitPromptLocked(false)
				r.mu.Unlock()
				r.terminalMu.Unlock()
				go r.interruptTurn()
			}
			continue
		}

		if b < utf8.RuneSelf {
			i++
			typedRune = rune(b)
			typed = []byte{b}
		} else {
			ch, size := utf8.DecodeRune(data[i:])
			if ch == utf8.RuneError && size == 1 {
				typedRune = ch
				typed = []byte(string(ch))
			} else {
				typedRune = ch
				typed = []byte(string(ch))
			}
			i += size
		}

		r.terminalMu.Lock()
		r.mu.Lock()
		switch b {
		case 0x03: // Ctrl+C
			r.inputBuffer = nil
			r.inputLineVisible = false
			r.emit([]byte("^C\r\n"))
			r.emitPromptLocked(false)
			interrupt = true
		case 0x15: // Ctrl+U
			r.inputBuffer = nil
			if r.inputLineVisible {
				r.emit([]byte(codexClearInputLine))
				r.inputLineVisible = false
				r.emitPromptLocked(false)
			} else {
				r.emitPromptLocked(true)
			}
		case 0x7f, 0x08: // Backspace
			if len(r.inputBuffer) > 0 {
				r.inputBuffer = r.inputBuffer[:len(r.inputBuffer)-1]
				r.emit([]byte("\b \b"))
			}
		case '\r', '\n':
			text = strings.TrimSpace(string(r.inputBuffer))
			r.inputBuffer = nil
			r.inputLineVisible = false
			r.emit([]byte("\r\n"))
			if text == "" {
				r.emitPromptLocked(false)
			}
		default:
			if typedRune >= 0x20 || typedRune == '\t' {
				if !r.inputLineVisible {
					r.emitPromptLocked(true)
				}
				r.inputBuffer = append(r.inputBuffer, typedRune)
				r.emit(typed)
			}
		}
		r.mu.Unlock()
		r.terminalMu.Unlock()

		if interrupt {
			go r.interruptTurn()
		}
		if text != "" {
			if isCodexSlashCommand(text) {
				go r.handleSlashCommand(text)
			} else {
				r.addCodexTranscriptBlock("user", text, "", "", "")
				go r.sendUserInput(text)
			}
		}
	}
	return len(data), nil
}

func consumeCodexTerminalEscape(data []byte, start int) (int, bool) {
	if start < 0 || start >= len(data) || data[start] != 0x1b {
		return start + 1, false
	}
	if start+1 >= len(data) {
		return start + 1, true
	}

	switch data[start+1] {
	case '[':
		i := start + 2
		for i < len(data) {
			if data[i] >= 0x40 && data[i] <= 0x7e {
				return i + 1, false
			}
			i++
		}
		return len(data), false
	case 'O':
		if start+2 < len(data) {
			return start + 3, false
		}
		return len(data), false
	case ']':
		i := start + 2
		for i < len(data) {
			if data[i] == 0x07 {
				return i + 1, false
			}
			if data[i] == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
				return i + 2, false
			}
			i++
		}
		return len(data), false
	default:
		return start + 1, true
	}
}

func (r *CodexRunner) Resize(rows, cols uint16) error { return nil }

func (r *CodexRunner) Wait() error {
	<-r.done
	return nil
}

func (r *CodexRunner) PID() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		return r.cmd.Process.Pid
	}
	return 0
}

func (r *CodexRunner) Done() <-chan struct{} { return r.done }

func (r *CodexRunner) resolveBinaryPath() (string, error) {
	raw := ""
	if r.cfg != nil {
		raw = r.cfg.BackendConfig
	}
	cc := parseCodexConfig(raw)
	if cc.BinaryPath != "" {
		return cc.BinaryPath, nil
	}
	return exec.LookPath("codex")
}

func (r *CodexRunner) buildEnv() []string {
	env := os.Environ()
	set := make(map[string]string)
	for _, entry := range env {
		if i := strings.IndexByte(entry, '='); i > 0 {
			set[entry[:i]] = entry[i+1:]
		}
	}
	for k, v := range r.envVars {
		set[k] = expandCodexHome(v)
	}
	raw := ""
	if r.cfg != nil {
		raw = r.cfg.BackendConfig
	}
	cc := parseCodexConfig(raw)
	if cc.HomePath != "" {
		set["CODEX_HOME"] = expandCodexHome(cc.HomePath)
	}
	if set["TERM"] == "" {
		set["TERM"] = "xterm-256color"
	}

	out := make([]string, 0, len(set))
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

func expandCodexHome(v string) string {
	if v == "~" || strings.HasPrefix(v, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if v == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(v, "~/"))
		}
	}
	return v
}

func (r *CodexRunner) initialize(ctx context.Context) error {
	_, err := r.request(ctx, "initialize", map[string]interface{}{
		"clientInfo": map[string]interface{}{
			"name":    "openpoet",
			"title":   "OpenPoet",
			"version": "0.1.0",
		},
		"capabilities": map[string]interface{}{
			"experimentalApi": true,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize codex app-server: %w", err)
	}
	return r.notify("initialized", map[string]interface{}{})
}

func (r *CodexRunner) openThread(ctx context.Context) error {
	params := r.threadParams()
	var result json.RawMessage
	var err error

	if r.cfg.IsReopen && r.cfg.ProviderSessionID != "" {
		params["threadId"] = r.cfg.ProviderSessionID
		result, err = r.request(ctx, "thread/resume", params)
		if err != nil {
			r.write([]byte(fmt.Sprintf("\x1b[33mCodex thread resume failed; starting a new thread: %v\x1b[0m\r\n", err)))
			delete(params, "threadId")
		}
	}
	if result == nil {
		result, err = r.request(ctx, "thread/start", params)
		if err != nil {
			return fmt.Errorf("open codex thread: %w", err)
		}
	}

	threadID := codexThreadIDFromResult(result)
	if threadID == "" {
		return fmt.Errorf("codex thread response did not include thread id")
	}

	r.mu.Lock()
	r.providerThreadID = threadID
	r.mu.Unlock()
	if r.db != nil && r.cfg != nil {
		_ = r.db.UpdateSessionProviderSessionID(context.Background(), r.cfg.SessionID, threadID)
	}
	r.write([]byte(fmt.Sprintf("\x1b[90mCodex thread: %s\x1b[0m\r\n\r\n", threadID)))
	return nil
}

func (r *CodexRunner) threadParams() map[string]interface{} {
	cc := r.effectiveCodexConfig()
	approval := cc.ApprovalPolicy
	sandbox := cc.SandboxMode
	if r.cfg != nil && r.cfg.DangerouslySkipPermissions {
		approval = "never"
		sandbox = "danger-full-access"
	}
	params := map[string]interface{}{
		"cwd":                r.workDir,
		"approvalPolicy":     approval,
		"approvalsReviewer":  "user",
		"sandbox":            sandbox,
		"sessionStartSource": "startup",
	}
	if cc.Model != "" {
		params["model"] = cc.Model
	}
	if cc.ServiceTier != "" {
		params["serviceTier"] = cc.ServiceTier
	}
	if r.cfg != nil && r.cfg.AppendSystemPrompt != "" {
		params["developerInstructions"] = r.cfg.AppendSystemPrompt
	}
	if config := r.codexRuntimeConfig(); len(config) > 0 {
		params["config"] = config
	}
	return params
}

func (r *CodexRunner) codexRuntimeConfig() map[string]interface{} {
	if r.cfg == nil || strings.TrimSpace(r.cfg.MCPConfigJSON) == "" {
		return nil
	}
	var source struct {
		MCPServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.cfg.MCPConfigJSON), &source); err != nil || len(source.MCPServers) == 0 {
		return nil
	}
	mcpServers := make(map[string]interface{}, len(source.MCPServers))
	for name, server := range source.MCPServers {
		mcpServers[name] = server
	}
	return map[string]interface{}{
		"mcp_servers": mcpServers,
	}
}

func (r *CodexRunner) effectiveCodexConfig() CodexConfig {
	raw := ""
	if r.cfg != nil {
		raw = r.cfg.BackendConfig
	}
	cc := parseCodexConfig(raw)
	r.mu.Lock()
	if r.modelOverrideSet {
		cc.Model = r.modelOverride
	}
	if r.reasoningSet {
		cc.ReasoningEffort = r.reasoningOverride
	}
	if r.serviceTierSet {
		cc.ServiceTier = r.serviceTierOverride
	}
	if r.approvalOverride != "" {
		cc.ApprovalPolicy = normalizeCodexApprovalPolicy(r.approvalOverride)
	}
	if r.sandboxOverride != "" {
		cc.SandboxMode = normalizeCodexSandboxMode(r.sandboxOverride)
	}
	r.mu.Unlock()
	return cc
}

func (r *CodexRunner) displayCodexConfig() CodexConfig {
	cc := r.effectiveCodexConfig()
	r.mu.Lock()
	defaultModel := r.modelDefaults
	defaultsByID := r.modelDefaultsByID
	r.mu.Unlock()

	if cc.Model == "" {
		cc.Model = defaultModel.Model
	}
	if cc.ReasoningEffort == "" {
		if defaults, ok := defaultsByID[cc.Model]; ok && defaults.ReasoningEffort != "" {
			cc.ReasoningEffort = defaults.ReasoningEffort
		} else {
			cc.ReasoningEffort = defaultModel.ReasoningEffort
		}
	}
	if cc.ServiceTier == "" {
		if defaults, ok := defaultsByID[cc.Model]; ok && defaults.ServiceTier != "" {
			cc.ServiceTier = defaults.ServiceTier
		} else {
			cc.ServiceTier = defaultModel.ServiceTier
		}
	}
	return cc
}

func (r *CodexRunner) sendUserInput(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	method, _, err := r.startOrSteerTurn(ctx, text)
	if err != nil {
		r.write([]byte(fmt.Sprintf("\r\n\x1b[31mCodex %s failed: %v\x1b[0m\r\n", method, err)))
		r.writePrompt()
		return
	}
}

func (r *CodexRunner) startOrSteerTurn(ctx context.Context, text string) (string, string, error) {
	r.mu.Lock()
	threadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	r.mu.Unlock()
	if threadID == "" {
		return "turn/start", "", errors.New("Codex thread is not ready yet")
	}

	method := "turn/start"
	params := r.turnStartParams(threadID, text)
	if activeTurnID != "" {
		method = "turn/steer"
		params = map[string]interface{}{
			"threadId":       threadID,
			"expectedTurnId": activeTurnID,
			"input": []map[string]string{
				{"type": "text", "text": text},
			},
		}
	}

	result, err := r.request(ctx, method, params)
	if err != nil {
		return method, "", err
	}
	turnID := extractString(result, "turn.id")
	if turnID == "" {
		turnID = extractString(result, "turnId")
	}
	if turnID != "" {
		r.mu.Lock()
		r.activeTurnID = turnID
		r.mu.Unlock()
	}
	r.setCodexPhase("thinking", "Request sent")
	return method, turnID, nil
}

func (r *CodexRunner) turnStartParams(threadID, text string) map[string]interface{} {
	cc := r.effectiveCodexConfig()
	approval := cc.ApprovalPolicy
	sandbox := codexTurnSandboxPolicy(cc.SandboxMode)
	if r.cfg != nil && r.cfg.DangerouslySkipPermissions {
		approval = "never"
		sandbox = map[string]interface{}{"type": "dangerFullAccess"}
	}
	params := map[string]interface{}{
		"threadId":       threadID,
		"approvalPolicy": approval,
		"sandboxPolicy":  sandbox,
		"input": []map[string]string{
			{"type": "text", "text": text},
		},
	}
	if cc.Model != "" {
		params["model"] = cc.Model
	}
	if cc.ReasoningEffort != "" {
		params["effort"] = cc.ReasoningEffort
	}
	if cc.ServiceTier != "" {
		params["serviceTier"] = cc.ServiceTier
	}
	return params
}

var codexSlashCommandNames = map[string]bool{
	"agent":                 true,
	"app":                   true,
	"apps":                  true,
	"approve":               true,
	"archive":               true,
	"btw":                   true,
	"clear":                 true,
	"compact":               true,
	"copy":                  true,
	"debug-config":          true,
	"delete":                true,
	"diff":                  true,
	"exit":                  true,
	"experimental":          true,
	"fast":                  true,
	"feedback":              true,
	"fork":                  true,
	"goal":                  true,
	"help":                  true,
	"hooks":                 true,
	"ide":                   true,
	"import":                true,
	"init":                  true,
	"keymap":                true,
	"logout":                true,
	"mcp":                   true,
	"memories":              true,
	"mention":               true,
	"model":                 true,
	"new":                   true,
	"pet":                   true,
	"pets":                  true,
	"permissions":           true,
	"personality":           true,
	"plan":                  true,
	"plugins":               true,
	"ps":                    true,
	"quit":                  true,
	"raw":                   true,
	"rename":                true,
	"resume":                true,
	"review":                true,
	"sandbox-add-read-dir":  true,
	"setup-default-sandbox": true,
	"side":                  true,
	"skills":                true,
	"status":                true,
	"statusline":            true,
	"stop":                  true,
	"subagents":             true,
	"theme":                 true,
	"title":                 true,
	"usage":                 true,
	"vim":                   true,
}

func isCodexSlashCommand(text string) bool {
	name, _ := parseCodexSlashCommand(text)
	if name == "" {
		return false
	}
	return codexSlashCommandNames[name]
}

func parseCodexSlashCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	body := strings.TrimSpace(strings.TrimPrefix(text, "/"))
	if body == "" {
		return "help", ""
	}
	parts := strings.Fields(body)
	if len(parts) == 0 {
		return "help", ""
	}
	name := strings.ToLower(parts[0])
	args := strings.TrimSpace(strings.TrimPrefix(body, parts[0]))
	return name, args
}

func (r *CodexRunner) handleSlashCommand(text string) {
	name, args := parseCodexSlashCommand(text)
	switch name {
	case "help":
		r.writeSlashHelp()
	case "status":
		r.writeSlashStatus()
	case "model":
		r.handleSlashModel(args)
	case "fast":
		r.handleSlashFast(args)
	case "permissions":
		r.handleSlashPermissions(args)
	case "clear", "new":
		r.handleSlashNewThread(name)
	case "compact":
		r.handleSlashCompact()
	case "goal":
		r.handleSlashGoal(args)
	case "fork":
		r.handleSlashFork()
	case "resume":
		r.handleSlashResume(args)
	case "archive":
		r.handleSlashArchive(args)
	case "quit", "exit":
		r.write([]byte("\x1b[90mStopping Codex session.\x1b[0m\r\n"))
		go r.Stop()
	default:
		r.writeUnsupportedSlashCommand(name)
	}
}

func (r *CodexRunner) writeSlashHelp() {
	r.write([]byte("\x1b[1mCodex slash commands in OpenPoet\x1b[0m\r\n"))
	r.write([]byte("Supported: /help, /status, /model [name|default], /fast [on|off|status|default], /permissions [auto|read-only|workspace-write|danger-full-access|default], /goal [text|pause|resume|clear], /compact, /clear, /new, /fork, /resume [thread-id], /archive confirm, /quit, /exit.\r\n"))
	r.write([]byte("Other Codex TUI commands are recognized but not implemented in OpenPoet yet.\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) writeSlashStatus() {
	cc := r.displayCodexConfig()
	r.mu.Lock()
	threadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	modelOverrideSet := r.modelOverrideSet
	serviceTierSet := r.serviceTierSet
	approvalOverride := r.approvalOverride
	sandboxOverride := r.sandboxOverride
	sessionID := ""
	skipPermissions := false
	if r.cfg != nil {
		sessionID = r.cfg.SessionID
		skipPermissions = r.cfg.DangerouslySkipPermissions
	}
	r.mu.Unlock()

	approval := cc.ApprovalPolicy
	sandbox := cc.SandboxMode
	if skipPermissions {
		approval = "never"
		sandbox = "danger-full-access"
	}

	active := "no"
	if activeTurnID != "" {
		active = "yes (" + activeTurnID + ")"
	}

	r.write([]byte("\x1b[1mCodex status\x1b[0m\r\n"))
	r.write([]byte(fmt.Sprintf("Session: %s\r\n", displayCodexSlashValue(sessionID))))
	r.write([]byte(fmt.Sprintf("Thread: %s\r\n", displayCodexSlashValue(threadID))))
	r.write([]byte(fmt.Sprintf("Active turn: %s\r\n", active)))
	r.write([]byte(fmt.Sprintf("Model: %s%s\r\n", displayCodexSlashValue(cc.Model), codexSlashOverrideSuffix(modelOverrideSet))))
	r.write([]byte(fmt.Sprintf("Reasoning effort: %s\r\n", displayCodexSlashValue(cc.ReasoningEffort))))
	r.write([]byte(fmt.Sprintf("Service tier: %s%s\r\n", displayCodexSlashValue(cc.ServiceTier), codexSlashOverrideSuffix(serviceTierSet))))
	r.write([]byte(fmt.Sprintf("Approval policy: %s%s\r\n", approval, codexSlashOverrideSuffix(approvalOverride != ""))))
	r.write([]byte(fmt.Sprintf("Sandbox: %s%s\r\n", sandbox, codexSlashOverrideSuffix(sandboxOverride != ""))))
	r.writePrompt()
}

func displayCodexSlashValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(default)"
	}
	return v
}

func codexSlashOverrideSuffix(active bool) string {
	if active {
		return " (override)"
	}
	return ""
}

func (r *CodexRunner) handleSlashModel(args string) {
	arg := strings.TrimSpace(args)
	if arg == "" || strings.EqualFold(arg, "status") {
		cc := r.displayCodexConfig()
		r.write([]byte(fmt.Sprintf("Model: %s\r\n", displayCodexSlashValue(cc.Model))))
		r.writePrompt()
		return
	}
	if strings.EqualFold(arg, "default") || strings.EqualFold(arg, "reset") {
		r.mu.Lock()
		r.modelOverride = ""
		r.modelOverrideSet = false
		r.mu.Unlock()
		r.write([]byte("Model override cleared. The configured default will be used on future turns.\r\n"))
		r.writePrompt()
		return
	}
	if strings.ContainsAny(arg, "\r\n\t ") {
		r.writeSlashUsage("/model <model-name|default>")
		return
	}
	r.mu.Lock()
	r.modelOverride = arg
	r.modelOverrideSet = true
	r.mu.Unlock()
	r.write([]byte(fmt.Sprintf("Model override set to %s for future turns.\r\n", arg)))
	r.writePrompt()
}

func (r *CodexRunner) handleSlashFast(args string) {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" || arg == "status" {
		cc := r.displayCodexConfig()
		r.write([]byte(fmt.Sprintf("Service tier: %s\r\n", displayCodexSlashValue(cc.ServiceTier))))
		r.writePrompt()
		return
	}
	switch arg {
	case "on", "fast":
		r.mu.Lock()
		r.serviceTierOverride = "fast"
		r.serviceTierSet = true
		r.mu.Unlock()
		r.write([]byte("Fast service tier requested for future turns.\r\n"))
	case "off":
		r.mu.Lock()
		r.serviceTierOverride = ""
		r.serviceTierSet = true
		r.mu.Unlock()
		r.write([]byte("Fast service tier disabled for future turns.\r\n"))
	case "default", "reset":
		r.mu.Lock()
		r.serviceTierOverride = ""
		r.serviceTierSet = false
		r.mu.Unlock()
		r.write([]byte("Service tier override cleared. The configured default will be used on future turns.\r\n"))
	default:
		r.writeSlashUsage("/fast [on|off|status|default]")
		return
	}
	r.writePrompt()
}

func (r *CodexRunner) handleSlashPermissions(args string) {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" || arg == "status" {
		cc := r.effectiveCodexConfig()
		r.write([]byte(fmt.Sprintf("Permissions: approval=%s sandbox=%s\r\n", cc.ApprovalPolicy, cc.SandboxMode)))
		r.writeSlashUsage("/permissions [auto|read-only|workspace-write|danger-full-access|default]")
		return
	}
	if arg == "default" || arg == "reset" {
		r.mu.Lock()
		r.approvalOverride = ""
		r.sandboxOverride = ""
		r.mu.Unlock()
		cc := r.effectiveCodexConfig()
		r.write([]byte(fmt.Sprintf("Permission overrides cleared. Using approval=%s sandbox=%s\r\n", cc.ApprovalPolicy, cc.SandboxMode)))
		r.writePrompt()
		return
	}

	approval, sandbox, ok := codexSlashPermissionPreset(arg)
	if !ok {
		r.writeSlashUsage("/permissions [auto|read-only|workspace-write|danger-full-access|default]")
		return
	}
	r.mu.Lock()
	if approval != "" {
		r.approvalOverride = approval
	}
	if sandbox != "" {
		r.sandboxOverride = sandbox
	}
	r.mu.Unlock()
	cc := r.effectiveCodexConfig()
	r.write([]byte(fmt.Sprintf("Permissions updated for future turns: approval=%s sandbox=%s\r\n", cc.ApprovalPolicy, cc.SandboxMode)))
	r.writePrompt()
}

func codexSlashPermissionPreset(arg string) (string, string, bool) {
	switch arg {
	case "auto", "workspace", "workspace-write":
		return "on-request", "workspace-write", true
	case "read-only", "readonly", "read":
		return "untrusted", "read-only", true
	case "full-access", "danger-full-access":
		return "never", "danger-full-access", true
	case "untrusted", "on-request", "never":
		return arg, "", true
	}
	if sandbox := normalizeCodexSandboxMode(arg); sandbox == arg {
		return "", sandbox, true
	}
	return "", "", false
}

func (r *CodexRunner) handleSlashNewThread(command string) {
	if _, ok := r.slashThread(command, true); !ok {
		return
	}

	params := r.threadParams()
	params["sessionStartSource"] = "clear"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := r.request(ctx, "thread/start", params)
	if err != nil {
		r.writeSlashError("Codex thread/start failed: %v", err)
		return
	}
	threadID := codexThreadIDFromResult(result)
	if threadID == "" {
		r.writeSlashError("Codex thread/start response did not include a thread id")
		return
	}
	r.switchCodexThread(threadID)
	r.write([]byte("\x1b[2J\x1b[H"))
	r.write([]byte(fmt.Sprintf("\x1b[90mStarted new Codex thread: %s\x1b[0m\r\n", threadID)))
	r.writePrompt()
}

func (r *CodexRunner) handleSlashCompact() {
	threadID, ok := r.slashThread("compact", true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := r.request(ctx, "thread/compact/start", map[string]interface{}{"threadId": threadID}); err != nil {
		r.writeSlashError("Codex compact failed: %v", err)
		return
	}
	r.write([]byte("Codex compaction requested.\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) handleSlashGoal(args string) {
	threadID, ok := r.slashThread("goal", false)
	if !ok {
		return
	}
	arg := strings.TrimSpace(args)
	switch strings.ToLower(arg) {
	case "", "status", "view":
		r.readCodexGoal(threadID)
	case "clear", "unset", "remove":
		r.clearCodexGoal(threadID)
	case "pause", "paused":
		r.setCodexGoalStatus(threadID, "paused")
	case "resume", "active":
		r.setCodexGoalStatus(threadID, "active")
	case "complete", "completed", "done":
		r.setCodexGoalStatus(threadID, "complete")
	case "blocked":
		r.setCodexGoalStatus(threadID, "blocked")
	default:
		if len(arg) > 4000 {
			r.writeSlashError("Codex goal is too long; keep it at 4000 characters or less")
			return
		}
		r.setCodexGoal(threadID, arg)
	}
}

func (r *CodexRunner) readCodexGoal(threadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.request(ctx, "thread/goal/get", map[string]interface{}{"threadId": threadID})
	if err != nil {
		r.writeSlashError("Codex goal get failed: %v", err)
		return
	}
	r.write([]byte(codexGoalDescription(result) + "\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) clearCodexGoal(threadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.request(ctx, "thread/goal/clear", map[string]interface{}{"threadId": threadID}); err != nil {
		r.writeSlashError("Codex goal clear failed: %v", err)
		return
	}
	r.write([]byte("Codex goal cleared.\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) setCodexGoalStatus(threadID, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.request(ctx, "thread/goal/set", map[string]interface{}{
		"threadId": threadID,
		"status":   status,
	})
	if err != nil {
		r.writeSlashError("Codex goal update failed: %v", err)
		return
	}
	r.write([]byte(codexGoalDescription(result) + "\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) setCodexGoal(threadID, objective string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.request(ctx, "thread/goal/set", map[string]interface{}{
		"threadId":  threadID,
		"objective": objective,
		"status":    "active",
	})
	if err != nil {
		r.writeSlashError("Codex goal set failed: %v", err)
		return
	}
	r.write([]byte(codexGoalDescription(result) + "\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) handleSlashFork() {
	threadID, ok := r.slashThread("fork", true)
	if !ok {
		return
	}
	params := r.threadParams()
	params["threadId"] = threadID
	params["threadSource"] = "user"
	delete(params, "sessionStartSource")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := r.request(ctx, "thread/fork", params)
	if err != nil {
		r.writeSlashError("Codex fork failed: %v", err)
		return
	}
	newThreadID := codexThreadIDFromResult(result)
	if newThreadID == "" {
		r.writeSlashError("Codex fork response did not include a thread id")
		return
	}
	r.switchCodexThread(newThreadID)
	r.write([]byte(fmt.Sprintf("Forked Codex thread: %s\r\n", newThreadID)))
	r.writePrompt()
}

func (r *CodexRunner) handleSlashResume(args string) {
	threadID := strings.TrimSpace(args)
	if threadID == "" {
		r.write([]byte("Usage: /resume <codex-thread-id>\r\nOpenPoet session history is still managed from the Sessions list.\r\n"))
		r.writePrompt()
		return
	}
	if strings.ContainsAny(threadID, " \t\r\n") {
		r.writeSlashUsage("/resume <codex-thread-id>")
		return
	}
	if _, ok := r.slashThread("resume", true); !ok {
		return
	}

	params := r.threadParams()
	params["threadId"] = threadID
	delete(params, "sessionStartSource")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := r.request(ctx, "thread/resume", params)
	if err != nil {
		r.writeSlashError("Codex resume failed: %v", err)
		return
	}
	resumedThreadID := codexThreadIDFromResult(result)
	if resumedThreadID == "" {
		resumedThreadID = threadID
	}
	r.switchCodexThread(resumedThreadID)
	r.replaceCodexTranscriptFromThreadResponse(result)
	r.write([]byte(fmt.Sprintf("Resumed Codex thread: %s\r\n", resumedThreadID)))
	r.writePrompt()
}

func (r *CodexRunner) handleSlashArchive(args string) {
	if !strings.EqualFold(strings.TrimSpace(args), "confirm") {
		r.write([]byte("Archive exits this OpenPoet session. Run /archive confirm to archive the Codex thread.\r\n"))
		r.writePrompt()
		return
	}
	threadID, ok := r.slashThread("archive", true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.request(ctx, "thread/archive", map[string]interface{}{"threadId": threadID}); err != nil {
		r.writeSlashError("Codex archive failed: %v", err)
		return
	}
	r.write([]byte("Codex thread archived. Stopping session.\r\n"))
	go r.Stop()
}

func (r *CodexRunner) slashThread(command string, requireIdle bool) (string, bool) {
	r.mu.Lock()
	threadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	r.mu.Unlock()
	if threadID == "" {
		r.write([]byte("\x1b[31mCodex thread is not ready yet.\x1b[0m\r\n"))
		r.writePrompt()
		return "", false
	}
	if requireIdle && activeTurnID != "" {
		r.write([]byte(fmt.Sprintf("\x1b[33m/%s is unavailable while Codex is working. Press Ctrl+C first if you need to interrupt.\x1b[0m\r\n", command)))
		r.writePrompt()
		return "", false
	}
	return threadID, true
}

func (r *CodexRunner) switchCodexThread(threadID string) {
	r.mu.Lock()
	r.providerThreadID = threadID
	r.activeTurnID = ""
	r.interruptedTurns = make(map[string]bool)
	r.commandProcesses = make(map[string]map[string]codexCommandProcess)
	r.lastAssistantChunk = false
	r.lastReasoningChunk = false
	r.lastCommandOutChunk = false
	r.mu.Unlock()
	if r.db != nil && r.cfg != nil {
		_ = r.db.UpdateSessionProviderSessionID(context.Background(), r.cfg.SessionID, threadID)
	}
	r.setCodexPhase("idle", "Thread ready")
}

func codexThreadIDFromResult(result json.RawMessage) string {
	threadID := extractString(result, "thread.id")
	if threadID == "" {
		threadID = extractString(result, "threadId")
	}
	if threadID == "" {
		threadID = extractString(result, "id")
	}
	return threadID
}

type codexGoalPayload struct {
	Goal *codexGoal `json:"goal"`
}

type codexGoal struct {
	Objective   string `json:"objective"`
	Status      string `json:"status"`
	TokenBudget *int64 `json:"tokenBudget"`
}

func codexGoalDescription(raw json.RawMessage) string {
	var payload codexGoalPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Goal == nil {
		return "No Codex goal is set."
	}
	goal := payload.Goal
	parts := []string{"Codex goal"}
	if goal.Status != "" {
		parts = append(parts, "status="+goal.Status)
	}
	if goal.TokenBudget != nil {
		parts = append(parts, fmt.Sprintf("token_budget=%d", *goal.TokenBudget))
	}
	if strings.TrimSpace(goal.Objective) == "" {
		parts = append(parts, "objective=(empty)")
	} else {
		parts = append(parts, "objective="+goal.Objective)
	}
	return strings.Join(parts, " ")
}

func (r *CodexRunner) writeUnsupportedSlashCommand(name string) {
	if name == "" {
		name = "unknown"
	}
	r.write([]byte(fmt.Sprintf("Codex slash command /%s is not implemented in OpenPoet yet. Run /help for supported commands.\r\n", name)))
	r.writePrompt()
}

func (r *CodexRunner) writeSlashUsage(usage string) {
	r.write([]byte("Usage: " + usage + "\r\n"))
	r.writePrompt()
}

func (r *CodexRunner) writeSlashError(format string, args ...interface{}) {
	r.write([]byte(fmt.Sprintf("\x1b[31m"+format+"\x1b[0m\r\n", args...)))
	r.writePrompt()
}

func codexTurnSandboxPolicy(mode string) map[string]interface{} {
	switch normalizeCodexSandboxMode(mode) {
	case "read-only":
		return map[string]interface{}{"type": "readOnly"}
	case "danger-full-access":
		return map[string]interface{}{"type": "dangerFullAccess"}
	default:
		return map[string]interface{}{"type": "workspaceWrite"}
	}
}

func (r *CodexRunner) interruptTurn() {
	r.mu.Lock()
	threadID := r.providerThreadID
	turnID := r.activeTurnID
	processIDs := r.commandProcessIDsLocked(turnID)
	if turnID != "" {
		r.interruptedTurns[turnID] = true
		r.activeTurnID = ""
		r.lastAssistantChunk = false
		r.lastReasoningChunk = false
		r.lastCommandOutChunk = false
	}
	r.mu.Unlock()
	if threadID == "" || turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, processID := range processIDs {
		if err := signalCodexCommandProcess(processID); err != nil {
			log.Printf("[codex] local interrupt for turn %s process %s failed: %v", turnID, processID, err)
		}
	}
	_, _ = r.request(ctx, "turn/interrupt", map[string]interface{}{
		"threadId": threadID,
		"turnId":   turnID,
	})
}

func signalCodexCommandProcess(processID string) error {
	pid, err := strconv.Atoi(processID)
	if err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("invalid process id")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(os.Interrupt)
}

func (r *CodexRunner) request(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	ch := make(chan codexRPCResponse, 1)
	r.pending[id] = ch
	r.mu.Unlock()

	if err := r.send(map[string]interface{}{"id": id, "method": method, "params": params}); err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (r *CodexRunner) notify(method string, params interface{}) error {
	return r.send(map[string]interface{}{"method": method, "params": params})
}

func (r *CodexRunner) send(msg interface{}) error {
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	if stdin == nil {
		return errors.New("codex app-server stdin is not ready")
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = stdin.Write(b)
	return err
}

func (r *CodexRunner) readStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg codexWireMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("[codex] invalid app-server JSON: %v line=%s", err, string(line))
			continue
		}
		r.handleMessage(&msg)
	}
	if err := scanner.Err(); err != nil && !r.isShuttingDown() {
		log.Printf("[codex] stdout read error: %v", err)
	}
}

func (r *CodexRunner) readStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		r.recordStderrLine(line)
		log.Printf("[codex] %s", line)
	}
}

func (r *CodexRunner) recordStderrLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stderrTail = append(r.stderrTail, line)
	if len(r.stderrTail) > 5 {
		r.stderrTail = r.stderrTail[len(r.stderrTail)-5:]
	}
}

func (r *CodexRunner) stderrSummary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stderrTail) == 0 {
		return ""
	}
	return strings.Join(r.stderrTail, " | ")
}

func (r *CodexRunner) handleMessage(msg *codexWireMessage) {
	if len(msg.ID) > 0 && msg.Method == "" {
		if id, ok := parseJSONRPCID(msg.ID); ok {
			r.mu.Lock()
			ch := r.pending[id]
			delete(r.pending, id)
			r.mu.Unlock()
			if ch != nil {
				ch <- codexRPCResponse{Result: msg.Result, Error: msg.Error}
			}
		}
		return
	}
	if len(msg.ID) > 0 && msg.Method != "" {
		go r.handleServerRequest(msg)
		return
	}
	if msg.Method != "" {
		r.handleNotification(msg.Method, msg.Params)
	}
}

func parseJSONRPCID(raw json.RawMessage) (int, bool) {
	var id int
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}
	return 0, false
}

func (r *CodexRunner) handleServerRequest(msg *codexWireMessage) {
	switch msg.Method {
	case "item/commandExecution/requestApproval":
		decision := r.requestOpenPoetApproval("Bash", msg.Params)
		r.respond(msg.ID, map[string]interface{}{"decision": decision})
	case "execCommandApproval":
		decision := r.requestOpenPoetApproval("Bash", msg.Params)
		r.respond(msg.ID, map[string]interface{}{"decision": codexLegacyReviewDecision(decision)})
	case "item/fileChange/requestApproval":
		decision := r.requestOpenPoetApproval("FileChange", msg.Params)
		r.respond(msg.ID, map[string]interface{}{"decision": decision})
	case "applyPatchApproval":
		decision := r.requestOpenPoetApproval("FileChange", msg.Params)
		r.respond(msg.ID, map[string]interface{}{"decision": codexLegacyReviewDecision(decision)})
	case "item/permissions/requestApproval":
		result := r.requestOpenPoetPermissionsApproval(msg.Params)
		r.respond(msg.ID, result)
	case "item/tool/requestUserInput":
		answers := r.requestOpenPoetUserInput(msg.Params)
		r.respond(msg.ID, map[string]interface{}{"answers": answers})
	case "mcpServer/elicitation/request":
		result := r.requestOpenPoetMCPElicitation(msg.Params)
		r.respond(msg.ID, result)
	default:
		r.respondError(msg.ID, -32601, "Unsupported Codex app-server request: "+msg.Method)
	}
}

func codexLegacyReviewDecision(decision string) string {
	switch decision {
	case "accept":
		return "approved"
	case "acceptForSession":
		return "approved_for_session"
	case "decline":
		return "denied"
	default:
		return "abort"
	}
}

func (r *CodexRunner) respond(rawID json.RawMessage, result interface{}) {
	var id interface{}
	if err := json.Unmarshal(rawID, &id); err != nil {
		return
	}
	_ = r.send(map[string]interface{}{"id": id, "result": result})
}

func (r *CodexRunner) respondError(rawID json.RawMessage, code int, message string) {
	var id interface{}
	if err := json.Unmarshal(rawID, &id); err != nil {
		return
	}
	_ = r.send(map[string]interface{}{
		"id": id,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	})
}

func (r *CodexRunner) requestOpenPoetApproval(toolName string, params json.RawMessage) string {
	var payload map[string]interface{}
	_ = json.Unmarshal(params, &payload)

	if cacheKey, ok := codexApprovalCacheKey(toolName, payload); ok {
		r.mu.Lock()
		if r.allowForSession[cacheKey] {
			r.mu.Unlock()
			return "acceptForSession"
		}
		r.mu.Unlock()
	}

	hookEvent := map[string]interface{}{
		"hook_event_name": "PermissionRequest",
		"tool_name":       toolName,
		"tool_input":      payload,
	}
	if reason, ok := payload["reason"].(string); ok && reason != "" {
		hookEvent["message"] = reason
	}
	if proposed, ok := payload["proposedExecpolicyAmendment"]; ok {
		hookEvent["permission_suggestions"] = proposed
	}

	resp, err := r.postHook("permission", hookEvent)
	if err != nil {
		r.write([]byte(fmt.Sprintf("\r\n\x1b[31mOpenPoet approval request failed: %v\x1b[0m\r\n", err)))
		return "decline"
	}

	decision := codexApprovalDecisionFromHookResponse(resp)
	if decision == "acceptForSession" {
		if cacheKey, ok := codexApprovalCacheKey(toolName, payload); ok {
			r.mu.Lock()
			r.allowForSession[cacheKey] = true
			r.mu.Unlock()
		}
	}
	return decision
}

func codexApprovalDecisionFromHookResponse(resp json.RawMessage) string {
	behavior := extractString(resp, "hookSpecificOutput.decision.originalBehavior")
	if behavior == "" {
		behavior = extractString(resp, "hookSpecificOutput.decision.behavior")
	}
	switch behavior {
	case "allow":
		return "accept"
	case "allowAlways":
		return "acceptForSession"
	case "deny":
		return "decline"
	default:
		return "cancel"
	}
}

func codexApprovalCacheKey(toolName string, payload map[string]interface{}) (string, bool) {
	switch toolName {
	case "Bash":
		command := codexPayloadCommand(payload, "command")
		if command == "" {
			command = codexPayloadCommand(payload, "action.command")
		}
		if command == "" {
			return "", false
		}
		return "Bash:" + command, true
	case "FileChange":
		grantRoot := codexPayloadString(payload, "grantRoot")
		if grantRoot == "" {
			return "", false
		}
		return "FileChange:" + grantRoot, true
	default:
		return "", false
	}
}

func codexPayloadString(payload map[string]interface{}, dotted string) string {
	var cur interface{} = payload
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = m[part]
	}
	s, _ := cur.(string)
	return s
}

func codexPayloadCommand(payload map[string]interface{}, dotted string) string {
	v := codexPayloadValue(payload, dotted)
	switch command := v.(type) {
	case string:
		return command
	case []interface{}:
		parts := make([]string, 0, len(command))
		for _, part := range command {
			if s, ok := part.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case []string:
		return strings.Join(command, " ")
	default:
		return ""
	}
}

func codexPayloadValue(payload map[string]interface{}, dotted string) interface{} {
	var cur interface{} = payload
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func (r *CodexRunner) requestOpenPoetPermissionsApproval(params json.RawMessage) map[string]interface{} {
	var payload map[string]interface{}
	_ = json.Unmarshal(params, &payload)

	hookEvent := map[string]interface{}{
		"hook_event_name": "PermissionRequest",
		"tool_name":       "Permissions",
		"tool_input":      payload,
	}
	if reason, ok := payload["reason"].(string); ok && reason != "" {
		hookEvent["message"] = reason
	}

	resp, err := r.postHook("permission", hookEvent)
	if err != nil {
		r.write([]byte(fmt.Sprintf("\r\n\x1b[31mOpenPoet permissions request failed: %v\x1b[0m\r\n", err)))
		return codexDeniedPermissionProfile()
	}

	decision := codexApprovalDecisionFromHookResponse(resp)
	if decision != "accept" && decision != "acceptForSession" {
		return codexDeniedPermissionProfile()
	}

	permissions, _ := payload["permissions"].(map[string]interface{})
	result := map[string]interface{}{
		"permissions": permissions,
		"scope":       "turn",
	}
	if decision == "acceptForSession" {
		result["scope"] = "session"
	}
	return result
}

func codexDeniedPermissionProfile() map[string]interface{} {
	return map[string]interface{}{
		"permissions": map[string]interface{}{
			"network": map[string]interface{}{"enabled": false},
		},
		"scope": "turn",
	}
}

func (r *CodexRunner) requestOpenPoetMCPElicitation(params json.RawMessage) map[string]interface{} {
	var payload map[string]interface{}
	_ = json.Unmarshal(params, &payload)

	serverName, _ := payload["serverName"].(string)
	toolName := "MCP"
	if serverName != "" {
		toolName = "MCP:" + serverName
	}
	hookEvent := map[string]interface{}{
		"hook_event_name": "PermissionRequest",
		"tool_name":       toolName,
		"tool_input":      payload,
	}
	if message, ok := payload["message"].(string); ok && message != "" {
		hookEvent["message"] = message
	}

	resp, err := r.postHook("permission", hookEvent)
	if err != nil {
		r.write([]byte(fmt.Sprintf("\r\n\x1b[31mOpenPoet MCP approval request failed: %v\x1b[0m\r\n", err)))
		return map[string]interface{}{"action": "decline"}
	}
	return codexMCPElicitationResponseFromHookResponse(resp)
}

func codexMCPElicitationResponseFromHookResponse(resp json.RawMessage) map[string]interface{} {
	switch codexApprovalDecisionFromHookResponse(resp) {
	case "accept", "acceptForSession":
		return map[string]interface{}{"action": "accept"}
	case "decline":
		return map[string]interface{}{"action": "decline"}
	default:
		return map[string]interface{}{"action": "cancel"}
	}
}

func (r *CodexRunner) requestOpenPoetUserInput(params json.RawMessage) map[string]interface{} {
	var payload map[string]interface{}
	_ = json.Unmarshal(params, &payload)

	questions, _ := payload["questions"].([]interface{})
	if len(questions) == 0 {
		return map[string]interface{}{}
	}
	hookEvent := map[string]interface{}{
		"hook_event_name": "PermissionRequest",
		"tool_name":       "AskUserQuestion",
		"tool_input": map[string]interface{}{
			"questions": questions,
		},
	}

	resp, err := r.postHook("permission", hookEvent)
	if err != nil {
		r.write([]byte(fmt.Sprintf("\r\n\x1b[31mOpenPoet user input request failed: %v\x1b[0m\r\n", err)))
		return map[string]interface{}{}
	}

	answers := extractMap(resp, "hookSpecificOutput.decision.updatedInput.answers")
	return codexUserInputAnswersForQuestions(questions, answers)
}

func codexUserInputAnswersForQuestions(questions []interface{}, answers map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	for _, q := range questions {
		qm, ok := q.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := qm["id"].(string)
		question, _ := qm["question"].(string)
		if id == "" {
			continue
		}
		value := answers[id]
		if value == nil && question != "" {
			value = answers[question]
		}
		switch v := value.(type) {
		case []interface{}:
			arr := make([]string, 0, len(v))
			for _, entry := range v {
				if s, ok := entry.(string); ok {
					arr = append(arr, s)
				}
			}
			out[id] = map[string]interface{}{"answers": arr}
		case string:
			out[id] = map[string]interface{}{"answers": []string{v}}
		}
	}
	return out
}

func (r *CodexRunner) postHook(kind string, event map[string]interface{}) (json.RawMessage, error) {
	if r.cfg.ServerAddr == "" {
		return nil, errors.New("missing OpenPoet server address")
	}
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://%s/api/hooks/%s", r.cfg.ServerAddr, kind)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", r.cfg.SessionID)
	req.Header.Set("X-Backend", "codex")

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hook %s returned %s: %s", kind, resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (r *CodexRunner) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "thread/started":
		if id := extractString(params, "thread.id"); id != "" {
			r.mu.Lock()
			r.providerThreadID = id
			r.mu.Unlock()
			if r.db != nil && r.cfg != nil {
				_ = r.db.UpdateSessionProviderSessionID(context.Background(), r.cfg.SessionID, id)
			}
			r.setCodexPhase("idle", "Thread ready")
		}
	case "thread/compacted":
		r.addCodexTranscriptBlock("status", "Compaction complete.", "Codex", "", "complete")
		r.write([]byte("\r\n\x1b[90mCodex compaction complete.\x1b[0m\r\n"))
		r.writePrompt()
		r.setCodexPhase("idle", "Compaction complete")
	case "thread/goal/updated":
		// Goal slash commands print their own request result. Startup can also
		// replay goal notifications, so keep these out of the terminal.
	case "thread/goal/cleared":
		// See thread/goal/updated.
	case "turn/started":
		if id := extractString(params, "turn.id"); id != "" {
			r.mu.Lock()
			r.activeTurnID = id
			r.lastAssistantChunk = false
			r.lastReasoningChunk = false
			r.lastCommandOutChunk = false
			r.mu.Unlock()
			r.resetCodexTranscriptStream()
			r.setCodexPhase("thinking", "Starting turn")
		}
	case "turn/completed", "turn/failed":
		turnID := extractString(params, "turn.id")
		if turnID == "" {
			turnID = extractString(params, "turnId")
		}
		turnStatus := extractString(params, "turn.status")
		r.mu.Lock()
		interrupted := turnID != "" && (r.interruptedTurns[turnID] || turnStatus == "interrupted")
		if turnID != "" && turnStatus == "interrupted" {
			r.interruptedTurns[turnID] = true
		}
		if r.activeTurnID == "" || turnID == "" || r.activeTurnID == turnID {
			r.activeTurnID = ""
			r.lastAssistantChunk = false
			r.lastReasoningChunk = false
			r.lastCommandOutChunk = false
		}
		if turnID != "" {
			delete(r.commandProcesses, turnID)
		}
		r.mu.Unlock()
		if interrupted {
			r.resetCodexTranscriptStream()
			r.addCodexTranscriptBlock("status", "Turn interrupted.", "Codex", "", "interrupted")
			r.setCodexPhase("idle", "Turn interrupted")
			return
		}
		if method == "turn/failed" {
			msg := extractString(params, "error.message")
			if msg == "" {
				msg = string(params)
			}
			r.resetCodexTranscriptStream()
			r.addCodexTranscriptBlock("error", msg, "Codex turn failed", "", "failed")
			r.write([]byte(fmt.Sprintf("\r\n\x1b[31mCodex turn failed: %s\x1b[0m\r\n", msg)))
			r.setCodexPhase("error", msg)
		} else {
			r.resetCodexTranscriptStream()
			r.setCodexPhase("idle", "Waiting for instructions")
		}
		r.write([]byte("\r\n"))
		r.writePrompt()
	case "item/agentMessage/delta":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		r.setCodexPhase("responding", "Writing response")
		r.writeContentDelta(params, "assistant")
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		r.setCodexPhase("thinking", "Reasoning")
		r.writeContentDelta(params, "reasoning")
	case "item/commandExecution/outputDelta":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		r.setCodexPhase("running_command", "Reading command output")
		r.writeContentDelta(params, "command")
	case "item/fileChange/outputDelta":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		r.setCodexPhase("editing", "Applying file changes")
		r.writeContentDelta(params, "file")
	case "turn/plan/updated":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		plan := extractString(params, "plan")
		if plan == "" {
			plan = extractString(params, "markdown")
		}
		if plan != "" {
			r.persistPlan(plan)
		}
	case "item/plan/delta":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		delta := extractString(params, "delta")
		if delta != "" {
			r.addCodexTranscriptDelta("plan", codexItemID(params), delta)
			r.write(codexTerminalTextBytes(delta))
		}
	case "item/started":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		r.writeItemStarted(params)
	case "item/completed":
		if r.shouldSuppressTurnOutput(params) {
			return
		}
		r.writeItemCompleted(params)
	case "thread/tokenUsage/updated":
		r.writeTokenUsage(params)
	case "config/warning":
		msg := extractString(params, "message")
		if msg != "" {
			r.addCodexTranscriptBlock("warning", msg, "Config warning", "", "warning")
			r.write([]byte(fmt.Sprintf("\r\n\x1b[33m%s\x1b[0m\r\n", msg)))
		}
	case "account/rateLimits/updated":
		// Keep this out of the terminal by default; the payload is noisy and
		// can arrive often.
	default:
		// Unknown notifications are expected as app-server evolves. Keep them
		// in server logs without cluttering the user terminal.
		log.Printf("[codex] notification %s: %.500s", method, string(params))
	}
}

func (r *CodexRunner) writeContentDelta(params json.RawMessage, kind string) {
	delta := extractString(params, "delta")
	if delta == "" {
		return
	}
	r.mu.Lock()
	prefix := ""
	switch kind {
	case "assistant":
		if !r.lastAssistantChunk {
			prefix = "\r\n\x1b[32mCodex:\x1b[0m "
		}
		r.lastAssistantChunk = true
		r.lastReasoningChunk = false
		r.lastCommandOutChunk = false
	case "reasoning":
		if !r.lastReasoningChunk {
			prefix = "\r\n\x1b[90mReasoning:\x1b[0m "
		}
		r.lastReasoningChunk = true
		r.lastAssistantChunk = false
		r.lastCommandOutChunk = false
	case "command", "file":
		if !r.lastCommandOutChunk {
			prefix = "\r\n\x1b[90mOutput:\x1b[0m\r\n"
		}
		r.lastCommandOutChunk = true
		r.lastAssistantChunk = false
		r.lastReasoningChunk = false
	}
	r.mu.Unlock()
	r.addCodexTranscriptDelta(kind, codexItemID(params), delta)
	r.write(append([]byte(prefix), codexTerminalTextBytes(delta)...))
}

func codexItemID(params json.RawMessage) string {
	itemID := extractString(params, "itemId")
	if itemID == "" {
		itemID = extractString(params, "item.id")
	}
	return itemID
}

func (r *CodexRunner) shouldSuppressTurnOutput(params json.RawMessage) bool {
	turnID := extractString(params, "turnId")
	if turnID == "" {
		turnID = extractString(params, "turn.id")
	}
	if turnID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interruptedTurns[turnID]
}

func (r *CodexRunner) commandProcessIDsLocked(turnID string) []string {
	if turnID == "" {
		return nil
	}
	processes := r.commandProcesses[turnID]
	if len(processes) == 0 {
		return nil
	}
	out := make([]string, 0, len(processes))
	for processID := range processes {
		out = append(out, processID)
	}
	return out
}

func (r *CodexRunner) writeItemStarted(params json.RawMessage) {
	itemType := extractString(params, "item.type")
	if itemType == "" {
		itemType = extractString(params, "type")
	}
	switch itemType {
	case "commandExecution", "command_execution":
		r.trackCommandProcess(params)
		cmd := extractString(params, "item.command")
		if cmd == "" {
			cmd = extractString(params, "command")
		}
		if cmd != "" {
			r.setCodexPhase("running_command", cmd)
			r.addCodexTranscriptBlock("command", "", "Running command", cmd, "running")
			r.write([]byte(fmt.Sprintf("\r\n\x1b[90m$ %s\x1b[0m\r\n", cmd)))
		}
	case "file_change":
		path := extractString(params, "item.path")
		if path != "" {
			r.setCodexPhase("editing", path)
			r.addCodexTranscriptBlock("file", path, "Editing file", "", "editing")
			r.write([]byte(fmt.Sprintf("\r\n\x1b[90mEditing %s\x1b[0m\r\n", path)))
		}
	}
}

func (r *CodexRunner) trackCommandProcess(params json.RawMessage) {
	turnID := extractString(params, "turnId")
	processID := extractString(params, "item.processId")
	if processID == "" {
		processID = extractString(params, "processId")
	}
	command := extractString(params, "item.command")
	if command == "" {
		command = extractString(params, "command")
	}
	if turnID == "" || processID == "" {
		return
	}
	r.mu.Lock()
	if r.commandProcesses[turnID] == nil {
		r.commandProcesses[turnID] = make(map[string]codexCommandProcess)
	}
	r.commandProcesses[turnID][processID] = codexCommandProcess{
		ID:        processID,
		Command:   command,
		StartedAt: time.Now(),
	}
	r.mu.Unlock()
	r.publishCodexState()
}

func (r *CodexRunner) writeItemCompleted(params json.RawMessage) {
	r.untrackCommandProcess(params)
	status := extractString(params, "item.status")
	if status == "failed" {
		msg := extractString(params, "item.error.message")
		if msg == "" {
			msg = extractString(params, "error.message")
		}
		if msg != "" {
			r.addCodexTranscriptBlock("error", msg, "Item failed", "", "failed")
			r.write([]byte(fmt.Sprintf("\r\n\x1b[31m%s\x1b[0m\r\n", msg)))
		}
	}
}

func (r *CodexRunner) untrackCommandProcess(params json.RawMessage) {
	turnID := extractString(params, "turnId")
	processID := extractString(params, "item.processId")
	if processID == "" {
		processID = extractString(params, "processId")
	}
	if turnID == "" || processID == "" {
		return
	}
	r.mu.Lock()
	if processes := r.commandProcesses[turnID]; processes != nil {
		delete(processes, processID)
		if len(processes) == 0 {
			delete(r.commandProcesses, turnID)
		}
	}
	r.mu.Unlock()
	r.publishCodexState()
}

func (r *CodexRunner) writeTokenUsage(params json.RawMessage) {
	input := extractFloat(params, "usage.total.inputTokens")
	output := extractFloat(params, "usage.total.outputTokens")
	if input == 0 && output == 0 {
		input = extractFloat(params, "total.inputTokens")
		output = extractFloat(params, "total.outputTokens")
	}
	if input > 0 || output > 0 {
		r.mu.Lock()
		r.lastInputTokens = input
		r.lastOutputTokens = output
		r.mu.Unlock()
		r.addCodexTranscriptBlock("tokens", fmt.Sprintf("Input %.0f, output %.0f", input, output), "Token usage", "", "complete")
		r.write([]byte(fmt.Sprintf("\r\n\x1b[90mTokens: in %.0f, out %.0f\x1b[0m\r\n", input, output)))
		r.publishCodexState()
	}
}

func (r *CodexRunner) setCodexPhase(phase, detail string) {
	phase = strings.TrimSpace(phase)
	detail = strings.TrimSpace(detail)
	if phase == "" {
		phase = "idle"
	}
	r.mu.Lock()
	if r.agentPhase == phase && r.agentDetail == detail {
		r.mu.Unlock()
		return
	}
	r.agentPhase = phase
	r.agentDetail = detail
	r.mu.Unlock()
	r.publishCodexState()
}

func (r *CodexRunner) publishCodexState() {
	if r.hub == nil || r.cfg == nil || r.cfg.SessionID == "" {
		return
	}
	r.hub.BroadcastToChannel("session:"+r.cfg.SessionID, &websocket.Message{
		Type: websocket.MsgTypeCodexState,
		Data: r.codexStateSnapshot(),
	})
}

func (r *CodexRunner) codexStateSnapshot() map[string]interface{} {
	cc := r.displayCodexConfig()
	r.mu.Lock()
	defer r.mu.Unlock()

	sessionID := ""
	skipPermissions := false
	if r.cfg != nil {
		sessionID = r.cfg.SessionID
		skipPermissions = r.cfg.DangerouslySkipPermissions
	}
	approval := cc.ApprovalPolicy
	sandbox := cc.SandboxMode
	if skipPermissions {
		approval = "never"
		sandbox = "danger-full-access"
	}

	phase := r.agentPhase
	detail := r.agentDetail
	if phase == "" {
		if r.activeTurnID != "" {
			phase = "working"
		} else if r.initialized {
			phase = "idle"
		} else {
			phase = "starting"
		}
	}
	if detail == "" {
		if r.activeTurnID != "" {
			detail = "Turn in progress"
		} else {
			detail = "Waiting for instructions"
		}
	}

	processes := make([]map[string]interface{}, 0)
	for turnID, byProcess := range r.commandProcesses {
		for _, proc := range byProcess {
			processes = append(processes, map[string]interface{}{
				"id":         proc.ID,
				"turnId":     turnID,
				"command":    proc.Command,
				"started_at": proc.StartedAt,
			})
		}
	}

	return map[string]interface{}{
		"sessionId":              sessionID,
		"threadId":               r.providerThreadID,
		"activeTurnId":           r.activeTurnID,
		"activeTurn":             r.activeTurnID != "",
		"phase":                  phase,
		"detail":                 detail,
		"model":                  cc.Model,
		"reasoningEffort":        cc.ReasoningEffort,
		"serviceTier":            cc.ServiceTier,
		"approvalPolicy":         approval,
		"sandboxMode":            sandbox,
		"modelOverride":          r.modelOverrideSet,
		"reasoningOverride":      r.reasoningSet,
		"serviceTierOverride":    r.serviceTierSet,
		"approvalOverride":       r.approvalOverride != "",
		"sandboxOverride":        r.sandboxOverride != "",
		"skipPermissions":        skipPermissions,
		"lastInputTokens":        r.lastInputTokens,
		"lastOutputTokens":       r.lastOutputTokens,
		"activeProcessCount":     len(processes),
		"activeCommandProcesses": processes,
	}
}

const (
	// codexTranscriptSnapshotMaxBytes bounds the total transcript text sent to
	// a client when it opens a session. Long-running sessions accumulate tens
	// of MB of streamed deltas; replaying all of it freezes the browser tab.
	codexTranscriptSnapshotMaxBytes = 1_500_000
	// codexTranscriptSnapshotMaxItemBytes bounds a single merged item so one
	// giant command output cannot consume the whole snapshot budget.
	codexTranscriptSnapshotMaxItemBytes = 256_000
)

func (r *CodexRunner) codexTranscriptSnapshot() map[string]interface{} {
	r.mu.Lock()
	events := make([]codexTranscriptEvent, len(r.transcript))
	copy(events, r.transcript)
	r.mu.Unlock()

	merged := mergeCodexTranscriptEvents(events)
	merged, truncated := capCodexTranscriptEvents(merged,
		codexTranscriptSnapshotMaxBytes, codexTranscriptSnapshotMaxItemBytes)
	return map[string]interface{}{"events": merged, "truncated": truncated}
}

// mergeCodexTranscriptEvents collapses streamed delta chunks (same ID with
// Append set) into one complete event per ID, mirroring the client-side merge
// in structured-view.js. Sending merged events lets the client render each
// card exactly once instead of re-rendering per chunk.
func mergeCodexTranscriptEvents(events []codexTranscriptEvent) []codexTranscriptEvent {
	merged := make([]codexTranscriptEvent, 0, len(events))
	index := make(map[int]int, len(events))
	for _, event := range events {
		i, ok := index[event.ID]
		if !ok {
			event.Append = false
			index[event.ID] = len(merged)
			merged = append(merged, event)
			continue
		}
		item := &merged[i]
		if event.Kind != "" {
			item.Kind = event.Kind
		}
		if event.Title != "" {
			item.Title = event.Title
		}
		if event.Command != "" {
			item.Command = event.Command
		}
		if event.Status != "" {
			item.Status = event.Status
		}
		if !event.CreatedAt.IsZero() {
			item.CreatedAt = event.CreatedAt
		}
		if event.Append {
			item.Text += event.Text
		} else if event.Text != "" {
			item.Text = event.Text
		}
	}
	return merged
}

// capCodexTranscriptEvents keeps the newest events whose combined text fits
// maxTotalBytes, truncating any single item to maxItemBytes. Returns the
// capped slice and whether anything was dropped or trimmed.
func capCodexTranscriptEvents(events []codexTranscriptEvent, maxTotalBytes, maxItemBytes int) ([]codexTranscriptEvent, bool) {
	truncated := false
	total := 0
	start := 0
	for i := len(events) - 1; i >= 0; i-- {
		if len(events[i].Text) > maxItemBytes {
			events[i].Text = "… [earlier output trimmed]\n" + tailBytesUTF8(events[i].Text, maxItemBytes)
			truncated = true
		}
		size := len(events[i].Text) + len(events[i].Command) + len(events[i].Title)
		if total+size > maxTotalBytes && i < len(events)-1 {
			start = i + 1
			truncated = true
			break
		}
		total += size
	}
	return events[start:], truncated
}

// tailBytesUTF8 returns the last n bytes of s, extended forward as needed so
// the cut does not split a UTF-8 rune.
func tailBytesUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func (r *CodexRunner) replaceCodexTranscriptFromThreadResponse(raw json.RawMessage) bool {
	var response codexThreadResumeResponse
	if len(raw) == 0 || json.Unmarshal(raw, &response) != nil {
		return false
	}
	if response.Thread == nil {
		return false
	}

	events := codexTranscriptEventsFromThreadTurns(response.Thread.Turns)
	r.replaceCodexTranscript(events)
	return true
}

func codexTranscriptEventsFromThreadTurns(turns []codexThreadResumeTurn) []codexTranscriptEvent {
	var events []codexTranscriptEvent
	for _, turn := range turns {
		createdAt := time.Now()
		if turn.StartedAt != nil && *turn.StartedAt > 0 {
			createdAt = time.Unix(*turn.StartedAt, 0)
		}
		for _, item := range turn.Items {
			event, ok := codexTranscriptEventFromThreadItem(item, createdAt)
			if ok {
				events = append(events, event)
			}
		}
		if strings.EqualFold(turn.Status, "failed") && turn.Error != nil && strings.TrimSpace(turn.Error.Message) != "" {
			events = append(events, codexTranscriptEvent{
				Kind:      "error",
				Text:      turn.Error.Message,
				Title:     "Codex turn failed",
				Status:    "failed",
				CreatedAt: createdAt,
			})
		}
	}
	return events
}

func codexTranscriptEventFromThreadItem(item codexThreadResumeItem, createdAt time.Time) (codexTranscriptEvent, bool) {
	event := codexTranscriptEvent{CreatedAt: createdAt}
	switch item.Type {
	case "userMessage":
		text := strings.TrimSpace(codexThreadUserInputText(item.Content))
		if text == "" {
			return event, false
		}
		event.Kind = "user"
		event.Text = text
	case "agentMessage":
		text := strings.TrimSpace(item.Text)
		if text == "" {
			return event, false
		}
		event.Kind = "assistant"
		event.Text = text
	case "plan":
		event.Kind = "plan"
		event.Text = item.Text
	case "reasoning":
		text := strings.TrimSpace(strings.Join(item.Summary, "\n\n"))
		if text == "" {
			text = strings.TrimSpace(strings.Join(codexThreadContentStrings(item.Content), "\n\n"))
		}
		if text == "" {
			return event, false
		}
		event.Kind = "reasoning"
		event.Text = text
	case "commandExecution":
		event.Kind = "command"
		event.Title = "Command"
		event.Command = item.Command
		event.Status = item.Status
		if item.AggregatedOutput != nil {
			event.Text = *item.AggregatedOutput
		}
	case "fileChange":
		event.Kind = "file"
		event.Title = "File change"
		event.Text = strings.Join(codexThreadChangedPaths(item.Changes), "\n")
		event.Status = item.Status
	case "mcpToolCall":
		event.Kind = "command"
		event.Title = "MCP tool"
		event.Command = strings.Trim(strings.Join([]string{item.Server, item.Tool}, " "), " ")
		event.Text = codexThreadToolText(item.Result, item.Error)
		event.Status = item.Status
	case "dynamicToolCall":
		event.Kind = "command"
		event.Title = "Tool"
		event.Command = strings.Trim(strings.Join([]string{item.Namespace, item.Tool}, " "), " ")
		event.Text = codexThreadToolText(item.Result, item.Error)
		event.Status = item.Status
	case "webSearch":
		event.Kind = "command"
		event.Title = "Web search"
		event.Command = item.Query
	case "imageView":
		event.Kind = "file"
		event.Title = "Image"
		event.Text = item.Path
	case "imageGeneration":
		event.Kind = "file"
		event.Title = "Image generation"
		event.Text = codexThreadToolText(item.Result, item.Error)
		event.Status = item.Status
	case "enteredReviewMode", "exitedReviewMode":
		event.Kind = "status"
		event.Title = "Review"
		event.Text = item.Review
	case "contextCompaction":
		event.Kind = "status"
		event.Title = "Codex"
		event.Text = "Context compacted."
		event.Status = "complete"
	default:
		return event, false
	}
	if event.Kind == "" {
		return event, false
	}
	return event, true
}

func codexThreadContentStrings(raw json.RawMessage) []string {
	var stringsContent []string
	if len(raw) > 0 && json.Unmarshal(raw, &stringsContent) == nil {
		return stringsContent
	}
	var inputContent []codexThreadUserInput
	if len(raw) > 0 && json.Unmarshal(raw, &inputContent) == nil {
		parts := make([]string, 0, len(inputContent))
		for _, entry := range inputContent {
			if entry.Text != "" {
				parts = append(parts, entry.Text)
			}
		}
		return parts
	}
	return nil
}

func codexThreadUserInputText(raw json.RawMessage) string {
	var content []codexThreadUserInput
	if len(raw) == 0 || json.Unmarshal(raw, &content) != nil {
		return ""
	}
	parts := make([]string, 0, len(content))
	for _, item := range content {
		switch item.Type {
		case "text":
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		case "image":
			parts = append(parts, "[image] "+item.URL)
		case "localImage":
			parts = append(parts, "[image] "+item.Path)
		case "skill", "mention":
			label := item.Name
			if label == "" {
				label = item.Path
			}
			if label != "" {
				parts = append(parts, "@"+label)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func codexThreadChangedPaths(changes []codexThreadFileChange) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		if change.Path != "" {
			paths = append(paths, change.Path)
		}
	}
	return paths
}

func codexThreadToolText(result interface{}, itemErr *codexThreadResumeError) string {
	if itemErr != nil && itemErr.Message != "" {
		return itemErr.Message
	}
	switch v := result.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

func (r *CodexRunner) replaceCodexTranscript(events []codexTranscriptEvent) {
	for i := range events {
		events[i].ID = i + 1
		if events[i].CreatedAt.IsZero() {
			events[i].CreatedAt = time.Now()
		}
	}

	r.mu.Lock()
	r.transcript = r.transcript[:0]
	r.transcriptSeq = len(events)
	r.transcriptStreams = nil
	for _, event := range events {
		r.appendCodexTranscriptLocked(event)
	}
	r.mu.Unlock()

	if r.db == nil || r.cfg == nil || r.cfg.SessionID == "" {
		return
	}
	ctx := context.Background()
	if err := r.db.ClearCodexTranscriptEvents(ctx, r.cfg.SessionID); err != nil {
		log.Printf("[Codex] failed to clear transcript events for session %s: %v", r.cfg.SessionID, err)
		return
	}
	for _, event := range events {
		r.persistCodexTranscriptEvent(event)
	}
}

func (r *CodexRunner) addCodexTranscriptBlock(kind, text, title, command, status string) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "status"
	}
	event := codexTranscriptEvent{
		Kind:      kind,
		Text:      text,
		Title:     title,
		Command:   command,
		Status:    status,
		CreatedAt: time.Now(),
	}
	r.mu.Lock()
	r.transcriptSeq++
	event.ID = r.transcriptSeq
	r.appendCodexTranscriptLocked(event)
	r.mu.Unlock()
	r.persistCodexTranscriptEvent(event)
	r.publishCodexTranscript(event)
}

// addCodexTranscriptDelta appends streamed text to the transcript event that
// accumulates the given item's content. Streams are keyed by kind+itemID so a
// message keeps growing in one event even when tool-call blocks land between
// its deltas (the app-server interleaves parallel item streams). An empty
// itemID falls back to a per-kind stream scoped to the current turn.
func (r *CodexRunner) addCodexTranscriptDelta(kind, itemID, text string) {
	if text == "" {
		return
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "assistant"
	}
	streamKey := kind + ":" + itemID
	event := codexTranscriptEvent{
		Kind:      kind,
		Text:      text,
		CreatedAt: time.Now(),
	}
	r.mu.Lock()
	if r.transcriptStreams == nil {
		r.transcriptStreams = make(map[string]int)
	}
	if id, ok := r.transcriptStreams[streamKey]; ok {
		event.ID = id
		event.Append = true
	} else {
		r.transcriptSeq++
		r.transcriptStreams[streamKey] = r.transcriptSeq
		event.ID = r.transcriptSeq
	}
	r.appendCodexTranscriptLocked(event)
	r.mu.Unlock()
	r.persistCodexTranscriptEvent(event)
	r.publishCodexTranscript(event)
}

func (r *CodexRunner) resetCodexTranscriptStream() {
	r.mu.Lock()
	r.transcriptStreams = nil
	r.mu.Unlock()
}

func (r *CodexRunner) appendCodexTranscriptLocked(event codexTranscriptEvent) {
	r.transcript = append(r.transcript, event)
	const maxTranscriptEvents = 2000
	if len(r.transcript) > maxTranscriptEvents {
		copy(r.transcript, r.transcript[len(r.transcript)-maxTranscriptEvents:])
		r.transcript = r.transcript[:maxTranscriptEvents]
	}
}

func (r *CodexRunner) publishCodexTranscript(event codexTranscriptEvent) {
	if r.hub == nil || r.cfg == nil || r.cfg.SessionID == "" {
		return
	}
	r.hub.BroadcastToChannel("session:"+r.cfg.SessionID, &websocket.Message{
		Type: websocket.MsgTypeCodexTranscript,
		Data: event,
	})
}

func (r *CodexRunner) loadPersistedCodexTranscript(ctx context.Context) {
	if r.db == nil || r.cfg == nil || r.cfg.SessionID == "" {
		return
	}
	events, err := r.db.ListCodexTranscriptEvents(ctx, r.cfg.SessionID, 4000)
	if err != nil || len(events) == 0 {
		if err != nil {
			log.Printf("[Codex] failed to load transcript history for session %s: %v", r.cfg.SessionID, err)
		}
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcript = r.transcript[:0]
	maxEventID := 0
	for _, persisted := range events {
		event := codexTranscriptEvent{
			ID:        persisted.EventID,
			Kind:      persisted.Kind,
			Text:      persisted.Text,
			Title:     persisted.Title,
			Command:   persisted.Command,
			Status:    persisted.Status,
			Append:    persisted.Append,
			CreatedAt: persisted.CreatedAt,
		}
		if event.ID > maxEventID {
			maxEventID = event.ID
		}
		r.appendCodexTranscriptLocked(event)
	}
	if maxEventID > r.transcriptSeq {
		r.transcriptSeq = maxEventID
	}
	r.transcriptStreams = nil
}

func (r *CodexRunner) persistCodexTranscriptEvent(event codexTranscriptEvent) {
	if r.db == nil || r.cfg == nil || r.cfg.SessionID == "" || event.ID <= 0 {
		return
	}
	err := r.db.InsertCodexTranscriptEvent(context.Background(), &database.CodexTranscriptEvent{
		SessionID: r.cfg.SessionID,
		EventID:   event.ID,
		Kind:      event.Kind,
		Text:      event.Text,
		Title:     event.Title,
		Command:   event.Command,
		Status:    event.Status,
		Append:    event.Append,
		CreatedAt: event.CreatedAt,
	})
	if err != nil {
		log.Printf("[Codex] failed to persist transcript event for session %s: %v", r.cfg.SessionID, err)
	}
}

func (r *CodexRunner) persistPlan(plan string) {
	if r.db != nil && r.cfg != nil {
		_ = r.db.UpdateSessionPlan(context.Background(), r.cfg.SessionID, plan)
	}
	if r.hub != nil && r.cfg != nil {
		r.hub.BroadcastToChannel("session:"+r.cfg.SessionID, &websocket.Message{
			Type: websocket.MsgTypeSessionPlanUpdated,
			Data: map[string]interface{}{
				"session_id": r.cfg.SessionID,
				"plan":       plan,
			},
		})
	}
	out := append([]byte("\r\n\x1b[35mPlan updated:\x1b[0m\r\n"), codexTerminalTextBytes(plan)...)
	out = append(out, []byte("\r\n")...)
	r.write(out)
}

func (r *CodexRunner) writePrompt() {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitPromptLocked(false)
}

func codexTerminalTextBytes(text string) []byte {
	if !strings.Contains(text, "\n") {
		return []byte(text)
	}

	var b strings.Builder
	b.Grow(len(text) + strings.Count(text, "\n"))
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' && (i == 0 || text[i-1] != '\r') {
			b.WriteByte('\r')
		}
		b.WriteByte(text[i])
	}
	return []byte(b.String())
}

func (r *CodexRunner) write(data []byte) {
	if len(data) == 0 {
		return
	}

	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()

	r.mu.Lock()
	inputLineVisible := r.inputLineVisible
	input := string(r.inputBuffer)
	r.mu.Unlock()

	if inputLineVisible {
		r.emit([]byte(codexClearInputLine))
	}
	r.emit(data)
	if inputLineVisible {
		if !codexTerminalBytesEndAtLineStart(data) {
			r.emit([]byte("\r\n"))
		}
		r.emit([]byte(codexPromptANSI))
		r.emit([]byte(input))
	}
}

func (r *CodexRunner) emitPromptLocked(forceNewLine bool) {
	if r.inputLineVisible {
		return
	}
	if forceNewLine {
		r.emit([]byte("\r\n"))
	}
	r.inputLineVisible = true
	r.emit([]byte(codexPromptANSI))
}

func (r *CodexRunner) emit(data []byte) {
	if r.outputHandler != nil && len(data) > 0 {
		r.outputHandler(data)
	}
}

func codexTerminalBytesEndAtLineStart(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	switch data[len(data)-1] {
	case '\n', '\r':
		return true
	default:
		return false
	}
}

func (r *CodexRunner) failPending(err *codexRPCError) {
	r.mu.Lock()
	pending := r.pending
	r.pending = make(map[int]chan codexRPCResponse)
	r.mu.Unlock()
	for _, ch := range pending {
		ch <- codexRPCResponse{Error: err}
	}
}

func (r *CodexRunner) isShuttingDown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shuttingDown
}

func extractString(raw json.RawMessage, dotted string) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	cur := v
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = m[part]
	}
	switch s := cur.(type) {
	case string:
		return s
	case fmt.Stringer:
		return s.String()
	default:
		return ""
	}
}

func extractFloat(raw json.RawMessage, dotted string) float64 {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0
	}
	cur := v
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return 0
		}
		cur = m[part]
	}
	switch n := cur.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

func extractMap(raw json.RawMessage, dotted string) map[string]interface{} {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	cur := v
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[part]
	}
	m, _ := cur.(map[string]interface{})
	return m
}
