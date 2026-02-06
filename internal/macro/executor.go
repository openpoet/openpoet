package macro

import (
	"context"
	"devmanager/internal/database"
	"devmanager/internal/websocket"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

type Executor struct {
	db          *database.DB
	hub         *websocket.Hub
	decryptFunc func(string, string) (string, error)

	mu         sync.RWMutex
	executions map[string]*execution
}

type execution struct {
	ID        string
	MacroID   int64
	ProjectID int64
	Status    string
	StartTime time.Time
	cancel    context.CancelFunc
}

func NewExecutor(db *database.DB, hub *websocket.Hub, decryptFunc func(string, string) (string, error)) *Executor {
	return &Executor{
		db:          db,
		hub:         hub,
		decryptFunc: decryptFunc,
		executions:  make(map[string]*execution),
	}
}

func (e *Executor) Run(ctx context.Context, macro *database.Macro, project *database.Project) (string, error) {
	// Validate target type
	if macro.TargetType != "any" && macro.TargetType != project.Type {
		return "", fmt.Errorf("macro target type '%s' does not match project type '%s'", macro.TargetType, project.Type)
	}

	executionID := uuid.New().String()
	execCtx, cancel := context.WithCancel(ctx)

	exec := &execution{
		ID:        executionID,
		MacroID:   macro.ID,
		ProjectID: project.ID,
		Status:    "running",
		StartTime: time.Now(),
		cancel:    cancel,
	}

	e.mu.Lock()
	e.executions[executionID] = exec
	e.mu.Unlock()

	e.hub.BroadcastMacroStatus(executionID, "running")

	// Run in background
	go func() {
		var err error
		if project.Type == "local" {
			err = e.runLocal(execCtx, executionID, macro.Script, project.Path)
		} else {
			err = e.runRemote(execCtx, executionID, macro.Script, project)
		}

		e.mu.Lock()
		if err != nil {
			exec.Status = "error"
		} else {
			exec.Status = "completed"
		}
		e.mu.Unlock()

		e.hub.BroadcastMacroStatus(executionID, exec.Status)

		// Clean up after a delay
		time.AfterFunc(5*time.Minute, func() {
			e.mu.Lock()
			delete(e.executions, executionID)
			e.mu.Unlock()
		})
	}()

	return executionID, nil
}

func (e *Executor) runLocal(ctx context.Context, executionID, script, workDir string) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	// Use PTY for interactive output
	ptmx, err := pty.Start(cmd)
	if err != nil {
		e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("Error starting command: %v\n", err)))
		return err
	}
	defer ptmx.Close()

	// Read output
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			e.hub.BroadcastMacroOutput(executionID, data)
		}
		if err != nil {
			if err != io.EOF {
				e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("\nError: %v\n", err)))
			}
			break
		}
	}

	return cmd.Wait()
}

func (e *Executor) runRemote(ctx context.Context, executionID, script string, project *database.Project) error {
	// Build SSH config
	config, err := e.buildSSHConfig(project)
	if err != nil {
		e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("SSH config error: %v\n", err)))
		return err
	}

	// Connect
	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("SSH connection error: %v\n", err)))
		return err
	}
	defer client.Close()

	// Create session
	session, err := client.NewSession()
	if err != nil {
		e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("SSH session error: %v\n", err)))
		return err
	}
	defer session.Close()

	// Request PTY
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("PTY request error: %v\n", err)))
		return err
	}

	// Get stdout/stderr
	stdout, _ := session.StdoutPipe()
	stderr, _ := session.StderrPipe()

	// Build command
	cmd := fmt.Sprintf("cd %s && bash -c %q", project.Path, script)

	// Start
	if err := session.Start(cmd); err != nil {
		e.hub.BroadcastMacroOutput(executionID, []byte(fmt.Sprintf("Command start error: %v\n", err)))
		return err
	}

	// Read output
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				e.hub.BroadcastMacroOutput(executionID, data)
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				e.hub.BroadcastMacroOutput(executionID, data)
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	err = session.Wait()
	<-done
	<-done

	return err
}

func (e *Executor) buildSSHConfig(project *database.Project) (*ssh.ClientConfig, error) {
	authType := project.SSHAuthType.String
	user := project.SSHUser.String

	var authMethods []ssh.AuthMethod

	if authType == "password" {
		password, err := e.decryptFunc(
			project.SSHCredentialEncrypted.String,
			project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.Password(password))
	} else if authType == "key" || authType == "key_passphrase" {
		keyData, err := e.decryptFunc(
			project.SSHCredentialEncrypted.String,
			project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}

		signer, err := ssh.ParsePrivateKey([]byte(keyData))
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	} else {
		// Try default keys
		homeDir, _ := os.UserHomeDir()
		keyPaths := []string{
			homeDir + "/.ssh/id_rsa",
			homeDir + "/.ssh/id_ed25519",
			homeDir + "/.ssh/id_ecdsa",
		}
		for _, keyPath := range keyPaths {
			if keyData, err := os.ReadFile(keyPath); err == nil {
				if signer, err := ssh.ParsePrivateKey(keyData); err == nil {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
					break
				}
			}
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}, nil
}

func (e *Executor) Cancel(executionID string) error {
	e.mu.RLock()
	exec, ok := e.executions[executionID]
	e.mu.RUnlock()

	if !ok {
		return fmt.Errorf("execution not found: %s", executionID)
	}

	exec.cancel()
	return nil
}

func (e *Executor) GetStatus(executionID string) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	exec, ok := e.executions[executionID]
	if !ok {
		return "", false
	}
	return exec.Status, true
}
