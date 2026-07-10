package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"openpoet/internal/database"
	"openpoet/internal/mcp"
	"openpoet/internal/secretvalue"
	"openpoet/internal/sessionmeta"
	"openpoet/internal/websocket"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var ErrSessionNotRunning = errors.New("session not running")

var (
	ErrInvalidSessionSetting     = errors.New("invalid session setting")
	ErrSessionSettingUnsupported = errors.New("session setting unsupported")
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

type CodexCommandHandler interface {
	HandleCodexCommand(ctx context.Context, data json.RawMessage) (interface{}, error)
}

type CodexProviderSessionIDDiscoverer interface {
	DiscoverCodexProviderSessionID(workDir string, since time.Time) (string, error)
}

type OpenCodeProviderSessionIDDiscoverer interface {
	DiscoverOpenCodeProviderSessionID(workDir string, since time.Time) (string, error)
}

type TermSize struct {
	Rows uint16
	Cols uint16
}

type Manager struct {
	db            *database.DB
	hub           *websocket.Hub
	serverAddr    string // OpenPoet server address (e.g. "localhost:8080")
	execPath      string // Resolved path to this binary (for MCP subprocess)
	secretDecrypt secretvalue.DecryptFunc

	mu           sync.RWMutex
	settingsMu   sync.Mutex
	sessions     map[string]*runningSession
	clientSizes  map[string]map[string]TermSize // sessionID -> clientID -> size
	ptySizes     map[string]TermSize            // sessionID -> last size applied to PTY
	shuttingDown bool                           // true during graceful shutdown for restart

	// Callbacks for AI session evaluation
	OnSessionStart        func(sessionID string)
	OnSessionEnd          func(sessionID string, output []byte)
	OnUserPromptSubmitted func(sessionID string)
	OnSessionFlush        func(sessionID string) // Always called on session end (even user-stopped) for OTEL flush
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
		ptySizes:    make(map[string]TermSize),
	}
}

// SetSecretDecryptor injects the process-owned decryptor used only while
// materializing runtime MCP configuration. Plaintext is never retained.
func (m *Manager) SetSecretDecryptor(decrypt func(string, string) (string, error)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.secretDecrypt = secretvalue.DecryptFunc(decrypt)
	m.mu.Unlock()
}

