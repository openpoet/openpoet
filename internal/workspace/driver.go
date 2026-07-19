package workspace

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProcessDriver spawns a manifest service as a child process with a SCRUBBED
// environment (LocalRunner discipline — no parent env leaks), in its own process
// group so the whole tree can be killed, tracks the PID, and kills it on
// teardown. `none` (externally-managed) and `compose` drivers are separate: none
// is a no-op, compose is deferred (Phase 6 optional leg).
type ProcessDriver struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// Spawn starts `command` (via sh -c) in workDir with a scrubbed env plus the
// rendered manifest env (PORT, NODE_ENV, ...). Returns the child PID.
func (d *ProcessDriver) Spawn(command, workDir string, env map[string]string) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	base := scrubbedEnv()
	for k, v := range env {
		base = append(base, k+"="+v)
	}
	cmd.Env = base
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group → kill the tree
	if err := cmd.Start(); err != nil {
		cancel()
		return 0, err
	}
	d.mu.Lock()
	d.cmd, d.cancel = cmd, cancel
	d.mu.Unlock()
	return cmd.Process.Pid, nil
}

// Kill terminates the service process group.
func (d *ProcessDriver) Kill() {
	d.mu.Lock()
	cmd, cancel := d.cmd, d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		KillPID(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	}
}

// KillPID kills a process AND its group by pid — used at teardown when only the
// persisted runtime_ref survives (no live driver instance).
func KillPID(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL) // the group
	_ = syscall.Kill(pid, syscall.SIGKILL)  // the leader, belt-and-suspenders
}

// WaitHealthy polls a health URL until it answers (any non-5xx) or times out. An
// empty URL means "no health check declared" → healthy.
func WaitHealthy(url string, timeout time.Duration) bool {
	if strings.TrimSpace(url) == "" {
		return true
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// scrubbedEnv returns only a safe allowlist of host env vars, mirroring
// LocalRunner's "NO parent environment inherited" stance.
func scrubbedEnv() []string {
	keep := []string{"PATH", "HOME", "USER", "TERM", "SHELL", "LANG", "TMPDIR"}
	var out []string
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}
