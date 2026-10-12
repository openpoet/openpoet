package session

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH keepalive for long-lived remote sessions. Without it a half-open TCP
// connection (NAT timeout, sleeping host, Wi-Fi drop) is only noticed when a
// write finally fails — possibly never for an idle session. Probing with
// keepalive@openssh.com and closing the client after too many unanswered
// probes turns a silent hang into an SSH error the manager can act on
// (status "error" + the reconnect shot).
//
// Configurable through the environment (seconds / count):
//
//	OPENPOET_SSH_KEEPALIVE_INTERVAL  probe interval, default 30; 0 disables
//	OPENPOET_SSH_KEEPALIVE_MAX       consecutive unanswered probes before the
//	                                 connection is closed, default 3
const (
	defaultSSHKeepaliveInterval = 30 * time.Second
	defaultSSHKeepaliveMax      = 3
	sshKeepaliveRequest         = "keepalive@openssh.com"
)

type sshKeepaliveConfig struct {
	Interval time.Duration
	Max      int
}

func sshKeepaliveConfigFromEnv() sshKeepaliveConfig {
	cfg := sshKeepaliveConfig{Interval: defaultSSHKeepaliveInterval, Max: defaultSSHKeepaliveMax}
	if raw := strings.TrimSpace(os.Getenv("OPENPOET_SSH_KEEPALIVE_INTERVAL")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
			cfg.Interval = time.Duration(secs) * time.Second
		} else {
			log.Printf("[ssh-keepalive] ignoring invalid OPENPOET_SSH_KEEPALIVE_INTERVAL=%q", raw)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("OPENPOET_SSH_KEEPALIVE_MAX")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			cfg.Max = n
		} else {
			log.Printf("[ssh-keepalive] ignoring invalid OPENPOET_SSH_KEEPALIVE_MAX=%q", raw)
		}
	}
	return cfg
}

// sshKeepaliveConn is the part of *ssh.Client the keepalive loop uses.
type sshKeepaliveConn interface {
	SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error)
	Wait() error
	Close() error
}

var _ sshKeepaliveConn = (*ssh.Client)(nil)

// startSSHKeepalive probes conn every cfg.Interval until the connection ends.
// Any reply counts as alive — OpenSSH (and Windows OpenSSH) answer
// keepalive@openssh.com with a failure reply, which is still proof of life.
// A probe that errors or gets no reply within the interval is a miss; after
// cfg.Max consecutive misses the connection is closed, which makes every
// session on it fail its Wait.
func startSSHKeepalive(conn sshKeepaliveConn, cfg sshKeepaliveConfig, label string) {
	if conn == nil || cfg.Interval <= 0 {
		return
	}
	if cfg.Max <= 0 {
		cfg.Max = defaultSSHKeepaliveMax
	}
	closed := make(chan struct{})
	go func() {
		_ = conn.Wait()
		close(closed)
	}()
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		misses := 0
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
			}
			replied := make(chan error, 1)
			go func() {
				_, _, err := conn.SendRequest(sshKeepaliveRequest, true, nil)
				replied <- err
			}()
			var err error
			select {
			case <-closed:
				return
			case err = <-replied:
			case <-time.After(cfg.Interval):
				err = errSSHKeepaliveTimeout
			}
			if err == nil {
				misses = 0
				continue
			}
			misses++
			log.Printf("[ssh-keepalive] %s: probe %d/%d failed: %v", label, misses, cfg.Max, err)
			if misses >= cfg.Max {
				log.Printf("[ssh-keepalive] %s: no answer to %d keepalives — closing the SSH connection", label, misses)
				_ = conn.Close()
				return
			}
		}
	}()
}

var errSSHKeepaliveTimeout = errors.New("no reply")
