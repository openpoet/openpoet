package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"openpoet/internal/database"
	"openpoet/internal/websocket"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// RemoteCodexRunner runs `codex app-server` on a remote SSH host and reuses
// CodexRunner's app-server protocol handling so approvals and MCP elicitations
// still flow through OpenPoet modals.
type RemoteCodexRunner struct {
	inner       *CodexRunner
	project     *database.Project
	decryptFunc func(string, string) (string, error)

	mu             sync.Mutex
	client         *ssh.Client
	session        *ssh.Session
	tunnelListener net.Listener
	launcherPath   string
	isWindows      bool
}

func NewRemoteCodexRunner(
	project *database.Project,
	envVars map[string]string,
	outputHandler func([]byte),
	cfg *SessionConfig,
	db *database.DB,
	hub *websocket.Hub,
	decryptFunc func(string, string) (string, error),
) (*RemoteCodexRunner, error) {
	inner, err := newCodexRunner(project.Path, envVars, outputHandler, cfg, db, hub, false)
	if err != nil {
		return nil, err
	}
	return &RemoteCodexRunner{
		inner:       inner,
		project:     project,
		decryptFunc: decryptFunc,
	}, nil
}

func (r *RemoteCodexRunner) Start(ctx context.Context) error {
	r.inner.mu.Lock()
	if r.inner.initialized {
		r.inner.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.inner.ctx = runCtx
	r.inner.cancel = cancel
	r.inner.mu.Unlock()

	remote := &RemoteRunner{
		project:       r.project,
		envVars:       r.inner.envVars,
		outputHandler: r.inner.outputHandler,
		decryptFunc:   r.decryptFunc,
		backend:       &CodexBackend{},
		done:          make(chan struct{}),
	}

	config, err := remote.buildSSHConfig()
	if err != nil {
		close(r.inner.done)
		return fmt.Errorf("failed to build SSH config: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", r.project.SSHHost.String, r.project.SSHPort.Int64)
	log.Printf("[remote-codex] Connecting to SSH server: %s", addr)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		close(r.inner.done)
		return fmt.Errorf("failed to connect to SSH server: %w", err)
	}
	r.client = client
	remote.client = client
	r.isWindows = isWindowsServer(client)
	remote.isWindows = r.isWindows
	log.Printf("[remote-codex] SSH connection established to %s (server=%s, windows=%v)", addr, client.ServerVersion(), r.isWindows)

	remote.setupReverseTunnel(client)
	r.tunnelListener = remote.tunnelListener
	rewriteCodexMCPConfigForRemote(r.inner.cfg, r.tunnelListener)

	session, err := client.NewSession()
	if err != nil {
		r.cleanup()
		close(r.inner.done)
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	r.session = session

	stdin, err := session.StdinPipe()
	if err != nil {
		r.cleanup()
		close(r.inner.done)
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		r.cleanup()
		close(r.inner.done)
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		r.cleanup()
		close(r.inner.done)
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	command := remote.backendCommand()
	r.inner.write([]byte((&CodexBackend{}).StartupMessage(command, r.project.Path)))
	cmd, err := r.remoteAppServerCommand(client, command)
	if err != nil {
		r.cleanup()
		close(r.inner.done)
		return err
	}
	log.Printf("[remote-codex] Starting command: %s", cmd)

	r.inner.mu.Lock()
	r.inner.stdin = stdin
	r.inner.mu.Unlock()

	if err := session.Start(cmd); err != nil {
		r.cleanup()
		close(r.inner.done)
		return fmt.Errorf("failed to start remote codex app-server: %w", err)
	}

	go r.inner.readStdout(stdout)
	go r.inner.readStderr(stderr)
	go func() {
		err := session.Wait()
		exitMessage := "remote codex app-server exited"
		if detail := r.inner.stderrSummary(); detail != "" {
			exitMessage += ": " + detail
		}
		r.inner.failPending(&codexRPCError{Code: -32000, Message: exitMessage})
		if err != nil && !r.inner.isShuttingDown() {
			if detail := r.inner.stderrSummary(); detail != "" {
				r.inner.write([]byte(fmt.Sprintf("\r\n\x1b[31mRemote Codex app-server exited: %v\r\n%s\x1b[0m\r\n", err, detail)))
			} else {
				r.inner.write([]byte(fmt.Sprintf("\r\n\x1b[31mRemote Codex app-server exited: %v\x1b[0m\r\n", err)))
			}
		}
		close(r.inner.done)
	}()

	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()
	if err := r.inner.initialize(initCtx); err != nil {
		_ = r.Stop()
		return err
	}
	if err := r.inner.openThread(initCtx); err != nil {
		_ = r.Stop()
		return err
	}

	r.inner.mu.Lock()
	r.inner.initialized = true
	r.inner.mu.Unlock()
	r.inner.writePrompt()
	r.inner.setCodexPhase("idle", "Waiting for instructions")
	return nil
}

func (r *RemoteCodexRunner) remoteAppServerCommand(client *ssh.Client, command string) (string, error) {
	if r.isWindows {
		script := buildWindowsBatchScript(r.inner.envVars, r.project.Path, command, []string{"app-server"})
		sftpPath, cmdPath, err := uploadWindowsLauncher(client, script)
		if err != nil {
			return "", fmt.Errorf("failed to upload Windows launcher: %w", err)
		}
		r.launcherPath = sftpPath
		return fmt.Sprintf(`"%s"`, cmdPath), nil
	}

	return buildRemoteCodexPOSIXCommand(r.inner.envVars, r.project.Path, command), nil
}

func buildRemoteCodexPOSIXCommand(envVars map[string]string, workDir, command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "codex"
	}

	var script strings.Builder
	script.WriteString("set +e\n")
	script.WriteString("for f in \"$HOME/.profile\" \"$HOME/.bash_profile\" \"$HOME/.bashrc\"; do\n")
	script.WriteString("  if [ -r \"$f\" ]; then . \"$f\" >/dev/null 2>&1 || true; fi\n")
	script.WriteString("done\n")
	script.WriteString("PATH=\"$HOME/.local/bin:$HOME/bin:$HOME/.bun/bin:$HOME/.cargo/bin:$HOME/.npm-global/bin:$HOME/.yarn/bin:$HOME/.config/yarn/global/node_modules/.bin:$HOME/.nvm/current/bin:/usr/local/bin:/opt/homebrew/bin:$PATH\"\n")
	script.WriteString("for d in \"$HOME\"/.nvm/versions/node/*/bin; do\n")
	script.WriteString("  [ -d \"$d\" ] && PATH=\"$d:$PATH\"\n")
	script.WriteString("done\n")
	script.WriteString("export PATH\n")
	script.WriteString("hash -r 2>/dev/null || true\n")

	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		script.WriteString(fmt.Sprintf("export %s=%s\n", k, shellQuote(envVars[k])))
	}

	script.WriteString(fmt.Sprintf("OPENPOET_CODEX_BIN=%s\n", shellQuote(command)))
	script.WriteString(fmt.Sprintf("cd %s\n", shellQuote(workDir)))
	script.WriteString("if ! command -v \"$OPENPOET_CODEX_BIN\" >/dev/null 2>&1; then\n")
	script.WriteString("  echo \"OpenPoet remote Codex error: codex not found for this non-interactive SSH shell.\" >&2\n")
	script.WriteString("  echo \"Configure Codex Binary with an absolute remote path, or expose codex from ~/.profile, ~/.bash_profile, or ~/.bashrc.\" >&2\n")
	script.WriteString("  echo \"PATH=$PATH\" >&2\n")
	script.WriteString("  exit 127\n")
	script.WriteString("fi\n")
	script.WriteString("exec \"$OPENPOET_CODEX_BIN\" app-server\n")

	return "bash -c " + shellQuote(script.String())
}

func rewriteCodexMCPConfigForRemote(cfg *SessionConfig, listener net.Listener) {
	if cfg == nil || strings.TrimSpace(cfg.MCPConfigJSON) == "" || listener == nil {
		return
	}

	var config struct {
		MCPServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg.MCPConfigJSON), &config); err != nil || len(config.MCPServers) == 0 {
		return
	}
	if _, ok := config.MCPServers["openpoet"]; !ok {
		return
	}

	mcpURL := fmt.Sprintf("http://%s/mcp", listener.Addr().String())
	if cfg.SessionID != "" {
		mcpURL += "?session_id=" + cfg.SessionID
	}
	openpoetEntry := map[string]interface{}{
		"url": mcpURL,
	}
	if cfg.MCPToken != "" {
		openpoetEntry["headers"] = map[string]interface{}{
			"Authorization": "Bearer " + cfg.MCPToken,
		}
	}
	config.MCPServers["openpoet"] = openpoetEntry

	out, err := json.Marshal(config)
	if err != nil {
		return
	}
	cfg.MCPConfigJSON = string(out)
	log.Printf("[remote-codex] MCP rewrite: openpoet -> HTTP %s", mcpURL)
}

