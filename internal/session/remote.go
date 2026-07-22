package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"openpoet/internal/database"
	"openpoet/internal/sshauth"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type RemoteRunner struct {
	project       *database.Project
	envVars       map[string]string
	outputHandler func([]byte)
	decryptFunc   func(string, string) (string, error)
	cliArgs       []string
	backend       BackendStrategy

	mu                     sync.Mutex
	client                 *ssh.Client
	session                *ssh.Session
	stdin                  io.WriteCloser
	tunnelListener         net.Listener
	providerTunnelListener net.Listener
	providerTunnelServer   *http.Server
	ctx                    context.Context
	cancel                 context.CancelFunc
	done                   chan struct{}
	waitErr                error
	isWindows              bool
	launcherPath           string // remote path of uploaded launcher, deleted on Stop
}

// RemoteProviderTunnelEnv is an internal launch marker. It is consumed by the
// remote runner and is never exported to the remote process.
const RemoteProviderTunnelEnv = "OPENPOET_REMOTE_PROVIDER_TUNNEL"

func NewRemoteRunner(
	project *database.Project,
	envVars map[string]string,
	outputHandler func([]byte),
	decryptFunc func(string, string) (string, error),
	cliArgs []string,
) (*RemoteRunner, error) {
	return &RemoteRunner{
		project:       project,
		envVars:       envVars,
		outputHandler: outputHandler,
		decryptFunc:   decryptFunc,
		cliArgs:       cliArgs,
		backend:       GetBackend(project.Backend),
		done:          make(chan struct{}),
	}, nil
}

