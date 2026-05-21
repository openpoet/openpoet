package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ptyDumper writes raw PTY output to a per-session log file when the
// OPENPOET_DEBUG_PTY_DUMP env var is set. The dump file uses a mixed format
// per chunk: a header line "[ts kind=remote|local size=N]" followed by the
// chunk rendered as Go-quoted bytes (so ANSI escapes show up as "\x1b[..."
// and are diffable across local/remote runs).
//
// Gate intentionally heavy. This logger reopens the file on every chunk so
// crashes don't lose the tail, which is fine for ad-hoc debugging.
type ptyDumper struct {
	mu      sync.Mutex
	path    string
	enabled bool
	kind    string
}

// newPTYDumper returns a dumper. If the env var isn't set or sessionID is
// empty, the dumper is a no-op.
func newPTYDumper(sessionID, kind string) *ptyDumper {
	enabled := os.Getenv("OPENPOET_DEBUG_PTY_DUMP") != "" && sessionID != ""
	if !enabled {
		return &ptyDumper{}
	}
	runDir := ".run"
	if env := os.Getenv("OPENPOET_DEBUG_PTY_DUMP_DIR"); env != "" {
		runDir = env
	}
	_ = os.MkdirAll(runDir, 0o755)
	return &ptyDumper{
		path:    filepath.Join(runDir, fmt.Sprintf("pty-dump-%s-%s.log", kind, sessionID)),
		enabled: true,
		kind:    kind,
	}
}

// write appends one chunk to the dump file. Safe to call from any goroutine.
func (d *ptyDumper) write(data []byte) {
	if !d.enabled || len(data) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	f, err := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	ts := time.Now().UnixNano() / int64(time.Millisecond)
	fmt.Fprintf(f, "[%d %s size=%d]\n%q\n", ts, d.kind, len(data), string(data))
}
