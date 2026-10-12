package mcp

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func shortenMCPStreamTimings(t *testing.T, ping, write time.Duration) {
	t.Helper()
	oldPing, oldWrite := mcpStreamPingInterval, mcpStreamWriteTimeout
	mcpStreamPingInterval, mcpStreamWriteTimeout = ping, write
	t.Cleanup(func() { mcpStreamPingInterval, mcpStreamWriteTimeout = oldPing, oldWrite })
}

// The GET /mcp stream must outlive both the old 10-minute cut and the server's
// WriteTimeout: codex holds it for the whole session and a Windows codex does
// not reopen it. Scaled down: the server WriteTimeout is 300ms and the stream
// must stay open (pings flowing) well past it, through the same chi
// middleware wrappers production uses (Logger, Compress).
func TestMCPStreamOutlivesServerWriteTimeout(t *testing.T) {
	shortenMCPStreamTimings(t, 50*time.Millisecond, time.Second)

	h := &HTTPHandler{sessions: map[string]*httpSession{}}
	r := chi.NewRouter()
	r.Use(middleware.Logger, middleware.Recoverer, middleware.Compress(5))
	r.Get("/mcp", h.handleGetStream)

	srv := httptest.NewUnstartedServer(r)
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp?session_id=s1", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	lines := make(chan string, 256)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		readErr <- sc.Err()
	}()

	deadline := time.After(1500 * time.Millisecond) // 5x the server WriteTimeout
	pings := 0
	var lastPing time.Time
	for {
		select {
		case l := <-lines:
			if l == ": ping" {
				pings++
				lastPing = time.Now()
			}
		case err := <-readErr:
			t.Fatalf("stream ended after %d pings: %v", pings, err)
		case <-deadline:
			if pings < 10 || time.Since(lastPing) > 200*time.Millisecond {
				t.Fatalf("stream stalled: %d pings, last %v ago", pings, time.Since(lastPing))
			}
			return
		}
	}
}

// failingStreamWriter accepts the opening frame, then fails every write — the
// shape of a peer that went away without closing the connection.
type failingStreamWriter struct {
	header    http.Header
	writes    int
	deadlines []time.Time
}

func (w *failingStreamWriter) SetWriteDeadline(t time.Time) error {
	w.deadlines = append(w.deadlines, t)
	return nil
}
func (w *failingStreamWriter) SetReadDeadline(time.Time) error { return nil }

func (w *failingStreamWriter) Header() http.Header { return w.header }
func (w *failingStreamWriter) WriteHeader(int)     {}
func (w *failingStreamWriter) Flush()              {}
func (w *failingStreamWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("broken pipe")
	}
	return len(b), nil
}

// A dead connection is detected by the heartbeat: the first failed ping ends
// the stream even though the request context never fires. Every frame is
// written under its own deadline, so a peer that stops reading cannot block
// the handler forever either.
func TestMCPStreamHeartbeatDetectsDeadConnection(t *testing.T) {
	shortenMCPStreamTimings(t, 20*time.Millisecond, time.Second)

	h := &HTTPHandler{sessions: map[string]*httpSession{}}
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(context.Background())
	req.Header.Set("Accept", "text/event-stream")
	w := &failingStreamWriter{header: http.Header{}}

	done := make(chan struct{})
	go func() { h.handleGetStream(w, req); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream kept running after heartbeat writes failed")
	}
	if len(w.deadlines) != 2 {
		t.Fatalf("write deadlines set = %d, want one per frame (2)", len(w.deadlines))
	}
	for _, d := range w.deadlines {
		if until := time.Until(d); until <= 0 || until > time.Second {
			t.Fatalf("write deadline %v from now, want within mcpStreamWriteTimeout", until)
		}
	}
}