func (r *RemoteRunner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ctx, r.cancel = context.WithCancel(ctx)

	// Build SSH config
	config, err := r.buildSSHConfig()
	if err != nil {
		return fmt.Errorf("failed to build SSH config: %w", err)
	}

	// Connect to SSH server
	addr := fmt.Sprintf("%s:%d", r.project.SSHHost.String, r.project.SSHPort.Int64)
	log.Printf("[remote] Connecting to SSH server: %s", addr)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("failed to connect to SSH server: %w", err)
	}
	r.client = client
	r.isWindows = isWindowsServer(client)
	log.Printf("[remote] SSH connection established to %s (server=%s, windows=%v)", addr, client.ServerVersion(), r.isWindows)

	// Set up reverse tunnel so remote bridge.sh can reach local OpenPoet
	log.Printf("[remote] Setting up reverse tunnel, envVars before: OPENPOET_HOOK_URL=%s", r.envVars["OPENPOET_HOOK_URL"])
	r.setupReverseTunnel(client)
	log.Printf("[remote] After tunnel setup, envVars: OPENPOET_HOOK_URL=%s", r.envVars["OPENPOET_HOOK_URL"])
	if err := r.setupProviderTunnel(client); err != nil {
		if r.tunnelListener != nil {
			_ = r.tunnelListener.Close()
		}
		_ = client.Close()
		return fmt.Errorf("failed to set up remote provider tunnel: %w", err)
	}

	// Rewrite OpenPoet MCP from subprocess to URL-based transport via tunnel
	r.rewriteMCPConfigForRemote()

	// Create session
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	r.session = session

	// Request PTY
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	// Initial PTY size matches the typical post-fit dimensions on a 1400px
	// terminal wrapper with the default font metrics (~164 cols x 48 rows).
	// This minimizes the SIGWINCH the remote PTY sees as soon as the client
	// connects and applies the CSS cap — the Claude Code TUI on Windows
	// ConPTY duplicates its task-list / footer when reflowing from a small
	// initial size (24x80) to a wide terminal, leaving stale rows in the
	// buffer. Starting close to the final size keeps the reflow minimal.
	if err := session.RequestPty("xterm-256color", 48, 164, modes); err != nil {
		session.Close()
		client.Close()
		return fmt.Errorf("failed to request PTY: %w", err)
	}

	// Get stdin
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	r.stdin = stdin

	// Get stdout/stderr
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		client.Close()
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Build command with exported environment variables and directory change.
	// On Windows OpenSSH the default shell is cmd.exe, which can't parse the
	// POSIX `export ... && ...` form, so we upload a .cmd launcher via SFTP
	// and invoke it instead. That also sidesteps cmd.exe's brutal inline
	// quoting rules for arguments like the JSON --mcp-config payload.
	var cmd string
	backendCommand := r.backendCommand()
	if r.isWindows {
		script := buildWindowsBatchScript(r.envVars, r.project.Path, backendCommand, r.cliArgs)
		sftpPath, cmdPath, uerr := uploadWindowsLauncher(client, script)
		if uerr != nil {
			session.Close()
			client.Close()
			return fmt.Errorf("failed to upload Windows launcher: %w", uerr)
		}
		r.launcherPath = sftpPath
		// Quote the path so directories with spaces still work. Within a cmd.exe
		// command line, double quotes around the executable path are stripped.
		cmd = fmt.Sprintf(`"%s"`, cmdPath)
	} else {
		// Using 'export' so env vars are inherited by the backend and its child processes.
		// Prepend common binary paths so tools like claude are found in non-interactive SSH sessions
		cmd = "export PATH=$HOME/.local/bin:$HOME/.nvm/current/bin:/usr/local/bin:$PATH && "
		for k, v := range r.envVars {
			cmd += fmt.Sprintf("export %s=%s && ", k, shellQuote(v))
		}
		cmd += fmt.Sprintf("cd %s && %s", shellQuote(r.project.Path), shellCommandWord(backendCommand))
		for _, arg := range r.cliArgs {
			cmd += " " + shellQuote(arg)
		}
	}
	// The command contains environment values. Never log it: remote provider
	// tunnel credentials and legacy provider API keys must remain out of logs.
	log.Printf("[remote] Starting backend=%s for project=%d path=%q", r.project.Backend, r.project.ID, r.project.Path)

	// Start command
	if err := session.Start(cmd); err != nil {
		session.Close()
		client.Close()
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Read output
	go r.readOutput(stdout)
	go r.readOutput(stderr)

	// Monitor for completion
	go func() {
		if err := session.Wait(); err != nil {
			log.Printf("[remote] Session exited with error: %v", err)
			r.waitErr = err
		}
		close(r.done)
	}()

	return nil
}

func (r *RemoteRunner) backendCommand() string {
	if override := strings.TrimSpace(r.envVars["OPENPOET_BACKEND_BINARY"]); override != "" {
		return override
	}
	if r.backend != nil {
		if binary := strings.TrimSpace(r.backend.BinaryName()); binary != "" {
			return binary
		}
	}
	return "claude"
}

