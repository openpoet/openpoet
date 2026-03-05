//go:build !windows

package session

import (
	"errors"
	"os"
	"syscall"
)

func sendTermSignal(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

func sendKillSignal(p *os.Process) error {
	return p.Signal(syscall.SIGKILL)
}

func isNormalExitError(err error) bool {
	return errors.Is(err, syscall.EIO)
}
