// Package sessiontoken mints and verifies the per-session credentials that tie
// a hook bridge and MCP/CLI caller cryptographically to the session that owns
// them. Two token families:
//
//   - MCP/REST bearer:  opst1_<sessionID>.<secret>  (carries the session id so
//     the server can resolve the actor from the token alone)
//   - Hook bridge token: opht1_<secret>             (opaque; the bridge always
//     sends X-Session-ID alongside, so the id need not be embedded)
//
// Only the SHA-256 hex digest of each token is ever stored (sessions.mcp_token_hash
// / hook_token_hash). Verification is a constant-time compare of the digest.
package sessiontoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	// MCPScheme prefixes the MCP/REST session bearer.
	MCPScheme = "opst1"
	// HookScheme prefixes the hook bridge token.
	HookScheme = "opht1"
)

func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashHex returns the SHA-256 hex digest of a token string.
func HashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewMCPToken mints an opst1_ bearer bound to sessionID and returns the token
// plus the hex digest to persist in sessions.mcp_token_hash.
func NewMCPToken(sessionID string) (token, hashHex string, err error) {
	secret, err := randomSecret()
	if err != nil {
		return "", "", err
	}
	token = MCPScheme + "_" + sessionID + "." + secret
	return token, HashHex(token), nil
}

// NewHookToken mints an opaque opht1_ token and returns it plus the hex digest
// to persist in sessions.hook_token_hash.
func NewHookToken() (token, hashHex string, err error) {
	secret, err := randomSecret()
	if err != nil {
		return "", "", err
	}
	token = HookScheme + "_" + secret
	return token, HashHex(token), nil
}

// IsMCPToken reports whether a bearer looks like an opst1_ session token.
func IsMCPToken(token string) bool {
	return strings.HasPrefix(token, MCPScheme+"_")
}

// SessionIDFromMCPToken extracts the session id embedded in an opst1_ token.
// It does NOT verify the token — the caller must still compare the digest.
func SessionIDFromMCPToken(token string) (sessionID string, ok bool) {
	rest, found := strings.CutPrefix(token, MCPScheme+"_")
	if !found {
		return "", false
	}
	id, secret, found := strings.Cut(rest, ".")
	if !found || id == "" || secret == "" {
		return "", false
	}
	return id, true
}

// EqualHash constant-time compares a presented token's digest against a stored
// hex digest. An empty storedHex means no credential is set (returns false).
func EqualHash(token, storedHex string) bool {
	if storedHex == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashHex(token)), []byte(storedHex)) == 1
}
