package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"openpoet/internal/database"
	"openpoet/internal/mcp"
	"openpoet/internal/websocket"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Runner interface {
	Start(ctx context.Context) error
	Stop() error
	Write(data []byte) (int, error)
	Resize(rows, cols uint16) error
	Wait() error
	PID() int
	Done() <-chan struct{} // closed when the process exits
}

type TermSize struct {
	Rows uint16
	Cols uint16
}

type Manager struct {
	db         *database.DB
	hub        *websocket.Hub
	serverAddr string // OpenPoet server address (e.g. "localhost:8080")
	execPath   string // Resolved path to this binary (for MCP subprocess)

	mu           sync.RWMutex
	sessions     map[string]*runningSession
	clientSizes  map[string]map[string]TermSize // sessionID -> clientID -> size
	shuttingDown bool                           // true during graceful shutdown for restart

	// Callbacks for AI session evaluation
	OnSessionStart func(sessionID string)
	OnSessionEnd   func(sessionID string, output []byte)
	OnSessionFlush func(sessionID string) // Always called on session end (even user-stopped) for OTEL flush
}

// OutputBuffer is a ring buffer for storing recent terminal output
type OutputBuffer struct {
	mu       sync.Mutex
	buffer   []byte
	maxSize  int
	writePos int
	wrapped  bool
}

func NewOutputBuffer(maxSize int) *OutputBuffer {
	return &OutputBuffer{
		buffer:  make([]byte, maxSize),
		maxSize: maxSize,
	}
}

func (b *OutputBuffer) Write(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, byte := range data {
		b.buffer[b.writePos] = byte
		b.writePos++
		if b.writePos >= b.maxSize {
			b.writePos = 0
			b.wrapped = true
		}
	}
}

func (b *OutputBuffer) GetAll() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.wrapped {
		// Not wrapped yet, return from start to writePos
		result := make([]byte, b.writePos)
		copy(result, b.buffer[:b.writePos])
		return result
	}

	// Wrapped - return from writePos to end, then start to writePos
	result := make([]byte, b.maxSize)
	copy(result, b.buffer[b.writePos:])
	copy(result[b.maxSize-b.writePos:], b.buffer[:b.writePos])
	return result
}

type runningSession struct {
	session      *database.Session
	runner       Runner
	cancel       context.CancelFunc
	output       chan []byte
	outputBuffer *OutputBuffer
	userStopped  bool // set when user explicitly stops the session
}

func NewManager(db *database.DB, hub *websocket.Hub, serverAddr string) *Manager {
	// Normalize 0.0.0.0 to localhost for client connections (OTLP, hooks)
	// 0.0.0.0 is valid for server bind but not for client connections
	clientAddr := serverAddr
	if strings.HasPrefix(clientAddr, "0.0.0.0:") {
		clientAddr = "localhost:" + strings.TrimPrefix(clientAddr, "0.0.0.0:")
	}

	// Resolve executable path once at startup while the binary is guaranteed to exist.
	// os.Executable() may return a stale path if the binary was started from a temp
	// directory that is later cleaned up (e.g. during deployment).
	execPath, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
			execPath = resolved
		}
	}
	if execPath != "" {
		log.Printf("[Session] Resolved executable path: %s", execPath)
	}

	return &Manager{
		db:          db,
		hub:         hub,
		serverAddr:  clientAddr,
		execPath:    execPath,
		sessions:    make(map[string]*runningSession),
		clientSizes: make(map[string]map[string]TermSize),
	}
}

