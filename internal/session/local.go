package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	backend       BackendStrategy

	mu     sync.Mutex
	cmd    *exec.Cmd
	ptmx   *os.File
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func NewLocalRunner(workDir string, envVars map[string]string, outputHandler func([]byte), cliArgs []string, backend BackendStrategy) (*LocalRunner, error) {
	// Verify work directory exists
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("work directory does not exist: %s", workDir)
	}

	return &LocalRunner{
		workDir:       workDir,
		envVars:       envVars,
		outputHandler: outputHandler,
		cliArgs:       cliArgs,
		backend:       backend,
		done:          make(chan struct{}),
	}, nil
}

func (r *LocalRunner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ctx, r.cancel = context.WithCancel(ctx)

	// Check if CLI binary exists and get full path
	binaryName := r.backend.BinaryName()
	binaryPath, err := exec.LookPath(binaryName)
	if err != nil {
		// Try finding the binary as a sibling of the openpoet binary
		if binPath, ok := r.envVars["OPENPOET_BIN"]; ok && binPath != "" {
			sibling := filepath.Join(filepath.Dir(binPath), binaryName)
			if _, statErr := os.Stat(sibling); statErr == nil {
				binaryPath = sibling
				err = nil
			}
		}
	}
	if err != nil {
		if r.outputHandler != nil {
			r.outputHandler([]byte(r.backend.NotFoundMessage()))
		}
		return fmt.Errorf("%s command not found: %w", binaryName, err)
	}

	// Send startup message
	if r.outputHandler != nil {
		r.outputHandler([]byte(r.backend.StartupMessage(binaryPath, r.workDir)))
	}

	// Create command using full path (avoids needing PATH in env)
	r.cmd = exec.CommandContext(r.ctx, binaryPath, r.cliArgs...)
	r.cmd.Dir = r.workDir

	// NO parent environment variables are inherited.
	// Only use injected envVars for complete isolation from parent shell.
	// Add minimal system vars needed for the process to function.
	r.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
		"TERM=xterm-256color",
	}
	// Add all injected env vars
	for k, v := range r.envVars {
		r.cmd.Env = append(r.cmd.Env, fmt.Sprintf("%s=%s", k, v))
		// Log ANTHROPIC vars for debugging
		if strings.HasPrefix(k, "ANTHROPIC_") {
			if k == "ANTHROPIC_API_KEY" && len(v) > 10 {
				log.Printf("[Session] Env: %s=%s... (len=%d)", k, v[:10]+"...", len(v))
			} else {
				log.Printf("[Session] Env: %s=%s", k, v)
			}
		}
	}

	// Start with PTY
	ptmx, err := pty.Start(r.cmd)
	if err != nil {
		errMsg := fmt.Sprintf("\r\n\x1b[31mError starting %s: %v\x1b[0m\r\n", binaryName, err)
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
			// EIO is the normal signal when the PTY child process exits — suppress it.
			if err != io.EOF && !errors.Is(err, syscall.EIO) && r.outputHandler != nil {
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

		// Wait for process exit, then close the PTY.
		// Closing the PTY master before the process exits causes spurious EIO
		// errors in the child (e.g. copilot's "Error: read EIO").
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
				// Wait for the kill to take effect
				<-r.done
			}
			// Close PTY after the process has exited
			r.mu.Lock()
			if r.ptmx != nil {
				r.ptmx.Close()
			}
			r.mu.Unlock()
		}()
	} else if r.ptmx != nil {
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

func (r *LocalRunner) Done() <-chan struct{} {
	return r.done
}