func (r *RemoteRunner) buildSSHConfig() (*ssh.ClientConfig, error) {
	authType := r.project.SSHAuthType.String
	user := r.project.SSHUser.String

	var authMethods []ssh.AuthMethod

	if authType == "password" {
		if !r.project.SSHCredentialEncrypted.Valid || r.project.SSHCredentialEncrypted.String == "" {
			return nil, fmt.Errorf("SSH password not configured for project %q — edit the project and enter a password", r.project.Name)
		}
		password, err := r.decryptFunc(
			r.project.SSHCredentialEncrypted.String,
			r.project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt password: %w", err)
		}
		authMethods = append(authMethods, ssh.Password(password))
	} else if authType == "key" || authType == "key_passphrase" {
		if !r.project.SSHCredentialEncrypted.Valid || r.project.SSHCredentialEncrypted.String == "" {
			// No pasted key: fall back to the user's default keys (standard ssh
			// behavior; parity with internal/files and configsync).
			if defaults := sshauth.DefaultKeyAuthMethods(); len(defaults) > 0 {
				authMethods = append(authMethods, defaults...)
			} else {
				return nil, fmt.Errorf("SSH private key not configured for project %q — edit the project and paste your private key", r.project.Name)
			}
		} else {
			keyData, err := r.decryptFunc(
				r.project.SSHCredentialEncrypted.String,
				r.project.SSHCredentialIV.String,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt key: %w", err)
			}

			var signer ssh.Signer
			signer, err = ssh.ParsePrivateKey([]byte(keyData))
			if err != nil {
				return nil, fmt.Errorf("failed to parse private key: %w", err)
			}
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: sshauth.HostKeyCallback(), // TOFU ledger (V72): mismatch fails closed
		Timeout:         30 * time.Second,
	}, nil
}

// setupReverseTunnel creates an SSH reverse tunnel so bridge.sh on the remote
// machine can reach the local OpenPoet API.
func (r *RemoteRunner) setupReverseTunnel(client *ssh.Client) {
	hookURL, ok := r.envVars["OPENPOET_HOOK_URL"]
	if !ok || hookURL == "" {
		log.Printf("[remote] Tunnel: no OPENPOET_HOOK_URL set, skipping tunnel")
		return
	}

	// Parse the local OpenPoet address from the env var (e.g. "http://0.0.0.0:8080")
	localAddr := strings.TrimPrefix(hookURL, "http://")
	// Replace 0.0.0.0 with 127.0.0.1 for local connections
	localAddr = strings.Replace(localAddr, "0.0.0.0", "127.0.0.1", 1)
	log.Printf("[remote] Tunnel: local target address: %s (from hook URL: %s)", localAddr, hookURL)

	// Create a listener on the remote side (port 0 = OS picks a free port)
	log.Printf("[remote] Tunnel: requesting remote listener on 127.0.0.1:0...")
	listener, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("[remote] Tunnel: FAILED to create reverse tunnel: %v — hooks will not work for this session", err)
		return
	}

	tunnelAddr := listener.Addr().String()
	r.tunnelListener = listener
	log.Printf("[remote] Tunnel: remote listener created at %s", tunnelAddr)

	// Update the env var to point to the tunnel on the remote side
	r.envVars["OPENPOET_HOOK_URL"] = "http://" + tunnelAddr
	log.Printf("[remote] Tunnel: OPENPOET_HOOK_URL updated to http://%s", tunnelAddr)
	log.Printf("[remote] Tunnel: active — remote %s -> local %s", tunnelAddr, localAddr)

	// Forward connections from remote listener to local OpenPoet
	go func() {
		connCount := 0
		for {
			remoteConn, err := listener.Accept()
			if err != nil {
				log.Printf("[remote] Tunnel: listener closed: %v", err)
				return
			}
			connCount++
			connID := connCount
			log.Printf("[remote] Tunnel: accepted connection #%d from remote", connID)

			go func(remote net.Conn, id int) {
				defer remote.Close()
				log.Printf("[remote] Tunnel: conn #%d — dialing local OpenPoet at %s", id, localAddr)
				local, err := net.Dial("tcp", localAddr)
				if err != nil {
					log.Printf("[remote] Tunnel: conn #%d — FAILED to connect to local OpenPoet %s: %v", id, localAddr, err)
					return
				}
				defer local.Close()
				log.Printf("[remote] Tunnel: conn #%d — connected, forwarding data", id)

				done := make(chan struct{}, 2)
				go func() { io.Copy(local, remote); done <- struct{}{} }()
				go func() { io.Copy(remote, local); done <- struct{}{} }()
				<-done
				log.Printf("[remote] Tunnel: conn #%d — done", id)
			}(remoteConn, connID)
		}
	}()
}

type remoteListenerClient interface {
	Listen(network, address string) (net.Listener, error)
}

