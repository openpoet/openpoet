//go:build windows

// NOTE: Windows support is experimental and not functional.
// This code is retained for potential future use but is not built,
// tested, or supported. Windows users should use WSL instead.

package session

import (
	"errors"
	"os"
	"syscall"
)

func sendTermSignal(p *os.Process) error {
	// Windows has no SIGTERM — kill the process directly.
	return p.Kill()
}

func sendKillSignal(p *os.Process) error {
	return p.Kill()
}

func isNormalExitError(err error) bool {
	// On Windows with ConPTY, a closed pipe or broken pipe is the normal
	// signal when the child exits.
	return errors.Is(err, syscall.ERROR_BROKEN_PIPE) ||
		errors.Is(err, os.ErrClosed)
}
