package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestSSHServer runs an in-process SSH server. Every "exec" request is
// handed to onExec with the raw TCP connection, so a test can end the command
// with an exit status or cut the connection under it.
func startTestSSHServer(t *testing.T, onExec func(ch ssh.Channel, tcp net.Conn)) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			tcp, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(tcp, cfg)
				if err != nil {
					tcp.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, chReqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range chReqs {
							_ = req.Reply(req.Type == "exec", nil)
							if req.Type == "exec" {
								go onExec(ch, tcp)
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func runOverTestSSH(t *testing.T, addr string) error {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: "dev", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("codex app-server"); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	select {
	case err := <-waited:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("session.Wait never returned")
		return nil
	}
}

// The real ssh errors behind a remote codex death map to an unexpected-exit
// error naming the cause: an exit status, or the connection dropping.
func TestRemoteCodexExitErrorsFromRealSSH(t *testing.T) {
	t.Run("non-zero exit status", func(t *testing.T) {
		addr := startTestSSHServer(t, func(ch ssh.Channel, _ net.Conn) {
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, 7)
			_, _ = ch.SendRequest("exit-status", false, status)
			_ = ch.Close()
		})
		err := codexExitError("remote codex app-server", runOverTestSSH(t, addr), "")
		if !strings.Contains(err.Error(), "status 7") {
			t.Fatalf("got %v, want the exit status", err)
		}
	})
	t.Run("connection dropped", func(t *testing.T) {
		addr := startTestSSHServer(t, func(_ ssh.Channel, tcp net.Conn) {
			_ = tcp.Close()
		})
		waitErr := runOverTestSSH(t, addr)
		err := codexExitError("remote codex app-server", waitErr, "")
		if !strings.Contains(err.Error(), "connection lost") {
			t.Fatalf("Wait() = %v -> %v, want a connection-lost error", waitErr, err)
		}
	})
}

// A real SSH server answers keepalive@openssh.com with a failure reply (it
// does not know the request); that must count as alive, not as a miss.
func TestSSHKeepaliveAgainstRealServer(t *testing.T) {
	addr := startTestSSHServer(t, func(ssh.Channel, net.Conn) {})
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: "dev", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	startSSHKeepalive(client, sshKeepaliveConfig{Interval: 10 * time.Millisecond, Max: 1}, "test")
	time.Sleep(200 * time.Millisecond)
	if _, err := client.NewSession(); err != nil {
		t.Fatalf("keepalive closed a live connection: %v", err)
	}
}