func (r *RemoteRunner) setupProviderTunnel(client remoteListenerClient) error {
	required := strings.TrimSpace(r.envVars[RemoteProviderTunnelEnv]) == "1"
	delete(r.envVars, RemoteProviderTunnelEnv)
	if !required {
		return nil
	}

	target, err := parseProviderTunnelTarget(r.envVars["ANTHROPIC_BASE_URL"])
	if err != nil {
		return err
	}
	listener, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("request remote loopback listener: %w", err)
	}
	token, err := newProviderTunnelToken()
	if err != nil {
		_ = listener.Close()
		return err
	}

	server := &http.Server{
		Handler:           newProviderTunnelHandler(target, token),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	r.providerTunnelListener = listener
	r.providerTunnelServer = server
	r.envVars["ANTHROPIC_BASE_URL"] = "http://" + listener.Addr().String()
	r.envVars["ANTHROPIC_AUTH_TOKEN"] = token
	r.envVars["ANTHROPIC_API_KEY"] = ""

	log.Printf("[remote] Provider tunnel active — remote %s -> local loopback %s", listener.Addr(), target.Host)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			log.Printf("[remote] Provider tunnel stopped with error: %v", serveErr)
		}
	}()
	return nil
}

func parseProviderTunnelTarget(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse provider bridge URL: %w", err)
	}
	if target.Scheme != "http" || target.Host == "" || target.Port() == "" || target.User != nil {
		return nil, errors.New("provider bridge must be an HTTP loopback URL with an explicit port")
	}
	host := target.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("provider bridge tunnel target must be loopback-only")
	}
	if target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("provider bridge URL must not contain a query or fragment")
	}
	return target, nil
}

func newProviderTunnelToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate provider tunnel credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func newProviderTunnelHandler(target *url.URL, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !providerTunnelAuthorized(request.Header, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		forwarded := request.Clone(request.Context())
		forwarded.URL = cloneProviderTunnelURL(target, request.URL)
		forwarded.RequestURI = ""
		forwarded.Host = target.Host
		forwarded.Header = request.Header.Clone()
		removeProviderTunnelHopHeaders(forwarded.Header)
		forwarded.Header.Del("X-Api-Key")
		forwarded.Header.Set("Authorization", "Bearer unused")

		response, err := http.DefaultTransport.RoundTrip(forwarded)
		if err != nil {
			log.Printf("[remote] Provider tunnel upstream error: %v", err)
			http.Error(w, "provider bridge unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		removeProviderTunnelHopHeaders(response.Header)
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(providerTunnelResponseWriter{ResponseWriter: w}, response.Body)
	})
}

type providerTunnelResponseWriter struct {
	http.ResponseWriter
}

func (w providerTunnelResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func cloneProviderTunnelURL(target, request *url.URL) *url.URL {
	result := *target
	if strings.HasSuffix(target.Path, "/") && strings.HasPrefix(request.Path, "/") {
		result.Path = target.Path + strings.TrimPrefix(request.Path, "/")
	} else if target.Path == "" || strings.HasSuffix(target.Path, "/") || strings.HasPrefix(request.Path, "/") {
		result.Path = target.Path + request.Path
	} else {
		result.Path = target.Path + "/" + request.Path
	}
	result.RawPath = ""
	result.RawQuery = request.RawQuery
	return &result
}

func removeProviderTunnelHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(key)
	}
}

func providerTunnelAuthorized(header http.Header, token string) bool {
	if secureProviderTokenEqual(strings.TrimSpace(header.Get("X-Api-Key")), token) {
		return true
	}
	parts := strings.Fields(header.Get("Authorization"))
	return len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && secureProviderTokenEqual(parts[1], token)
}