func (m *Manager) StartSession(ctx context.Context, project *database.Project, envVars map[string]string) (*database.Session, error) {
	backend := GetBackend(project.Backend)
	sessionID := uuid.New().String()
	meta := sessionmeta.FromProjectConfig(project.Backend, project.BackendConfig)
	session := &database.Session{
		ID:        sessionID,
		ProjectID: project.ID,
		Status:    "starting",
		StartTime: time.Now(),
		Backend:   project.Backend,
		Model:     meta.Model,
		Effort:    meta.Effort,
		Harness:   meta.Harness,
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
	mcpConfig, configErr := m.buildMCPConfigJSON(ctx, project, sessionID)
	if configErr != nil {
		_ = m.db.EndSession(ctx, sessionID, "error")
		return nil, fmt.Errorf("build MCP configuration: %w", configErr)
	}
	cfg.MCPConfigJSON = mcpConfig

	// Let the backend build its CLI args and env vars
	for k, v := range backend.BuildEnvVars(cfg) {
		envVars[k] = v
	}
	cliArgs := backend.BuildCLIArgs(cfg)

	// Create runner based on project type
	var runner Runner
	var err error

	if project.Type == "local" {
		dumper := newPTYDumper(sessionID, "local")
		outputHandler := func(data []byte) {
			dumper.write(data)
			outputBuffer.Write(data)
			m.hub.BroadcastSessionOutput(sessionID, data)
			m.checkForNotificationTriggers(sessionID, data)
		}
		if useCodexAppServer(project.Backend, project.BackendConfig) {
			runner, err = NewCodexRunner(project.Path, envVars, outputHandler, cfg, m.db, m.hub)
		} else {
			runner, err = NewLocalRunner(project.Path, envVars, outputHandler, cliArgs, backend)
		}
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
	if project.Type == "local" && backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) {
		go m.captureCodexProviderSessionID(sessionID, project.Path, envVars, session.StartTime)
	}
	if project.Type == "local" && backend.Type() == BackendOpenCode {
		go m.captureOpenCodeProviderSessionID(sessionID, project.Path, envVars, session.StartTime)
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
	meta := sessionmeta.WithSessionValues(
		sessionmeta.FromProjectConfig(session.Backend, project.BackendConfig),
		session.Model,
		session.Effort,
		session.Harness,
	)
	session.Model = meta.Model
	session.Effort = meta.Effort
	session.Harness = meta.Harness
	if err := m.db.UpdateSessionRuntimeMetadata(ctx, sessionID, session.Model, session.Effort, session.Harness); err != nil {
		return fmt.Errorf("failed to restore session runtime metadata: %w", err)
	}

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
	reopenComplete := false
	defer func() {
		if !reopenComplete {
			m.db.EndSession(ctx, sessionID, "error")
			m.hub.BroadcastSessionStatus(sessionID, "error")
		}
	}()

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
		SessionID:         sessionID,
		ProviderSessionID: session.ProviderSessionID,
		ServerAddr:        m.serverAddr,
		ExecPath:          m.execPath,
		IsReopen:          true,
		BackendConfig:     project.BackendConfig,
	}
	if backend.Type() == BackendCodex {
		cfg.BackendConfig = sessionmeta.ApplyRuntimeValues(project.BackendConfig, session.Model, session.Effort)
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
	mcpConfig, configErr := m.buildMCPConfigJSON(ctx, project, sessionID)
	if configErr != nil {
		return fmt.Errorf("build MCP configuration: %w", configErr)
	}
	cfg.MCPConfigJSON = mcpConfig

	// Let the backend build its CLI args and env vars
	for k, v := range backend.BuildEnvVars(cfg) {
		envVars[k] = v
	}
	if project.Type == "local" && backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) && !m.codexProviderSessionIDUsable(envVars, cfg.ProviderSessionID) {
		if providerID := m.resolveCodexProviderSessionID(ctx, sessionID, project.Path, envVars, session.StartTime.Add(-2*time.Minute)); providerID != "" {
			cfg.ProviderSessionID = providerID
			session.ProviderSessionID = providerID
		}
	}
	if project.Type == "local" && backend.Type() == BackendOpenCode && strings.TrimSpace(cfg.ProviderSessionID) == "" {
		if providerID := m.resolveOpenCodeProviderSessionID(ctx, sessionID, project.Path, envVars, session.StartTime.Add(-2*time.Minute)); providerID != "" {
			cfg.ProviderSessionID = providerID
			session.ProviderSessionID = providerID
		}
	}
	if project.Type != "local" && backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) && strings.TrimSpace(cfg.ProviderSessionID) == "" {
		if providerID := m.resolveRemoteCodexProviderSessionID(ctx, sessionID, project, decryptFunc, session.StartTime.Add(-2*time.Minute)); providerID != "" {
			cfg.ProviderSessionID = providerID
			session.ProviderSessionID = providerID
		}
	}
	if project.Type != "local" && backend.Type() == BackendOpenCode && strings.TrimSpace(cfg.ProviderSessionID) == "" {
		if providerID := m.resolveRemoteOpenCodeProviderSessionID(ctx, sessionID, project, decryptFunc, session.StartTime.Add(-2*time.Minute)); providerID != "" {
			cfg.ProviderSessionID = providerID
			session.ProviderSessionID = providerID
		}
	}
	if backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) {
		if strings.TrimSpace(cfg.ProviderSessionID) == "" {
			return fmt.Errorf("cannot reopen Codex session %s: provider session id not found", sessionID)
		}
		if project.Type == "local" && !m.codexProviderSessionIDUsable(envVars, cfg.ProviderSessionID) {
			return fmt.Errorf("cannot reopen Codex session %s: provider session id %s was not found in CODEX_HOME", sessionID, cfg.ProviderSessionID)
		}
	}
	if backend.Type() == BackendOpenCode && strings.TrimSpace(cfg.ProviderSessionID) == "" {
		return fmt.Errorf("cannot reopen OpenCode session %s: provider session id not found", sessionID)
	}
	cliArgs := backend.BuildCLIArgs(cfg)

	// Create runner based on project type
	var runner Runner
	var err error

	dumperKind := "local"
	if project.Type != "local" {
		dumperKind = "remote"
	}
	dumper := newPTYDumper(sessionID, dumperKind)
	outputHandler := func(data []byte) {
		dumper.write(data)
		outputBuffer.Write(data)
		m.hub.BroadcastSessionOutput(sessionID, data)
		m.checkForNotificationTriggers(sessionID, data)
	}

	if project.Type == "local" {
		if useCodexAppServer(project.Backend, project.BackendConfig) {
			runner, err = NewCodexRunner(project.Path, envVars, outputHandler, cfg, m.db, m.hub)
		} else {
			runner, err = NewLocalRunner(project.Path, envVars, outputHandler, cliArgs, backend)
		}
	} else {
		if backend.Type() == BackendCodex && useCodexAppServer(project.Backend, project.BackendConfig) {
			runner, err = NewRemoteCodexRunner(project, envVars, outputHandler, cfg, m.db, m.hub, decryptFunc)
		} else {
			if remoteRunnerFactory == nil {
				return fmt.Errorf("remote runner factory not set")
			}
			if backend.Type() == BackendCodex {
				if cfg.MCPConfigJSON != "" {
					envVars["OPENPOET_MCP_CONFIG_JSON"] = cfg.MCPConfigJSON
				}
				if cfg.ProviderSessionID != "" {
					envVars["OPENPOET_PROVIDER_SESSION_ID"] = cfg.ProviderSessionID
				}
			}
			runner, err = remoteRunnerFactory(project, envVars, outputHandler, decryptFunc, cliArgs)
		}
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
	if project.Type == "local" && backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) {
		go m.captureCodexProviderSessionID(sessionID, project.Path, envVars, session.StartTime)
	}
	if project.Type == "local" && backend.Type() == BackendOpenCode {
		go m.captureOpenCodeProviderSessionID(sessionID, project.Path, envVars, session.StartTime)
	}
	if project.Type != "local" && backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) {
		if discoverer, ok := runner.(CodexProviderSessionIDDiscoverer); ok {
			go m.captureCodexProviderSessionIDFromDiscoverer(sessionID, project.Path, session.StartTime, discoverer)
		}
	}
	if project.Type != "local" && backend.Type() == BackendOpenCode {
		if discoverer, ok := runner.(OpenCodeProviderSessionIDDiscoverer); ok {
			go m.captureOpenCodeProviderSessionIDFromDiscoverer(sessionID, project.Path, session.StartTime, discoverer)
		}
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

	reopenComplete = true
	return nil
}

