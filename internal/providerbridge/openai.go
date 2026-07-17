package providerbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"openpoet/internal/database"
	"openpoet/internal/security"
)

const ProviderTypeOpenAIOAuth = "openai_oauth"

var (
	ErrNotAuthenticated  = errors.New("OpenAI OAuth profile is not authenticated")
	ErrHelperUnavailable = errors.New("claude-code-proxy is not available")
)

// CredentialStore owns the OAuth token bundle used by the bridge. Implementations
// must never return the plaintext through an HTTP response or log it.
type CredentialStore interface {
	Load(context.Context, int64) (string, error)
	Save(context.Context, int64, string) error
	Clear(context.Context, int64) error
	Connected(context.Context, int64) (bool, error)
}

// EncryptedAIConfigStore persists an OAuth token bundle in the encrypted secret
// columns already used by OpenPoet's AI provider profiles.
type EncryptedAIConfigStore struct {
	db        *database.DB
	encryptor *security.Encryptor
}

func NewEncryptedAIConfigStore(db *database.DB, encryptor *security.Encryptor) *EncryptedAIConfigStore {
	return &EncryptedAIConfigStore{db: db, encryptor: encryptor}
}

func (s *EncryptedAIConfigStore) profile(ctx context.Context, id int64) (*database.AIConfig, error) {
	if s == nil || s.db == nil || s.encryptor == nil {
		return nil, errors.New("OpenAI OAuth credential store is unavailable")
	}
	profile, err := s.db.GetAIConfig(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load AI provider profile: %w", err)
	}
	if profile.ProviderType != ProviderTypeOpenAIOAuth {
		return nil, fmt.Errorf("AI provider profile %d is not an OpenAI OAuth profile", id)
	}
	return profile, nil
}

func (s *EncryptedAIConfigStore) Load(ctx context.Context, id int64) (string, error) {
	profile, err := s.profile(ctx, id)
	if err != nil {
		return "", err
	}
	if profile.APIKeyEncrypted == "" || profile.APIKeyIV == "" {
		return "", ErrNotAuthenticated
	}
	raw, err := s.encryptor.Decrypt(profile.APIKeyEncrypted, profile.APIKeyIV)
	if err != nil {
		return "", fmt.Errorf("decrypt OpenAI OAuth profile: %w", err)
	}
	if err := validateStoredAuth(raw); err != nil {
		return "", fmt.Errorf("stored OpenAI OAuth profile is invalid: %w", err)
	}
	return raw, nil
}

func (s *EncryptedAIConfigStore) Save(ctx context.Context, id int64, raw string) error {
	if err := validateStoredAuth(raw); err != nil {
		return err
	}
	profile, err := s.profile(ctx, id)
	if err != nil {
		return err
	}
	ciphertext, iv, err := s.encryptor.Encrypt(raw)
	if err != nil {
		return fmt.Errorf("encrypt OpenAI OAuth profile: %w", err)
	}
	profile.APIKeyEncrypted = ciphertext
	profile.APIKeyIV = iv
	profile.APIKeyPreview = "OAuth connected"
	if err := s.db.UpdateAIConfig(ctx, profile); err != nil {
		return fmt.Errorf("save OpenAI OAuth profile: %w", err)
	}
	return nil
}

func (s *EncryptedAIConfigStore) Clear(ctx context.Context, id int64) error {
	profile, err := s.profile(ctx, id)
	if err != nil {
		return err
	}
	profile.APIKeyEncrypted = ""
	profile.APIKeyIV = ""
	profile.APIKeyPreview = ""
	if err := s.db.UpdateAIConfig(ctx, profile); err != nil {
		return fmt.Errorf("clear OpenAI OAuth profile: %w", err)
	}
	return nil
}

func (s *EncryptedAIConfigStore) Connected(ctx context.Context, id int64) (bool, error) {
	profile, err := s.profile(ctx, id)
	if err != nil {
		return false, err
	}
	return profile.APIKeyEncrypted != "" && profile.APIKeyIV != "", nil
}

type storedAuth struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   uint64 `json:"expires"`
	AccountID string `json:"accountId,omitempty"`
}

