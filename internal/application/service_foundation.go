package application

import (
	"context"
	"encoding/json"

	"openpoet/internal/secretvalue"
)

// R4Boundary names the shared action authorization contract at call sites that
// can expose credentials or install/execute arbitrary commands.
type R4Boundary = ActionAuthorization

func requireR4(boundary R4Boundary) error {
	return requireExplicitActionApproval(boundary)
}

type SecretCodec = secretvalue.Encryptor

func encryptEnvelope(codec SecretCodec, plaintext string) (string, error) {
	if codec == nil {
		return "", validationError("secret_codec_unavailable", "Secret encryption is unavailable")
	}
	return secretvalue.Encrypt(codec, plaintext)
}

func isEncryptedEnvelope(value string) bool {
	return secretvalue.IsEncrypted(value)
}

func needsEnvelopeEncryption(value string) (bool, error) {
	needsEncryption, err := secretvalue.NeedsEncryption(value)
	if err != nil {
		return false, validationError("secret_envelope_invalid", "Stored secret envelope is invalid")
	}
	return needsEncryption, nil
}

type ApplicationChange struct {
	Domain string
	Action string
	ID     int64
	Meta   map[string]any
}

type ApplicationEffects interface {
	PublishApplicationChange(context.Context, ApplicationChange)
}

func publishApplicationChange(ctx context.Context, effects ApplicationEffects, change ApplicationChange) {
	if effects != nil {
		effects.PublishApplicationChange(ctx, change)
	}
}

func validateJSONObject(value, code, message string) error {
	var object map[string]any
	if json.Unmarshal([]byte(value), &object) != nil {
		return validationError(code, message)
	}
	return nil
}

func validateJSONArray(value, code, message string) error {
	var array []any
	if json.Unmarshal([]byte(value), &array) != nil {
		return validationError(code, message)
	}
	return nil
}
