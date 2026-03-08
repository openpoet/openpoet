package updater

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// RestartSelf replaces the current process with the (potentially updated) binary.
// On Unix: uses syscall.Exec for seamless in-place replacement.
// On Windows: launches a new subprocess and exits the current one (experimental, not officially supported).
func RestartSelf() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	args := os.Args
	env := os.Environ()

	if runtime.GOOS == "windows" {
		cmd := exec.Command(execPath, args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = env
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start new process: %w", err)
		}
		log.Printf("[Updater] New process started (PID %d), exiting old process", cmd.Process.Pid)
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}

	// Unix: replace the process image entirely
	log.Printf("[Updater] Exec'ing new binary: %s %v", execPath, args)
	return syscall.Exec(execPath, args, env)
}
