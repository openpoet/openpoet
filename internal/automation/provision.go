package automation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"openpoet/internal/database"
)

const tokenScheme = "opav1"

type ClientProvisioner interface {
	CreateAutomationClient(ctx context.Context, client *database.AutomationClient) error
}

type ProvisionedClient struct {
	Client *database.AutomationClient
	Token  string
}

// ProvisionClient creates a random bearer token and persists only its SHA-256
// digest. The plaintext token is returned once to the trusted caller.
func ProvisionClient(
	ctx context.Context,
	store ClientProvisioner,
	name string,
	scopes []Scope,
	random io.Reader,
) (*ProvisionedClient, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("automation client name is required")
	}
	if store == nil {
		return nil, errors.New("automation client store is required")
	}
	if random == nil {
		random = rand.Reader
	}

	scopeSet, err := NewScopeSet(scopes...)
	if err != nil {
		return nil, err
	}
	scopesJSON, err := scopeSet.MarshalJSON()
	if err != nil {
		return nil, err
	}

	idBytes, err := randomBytes(random, 16)
	if err != nil {
		return nil, err
	}
	prefixBytes, err := randomBytes(random, 9)
	if err != nil {
		return nil, err
	}
	secretBytes, err := randomBytes(random, 32)
	if err != nil {
		return nil, err
	}

	prefix := base64.RawURLEncoding.EncodeToString(prefixBytes)
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	token := tokenScheme + "_" + prefix + "." + secret
	tokenHash := sha256.Sum256([]byte(token))

	client := &database.AutomationClient{
		ID:          hex.EncodeToString(idBytes),
		Name:        name,
		TokenPrefix: prefix,
		TokenHash:   tokenHash[:],
		Scopes:      string(scopesJSON),
		Enabled:     true,
	}
	if err := store.CreateAutomationClient(ctx, client); err != nil {
		return nil, err
	}
	return &ProvisionedClient{Client: client, Token: token}, nil
}

func randomBytes(reader io.Reader, size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(reader, value); err != nil {
		return nil, err
	}
	return value, nil
}

func parseTokenPrefix(token string) (string, bool) {
	schemeAndValue := strings.SplitN(token, "_", 2)
	if len(schemeAndValue) != 2 || schemeAndValue[0] != tokenScheme {
		return "", false
	}
	parts := strings.SplitN(schemeAndValue[1], ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return "", false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(secret) != 32 {
		return "", false
	}
	return parts[0], true
}
