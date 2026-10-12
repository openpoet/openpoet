package session

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeKeepaliveConn struct {
	mu       sync.Mutex
	requests int
	answer   bool // reply to probes
	closed   chan struct{}
	once     sync.Once
}

func newFakeKeepaliveConn(answer bool) *fakeKeepaliveConn {
	return &fakeKeepaliveConn{answer: answer, closed: make(chan struct{})}
}

func (c *fakeKeepaliveConn) SendRequest(name string, wantReply bool, _ []byte) (bool, []byte, error) {
	c.mu.Lock()
	c.requests++
	answer := c.answer
	c.mu.Unlock()
	if name != sshKeepaliveRequest || !wantReply {
		return false, nil, errors.New("unexpected request")
	}
	if !answer {
		<-c.closed // a dead peer: the reply never comes
		return false, nil, errors.New("closed")
	}
	return false, nil, nil // OpenSSH answers keepalive@ with a failure reply
}

func (c *fakeKeepaliveConn) Wait() error { <-c.closed; return errors.New("closed") }
func (c *fakeKeepaliveConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeKeepaliveConn) probes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func TestSSHKeepaliveClosesDeadConnection(t *testing.T) {
	conn := newFakeKeepaliveConn(false)
	startSSHKeepalive(conn, sshKeepaliveConfig{Interval: 20 * time.Millisecond, Max: 3}, "test")
	select {
	case <-conn.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive never closed a connection that stopped answering")
	}
	if n := conn.probes(); n < 3 {
		t.Fatalf("closed after %d probes, want 3", n)
	}
}

func TestSSHKeepaliveKeepsLiveConnection(t *testing.T) {
	conn := newFakeKeepaliveConn(true)
	startSSHKeepalive(conn, sshKeepaliveConfig{Interval: 10 * time.Millisecond, Max: 2}, "test")
	time.Sleep(150 * time.Millisecond)
	select {
	case <-conn.closed:
		t.Fatal("keepalive closed a connection that answers (with a failure reply)")
	default:
	}
	if conn.probes() < 5 {
		t.Fatalf("only %d probes in 150ms at a 10ms interval", conn.probes())
	}
	_ = conn.Close()
}

func TestSSHKeepaliveConfigFromEnv(t *testing.T) {
	t.Setenv("OPENPOET_SSH_KEEPALIVE_INTERVAL", "")
	t.Setenv("OPENPOET_SSH_KEEPALIVE_MAX", "")
	if got := sshKeepaliveConfigFromEnv(); got.Interval != defaultSSHKeepaliveInterval || got.Max != defaultSSHKeepaliveMax {
		t.Fatalf("defaults = %+v", got)
	}
	t.Setenv("OPENPOET_SSH_KEEPALIVE_INTERVAL", "10")
	t.Setenv("OPENPOET_SSH_KEEPALIVE_MAX", "5")
	if got := sshKeepaliveConfigFromEnv(); got.Interval != 10*time.Second || got.Max != 5 {
		t.Fatalf("configured = %+v", got)
	}
	t.Setenv("OPENPOET_SSH_KEEPALIVE_INTERVAL", "0")
	if got := sshKeepaliveConfigFromEnv(); got.Interval != 0 {
		t.Fatalf("interval 0 should disable, got %+v", got)
	}
	t.Setenv("OPENPOET_SSH_KEEPALIVE_INTERVAL", "junk")
	t.Setenv("OPENPOET_SSH_KEEPALIVE_MAX", "-1")
	if got := sshKeepaliveConfigFromEnv(); got.Interval != defaultSSHKeepaliveInterval || got.Max != defaultSSHKeepaliveMax {
		t.Fatalf("invalid values should fall back to defaults, got %+v", got)
	}
}
