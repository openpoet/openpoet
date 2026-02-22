package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"openpoet/internal/database"
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

	mu             sync.Mutex
	client         *ssh.Client
	session        *ssh.Session
	stdin          io.WriteCloser
	tunnelListener net.Listener
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	waitErr        error
}

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
	log.Printf("[remote] SSH connection established to %s", addr)

	// Set up reverse tunnel so remote bridge.sh can reach local OpenPoet
	log.Printf("[remote] Setting up reverse tunnel, envVars before: OPENPOET_HOOK_URL=%s", r.envVars["OPENPOET_HOOK_URL"])
	r.setupReverseTunnel(client)
	log.Printf("[remote] After tunnel setup, envVars: OPENPOET_HOOK_URL=%s", r.envVars["OPENPOET_HOOK_URL"])

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
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
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

	// Build command with exported environment variables and directory change
	// Using 'export' so env vars are inherited by claude and its child processes (bridge.sh)
	// Prepend common binary paths so tools like claude are found in non-interactive SSH sessions
	cmd := "export PATH=$HOME/.local/bin:$HOME/.nvm/current/bin:/usr/local/bin:$PATH && "
	for k, v := range r.envVars {
		cmd += fmt.Sprintf("export %s=%s && ", k, shellQuote(v))
	}
	cmd += fmt.Sprintf("cd %s && claude", r.project.Path)
	for _, arg := range r.cliArgs {
		cmd += " " + shellQuote(arg)
	}
	log.Printf("[remote] Starting command: %s", cmd)

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
			return nil, fmt.Errorf("SSH private key not configured for project %q — edit the project and paste your private key", r.project.Name)
		}
		keyData, err := r.decryptFunc(
			r.project.SSHCredentialEncrypted.String,
			r.project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt key: %w", err)
		}

		var signer ssh.Signer
		if authType == "key_passphrase" {
			signer, err = ssh.ParsePrivateKey([]byte(keyData))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(keyData))
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: Add proper host key verification
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

// rewriteMCPConfigForRemote replaces the openpoet subprocess MCP entry with a
// URL-based (Streamable HTTP) entry pointing through the reverse tunnel. The
// subprocess approach doesn't work on remote machines because the openpoet binary
// isn't installed there and the API URL points to localhost.
func (r *RemoteRunner) rewriteMCPConfigForRemote() {
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

		if _, hasOpenPoet := servers["openpoet"]; !hasOpenPoet {
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

		// Replace subprocess with URL-based transport through the tunnel
		mcpURL := fmt.Sprintf("http://%s/mcp", tunnelAddr)
		if sessionID != "" {
			mcpURL += "?session_id=" + sessionID
		}
		servers["openpoet"] = map[string]interface{}{
			"url": mcpURL,
		}

		newJSON, err := json.Marshal(config)
		if err != nil {
			log.Printf("[remote] MCP rewrite: failed to marshal: %v", err)
			return
		}

		r.cliArgs[i+1] = string(newJSON)
		log.Printf("[remote] MCP rewrite: openpoet -> URL %s", mcpURL)
		return
	}
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

	// Check if path exists
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

// Register factory on init
func init() {
	SetRemoteRunnerFactory(func(project *database.Project, envVars map[string]string, outputHandler func([]byte), decryptFunc func(string, string) (string, error), cliArgs []string) (Runner, error) {
		return NewRemoteRunner(project, envVars, outputHandler, decryptFunc, cliArgs)
	})
}