func secureProviderTokenEqual(actual, expected string) bool {
	if actual == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

// rewriteMCPConfigForRemote replaces the openpoet subprocess MCP entry with an
// HTTP-based entry pointing through the reverse tunnel. The subprocess approach
// doesn't work on remote machines because the openpoet binary isn't installed
// there and the API URL points to localhost.
func (r *RemoteRunner) rewriteMCPConfigForRemote() {
	if r.backend != nil && r.backend.Type() == BackendCodex {
		r.injectCodexOpenPoetMCPForRemote()
		return
	}
	if r.backend != nil && r.backend.Type() == BackendOpenCode {
		r.injectOpenCodeOpenPoetMCPForRemote()
		return
	}

	if r.tunnelListener == nil {
		log.Printf("[remote] MCP rewrite: no tunnel, skipping")
		return
	}

	tunnelAddr := r.tunnelListener.Addr().String()

	// Find --mcp-config in cliArgs
	for i, arg := range r.cliArgs {
		if arg != "--mcp-config" || i+1 >= len(r.cliArgs) {
			continue
		}

		var config map[string]interface{}
		if err := json.Unmarshal([]byte(r.cliArgs[i+1]), &config); err != nil {
			log.Printf("[remote] MCP rewrite: failed to parse config: %v", err)
			return
		}

		servers, ok := config["mcpServers"].(map[string]interface{})
		if !ok {
			return
		}

		openpoetEntry, hasOpenPoet := servers["openpoet"]
		if !hasOpenPoet {
			return
		}

		// Extract session_id from cliArgs (--session-id <id>)
		sessionID := ""
		for j, a := range r.cliArgs {
			if a == "--session-id" && j+1 < len(r.cliArgs) {
				sessionID = r.cliArgs[j+1]
				break
			}
		}

		// Extract the per-session bearer from the subprocess args so the HTTP
		// transport carries verified identity over the tunnel instead of the
		// spoofable ?session_id= query param alone.
		sessionToken := extractMCPServeArg(openpoetEntry, "--session-token")

		// Replace subprocess with HTTP transport through the tunnel.
		// Claude Code requires "type":"http" alongside "url".
		mcpURL := fmt.Sprintf("http://%s/mcp", tunnelAddr)
		if sessionID != "" {
			mcpURL += "?session_id=" + sessionID
		}
		httpEntry := map[string]interface{}{
			"type": "http",
			"url":  mcpURL,
		}
		if sessionToken != "" {
			httpEntry["headers"] = map[string]interface{}{
				"Authorization": "Bearer " + sessionToken,
			}
		}
		servers["openpoet"] = httpEntry

		newJSON, err := json.Marshal(config)
		if err != nil {
			log.Printf("[remote] MCP rewrite: failed to marshal: %v", err)
			return
		}

		r.cliArgs[i+1] = string(newJSON)
		log.Printf("[remote] MCP rewrite: openpoet -> HTTP %s", mcpURL)
		return
	}
}

// extractMCPServeArg pulls the value following flagName from an mcp server
// config entry's args array (e.g. the "--session-token" value of the injected
// openpoet mcp-serve subprocess). Returns "" when absent.
func extractMCPServeArg(entry interface{}, flagName string) string {
	m, ok := entry.(map[string]interface{})
	if !ok {
		return ""
	}
	args, ok := m["args"].([]interface{})
	if !ok {
		return ""
	}
	for j, a := range args {
		if s, _ := a.(string); s == flagName && j+1 < len(args) {
			if v, _ := args[j+1].(string); v != "" {
				return v
			}
		}
	}
	return ""
}

func (r *RemoteRunner) injectCodexOpenPoetMCPForRemote() {
	raw := r.envVars["OPENPOET_MCP_CONFIG_JSON"]
	delete(r.envVars, "OPENPOET_MCP_CONFIG_JSON")
	providerSessionID := r.envVars["OPENPOET_PROVIDER_SESSION_ID"]
	delete(r.envVars, "OPENPOET_PROVIDER_SESSION_ID")

	if r.tunnelListener == nil {
		log.Printf("[remote] Codex MCP inject: no tunnel, skipping")
		return
	}
	if strings.TrimSpace(raw) == "" {
		return
	}

	var config struct {
		MCPServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil || len(config.MCPServers) == 0 {
		log.Printf("[remote] Codex MCP inject: failed to parse config: %v", err)
		return
	}
	if _, ok := config.MCPServers["openpoet"]; !ok {
		return
	}

	mcpURL := fmt.Sprintf("http://%s/mcp", r.tunnelListener.Addr().String())
	if sessionID := strings.TrimSpace(r.envVars["OPENPOET_SESSION_ID"]); sessionID != "" {
		mcpURL += "?session_id=" + sessionID
	}
	r.insertCodexConfigOverride("mcp_servers.openpoet.url", mcpURL, providerSessionID)
	// Carry the per-session opst1_ bearer so remote-codex MCP calls authenticate
	// as the verified session instead of relying on the spoofable query param
	// (mirrors the OpenCode headers injection below). bearer_token_env_var is
	// supported by codex-cli >= 0.144 (validated via `codex mcp list`); older
	// versions that reject the key would fail config parse, so the token stays
	// in the env and only the key name is shipped here.
	if strings.TrimSpace(r.envVars["OPENPOET_SESSION_TOKEN"]) != "" {
		r.insertCodexConfigOverride("mcp_servers.openpoet.bearer_token_env_var", "OPENPOET_SESSION_TOKEN", providerSessionID)
	}
	log.Printf("[remote] Codex MCP inject: openpoet -> HTTP %s (bearer=%v)", mcpURL, strings.TrimSpace(r.envVars["OPENPOET_SESSION_TOKEN"]) != "")
}

func (r *RemoteRunner) injectOpenCodeOpenPoetMCPForRemote() {
	if r.tunnelListener == nil {
		log.Printf("[remote] OpenCode MCP inject: no tunnel, skipping")
		return
	}

	raw := r.envVars["OPENCODE_CONFIG_CONTENT"]
	if strings.TrimSpace(raw) == "" {
		return
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		log.Printf("[remote] OpenCode MCP inject: failed to parse config: %v", err)
		return
	}
	mcp, ok := config["mcp"].(map[string]interface{})
	if !ok {
		return
	}
	if _, ok := mcp["openpoet"]; !ok {
		return
	}

	mcpURL := fmt.Sprintf("http://%s/mcp", r.tunnelListener.Addr().String())
	if sessionID := strings.TrimSpace(r.envVars["OPENPOET_SESSION_ID"]); sessionID != "" {
		mcpURL += "?session_id=" + sessionID
	}
	openpoetEntry := map[string]interface{}{
		"type":    "remote",
		"url":     mcpURL,
		"enabled": true,
	}
	// Carry the per-session bearer so the remote OpenCode MCP calls authenticate
	// as the verified session instead of relying on the spoofable query param.
	if token := strings.TrimSpace(r.envVars["OPENPOET_SESSION_TOKEN"]); token != "" {
		openpoetEntry["headers"] = map[string]interface{}{
			"Authorization": "Bearer " + token,
		}
	}
	mcp["openpoet"] = openpoetEntry

	data, err := json.Marshal(config)
	if err != nil {
		log.Printf("[remote] OpenCode MCP inject: failed to marshal config: %v", err)
		return
	}
	r.envVars["OPENCODE_CONFIG_CONTENT"] = string(data)
	log.Printf("[remote] OpenCode MCP inject: openpoet -> HTTP %s", mcpURL)
}

func (r *RemoteRunner) insertCodexConfigOverride(key, value, providerSessionID string) {
	override := []string{"-c", key + "=" + codexTomlQuotedValue(value)}
	insertAt := len(r.cliArgs)
	if providerSessionID != "" && len(r.cliArgs) > 0 && r.cliArgs[len(r.cliArgs)-1] == providerSessionID {
		insertAt = len(r.cliArgs) - 1
	}
	r.cliArgs = append(r.cliArgs, "", "")
	copy(r.cliArgs[insertAt+2:], r.cliArgs[insertAt:])
	copy(r.cliArgs[insertAt:], override)
}

func (r *RemoteRunner) readOutput(reader io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if r.outputHandler != nil {
				r.outputHandler(data)
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *RemoteRunner) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cancel != nil {
		r.cancel()
	}

	if r.tunnelListener != nil {
		r.tunnelListener.Close()
	}
	if r.session != nil {
		// Send Ctrl+C first
		r.stdin.Write([]byte{0x03})
		time.Sleep(100 * time.Millisecond)
		r.session.Close()
	}

	if r.providerTunnelServer != nil {
		_ = r.providerTunnelServer.Close()
	} else if r.providerTunnelListener != nil {
		_ = r.providerTunnelListener.Close()
	}

	if r.launcherPath != "" {
		removeRemoteFile(r.client, r.launcherPath)
	}

	if r.client != nil {
		r.client.Close()
	}

	return nil
}

func (r *RemoteRunner) Write(data []byte) (int, error) {
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()

	if stdin == nil {
		return 0, fmt.Errorf("session not started")
	}

	return stdin.Write(data)
}

func (r *RemoteRunner) Resize(rows, cols uint16) error {
	r.mu.Lock()
	session := r.session
	r.mu.Unlock()

	if session == nil {
		return fmt.Errorf("session not started")
	}

	return session.WindowChange(int(rows), int(cols))
}

func (r *RemoteRunner) Wait() error {
	<-r.done
	return r.waitErr
}

func (r *RemoteRunner) PID() int {
	// Remote PIDs are not easily accessible
	return 0
}

func (r *RemoteRunner) Done() <-chan struct{} {
	return r.done
}

func (r *RemoteRunner) DiscoverCodexProviderSessionID(workDir string, since time.Time) (string, error) {
	r.mu.Lock()
	client := r.client
	codexHome := strings.TrimSpace(r.envVars["CODEX_HOME"])
	r.mu.Unlock()
	if client == nil {
		return "", fmt.Errorf("remote SSH client is not connected")
	}
	return discoverCodexProviderSessionIDOverSSH(client, codexHome, workDir, since)
}

func (r *RemoteRunner) DiscoverOpenCodeProviderSessionID(workDir string, since time.Time) (string, error) {
	r.mu.Lock()
	client := r.client
	binaryPath := strings.TrimSpace(r.envVars["OPENPOET_BACKEND_BINARY"])
	r.mu.Unlock()
	if client == nil {
		return "", fmt.Errorf("remote SSH client is not connected")
	}
	return discoverOpenCodeProviderSessionIDOverSSH(client, binaryPath, workDir, since)
}

func discoverRemoteCodexProviderSessionID(project *database.Project, decryptFunc func(string, string) (string, error), codexHome, workDir string, since time.Time) (string, error) {
	runner := &RemoteRunner{
		project:     project,
		decryptFunc: decryptFunc,
	}
	config, err := runner.buildSSHConfig()
	if err != nil {
		return "", err
	}
	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return discoverCodexProviderSessionIDOverSSH(client, codexHome, workDir, since)
}

func discoverRemoteOpenCodeProviderSessionID(project *database.Project, decryptFunc func(string, string) (string, error), workDir string, since time.Time) (string, error) {
	runner := &RemoteRunner{
		project:     project,
		decryptFunc: decryptFunc,
	}
	config, err := runner.buildSSHConfig()
	if err != nil {
		return "", err
	}
	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return discoverOpenCodeProviderSessionIDOverSSH(client, parseOpenCodeConfig(project.BackendConfig).BinaryPath, workDir, since)
}

func discoverOpenCodeProviderSessionIDOverSSH(client *ssh.Client, binaryPath, workDir string, since time.Time) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	command := strings.TrimSpace(binaryPath)
	if command == "" {
		command = "opencode"
	}
	cmd := "cd " + shellQuote(workDir) + " && " + shellCommandWord(command) + " session list --format json --max-count 25"
	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("remote OpenCode session discovery failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return selectOpenCodeProviderSessionID(output, workDir, since), nil
}

func discoverCodexProviderSessionIDOverSSH(client *ssh.Client, codexHome, workDir string, since time.Time) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	cmd := ""
	if strings.TrimSpace(codexHome) != "" {
		cmd += "CODEX_HOME=" + shellQuote(codexHome) + " "
	}
	cmd += "python3 -c " + shellQuote(remoteCodexProviderSessionIDScript) + " " + shellQuote(workDir) + " " + shellQuote(strconv.FormatInt(since.Unix(), 10))
	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("remote Codex session discovery failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

const remoteCodexProviderSessionIDScript = `
import json, os, pathlib, sys

work = os.path.realpath(sys.argv[1]) if len(sys.argv) > 1 and sys.argv[1] else ""
since = float(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2] else 0
home = os.environ.get("CODEX_HOME") or os.path.expanduser("~/.codex")
root = pathlib.Path(home).expanduser() / "sessions"
best = None

if root.is_dir():
    for path in root.rglob("*.jsonl"):
        try:
            stat = path.stat()
            if since and stat.st_mtime < since:
                continue
            with path.open("r", encoding="utf-8", errors="ignore") as fh:
                raw = json.loads(fh.readline())
            if raw.get("type") != "session_meta":
                continue
            payload = raw.get("payload") or {}
            sid = str(payload.get("id") or "").strip()
            cwd = str(payload.get("cwd") or "").strip()
            if not sid:
                continue
            if work and os.path.realpath(cwd) != work:
                continue
            origin = str(payload.get("originator") or "").lower()
            item = (1 if origin == "openpoet" else 0, stat.st_mtime, sid)
            if best is None or item[:2] > best[:2]:
                best = item
        except Exception:
            pass

if best:
    print(best[2])
`

// ValidateConnection tests SSH connection without starting a session
func ValidateConnection(project *database.Project, decryptFunc func(string, string) (string, error)) error {
	runner := &RemoteRunner{
		project:     project,
		decryptFunc: decryptFunc,
	}

	config, err := runner.buildSSHConfig()
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer client.Close()

	// Test if we can create a session and check if the path exists
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	if isWindowsServer(client) {
		// cmd.exe path check. `if exist` is a builtin, so we can call it directly.
		// `\` at end ensures `if exist` treats the target as a directory.
		path := normalizeWindowsPath(project.Path)
		probe := fmt.Sprintf(`if exist "%s\" (echo ok) else (echo missing)`, strings.TrimRight(path, `\/`))
		output, err := session.CombinedOutput(probe)
		if err != nil {
			return fmt.Errorf("failed to validate path on Windows host: %w", err)
		}
		if !strings.Contains(string(output), "ok") {
			return fmt.Errorf("path does not exist or is not accessible: %s", project.Path)
		}
		return nil
	}

	// Check if path exists (POSIX)
	output, err := session.CombinedOutput(fmt.Sprintf("test -d %s && echo ok", project.Path))
	if err != nil || string(output) != "ok\n" {
		return fmt.Errorf("path does not exist or is not accessible: %s", project.Path)
	}

	return nil
}

// shellQuote wraps a string in single quotes for safe use in shell commands.
// Internal single quotes are escaped using the '\” idiom.
func shellQuote(s string) string {
	return "'" + strings.Replace(s, "'", `'\''`, -1) + "'"
}

func shellCommandWord(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "claude"
	}
	if strings.ContainsAny(command, "/\\ \t'\"$`;&|()<>") {
		return shellQuote(command)
	}
	return command
}

// Register factory on init
func init() {
	SetRemoteRunnerFactory(func(project *database.Project, envVars map[string]string, outputHandler func([]byte), decryptFunc func(string, string) (string, error), cliArgs []string) (Runner, error) {
		return NewRemoteRunner(project, envVars, outputHandler, decryptFunc, cliArgs)
	})
}