func (m *Manager) StartSession(ctx context.Context, project *database.Project, envVars map[string]string) (*database.Session, error) {
	backend := GetBackend(project.Backend)
	sessionID := uuid.New().String()
	session := &database.Session{
		ID:        sessionID,
		ProjectID: project.ID,
		Status:    "starting",
		StartTime: time.Now(),
		Backend:   project.Backend,
	}

	if err := m.db.CreateSession(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Create output buffer (1MB max)
	outputBuffer := NewOutputBuffer(1024 * 1024)

	if envVars == nil {
		envVars = make(map[string]string)
	}

	// Build backend-agnostic session config
	cfg := &SessionConfig{
		SessionID:     sessionID,
		ServerAddr:    m.serverAddr,
		ExecPath:      m.execPath,
		BackendConfig: project.BackendConfig,
	}

	// Extract special env vars set by API handler before passing to backend
	if prompt, ok := envVars["OPENPOET_APPEND_SYSTEM_PROMPT"]; ok && prompt != "" {
		cfg.AppendSystemPrompt = prompt
		delete(envVars, "OPENPOET_APPEND_SYSTEM_PROMPT")
	}
	if v, ok := envVars["OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS"]; ok && v == "true" {
		cfg.DangerouslySkipPermissions = true
		m.db.UpdateSessionSkipPermissions(ctx, sessionID, true)
		delete(envVars, "OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS")
	}

	// Build MCP config
	cfg.MCPConfigJSON = m.buildMCPConfigJSON(ctx, project, sessionID)

	// Let the backend build its CLI args and env vars
	cliArgs := backend.BuildCLIArgs(cfg)
	for k, v := range backend.BuildEnvVars(cfg) {
		envVars[k] = v
	}

	// Create runner based on project type
	var runner Runner
	var err error

	if project.Type == "local" {
		runner, err = NewLocalRunner(project.Path, envVars, func(data []byte) {
			outputBuffer.Write(data)
			m.hub.BroadcastSessionOutput(sessionID, data)
			m.checkForNotificationTriggers(sessionID, data)
		}, cliArgs, backend)
	} else {
		return nil, fmt.Errorf("remote sessions not yet implemented")
	}

	if err != nil {
		m.db.EndSession(ctx, sessionID, "error")
		return nil, fmt.Errorf("failed to create runner: %w", err)
	}

	// Start the session
	sessionCtx, cancel := context.WithCancel(context.Background())
	rs := &runningSession{
		session:      session,
		runner:       runner,
		cancel:       cancel,
		output:       make(chan []byte, 100),
		outputBuffer: outputBuffer,
	}

	m.mu.Lock()
	m.sessions[sessionID] = rs
	m.mu.Unlock()

	if err := runner.Start(sessionCtx); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		m.db.EndSession(ctx, sessionID, "error")
		return nil, fmt.Errorf("failed to start runner: %w", err)
	}

	// Update session status and PID
	session.Status = "running"
	m.db.UpdateSessionStatus(ctx, sessionID, "running")
	if pid := runner.PID(); pid > 0 {
		m.db.UpdateSessionPID(ctx, sessionID, pid)
	}

	m.hub.BroadcastSessionStatus(sessionID, "running")

	// Monitor session completion
	go m.monitorSession(sessionID, rs)

	// Trigger AI evaluation for session start
	if m.OnSessionStart != nil {
		go m.OnSessionStart(sessionID)
	}

	return session, nil
}