func (r *RemoteCodexRunner) Stop() error {
	r.inner.mu.Lock()
	if r.inner.shuttingDown {
		r.inner.mu.Unlock()
		return nil
	}
	r.inner.shuttingDown = true
	cancel := r.inner.cancel
	stdin := r.inner.stdin
	r.inner.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	r.cleanup()
	return nil
}

func (r *RemoteCodexRunner) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.tunnelListener != nil {
		_ = r.tunnelListener.Close()
		r.tunnelListener = nil
	}
	if r.session != nil {
		_ = r.session.Close()
		r.session = nil
	}
	if r.launcherPath != "" {
		removeRemoteFile(r.client, r.launcherPath)
		r.launcherPath = ""
	}
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
}

func (r *RemoteCodexRunner) Write(data []byte) (int, error) { return r.inner.Write(data) }

func (r *RemoteCodexRunner) HandleCodexCommand(ctx context.Context, data json.RawMessage) (interface{}, error) {
	return r.inner.HandleCodexCommand(ctx, data)
}

func (r *RemoteCodexRunner) Resize(rows, cols uint16) error { return nil }

func (r *RemoteCodexRunner) Wait() error { return r.inner.Wait() }

func (r *RemoteCodexRunner) PID() int { return 0 }

func (r *RemoteCodexRunner) Done() <-chan struct{} { return r.inner.Done() }
