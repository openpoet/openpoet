// Package secretvalue owns the storage envelope used for encrypted runtime
// configuration. It intentionally has no dependency on database, application,
// session, or handlers so each layer can resolve the same format without an
// import cycle.
package secretvalue

import (
	"encoding/json"
	"errors"
	"fmt"
)

const currentEnvelopeVersion = 1

var (
	ErrEncryptorUnavailable = errors.New("secret envelope encryptor is unavailable")
	ErrDecryptorUnavailable = errors.New("secret envelope decryptor is unavailable")
	ErrInvalidEnvelope      = errors.New("secret envelope is invalid")
)

type Encryptor interface {
	Encrypt(string) (ciphertext string, iv string, err error)
}

type DecryptFunc func(ciphertext string, iv string) (string, error)

type envelope struct {
	Version    int    `json:"version"`
	Ciphertext string `json:"ciphertext"`
	IV         string `json:"iv"`
}

// Encrypt returns the stable JSON envelope stored in legacy text columns.
func Encrypt(encryptor Encryptor, plaintext string) (string, error) {
	if encryptor == nil {
		return "", ErrEncryptorUnavailable
	}
	ciphertext, iv, err := encryptor.Encrypt(plaintext)
	if err != nil {
		return "", fmt.Errorf("encrypt secret envelope: %w", err)
	}
	if ciphertext == "" || iv == "" {
		return "", ErrInvalidEnvelope
	}
	encoded, err := json.Marshal(envelope{
		Version: currentEnvelopeVersion, Ciphertext: ciphertext, IV: iv,
	})
	if err != nil {
		return "", fmt.Errorf("encode secret envelope: %w", err)
	}
	return string(encoded), nil
}

// Resolve decrypts a valid envelope and passes legacy plaintext through
// unchanged. JSON objects using any reserved envelope field fail closed when
// malformed so a damaged envelope can never become a shell command or config.
func Resolve(value string, decrypt DecryptFunc) (string, error) {
	parsed, state := inspect(value)
	switch state {
	case envelopeLegacy:
		return value, nil
	case envelopeInvalid:
		return "", ErrInvalidEnvelope
	case envelopeValid:
		if decrypt == nil {
			return "", ErrDecryptorUnavailable
		}
		plaintext, err := decrypt(parsed.Ciphertext, parsed.IV)
		if err != nil {
			return "", fmt.Errorf("decrypt secret envelope: %w", err)
		}
		return plaintext, nil
	default:
		return "", ErrInvalidEnvelope
	}
}

// NeedsEncryption distinguishes plaintext legacy values from valid envelopes.
// Malformed envelope-like values return an error instead of being encrypted as
// plaintext, which keeps migrations fail-closed and retryable.
func NeedsEncryption(value string) (bool, error) {
	_, state := inspect(value)
	switch state {
	case envelopeLegacy:
		return true, nil
	case envelopeValid:
		return false, nil
	default:
		return false, ErrInvalidEnvelope
	}
}

func IsEncrypted(value string) bool {
	_, state := inspect(value)
	return state == envelopeValid
}

type envelopeState uint8

const (
	envelopeLegacy envelopeState = iota
	envelopeValid
	envelopeInvalid
)

func inspect(value string) (envelope, envelopeState) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &fields); err != nil || fields == nil {
		return envelope{}, envelopeLegacy
	}
	_, hasCiphertext := fields["ciphertext"]
	_, hasIV := fields["iv"]
	// "version" is a plausible legacy environment variable. Ciphertext or IV
	// are the reserved markers that make the object envelope-like.
	if !hasCiphertext && !hasIV {
		return envelope{}, envelopeLegacy
	}
	var parsed envelope
	if err := json.Unmarshal([]byte(value), &parsed); err != nil ||
		parsed.Version != currentEnvelopeVersion || parsed.Ciphertext == "" || parsed.IV == "" {
		return envelope{}, envelopeInvalid
	}
	return parsed, envelopeValid
}