func (m *Manager) ReopenSession(ctx context.Context, session *database.Session, project *database.Project, envVars map[string]string, decryptFunc func(string, string) (string, error)) error {
	backend := GetBackend(session.Backend)
	sessionID := session.ID

	// Check backend supports resume
	if !backend.SupportsResume() {
		return fmt.Errorf("backend %q does not support session resume", session.Backend)
	}

	// Verify session is not already running
	m.mu.RLock()
	_, alreadyRunning := m.sessions[sessionID]
	m.mu.RUnlock()
	if alreadyRunning {
		return fmt.Errorf("session %s is already running", sessionID)
	}

	// Reset DB record
	if err := m.db.ReopenSession(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to reopen session in DB: %w", err)
	}

	// Clean up copilot session errors before resume.
	// Copilot replays events.jsonl on --resume, including session.error entries
	// from previous SIGTERM kills. Removing them prevents "Error: read EIO" from
	// appearing in the terminal on reopen.
	if backend.Type() == BackendCopilot {
		cleanCopilotSessionErrors(sessionID)
	}

	// Create output buffer (1MB max)
	outputBuffer := NewOutputBuffer(1024 * 1024)

	if envVars == nil {
		envVars = make(map[string]string)
	}

	// Build backend-agnostic session config
	cfg := &SessionConfig{
		SessionID:     sessionID,
		ServerAddr:    m.serverAddr,
		ExecPath:      m.execPath,
		IsReopen:      true,
		BackendConfig: project.BackendConfig,
	}

	// Extract special env vars set by API handler
	if prompt, ok := envVars["OPENPOET_APPEND_SYSTEM_PROMPT"]; ok && prompt != "" {
		cfg.AppendSystemPrompt = prompt
		delete(envVars, "OPENPOET_APPEND_SYSTEM_PROMPT")
	}
	if v, ok := envVars["OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS"]; ok && v == "true" {
		cfg.DangerouslySkipPermissions = true
		m.db.UpdateSessionSkipPermissions(ctx, sessionID, true)
		delete(envVars, "OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS")
	}

	// Build MCP config
	cfg.MCPConfigJSON = m.buildMCPConfigJSON(ctx, project, sessionID)

	// Let the backend build its CLI args and env vars
	cliArgs := backend.BuildCLIArgs(cfg)
	for k, v := range backend.BuildEnvVars(cfg) {
		envVars[k] = v
	}

	// Create runner based on project type
	var runner Runner
	var err error

	outputHandler := func(data []byte) {
		outputBuffer.Write(data)
		m.hub.BroadcastSessionOutput(sessionID, data)
		m.checkForNotificationTriggers(sessionID, data)
	}

	if project.Type == "local" {
		runner, err = NewLocalRunner(project.Path, envVars, outputHandler, cliArgs, backend)
	} else {
		if remoteRunnerFactory == nil {
			return fmt.Errorf("remote runner factory not set")
		}
		runner, err = remoteRunnerFactory(project, envVars, outputHandler, decryptFunc, cliArgs)
	}

	if err != nil {
		m.db.EndSession(ctx, sessionID, "error")
		return fmt.Errorf("failed to create runner: %w", err)
	}

	// Start the session
	sessionCtx, cancel := context.WithCancel(context.Background())
	rs := &runningSession{
		session:      session,
		runner:       runner,
		cancel:       cancel,
		output:       make(chan []byte, 100),
		outputBuffer: outputBuffer,
	}

	m.mu.Lock()
	m.sessions[sessionID] = rs
	m.mu.Unlock()

	if err := runner.Start(sessionCtx); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		m.db.EndSession(ctx, sessionID, "error")
		return fmt.Errorf("failed to start runner: %w", err)
	}

	// Update session status and PID
	session.Status = "running"
	m.db.UpdateSessionStatus(ctx, sessionID, "running")
	if pid := runner.PID(); pid > 0 {
		m.db.UpdateSessionPID(ctx, sessionID, pid)
	}

	m.hub.BroadcastSessionStatus(sessionID, "running")

	// Monitor session completion
	go m.monitorSession(sessionID, rs)

	if m.OnSessionStart != nil {
		go m.OnSessionStart(sessionID)
	}

	return nil
}

func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	rs.userStopped = true

	// Try graceful exit first: send /exit to the CLI.
	// This lets copilot/claude record a clean session.end instead of session.error.
	// Uses the same Ctrl+U → text → Enter sequence as the mobile input,
	// with delays so the TUI processes each step.
	backend := BackendType(rs.session.Backend)
	if backend == BackendCopilot || backend == BackendClaudeCode {
		rs.runner.Write([]byte("\x15")) // Ctrl+U: clear current line
		time.Sleep(50 * time.Millisecond)
		rs.runner.Write([]byte("/exit"))
		time.Sleep(50 * time.Millisecond)
		rs.runner.Write([]byte("\r")) // Enter
		// Give the CLI time to process the exit command
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-rs.runner.Done():
			timer.Stop()
			// Exited cleanly, no need for SIGTERM
			rs.cancel()
			return nil
		case <-timer.C:
			// Didn't exit in time, fall through to SIGTERM
		}
	}

	rs.cancel()
	if err := rs.runner.Stop(); err != nil {
		log.Printf("Error stopping runner: %v", err)
	}

	return nil
}

func (m *Manager) WriteToSession(sessionID string, data []byte) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	_, err := rs.runner.Write(data)
	return err
}

func (m *Manager) ResizeSession(sessionID string, rows, cols uint16) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	return rs.runner.Resize(rows, cols)
}

// RegisterClientSize stores a client's terminal size and resizes the PTY
// to the minimum dimensions across all connected clients.
func (m *Manager) RegisterClientSize(sessionID, clientID string, rows, cols uint16) error {
	m.mu.Lock()
	rs, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if m.clientSizes[sessionID] == nil {
		m.clientSizes[sessionID] = make(map[string]TermSize)
	}
	m.clientSizes[sessionID][clientID] = TermSize{Rows: rows, Cols: cols}

	minRows, minCols := m.computeMinSize(sessionID)
	m.mu.Unlock()

	log.Printf("Session %s: client %s reported %dx%d, PTY min is %dx%d (%d clients)",
		sessionID[:8], clientID[:8], cols, rows, minCols, minRows, len(m.clientSizes[sessionID]))

	return rs.runner.Resize(minRows, minCols)
}

