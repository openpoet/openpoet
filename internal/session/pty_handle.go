package session

// ptyHandle abstracts a pseudo-terminal across platforms.
// On Unix it wraps creack/pty, on Windows it wraps ConPTY (+ WSL).
type ptyHandle interface {
	Read(p []byte) (n int, err error)
	Write(p []byte) (n int, err error)
	Close() error
	Resize(rows, cols uint16) error
}

// startPTY creates a PTY and starts cmd inside it.
// Implemented per-platform in pty_unix.go and pty_windows.go.

// sendTermSignal sends a graceful termination signal (SIGTERM on Unix,
// TerminateProcess on Windows).
// Implemented per-platform in signal_unix.go and signal_windows.go.

// sendKillSignal sends a forceful kill signal (SIGKILL on Unix,
// TerminateProcess on Windows).
// Implemented per-platform in signal_unix.go and signal_windows.go.

// isNormalExitError returns true if the error is expected when a PTY child
// exits (EIO on Unix, broken pipe / closed handle on Windows).
// Implemented per-platform in signal_unix.go and signal_windows.go.
