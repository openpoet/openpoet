package session

// ptyHandle abstracts a pseudo-terminal.
// On Unix (macOS/Linux) it wraps creack/pty.
// Windows support (ConPTY + WSL) is experimental and non-functional.
type ptyHandle interface {
	Read(p []byte) (n int, err error)
	Write(p []byte) (n int, err error)
	Close() error
	Resize(rows, cols uint16) error
}

// startPTY creates a PTY and starts cmd inside it.
// Implemented in pty_unix.go (pty_windows.go exists but is experimental/non-functional).

// sendTermSignal sends a graceful termination signal (SIGTERM).
// sendKillSignal sends a forceful kill signal (SIGKILL).
// isNormalExitError returns true if the error is expected when a PTY child exits (EIO).
// Implemented in signal_unix.go.