// UnregisterClientSize removes a client's size and resizes the PTY to the
// minimum of the remaining clients. If no clients remain, the PTY keeps its
// current size.
func (m *Manager) UnregisterClientSize(sessionID, clientID string) {
	m.mu.Lock()
	clients, ok := m.clientSizes[sessionID]
	if !ok || clients == nil {
		m.mu.Unlock()
		return
	}

	delete(clients, clientID)

	if len(clients) == 0 {
		delete(m.clientSizes, sessionID)
		m.mu.Unlock()
		return
	}

	rs, sessionOk := m.sessions[sessionID]
	minRows, minCols := m.computeMinSize(sessionID)
	m.mu.Unlock()

	if sessionOk {
		log.Printf("Session %s: client %s disconnected, PTY resized to %dx%d (%d clients remain)",
			sessionID[:8], clientID[:8], minCols, minRows, len(clients))
		rs.runner.Resize(minRows, minCols)
	}
}

// computeMinSize returns the minimum rows and cols across all clients for a session.
// Must be called with m.mu held.
func (m *Manager) computeMinSize(sessionID string) (rows, cols uint16) {
	clients := m.clientSizes[sessionID]
	first := true
	for _, s := range clients {
		if first {
			rows, cols = s.Rows, s.Cols
			first = false
		} else {
			if s.Cols < cols {
				cols = s.Cols
			}
			if s.Rows < rows {
				rows = s.Rows
			}
		}
	}
	return
}

func (m *Manager) GetSession(sessionID string) (*database.Session, bool) {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return nil, false
	}

	return rs.session, true
}

func (m *Manager) IsSessionRunning(sessionID string) bool {
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	return ok
}

// cleanCopilotSessionErrors removes session.error lines from copilot's events.jsonl
// to prevent "Error: read EIO" from being replayed on resume.
func cleanCopilotSessionErrors(sessionID string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	eventsPath := filepath.Join(home, ".copilot", "session-state", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		return
	}

	var cleaned []byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if bytes.Contains(line, []byte(`"session.error"`)) {
			continue
		}
		cleaned = append(cleaned, line...)
		cleaned = append(cleaned, '\n')
	}

	if len(cleaned) != len(data) {
		os.WriteFile(eventsPath, cleaned, 0600)
	}
}

// GetSessionOutput returns the buffered output for a session
func (m *Manager) GetSessionOutput(sessionID string) ([]byte, error) {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	return rs.outputBuffer.GetAll(), nil
}

func (m *Manager) ListRunningSessions() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) monitorSession(sessionID string, rs *runningSession) {
	err := rs.runner.Wait()

	// If we're shutting down for restart, keep sessions as "running" in DB
	// so they can be auto-restored on next startup.
	m.mu.Lock()
	shuttingDown := m.shuttingDown
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if shuttingDown {
		log.Printf("Session %s stopped for restart (preserving DB state)", sessionID)
		if m.OnSessionFlush != nil {
			m.OnSessionFlush(sessionID)
		}
		return
	}

	// Capture output snapshot BEFORE cleanup for AI evaluation
	var outputSnapshot []byte
	if rs.outputBuffer != nil {
		outputSnapshot = rs.outputBuffer.GetAll()
	}

	// Determine status: user-initiated stops are not errors
	userStopped := rs.userStopped
	status := "completed"
	if userStopped {
		status = "stopped"
	} else if err != nil {
		status = "error"
	}

	// Send exit message to terminal before cleaning up
	if rs.outputBuffer != nil {
		var exitMsg string
		switch status {
		case "error":
			exitMsg = fmt.Sprintf("\r\n\x1b[31mSession exited with error: %v\x1b[0m\r\n", err)
		case "stopped":
			exitMsg = "\r\n\x1b[33mSession stopped by user\x1b[0m\r\n"
		default:
			exitMsg = "\r\n\x1b[33mSession completed\x1b[0m\r\n"
		}
		rs.outputBuffer.Write([]byte(exitMsg))
		m.hub.BroadcastSessionOutput(sessionID, []byte(exitMsg))
	}

	ctx := context.Background()
	if status == "error" {
		log.Printf("Session %s ended with error: %v", sessionID, err)
	}

	m.db.EndSession(ctx, sessionID, status)
	m.hub.BroadcastSessionStatus(sessionID, status)

	log.Printf("Session %s ended with status: %s", sessionID, status)

	// Always flush OTEL metrics regardless of how the session ended
	if m.OnSessionFlush != nil {
		m.OnSessionFlush(sessionID)
	}

	// Trigger AI evaluation for session end (skip for user-stopped sessions)
	if m.OnSessionEnd != nil && !userStopped {
		go m.OnSessionEnd(sessionID, outputSnapshot)
	}
}

