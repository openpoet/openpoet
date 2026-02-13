package session

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type LocalRunner struct {
	workDir       string
	envVars       map[string]string
	outputHandler func([]byte)
	cliArgs       []string

	mu     sync.Mutex
	cmd    *exec.Cmd
	ptmx   *os.File
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func NewLocalRunner(workDir string, envVars map[string]string, outputHandler func([]byte), cliArgs []string) (*LocalRunner, error) {
	// Verify work directory exists
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("work directory does not exist: %s", workDir)
	}

	return &LocalRunner{
		workDir:       workDir,
		envVars:       envVars,
		outputHandler: outputHandler,
		cliArgs:       cliArgs,
		done:          make(chan struct{}),
	}, nil
}

func (r *LocalRunner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ctx, r.cancel = context.WithCancel(ctx)

	// Check if claude command exists
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		errMsg := fmt.Sprintf("\r\n\x1b[31mError: Claude Code CLI not found in PATH.\r\nPlease install it with: npm install -g @anthropic-ai/claude-code\x1b[0m\r\n")
		if r.outputHandler != nil {
			r.outputHandler([]byte(errMsg))
		}
		return fmt.Errorf("claude command not found: %w", err)
	}

	// Send startup message
	if r.outputHandler != nil {
		startMsg := fmt.Sprintf("\x1b[90mStarting Claude Code from: %s\r\nWorking directory: %s\x1b[0m\r\n\r\n", claudePath, r.workDir)
		r.outputHandler([]byte(startMsg))
	}

	// Create command to run claude with optional CLI args (e.g. --mcp-config)
	r.cmd = exec.CommandContext(r.ctx, "claude", r.cliArgs...)
	r.cmd.Dir = r.workDir

	// Set environment variables
	r.cmd.Env = os.Environ()
	for k, v := range r.envVars {
		r.cmd.Env = append(r.cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Start with PTY
	ptmx, err := pty.Start(r.cmd)
	if err != nil {
		errMsg := fmt.Sprintf("\r\n\x1b[31mError starting Claude Code: %v\x1b[0m\r\n", err)
		if r.outputHandler != nil {
			r.outputHandler([]byte(errMsg))
		}
		return fmt.Errorf("failed to start PTY: %w", err)
	}
	r.ptmx = ptmx

	// Set initial terminal size
	pty.Setsize(ptmx, &pty.Winsize{
		Rows: 24,
		Cols: 80,
	})

	// Start reading output
	go r.readOutput()

	return nil
}

func (r *LocalRunner) readOutput() {
	defer close(r.done)

	buf := make([]byte, 4096)
	for {
		n, err := r.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if r.outputHandler != nil {
				r.outputHandler(data)
			}
		}
		if err != nil {
			if err != io.EOF && r.outputHandler != nil {
				errMsg := fmt.Sprintf("\r\n\x1b[31mSession read error: %v\x1b[0m\r\n", err)
				r.outputHandler([]byte(errMsg))
			}
			return
		}
	}
}

func (r *LocalRunner) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cancel != nil {
		r.cancel()
	}

	if r.cmd != nil && r.cmd.Process != nil {
		// Send SIGTERM first for graceful shutdown
		r.cmd.Process.Signal(syscall.SIGTERM)

		// Wait up to 5 seconds for graceful exit, then force kill.
		// Without this, zombie processes accumulate and exhaust system resources
		// (PTYs, file descriptors), causing cascading failures across all local sessions.
		go func() {
			select {
			case <-r.done:
				// Process exited gracefully
			case <-time.After(5 * time.Second):
				// Force kill — process didn't respond to SIGTERM
				r.mu.Lock()
				if r.cmd != nil && r.cmd.Process != nil {
					log.Printf("[local] Process %d did not exit after SIGTERM, sending SIGKILL", r.cmd.Process.Pid)
					r.cmd.Process.Signal(syscall.SIGKILL)
				}
				r.mu.Unlock()
			}
		}()
	}

	if r.ptmx != nil {
		r.ptmx.Close()
	}

	return nil
}

func (r *LocalRunner) Write(data []byte) (int, error) {
	r.mu.Lock()
	ptmx := r.ptmx
	r.mu.Unlock()

	if ptmx == nil {
		return 0, fmt.Errorf("session not started")
	}

	return ptmx.Write(data)
}

func (r *LocalRunner) Resize(rows, cols uint16) error {
	r.mu.Lock()
	ptmx := r.ptmx
	r.mu.Unlock()

	if ptmx == nil {
		return fmt.Errorf("session not started")
	}

	return pty.Setsize(ptmx, &pty.Winsize{
		Rows: rows,
		Cols: cols,
	})
}

func (r *LocalRunner) Wait() error {
	if r.cmd == nil {
		return nil
	}

	<-r.done
	return r.cmd.Wait()
}

func (r *LocalRunner) PID() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cmd != nil && r.cmd.Process != nil {
		return r.cmd.Process.Pid
	}
	return 0
}
