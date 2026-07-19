// Package sshauth holds the shared SSH auth-method fallbacks used by the three
// SSH surfaces (session runner, file manager, config syncer) until they are
// consolidated by the Phase 7.4 SSH hardening.
package sshauth

import (
	"os"

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