func validateStoredAuth(raw string) error {
	var auth storedAuth
	if err := json.Unmarshal([]byte(raw), &auth); err != nil {
		return errors.New("OAuth helper returned invalid credential JSON")
	}
	if strings.TrimSpace(auth.Access) == "" || strings.TrimSpace(auth.Refresh) == "" || auth.Expires == 0 {
		return errors.New("OAuth helper returned an incomplete credential bundle")
	}
	return nil
}

type DeviceLogin struct {
	ID              string    `json:"id"`
	ConfigID        int64     `json:"config_id"`
	Status          string    `json:"status"`
	VerificationURL string    `json:"verification_url,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
}

type deviceLoginState struct {
	snapshot  DeviceLogin
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan struct{}
	readyOnce sync.Once
	rootDir   string
	configDir string
}

type proxyProcess struct {
	configID  int64
	baseURL   string
	rootDir   string
	configDir string
	cancel    context.CancelFunc
	ready     chan struct{}
	exited    chan struct{}
	err       error
	exitErr   error
	lastAuth  [sha256.Size]byte
}

// Manager owns the dedicated OAuth login helpers and loopback-only provider
// proxy processes. It always supplies an isolated HOME and CCP_CONFIG_DIR, so
// neither the native Codex CLI credentials nor ~/.codex can be discovered.
type Manager struct {
	store          CredentialStore
	binaryOverride string

	mu      sync.Mutex
	closed  bool
	logins  map[string]*deviceLoginState
	proxies map[int64]*proxyProcess
}

func NewManager(store CredentialStore, binaryOverride string) *Manager {
	return &Manager{
		store:          store,
		binaryOverride: strings.TrimSpace(binaryOverride),
		logins:         make(map[string]*deviceLoginState),
		proxies:        make(map[int64]*proxyProcess),
	}
}

func (m *Manager) BinaryPath() (string, error) {
	if m == nil {
		return "", ErrHelperUnavailable
	}
	candidates := []string{}
	if m.binaryOverride != "" {
		candidates = append(candidates, m.binaryOverride)
	}
	if env := strings.TrimSpace(os.Getenv("OPENPOET_CLAUDE_CODE_PROXY_BIN")); env != "" {
		candidates = append(candidates, env)
	}
	if current, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(current), "claude-code-proxy"))
	}
	for _, candidate := range candidates {
		if resolved, err := resolveExecutable(candidate); err == nil {
			return resolved, nil
		}
	}
	if resolved, err := exec.LookPath("claude-code-proxy"); err == nil {
		return filepath.Abs(resolved)
	}
	return "", fmt.Errorf("%w; install the audited helper or set OPENPOET_CLAUDE_CODE_PROXY_BIN", ErrHelperUnavailable)
}

func resolveExecutable(candidate string) (string, error) {
	if !filepath.IsAbs(candidate) {
		return exec.LookPath(candidate)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("provider helper is not executable")
	}
	return candidate, nil
}

func (m *Manager) Connected(ctx context.Context, configID int64) (bool, error) {
	if m == nil || m.store == nil {
		return false, errors.New("OpenAI provider bridge is unavailable")
	}
	return m.store.Connected(ctx, configID)
}

func (m *Manager) StartDeviceLogin(ctx context.Context, configID int64) (DeviceLogin, error) {
	if configID <= 0 {
		return DeviceLogin{}, errors.New("OpenAI OAuth profile ID is required")
	}
	if _, err := m.store.Connected(ctx, configID); err != nil {
		return DeviceLogin{}, err
	}
	binary, err := m.BinaryPath()
	if err != nil {
		return DeviceLogin{}, err
	}
	rootDir, configDir, err := makeIsolatedRuntime("openpoet-openai-login-")
	if err != nil {
		return DeviceLogin{}, err
	}
	loginCtx, cancel := context.WithCancel(context.Background())
	state := &deviceLoginState{
		snapshot: DeviceLogin{
			ID: uuid.NewString(), ConfigID: configID, Status: "starting", CreatedAt: time.Now().UTC(),
		},
		cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}),
		rootDir: rootDir, configDir: configDir,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		_ = os.RemoveAll(rootDir)
		return DeviceLogin{}, errors.New("OpenAI provider bridge is closed")
	}
	for _, existing := range m.logins {
		if existing.snapshot.ConfigID == configID && (existing.snapshot.Status == "starting" || existing.snapshot.Status == "waiting") {
			m.mu.Unlock()
			cancel()
			_ = os.RemoveAll(rootDir)
			return DeviceLogin{}, errors.New("an OAuth login is already pending for this profile")
		}
	}
	m.logins[state.snapshot.ID] = state
	m.mu.Unlock()

	cmd := exec.CommandContext(loginCtx, binary, "codex", "auth", "device")
	cmd.Env = isolatedEnv(rootDir, configDir)
	stdout, stdoutErr := cmd.StdoutPipe()
	stderr, stderrErr := cmd.StderrPipe()
	if stdoutErr != nil || stderrErr != nil {
		cancel()
		_ = os.RemoveAll(rootDir)
		return DeviceLogin{}, errors.New("failed to prepare OAuth helper output")
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(rootDir)
		m.finishLogin(state.snapshot.ID, "failed", "failed to start OAuth helper")
		return DeviceLogin{}, fmt.Errorf("start OAuth helper: %w", err)
	}
	go m.runDeviceLogin(state, cmd, stdout, stderr)

	select {
	case <-state.ready:
	case <-state.done:
	case <-ctx.Done():
		return DeviceLogin{}, ctx.Err()
	case <-time.After(20 * time.Second):
		return m.DeviceLoginStatus(state.snapshot.ID)
	}
	return m.DeviceLoginStatus(state.snapshot.ID)
}

func (m *Manager) runDeviceLogin(state *deviceLoginState, cmd *exec.Cmd, stdout, stderr io.Reader) {
	var stderrBuf bytes.Buffer
	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go func() {
		defer scanWG.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			switch {
			case strings.HasPrefix(line, "Visit:"):
				verificationURL, err := trustedOpenAIVerificationURL(strings.TrimSpace(strings.TrimPrefix(line, "Visit:")))
				if err != nil {
					m.failLoginAndCancel(state.snapshot.ID, "OAuth helper returned an untrusted verification URL")
					return
				}
				m.updateLoginPrompt(state.snapshot.ID, verificationURL, "")
			case strings.HasPrefix(line, "Enter code:"):
				m.updateLoginPrompt(state.snapshot.ID, "", strings.TrimSpace(strings.TrimPrefix(line, "Enter code:")))
			}
		}
	}()
	go func() {
		defer scanWG.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if stderrBuf.Len() < 2048 {
				stderrBuf.WriteString(scanner.Text())
				stderrBuf.WriteByte('\n')
			}
		}
	}()
	waitErr := cmd.Wait()
	scanWG.Wait()

	m.mu.Lock()
	status := state.snapshot.Status
	m.mu.Unlock()
	if status == "canceled" {
		_ = os.RemoveAll(state.rootDir)
		close(state.done)
		return
	}
	if status == "failed" {
		_ = os.RemoveAll(state.rootDir)
		close(state.done)
		return
	}
	if waitErr != nil {
		message := "OAuth login failed"
		if detail := safeHelperError(stderrBuf.String()); detail != "" {
			message += ": " + detail
		}
		m.finishLogin(state.snapshot.ID, "failed", message)
		_ = os.RemoveAll(state.rootDir)
		close(state.done)
		return
	}

	authPath := filepath.Join(state.configDir, "codex", "auth.json")
	raw, err := os.ReadFile(authPath)
	if err == nil {
		err = m.store.Save(context.Background(), state.snapshot.ConfigID, string(raw))
	}
	if err != nil {
		m.finishLogin(state.snapshot.ID, "failed", "OAuth completed but OpenPoet could not store the credential securely")
	} else {
		m.StopProxy(state.snapshot.ConfigID)
		m.finishLogin(state.snapshot.ID, "succeeded", "")
	}
	_ = os.RemoveAll(state.rootDir)
	close(state.done)
}

func safeHelperError(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" {
		return ""
	}
	switch {
	case strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"):
		return "device authorization timed out"
	case strings.Contains(lower, "denied"), strings.Contains(lower, "declined"):
		return "device authorization was denied"
	case strings.Contains(lower, "network"), strings.Contains(lower, "connect"), strings.Contains(lower, "request failed"):
		return "provider network request failed"
	default:
		return "provider helper failed; details were suppressed"
	}
}

func trustedOpenAIVerificationURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "auth.openai.com") {
		return "", errors.New("verification URL must use https://auth.openai.com")
	}
	if parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return "", errors.New("verification URL contains unsupported authority data")
	}
	return parsed.String(), nil
}

func (m *Manager) updateLoginPrompt(id, verificationURL, userCode string) {
	m.mu.Lock()
	state := m.logins[id]
	if state != nil {
		if verificationURL != "" {
			state.snapshot.VerificationURL = verificationURL
		}
		if userCode != "" {
			state.snapshot.UserCode = userCode
		}
		if state.snapshot.VerificationURL != "" && state.snapshot.UserCode != "" {
			state.snapshot.Status = "waiting"
			state.readyOnce.Do(func() { close(state.ready) })
		}
	}
	m.mu.Unlock()
}

func (m *Manager) finishLogin(id, status, message string) {
	m.mu.Lock()
	if state := m.logins[id]; state != nil {
		state.snapshot.Status = status
		state.snapshot.Error = message
		state.snapshot.CompletedAt = time.Now().UTC()
		state.readyOnce.Do(func() { close(state.ready) })
	}
	m.mu.Unlock()
}

func (m *Manager) failLoginAndCancel(id, message string) {
	var cancel context.CancelFunc
	m.mu.Lock()
	if state := m.logins[id]; state != nil && (state.snapshot.Status == "starting" || state.snapshot.Status == "waiting") {
		state.snapshot.Status = "failed"
		state.snapshot.Error = message
		state.snapshot.CompletedAt = time.Now().UTC()
		state.readyOnce.Do(func() { close(state.ready) })
		cancel = state.cancel
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) DeviceLoginStatus(id string) (DeviceLogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.logins[id]
	if state == nil {
		return DeviceLogin{}, errors.New("OAuth login was not found")
	}
	return state.snapshot, nil
}

func (m *Manager) CancelDeviceLogin(id string) error {
	m.mu.Lock()
	state := m.logins[id]
	if state == nil {
		m.mu.Unlock()
		return errors.New("OAuth login was not found")
	}
	if state.snapshot.Status == "starting" || state.snapshot.Status == "waiting" {
		state.snapshot.Status = "canceled"
		state.snapshot.CompletedAt = time.Now().UTC()
		state.cancel()
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Disconnect(ctx context.Context, configID int64) error {
	m.StopProxy(configID)
	return m.store.Clear(ctx, configID)
}

func (m *Manager) EnsureProxy(ctx context.Context, configID int64) (string, error) {
	if configID <= 0 {
		return "", errors.New("OpenAI OAuth profile ID is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("OpenAI provider bridge is closed")
	}
	if existing := m.proxies[configID]; existing != nil {
		ready := existing.ready
		m.mu.Unlock()
		select {
		case <-ready:
			m.mu.Lock()
			baseURL, err := existing.baseURL, existing.err
			m.mu.Unlock()
			return baseURL, err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	process := &proxyProcess{configID: configID, ready: make(chan struct{}), exited: make(chan struct{})}
	m.proxies[configID] = process
	m.mu.Unlock()

	err := m.startProxy(process)
	m.mu.Lock()
	process.err = err
	if err != nil {
		delete(m.proxies, configID)
	}
	close(process.ready)
	baseURL := process.baseURL
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	go m.monitorProxy(process)
	return baseURL, nil
}

func (m *Manager) startProxy(process *proxyProcess) error {
	binary, err := m.BinaryPath()
	if err != nil {
		return err
	}
	raw, err := m.store.Load(context.Background(), process.configID)
	if err != nil {
		return err
	}
	rootDir, configDir, err := makeIsolatedRuntime("openpoet-openai-proxy-")
	if err != nil {
		return err
	}
	process.rootDir, process.configDir = rootDir, configDir
	authDir := filepath.Join(configDir, "codex")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		_ = os.RemoveAll(rootDir)
		return err
	}
	authPath := filepath.Join(authDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(raw), 0o600); err != nil {
		_ = os.RemoveAll(rootDir)
		return err
	}
	process.lastAuth = sha256.Sum256([]byte(raw))

	port, err := availableLoopbackPort()
	if err != nil {
		_ = os.RemoveAll(rootDir)
		return err
	}
	proxyCtx, cancel := context.WithCancel(context.Background())
	process.cancel = cancel
	process.baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
	cmd := exec.CommandContext(proxyCtx, binary, "serve", "--port", strconv.Itoa(port), "--no-monitor")
	cmd.Env = append(isolatedEnv(rootDir, configDir), "CCP_BIND_ADDRESS=127.0.0.1", "RUST_LOG=warn")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(rootDir)
		return fmt.Errorf("start OpenAI provider proxy: %w", err)
	}
	go func() {
		process.exitErr = cmd.Wait()
		close(process.exited)
	}()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-process.exited:
			_ = os.RemoveAll(rootDir)
			return errors.New("OpenAI provider proxy exited before becoming ready")
		default:
		}
		response, requestErr := client.Get(process.baseURL + "/healthz")
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	cancel()
	<-process.exited
	_ = os.RemoveAll(rootDir)
	return errors.New("OpenAI provider proxy did not become ready")
}

func (m *Manager) monitorProxy(process *proxyProcess) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.persistProxyCredential(process)
		case <-process.exited:
			m.persistProxyCredential(process)
			_ = os.RemoveAll(process.rootDir)
			m.mu.Lock()
			if m.proxies[process.configID] == process {
				delete(m.proxies, process.configID)
			}
			m.mu.Unlock()
			return
		}
	}
}

func (m *Manager) persistProxyCredential(process *proxyProcess) {
	authPath := filepath.Join(process.configDir, "codex", "auth.json")
	raw, err := os.ReadFile(authPath)
	if err != nil || validateStoredAuth(string(raw)) != nil {
		return
	}
	hash := sha256.Sum256(raw)
	if hash == process.lastAuth {
		return
	}
	if m.store.Save(context.Background(), process.configID, string(raw)) == nil {
		process.lastAuth = hash
	}
}

func (m *Manager) StopProxy(configID int64) {
	m.mu.Lock()
	process := m.proxies[configID]
	if process != nil && process.cancel != nil {
		process.cancel()
	}
	m.mu.Unlock()
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	logins := make([]*deviceLoginState, 0, len(m.logins))
	for _, login := range m.logins {
		logins = append(logins, login)
	}
	proxies := make([]*proxyProcess, 0, len(m.proxies))
	for _, process := range m.proxies {
		proxies = append(proxies, process)
	}
	m.mu.Unlock()
	for _, login := range logins {
		login.cancel()
	}
	for _, process := range proxies {
		if process.cancel != nil {
			process.cancel()
		}
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, process := range proxies {
		select {
		case <-process.exited:
		case <-deadline.C:
			return
		}
	}
}

func makeIsolatedRuntime(prefix string) (string, string, error) {
	rootDir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(rootDir, 0o700); err != nil {
		_ = os.RemoveAll(rootDir)
		return "", "", err
	}
	configDir := filepath.Join(rootDir, "config")
	for _, dir := range []string{configDir, filepath.Join(rootDir, "state"), filepath.Join(rootDir, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			_ = os.RemoveAll(rootDir)
			return "", "", err
		}
	}
	return rootDir, configDir, nil
}

func isolatedEnv(rootDir, configDir string) []string {
	return []string{
		"HOME=" + rootDir,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + filepath.Join(rootDir, "tmp"),
		"CCP_CONFIG_DIR=" + configDir,
		"XDG_CONFIG_HOME=" + configDir,
		"XDG_STATE_HOME=" + filepath.Join(rootDir, "state"),
	}
}

func availableLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}
