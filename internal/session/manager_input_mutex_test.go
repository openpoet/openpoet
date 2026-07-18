package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"openpoet/internal/database"
)

// unsyncRunner is a Runner whose Write has NO internal synchronization, so the
// -race detector fires if Manager ever lets two writes into the same session
// run concurrently. It is the negative-space proof of the per-session input
// mutex: with the mutex, Write is only ever entered serially.
type unsyncRunner struct {
	writes [][]byte
	calls  int
}

func (r *unsyncRunner) Start(ctx context.Context) error { return nil }
func (r *unsyncRunner) Stop() error                     { return nil }
func (r *unsyncRunner) Write(data []byte) (int, error) {
	r.calls++ // deliberately unguarded
	r.writes = append(r.writes, append([]byte(nil), data...))
	return len(data), nil
}
func (r *unsyncRunner) Resize(rows, cols uint16) error { return nil }
func (r *unsyncRunner) Wait() error                    { return nil }
func (r *unsyncRunner) PID() int                       { return 0 }
func (r *unsyncRunner) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestWriteToSessionSerialWritesAreRaceFree drives many concurrent
// WriteToSession calls at one session and asserts (a) the -race detector stays
// quiet and (b) every write reached the runner. Without the per-session input
// mutex the unguarded runner races and loses writes.
func TestWriteToSessionSerialWritesAreRaceFree(t *testing.T) {
	runner := &unsyncRunner{}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {session: &database.Session{ID: "sess-1", Backend: "claude_code"}, runner: runner},
		},
	}

	const writers = 32
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			if err := m.WriteToSession("sess-1", []byte("x")); err != nil {
				t.Errorf("WriteToSession: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(runner.writes); got != writers {
		t.Fatalf("expected %d serialized writes, got %d (mutex not serializing)", writers, got)
	}
}

// TestInputMutexKeepsSubmitLineAtomic proves the text->Enter pair of
// SubmitLineToSession is never split by a concurrent single write: in the
// recorded stream every prompt is immediately followed by its carriage return.
func TestInputMutexKeepsSubmitLineAtomic(t *testing.T) {
	runner := &unsyncRunner{}
	m := &Manager{
		sessions: map[string]*runningSession{
			"sess-1": {session: &database.Session{ID: "sess-1", Backend: "claude_code"}, runner: runner},
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := m.SubmitLineToSession("sess-1", "PROMPT", 200*time.Microsecond); err != nil {
				t.Errorf("SubmitLineToSession: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := m.WriteToSession("sess-1", []byte("i")); err != nil {
				t.Errorf("WriteToSession: %v", err)
			}
		}
	}()
	wg.Wait()

	// Every "PROMPT" must be directly followed by "\r"; an interloper byte in
	// between means the input lock did not span the text->Enter window.
	for i, w := range runner.writes {
		if string(w) == "PROMPT" {
			if i+1 >= len(runner.writes) || string(runner.writes[i+1]) != "\r" {
				t.Fatalf("PROMPT at %d not immediately followed by \\r (interleaved write)", i)
			}
		}
	}
}
