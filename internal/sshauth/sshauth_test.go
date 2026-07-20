package sshauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"
)

type fakeLedger struct {
	records map[string]string
}

func (f *fakeLedger) key(host string, port int, keyType string) string {
	return host + ":" + keyType
}

func (f *fakeLedger) GetSSHKnownHost(_ context.Context, host string, port int, keyType string) (string, error) {
	return f.records[f.key(host, port, keyType)], nil
}

func (f *fakeLedger) RecordSSHKnownHost(_ context.Context, host string, port int, keyType, fingerprint string) error {
	f.records[f.key(host, port, keyType)] = fingerprint
	return nil
}

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestTOFUHostKeyMismatchFailsClosed: first contact records, same key passes,
// a CHANGED key fails closed with the typed mismatch error.
func TestTOFUHostKeyMismatchFailsClosed(t *testing.T) {
	ledger := &fakeLedger{records: map[string]string{}}
	SetKnownHostStore(ledger)
	t.Cleanup(func() { SetKnownHostStore(nil) })

	callback := HostKeyCallback()
	keyA := testPublicKey(t)

	if err := callback("example.com:22", nil, keyA); err != nil {
		t.Fatalf("first contact must record and accept: %v", err)
	}
	if ledger.records["example.com:"+keyA.Type()] == "" {
		t.Fatal("first contact did not record the fingerprint")
	}
	if err := callback("example.com:22", nil, keyA); err != nil {
		t.Fatalf("same key must keep passing: %v", err)
	}

	keyB := testPublicKey(t)
	err := callback("example.com:22", nil, keyB)
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("changed key must fail closed with ErrHostKeyMismatch, got %v", err)
	}

	// Without a ledger the callback accepts (pre-7.4 behavior for tests/tools).
	SetKnownHostStore(nil)
	if err := callback("example.com:22", nil, keyB); err != nil {
		t.Fatalf("ledger-less callback must accept: %v", err)
	}
}
