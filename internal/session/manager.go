package session

import (
	"context"
	"devmanager/internal/database"
	"devmanager/internal/websocket"
	"fmt"
	"log"
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
}

type TermSize struct {
	Rows uint16
	Cols uint16
}

type Manager struct {
	db         *database.DB
	hub        *websocket.Hub
	serverAddr string // DevManager server address (e.g. "localhost:8080")

	mu          sync.RWMutex
	sessions    map[string]*runningSession
	clientSizes map[string]map[string]TermSize // sessionID -> clientID -> size
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
}

func NewManager(db *database.DB, hub *websocket.Hub, serverAddr string) *Manager {
	return &Manager{
		db:          db,
		hub:         hub,
		serverAddr:  serverAddr,
		sessions:    make(map[string]*runningSession),
		clientSizes: make(map[string]map[string]TermSize),
	}
}

func (m *Manager) StartSession(ctx context.Context, project *database.Project, envVars map[string]string) (*database.Session, error) {
	sessionID := uuid.New().String()
	session := &database.Session{
		ID:        sessionID,
		ProjectID: project.ID,
		Status:    "starting",
		StartTime: time.Now(),
	}

	if err := m.db.CreateSession(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Create output buffer (1MB max)
	outputBuffer := NewOutputBuffer(1024 * 1024)

	// Inject hook environment variables
	if envVars == nil {
		envVars = make(map[string]string)
	}
	envVars["DEVMANAGER_HOOK_URL"] = "http://" + m.serverAddr
	envVars["DEVMANAGER_SESSION_ID"] = sessionID

	// Create runner based on project type
	var runner Runner
	var err error

	if project.Type == "local" {
		runner, err = NewLocalRunner(project.Path, envVars, func(data []byte) {
			outputBuffer.Write(data)
			m.hub.BroadcastSessionOutput(sessionID, data)
			m.checkForNotificationTriggers(sessionID, data)
		})
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

	return session, nil
}

func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
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

	// Send exit message to terminal before cleaning up
	if rs.outputBuffer != nil {
		var exitMsg string
		if err != nil {
			exitMsg = fmt.Sprintf("\r\n\x1b[31mSession exited with error: %v\x1b[0m\r\n", err)
		} else {
			exitMsg = "\r\n\x1b[33mSession completed\x1b[0m\r\n"
		}
		rs.outputBuffer.Write([]byte(exitMsg))
		m.hub.BroadcastSessionOutput(sessionID, []byte(exitMsg))
	}

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	ctx := context.Background()
	status := "completed"
	if err != nil {
		status = "error"
		log.Printf("Session %s ended with error: %v", sessionID, err)
	}

	m.db.EndSession(ctx, sessionID, status)
	m.hub.BroadcastSessionStatus(sessionID, status)

	log.Printf("Session %s ended with status: %s", sessionID, status)
}

func (m *Manager) checkForNotificationTriggers(sessionID string, data []byte) {
	// This will be implemented to parse output for notification triggers
	// For now, just a placeholder
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

// SetRemoteRunnerFactory allows setting a factory function for creating remote runners
// This will be called from the handlers package to inject the remote runner implementation
type RemoteRunnerFactory func(project *database.Project, envVars map[string]string, outputHandler func([]byte), decryptFunc func(string, string) (string, error)) (Runner, error)

var remoteRunnerFactory RemoteRunnerFactory

func SetRemoteRunnerFactory(factory RemoteRunnerFactory) {
	remoteRunnerFactory = factory
}

func (m *Manager) StartRemoteSession(ctx context.Context, project *database.Project, envVars map[string]string, decryptFunc func(string, string) (string, error)) (*database.Session, error) {
	if remoteRunnerFactory == nil {
		return nil, fmt.Errorf("remote runner factory not set")
	}

	sessionID := uuid.New().String()
	session := &database.Session{
		ID:        sessionID,
		ProjectID: project.ID,
		Status:    "starting",
		StartTime: time.Now(),
	}

	if err := m.db.CreateSession(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Inject hook environment variables for remote sessions
	if envVars == nil {
		envVars = make(map[string]string)
	}
	envVars["DEVMANAGER_HOOK_URL"] = "http://" + m.serverAddr
	envVars["DEVMANAGER_SESSION_ID"] = sessionID

	// Create output buffer (1MB max)
	outputBuffer := NewOutputBuffer(1024 * 1024)

	runner, err := remoteRunnerFactory(project, envVars, func(data []byte) {
		outputBuffer.Write(data)
		m.hub.BroadcastSessionOutput(sessionID, data)
		m.checkForNotificationTriggers(sessionID, data)
	}, decryptFunc)

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