func (m *Manager) checkForNotificationTriggers(sessionID string, data []byte) {
	// This will be implemented to parse output for notification triggers
	// For now, just a placeholder
}

// buildMCPConfigJSON builds a JSON string for the --mcp-config CLI flag.
// It includes user-configured MCP servers from the DB plus OpenPoet's own MCP server.
// OpenPoet's MCP server is only included if the effective tool policy allows at least one tool.
func (m *Manager) buildMCPConfigJSON(ctx context.Context, project *database.Project, sessionID string) string {
	mcpServers := make(map[string]interface{})

	// Add user-configured MCP servers from DB
	servers, err := m.db.ListEnabledMCPServers(ctx)
	if err != nil {
		log.Printf("Warning: failed to list MCP servers: %v", err)
	}
	for _, server := range servers {
		var args []string
		var env map[string]string
		json.Unmarshal([]byte(server.Args), &args)
		json.Unmarshal([]byte(server.Env), &env)

		serverConfig := map[string]interface{}{
			"command": server.Command,
		}
		if len(args) > 0 {
			serverConfig["args"] = args
		}
		if len(env) > 0 {
			serverConfig["env"] = env
		}
		mcpServers[server.Name] = serverConfig
	}

	// Add project-specific MCP servers (override global if same name)
	projectServers, err := m.db.ListEnabledProjectMCPServers(ctx, project.ID)
	if err != nil {
		log.Printf("Warning: failed to list project MCP servers: %v", err)
	}
	for _, server := range projectServers {
		var args []string
		var env map[string]string
		json.Unmarshal([]byte(server.Args), &args)
		json.Unmarshal([]byte(server.Env), &env)

		serverConfig := map[string]interface{}{
			"command": server.Command,
		}
		if len(args) > 0 {
			serverConfig["args"] = args
		}
		if len(env) > 0 {
			serverConfig["env"] = env
		}
		mcpServers[server.Name] = serverConfig
	}

	// Resolve effective tool policy for the project to decide whether to inject OpenPoet MCP.
	// If the project has an explicit policy, it overrides the global policy entirely.
	// If the project has no explicit policy, the global session policy is inherited.
	shouldInjectOpenPoet := false
	if m.serverAddr != "" {
		globalPolicy := mcp.ToolPolicy{Mode: "deny_all"}
		globalPolicyStr, _ := m.db.GetSetting(ctx, "mcp_tool_policy_session")
		if globalPolicyStr != "" {
			globalPolicy = mcp.ParsePolicy(globalPolicyStr)
		}
		effectivePolicy := mcp.ResolveProjectPolicy(globalPolicy, project.ToolPolicy)
		// If the project has shares configured, auto-allow the share tools in the policy.
		shares, sharesErr := m.db.ListProjectShares(ctx, project.ID)
		if sharesErr == nil && len(shares) > 0 {
			effectivePolicy = effectivePolicy.AllowTools(mcp.ShareToolNames)
		}
		shouldInjectOpenPoet = effectivePolicy.HasTools(mcp.AllTools())
		log.Printf("[MCP inject] project=%d globalPolicy=%q projectPolicy=%q effectiveMode=%s shares=%d inject=%v",
			project.ID, globalPolicyStr, project.ToolPolicy, effectivePolicy.Mode, len(shares), shouldInjectOpenPoet)
	}

	if shouldInjectOpenPoet && m.execPath != "" {
		mcpServers["openpoet"] = map[string]interface{}{
			"command": m.execPath,
			"args":    []string{"mcp-serve", "--session-id", sessionID, "--api-url", "http://" + m.serverAddr},
		}
	}

	if len(mcpServers) == 0 {
		return ""
	}

	config := map[string]interface{}{
		"mcpServers": mcpServers,
	}
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		log.Printf("Warning: failed to marshal MCP config: %v", err)
		return ""
	}
	return string(jsonBytes)
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, rs := range m.sessions {
		log.Printf("Stopping session: %s", id)
		rs.cancel()
		rs.runner.Stop()
	}
}