func useCodexAppServer(backend, rawConfig string) bool {
	if BackendType(backend) != BackendCodex {
		return false
	}
	return parseCodexConfig(rawConfig).Runtime == "app-server"
}

func (m *Manager) captureCodexProviderSessionID(sessionID, workDir string, envVars map[string]string, since time.Time) {
	deadline := time.Now().Add(12 * time.Second)
	for {
		if providerID := m.resolveCodexProviderSessionID(context.Background(), sessionID, workDir, envVars, since.Add(-2*time.Second)); providerID != "" {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("[Codex] provider session id was not discovered for OpenPoet session %s", sessionID)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (m *Manager) resolveCodexProviderSessionID(ctx context.Context, sessionID, workDir string, envVars map[string]string, since time.Time) string {
	codexHome := codexHomeFromEnv(envVars)
	providerID, err := discoverCodexProviderSessionID(codexHome, workDir, since)
	if err != nil {
		log.Printf("[Codex] failed to discover provider session id for OpenPoet session %s: %v", sessionID, err)
		return ""
	}
	if providerID == "" {
		return ""
	}
	if err := m.db.UpdateSessionProviderSessionID(ctx, sessionID, providerID); err != nil {
		log.Printf("[Codex] failed to persist provider session id %s for OpenPoet session %s: %v", providerID, sessionID, err)
		return ""
	}
	log.Printf("[Codex] persisted provider session id %s for OpenPoet session %s", providerID, sessionID)
	return providerID
}

func (m *Manager) captureCodexProviderSessionIDFromDiscoverer(sessionID, workDir string, since time.Time, discoverer CodexProviderSessionIDDiscoverer) {
	deadline := time.Now().Add(12 * time.Second)
	for {
		if providerID := m.resolveCodexProviderSessionIDFromDiscoverer(context.Background(), sessionID, workDir, since.Add(-2*time.Second), discoverer); providerID != "" {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("[Codex] provider session id was not discovered for remote OpenPoet session %s", sessionID)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (m *Manager) resolveCodexProviderSessionIDFromDiscoverer(ctx context.Context, sessionID, workDir string, since time.Time, discoverer CodexProviderSessionIDDiscoverer) string {
	providerID, err := discoverer.DiscoverCodexProviderSessionID(workDir, since)
	if err != nil {
		log.Printf("[Codex] failed to discover remote provider session id for OpenPoet session %s: %v", sessionID, err)
		return ""
	}
	if providerID == "" {
		return ""
	}
	if err := m.db.UpdateSessionProviderSessionID(ctx, sessionID, providerID); err != nil {
		log.Printf("[Codex] failed to persist provider session id %s for OpenPoet session %s: %v", providerID, sessionID, err)
		return ""
	}
	log.Printf("[Codex] persisted remote provider session id %s for OpenPoet session %s", providerID, sessionID)
	return providerID
}

func (m *Manager) resolveRemoteCodexProviderSessionID(ctx context.Context, sessionID string, project *database.Project, decryptFunc func(string, string) (string, error), since time.Time) string {
	codexHome := strings.TrimSpace(parseCodexConfig(project.BackendConfig).HomePath)
	providerID, err := discoverRemoteCodexProviderSessionID(project, decryptFunc, codexHome, project.Path, since)
	if err != nil {
		log.Printf("[Codex] failed to discover remote provider session id for OpenPoet session %s: %v", sessionID, err)
		return ""
	}
	if providerID == "" {
		return ""
	}
	if err := m.db.UpdateSessionProviderSessionID(ctx, sessionID, providerID); err != nil {
		log.Printf("[Codex] failed to persist remote provider session id %s for OpenPoet session %s: %v", providerID, sessionID, err)
		return ""
	}
	log.Printf("[Codex] persisted remote provider session id %s for OpenPoet session %s", providerID, sessionID)
	return providerID
}

func (m *Manager) PersistProviderSessionID(ctx context.Context, sessionID, providerID, backend string) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return
	}
	if err := m.db.UpdateSessionProviderSessionID(ctx, sessionID, providerID); err != nil {
		log.Printf("[%s] failed to persist provider session id %s for OpenPoet session %s: %v", backend, providerID, sessionID, err)
		return
	}
	log.Printf("[%s] persisted provider session id %s for OpenPoet session %s", backend, providerID, sessionID)
}

func (m *Manager) captureOpenCodeProviderSessionID(sessionID, workDir string, envVars map[string]string, since time.Time) {
	deadline := time.Now().Add(12 * time.Second)
	for {
		if providerID := m.resolveOpenCodeProviderSessionID(context.Background(), sessionID, workDir, envVars, since.Add(-2*time.Second)); providerID != "" {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("[OpenCode] provider session id was not discovered for OpenPoet session %s", sessionID)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (m *Manager) resolveOpenCodeProviderSessionID(ctx context.Context, sessionID, workDir string, envVars map[string]string, since time.Time) string {
	providerID, err := discoverOpenCodeProviderSessionID(ctx, openCodeBinaryFromEnv(envVars), workDir, since)
	if err != nil {
		log.Printf("[OpenCode] failed to discover provider session id for OpenPoet session %s: %v", sessionID, err)
		return ""
	}
	if providerID == "" {
		return ""
	}
	m.PersistProviderSessionID(ctx, sessionID, providerID, "OpenCode")
	return providerID
}

func (m *Manager) captureOpenCodeProviderSessionIDFromDiscoverer(sessionID, workDir string, since time.Time, discoverer OpenCodeProviderSessionIDDiscoverer) {
	deadline := time.Now().Add(12 * time.Second)
	for {
		if providerID := m.resolveOpenCodeProviderSessionIDFromDiscoverer(context.Background(), sessionID, workDir, since.Add(-2*time.Second), discoverer); providerID != "" {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("[OpenCode] provider session id was not discovered for remote OpenPoet session %s", sessionID)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (m *Manager) resolveOpenCodeProviderSessionIDFromDiscoverer(ctx context.Context, sessionID, workDir string, since time.Time, discoverer OpenCodeProviderSessionIDDiscoverer) string {
	providerID, err := discoverer.DiscoverOpenCodeProviderSessionID(workDir, since)
	if err != nil {
		log.Printf("[OpenCode] failed to discover remote provider session id for OpenPoet session %s: %v", sessionID, err)
		return ""
	}
	if providerID == "" {
		return ""
	}
	m.PersistProviderSessionID(ctx, sessionID, providerID, "OpenCode")
	return providerID
}

func (m *Manager) resolveRemoteOpenCodeProviderSessionID(ctx context.Context, sessionID string, project *database.Project, decryptFunc func(string, string) (string, error), since time.Time) string {
	providerID, err := discoverRemoteOpenCodeProviderSessionID(project, decryptFunc, project.Path, since)
	if err != nil {
		log.Printf("[OpenCode] failed to discover remote provider session id for OpenPoet session %s: %v", sessionID, err)
		return ""
	}
	if providerID == "" {
		return ""
	}
	m.PersistProviderSessionID(ctx, sessionID, providerID, "OpenCode")
	return providerID
}

func openCodeBinaryFromEnv(envVars map[string]string) string {
	if envVars != nil {
		if bin := strings.TrimSpace(envVars["OPENPOET_BACKEND_BINARY"]); bin != "" {
			return expandCodexHome(bin)
		}
	}
	return "opencode"
}

func (m *Manager) codexProviderSessionIDUsable(envVars map[string]string, providerID string) bool {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return false
	}
	return codexProviderSessionIDExists(codexHomeFromEnv(envVars), providerID)
}

func codexHomeFromEnv(envVars map[string]string) string {
	if envVars != nil {
		if home := strings.TrimSpace(envVars["CODEX_HOME"]); home != "" {
			return expandCodexHome(home)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex")
	}
	return ""
}

func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotRunning, sessionID)
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
		return fmt.Errorf("stop runner: %w", err)
	}

	stopCtx := ctx
	if stopCtx == nil {
		stopCtx = context.Background()
	}
	if _, ok := stopCtx.Deadline(); !ok {
		var cancel context.CancelFunc
		stopCtx, cancel = context.WithTimeout(stopCtx, 10*time.Second)
		defer cancel()
	}
	select {
	case <-rs.runner.Done():
		return nil
	case <-stopCtx.Done():
		return fmt.Errorf("session %s did not stop: %w", sessionID, stopCtx.Err())
	}
}

func (m *Manager) WriteToSession(sessionID string, data []byte) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	_, err := rs.runner.Write(data)
	if err == nil && m.isCodexTerminalSubmit(rs, data) {
		m.notifyUserPromptSubmitted(sessionID)
	}
	return err
}

// SubmitLineToSession sends a full prompt line as separate text and Enter writes.
// Some terminal TUIs need time to ingest pasted input before Enter arrives.
func (m *Manager) SubmitLineToSession(sessionID string, text string, textToEnterDelay time.Duration) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := m.WriteToSession(sessionID, []byte(text)); err != nil {
		return err
	}
	if textToEnterDelay > 0 {
		time.Sleep(textToEnterDelay)
	}
	return m.WriteToSession(sessionID, []byte("\r"))
}

// SetSessionModel changes the model used by future turns of an active session.
// Codex app-server sessions use the structured command channel and validate the
// ID against model/list. Claude Code and Codex TUI sessions use their slash command.
func (m *Manager) SetSessionModel(ctx context.Context, sessionID, model string, textToEnterDelay time.Duration) (*database.Session, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	model, err := validateSessionModelID(model)
	if err != nil {
		return nil, err
	}

	rs, err := m.runningSession(sessionID)
	if err != nil {
		return nil, err
	}

	if rs.session.Backend == string(BackendCodex) {
		if handler, ok := rs.runner.(CodexCommandHandler); ok {
			if err := setCodexSessionModel(ctx, handler, model); err != nil {
				return nil, err
			}
			return m.persistSessionRuntimeMetadata(ctx, sessionID, model, "")
		}
	}

	switch BackendType(rs.session.Backend) {
	case BackendClaudeCode, BackendCodex:
		if err := m.SubmitLineToSession(sessionID, "/model "+model, textToEnterDelay); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: backend %q cannot change model in an active session", ErrSessionSettingUnsupported, rs.session.Backend)
	}
	return m.persistSessionRuntimeMetadata(ctx, sessionID, model, "")
}

// SetSessionEffort changes the reasoning/thinking level used by future turns.
func (m *Manager) SetSessionEffort(ctx context.Context, sessionID, effort string, textToEnterDelay time.Duration) (*database.Session, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	effort, err := validateSessionEffort(effort)
	if err != nil {
		return nil, err
	}

	rs, err := m.runningSession(sessionID)
	if err != nil {
		return nil, err
	}

	if rs.session.Backend == string(BackendCodex) {
		if handler, ok := rs.runner.(CodexCommandHandler); ok {
			if err := setCodexSessionEffort(ctx, handler, rs.session.Model, effort); err != nil {
				return nil, err
			}
			return m.persistSessionRuntimeMetadata(ctx, sessionID, "", effort)
		}
		return nil, fmt.Errorf("%w: Codex TUI does not expose a non-interactive effort command; use the app-server runtime", ErrSessionSettingUnsupported)
	}

	if BackendType(rs.session.Backend) != BackendClaudeCode {
		return nil, fmt.Errorf("%w: backend %q cannot change effort in an active session", ErrSessionSettingUnsupported, rs.session.Backend)
	}
	if err := m.SubmitLineToSession(sessionID, "/effort "+effort, textToEnterDelay); err != nil {
		return nil, err
	}
	return m.persistSessionRuntimeMetadata(ctx, sessionID, "", effort)
}

func (m *Manager) runningSession(sessionID string) (*runningSession, error) {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok || rs == nil || rs.session == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotRunning, sessionID)
	}
	return rs, nil
}

func (m *Manager) persistSessionRuntimeMetadata(ctx context.Context, sessionID, model, effort string) (*database.Session, error) {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	if !ok || rs == nil || rs.session == nil {
		m.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionNotRunning, sessionID)
	}
	currentModel := rs.session.Model
	currentEffort := rs.session.Effort
	harness := rs.session.Harness
	m.mu.RUnlock()
	if model != "" {
		currentModel = model
	}
	if effort != "" {
		currentEffort = effort
	}

	if m.db != nil {
		if err := m.db.UpdateSessionRuntimeMetadata(ctx, sessionID, currentModel, currentEffort, harness); err != nil {
			return nil, fmt.Errorf("failed to persist session runtime metadata: %w", err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok = m.sessions[sessionID]
	if !ok || rs == nil || rs.session == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotRunning, sessionID)
	}
	rs.session.Model = currentModel
	rs.session.Effort = currentEffort
	copy := *rs.session
	return &copy, nil
}

func validateSessionModelID(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("%w: model is required", ErrInvalidSessionSetting)
	}
	if len(model) > 128 {
		return "", fmt.Errorf("%w: model id must be at most 128 characters", ErrInvalidSessionSetting)
	}
	for i, ch := range model {
		allowed := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._:/@+", ch)
		if !allowed || i == 0 && strings.ContainsRune("-._:/@+", ch) {
			return "", fmt.Errorf("%w: model id %q contains unsupported characters", ErrInvalidSessionSetting, model)
		}
	}
	if strings.EqualFold(model, "reset") {
		return "default", nil
	}
	return model, nil
}

func validateSessionEffort(effort string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "reset" {
		effort = "default"
	}
	allowed := map[string]bool{
		"default": true,
		"minimal": true,
		"low":     true,
		"medium":  true,
		"high":    true,
		"xhigh":   true,
		"max":     true,
	}
	if !allowed[effort] {
		return "", fmt.Errorf("%w: effort must be one of default, minimal, low, medium, high, xhigh, or max", ErrInvalidSessionSetting)
	}
	return effort, nil
}

type codexModelCatalog struct {
	Data []struct {
		ID                        string `json:"id"`
		Model                     string `json:"model"`
		SupportedReasoningEfforts []struct {
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"supportedReasoningEfforts"`
	} `json:"data"`
}

func getCodexModelCatalog(ctx context.Context, handler CodexCommandHandler) (codexModelCatalog, error) {
	request := json.RawMessage(`{"action":"model/list","params":{"limit":100,"includeHidden":true}}`)
	result, err := handler.HandleCodexCommand(ctx, request)
	if err != nil {
		return codexModelCatalog{}, fmt.Errorf("could not list Codex models for validation: %w", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return codexModelCatalog{}, fmt.Errorf("could not decode Codex model catalog: %w", err)
	}
	var catalog codexModelCatalog
	if err := json.Unmarshal(encoded, &catalog); err != nil {
		return codexModelCatalog{}, fmt.Errorf("could not decode Codex model catalog: %w", err)
	}
	if len(catalog.Data) == 0 {
		return codexModelCatalog{}, errors.New("Codex returned an empty model catalog; model was not changed")
	}
	return catalog, nil
}

func setCodexSessionModel(ctx context.Context, handler CodexCommandHandler, model string) error {
	if model != "default" {
		catalog, err := getCodexModelCatalog(ctx, handler)
		if err != nil {
			return err
		}
		found := false
		for _, candidate := range catalog.Data {
			if model == candidate.Model || model == candidate.ID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: Codex model %q does not exist or is not available to this session", ErrInvalidSessionSetting, model)
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"action": "session/model/set",
		"params": map[string]string{"model": model},
	})
	_, err := handler.HandleCodexCommand(ctx, payload)
	return err
}

func setCodexSessionEffort(ctx context.Context, handler CodexCommandHandler, currentModel, effort string) error {
	if effort != "default" {
		catalog, err := getCodexModelCatalog(ctx, handler)
		if err != nil {
			return err
		}
		modelMatched := false
		hasEffortCatalog := false
		effortSupported := false
		for _, candidate := range catalog.Data {
			if currentModel != "" && currentModel != "default" && currentModel != candidate.Model && currentModel != candidate.ID {
				continue
			}
			modelMatched = true
			if len(candidate.SupportedReasoningEfforts) > 0 {
				hasEffortCatalog = true
			}
			for _, supported := range candidate.SupportedReasoningEfforts {
				if effort == strings.ToLower(strings.TrimSpace(supported.ReasoningEffort)) {
					effortSupported = true
					break
				}
			}
			if effortSupported {
				break
			}
		}
		if modelMatched && hasEffortCatalog && !effortSupported {
			return fmt.Errorf("%w: effort %q is not supported by Codex model %q", ErrInvalidSessionSetting, effort, currentModel)
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"action": "session/effort/set",
		"params": map[string]string{"effort": effort},
	})
	_, err := handler.HandleCodexCommand(ctx, payload)
	return err
}

func (m *Manager) HandleCodexCommand(ctx context.Context, sessionID string, data json.RawMessage) (interface{}, error) {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	handler, ok := rs.runner.(CodexCommandHandler)
	if !ok {
		return nil, fmt.Errorf("session does not support Codex commands")
	}
	result, err := handler.HandleCodexCommand(ctx, data)
	if err == nil && codexCommandAction(data) == "input/send" {
		m.notifyUserPromptSubmitted(sessionID)
	}
	return result, err
}

func (m *Manager) isCodexTerminalSubmit(rs *runningSession, data []byte) bool {
	if rs == nil || rs.session == nil || rs.session.Backend != string(BackendCodex) {
		return false
	}
	return bytes.ContainsAny(data, "\r\n")
}

func (m *Manager) notifyUserPromptSubmitted(sessionID string) {
	if m.OnUserPromptSubmitted != nil {
		m.OnUserPromptSubmitted(sessionID)
	}
}

func codexCommandAction(data json.RawMessage) string {
	var req struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(data, &req)
	return strings.TrimSpace(req.Action)
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
	clientCount := len(m.clientSizes[sessionID])
	nextSize := TermSize{Rows: minRows, Cols: minCols}
	if prevSize, ok := m.ptySizes[sessionID]; ok && prevSize == nextSize {
		m.mu.Unlock()
		return nil
	}
	m.ptySizes[sessionID] = nextSize
	m.mu.Unlock()

	log.Printf("Session %s: client %s reported %dx%d, PTY min is %dx%d (%d clients)",
		sessionID[:8], clientID[:8], cols, rows, minCols, minRows, clientCount)

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
		delete(m.ptySizes, sessionID)
		m.mu.Unlock()
		return
	}

	rs, sessionOk := m.sessions[sessionID]
	minRows, minCols := m.computeMinSize(sessionID)
	clientCount := len(clients)
	nextSize := TermSize{Rows: minRows, Cols: minCols}
	if prevSize, ok := m.ptySizes[sessionID]; ok && prevSize == nextSize {
		m.mu.Unlock()
		return
	}
	m.ptySizes[sessionID] = nextSize
	m.mu.Unlock()

	if sessionOk {
		log.Printf("Session %s: client %s disconnected, PTY resized to %dx%d (%d clients remain)",
			sessionID[:8], clientID[:8], minCols, minRows, clientCount)
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
func (m *Manager) buildMCPConfigJSON(ctx context.Context, project *database.Project, sessionID string) (string, error) {
	mcpServers := make(map[string]interface{})
	m.mu.RLock()
	decrypt := m.secretDecrypt
	m.mu.RUnlock()

	// Add user-configured MCP servers from DB
	servers, err := m.db.ListEnabledMCPServers(ctx)
	if err != nil {
		log.Printf("Warning: failed to list MCP servers: %v", err)
	}
	for _, server := range servers {
		command, args, env, decodeErr := resolveSessionMCPParts(server.Name, server.Command, server.Args, server.Env, decrypt)
		if decodeErr != nil {
			return "", decodeErr
		}

		serverConfig := map[string]interface{}{
			"command": command,
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
		command, args, env, decodeErr := resolveSessionMCPParts(server.Name, server.Command, server.Args, server.Env, decrypt)
		if decodeErr != nil {
			return "", decodeErr
		}

		serverConfig := map[string]interface{}{
			"command": command,
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
		return "", nil
	}

	config := map[string]interface{}{
		"mcpServers": mcpServers,
	}
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshal MCP configuration: %w", err)
	}
	return string(jsonBytes), nil
}

func resolveSessionMCPParts(name, command, argsJSON, envJSON string, decrypt secretvalue.DecryptFunc) (string, []string, map[string]string, error) {
	command, err := secretvalue.Resolve(command, decrypt)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve MCP server %q command: %w", name, err)
	}
	argsJSON, err = secretvalue.Resolve(argsJSON, decrypt)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve MCP server %q args: %w", name, err)
	}
	envJSON, err = secretvalue.Resolve(envJSON, decrypt)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve MCP server %q environment: %w", name, err)
	}
	var args []string
	var env map[string]string
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", nil, nil, fmt.Errorf("MCP server %q args are invalid", name)
		}
	}
	if strings.TrimSpace(envJSON) != "" {
		if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
			return "", nil, nil, fmt.Errorf("MCP server %q environment is invalid", name)
		}
	}
	return command, args, env, nil
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
	backend := GetBackend(project.Backend)
	if remoteRunnerFactory == nil && !(backend.Type() == BackendCodex && useCodexAppServer(project.Backend, project.BackendConfig)) {
		return nil, fmt.Errorf("remote runner factory not set")
	}
	sessionID := uuid.New().String()
	meta := sessionmeta.FromProjectConfig(project.Backend, project.BackendConfig)
	session := &database.Session{
		ID:        sessionID,
		ProjectID: project.ID,
		Status:    "starting",
		StartTime: time.Now(),
		Backend:   project.Backend,
		Model:     meta.Model,
		Effort:    meta.Effort,
		Harness:   meta.Harness,
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
	mcpConfig, configErr := m.buildMCPConfigJSON(ctx, project, sessionID)
	if configErr != nil {
		_ = m.db.EndSession(ctx, sessionID, "error")
		return nil, fmt.Errorf("build MCP configuration: %w", configErr)
	}
	cfg.MCPConfigJSON = mcpConfig

	// Let the backend build its CLI args and env vars
	cliArgs := backend.BuildCLIArgs(cfg)
	for k, v := range backend.BuildEnvVars(cfg) {
		envVars[k] = v
	}

	// Create output buffer (1MB max)
	outputBuffer := NewOutputBuffer(1024 * 1024)

	dumper := newPTYDumper(sessionID, "remote")
	outputHandler := func(data []byte) {
		dumper.write(data)
		outputBuffer.Write(data)
		m.hub.BroadcastSessionOutput(sessionID, data)
		m.checkForNotificationTriggers(sessionID, data)
	}

	var runner Runner
	var err error
	if backend.Type() == BackendCodex && useCodexAppServer(project.Backend, project.BackendConfig) {
		runner, err = NewRemoteCodexRunner(project, envVars, outputHandler, cfg, m.db, m.hub, decryptFunc)
	} else {
		if backend.Type() == BackendCodex && cfg.MCPConfigJSON != "" {
			envVars["OPENPOET_MCP_CONFIG_JSON"] = cfg.MCPConfigJSON
		}
		runner, err = remoteRunnerFactory(project, envVars, outputHandler, decryptFunc, cliArgs)
	}

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
	if backend.Type() == BackendCodex && !useCodexAppServer(project.Backend, project.BackendConfig) {
		if discoverer, ok := runner.(CodexProviderSessionIDDiscoverer); ok {
			go m.captureCodexProviderSessionIDFromDiscoverer(sessionID, project.Path, session.StartTime, discoverer)
		}
	}
	if backend.Type() == BackendOpenCode {
		if discoverer, ok := runner.(OpenCodeProviderSessionIDDiscoverer); ok {
			go m.captureOpenCodeProviderSessionIDFromDiscoverer(sessionID, project.Path, session.StartTime, discoverer)
		}
	}

	go m.monitorSession(sessionID, rs)

	return session, nil
}
