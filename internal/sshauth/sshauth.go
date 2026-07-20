// Package sshauth holds the shared SSH auth-method fallbacks used by the three
// SSH surfaces (session runner, file manager, config syncer) until they are
// consolidated by the Phase 7.4 SSH hardening.
package sshauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

// DefaultKeyAuthMethods loads the user's standard private keys
// (~/.ssh/id_rsa|id_ed25519|id_ecdsa). Used when a remote project declares
// key auth but stores no pasted credential — the standard ssh behavior of
// "use my own keys" instead of a hard failure.
func DefaultKeyAuthMethods() []ssh.AuthMethod {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var methods []ssh.AuthMethod
	for _, keyPath := range []string{
		homeDir + "/.ssh/id_rsa",
		homeDir + "/.ssh/id_ed25519",
		homeDir + "/.ssh/id_ecdsa",
	} {
		keyData, err := os.ReadFile(keyPath)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
		break
	}
	return methods
}

// KnownHostStore is the TOFU ledger (implemented by *database.DB, V72).
type KnownHostStore interface {
	GetSSHKnownHost(ctx context.Context, host string, port int, keyType string) (string, error)
	RecordSSHKnownHost(ctx context.Context, host string, port int, keyType, fingerprint string) error
}

var (
	storeMu sync.RWMutex
	store   KnownHostStore
)

// SetKnownHostStore wires the process-wide TOFU ledger (called once at boot).
// Without a store, HostKeyCallback records nothing and accepts (pre-7.4
// behavior) — tests and offline tools keep working.
func SetKnownHostStore(s KnownHostStore) {
	storeMu.Lock()
	store = s
	storeMu.Unlock()
}

func knownHostStore() KnownHostStore {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return store
}

// ErrHostKeyMismatch is the fail-closed verdict for a changed host key.
var ErrHostKeyMismatch = errors.New("ssh host key mismatch — the remote host's key changed since first contact (possible MITM); if the change is expected, remove the ssh_known_hosts row")

// HostKeyCallback returns the TOFU callback shared by every SSH surface:
// first contact records the key's SHA-256 fingerprint; later contacts must
// match it exactly or the connection fails CLOSED.
func HostKeyCallback() ssh.HostKeyCallback {
	return func(hostport string, remote net.Addr, key ssh.PublicKey) error {
		ledger := knownHostStore()
		if ledger == nil {
			return nil // no ledger wired (tests/offline): accept, record nothing
		}
		host, portStr, err := net.SplitHostPort(hostport)
		if err != nil {
			host, portStr = hostport, "22"
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			port = 22
		}
		fingerprint := ssh.FingerprintSHA256(key)
		// NO cancelable/timeout context here: a cancel mid-write on the single
		// write connection is the documented poisoning class. Ledger ops are
		// local SQLite and fast; hanging is not the realistic failure mode.
		ctx := context.Background()
		recorded, err := ledger.GetSSHKnownHost(ctx, host, port, key.Type())
		if err != nil {
			return fmt.Errorf("ssh known-host ledger read failed: %w", err)
		}
		if recorded == "" {
			if err := ledger.RecordSSHKnownHost(ctx, host, port, key.Type(), fingerprint); err != nil {
				return fmt.Errorf("ssh known-host ledger write failed: %w", err)
			}
			// Two concurrent FIRST contacts can race (INSERT is DO NOTHING):
			// re-read and require OUR key to be the recorded one, so a diverging
			// simultaneous first contact fails closed instead of slipping by.
			recorded, err = ledger.GetSSHKnownHost(ctx, host, port, key.Type())
			if err != nil {
				return fmt.Errorf("ssh known-host ledger read failed: %w", err)
			}
		}
		if recorded != fingerprint {
			return fmt.Errorf("%w (host %s:%d, key %s)", ErrHostKeyMismatch, host, port, key.Type())
		}
		return nil
	}
}