// StopAllForRestart gracefully stops all sessions but preserves their "running"
// status in the database so they can be auto-restored on next startup.
func (m *Manager) StopAllForRestart() {
	m.mu.Lock()
	m.shuttingDown = true
	sessions := make(map[string]*runningSession, len(m.sessions))
	for id, rs := range m.sessions {
		sessions[id] = rs
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for id, rs := range sessions {
		wg.Add(1)
		go func(id string, rs *runningSession) {
			defer wg.Done()
			log.Printf("Stopping session for restart: %s", id)
			rs.cancel()
			rs.runner.Stop()
		}(id, rs)
	}
	wg.Wait()
}

// SetRemoteRunnerFactory allows setting a factory function for creating remote runners
// This will be called from the handlers package to inject the remote runner implementation
type RemoteRunnerFactory func(project *database.Project, envVars map[string]string, outputHandler func([]byte), decryptFunc func(string, string) (string, error), cliArgs []string) (Runner, error)

var remoteRunnerFactory RemoteRunnerFactory

func SetRemoteRunnerFactory(factory RemoteRunnerFactory) {
	remoteRunnerFactory = factory
}

func (m *Manager) StartRemoteSession(ctx context.Context, project *database.Project, envVars map[string]string, decryptFunc func(string, string) (string, error)) (*database.Session, error) {
	if remoteRunnerFactory == nil {
		return nil, fmt.Errorf("remote runner factory not set")
	}

	backend := GetBackend(project.Backend)
	sessionID := uuid.New().String()
	session := &database.Session{
		ID:        sessionID,
		ProjectID: project.ID,
		Status:    "starting",
		StartTime: time.Now(),
		Backend:   project.Backend,
	}

	if err := m.db.CreateSession(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	if envVars == nil {
		envVars = make(map[string]string)
	}

	// Build backend-agnostic session config
	cfg := &SessionConfig{
		SessionID:     sessionID,
		ServerAddr:    m.serverAddr,
		ExecPath:      m.execPath,
		BackendConfig: project.BackendConfig,
	}

	// Extract special env vars set by API handler
	if prompt, ok := envVars["OPENPOET_APPEND_SYSTEM_PROMPT"]; ok && prompt != "" {
		cfg.AppendSystemPrompt = prompt
		delete(envVars, "OPENPOET_APPEND_SYSTEM_PROMPT")
	}
	if v, ok := envVars["OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS"]; ok && v == "true" {
		cfg.DangerouslySkipPermissions = true
		delete(envVars, "OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS")
	}

	// Build MCP config
	cfg.MCPConfigJSON = m.buildMCPConfigJSON(ctx, project, sessionID)

	// Let the backend build its CLI args and env vars
	cliArgs := backend.BuildCLIArgs(cfg)
	for k, v := range backend.BuildEnvVars(cfg) {
		envVars[k] = v
	}

	// Create output buffer (1MB max)
	outputBuffer := NewOutputBuffer(1024 * 1024)

	runner, err := remoteRunnerFactory(project, envVars, func(data []byte) {
		outputBuffer.Write(data)
		m.hub.BroadcastSessionOutput(sessionID, data)
		m.checkForNotificationTriggers(sessionID, data)
	}, decryptFunc, cliArgs)

	if err != nil {
		m.db.EndSession(ctx, sessionID, "error")
		return nil, fmt.Errorf("failed to create remote runner: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	rs := &runningSession{
		session:      session,
		runner:       runner,
		cancel:       cancel,
		output:       make(chan []byte, 100),
		outputBuffer: outputBuffer,
	}

	m.mu.Lock()
	m.sessions[sessionID] = rs
	m.mu.Unlock()

	if err := runner.Start(sessionCtx); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		m.db.EndSession(ctx, sessionID, "error")
		return nil, fmt.Errorf("failed to start remote runner: %w", err)
	}

	session.Status = "running"
	m.db.UpdateSessionStatus(ctx, sessionID, "running")
	m.hub.BroadcastSessionStatus(sessionID, "running")

	go m.monitorSession(sessionID, rs)

	return session, nil
}
