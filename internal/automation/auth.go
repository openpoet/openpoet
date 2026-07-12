package automation

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strings"

	"openpoet/internal/database"
)

type ClientAuthenticator interface {
	GetAutomationClientByTokenPrefix(ctx context.Context, prefix string) (*database.AutomationClient, error)
}

type peerAddressContextKey struct{}

type peerMetadata struct {
	address       string
	tunnelRequest bool
}

// CapturePeerAddress must run before middleware.RealIP. It preserves the TCP
// peer used by the loopback policy instead of trusting forwarded headers.
func CapturePeerAddress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := peerMetadata{
			address:       r.RemoteAddr,
			tunnelRequest: r.Header.Get("X-Tunnel-Request") == "true",
		}
		ctx := context.WithValue(r.Context(), peerAddressContextKey{}, peer)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type Authenticator struct {
	store ClientAuthenticator
}

func NewAuthenticator(store ClientAuthenticator) *Authenticator {
	return &Authenticator{store: store}
}

func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "browser_origin_forbidden", "browser-originated automation requests are not allowed", false)
			return
		}
		if !requestCameFromLoopback(r) {
			writeError(w, http.StatusForbidden, "loopback_required", "automation API is restricted to the local machine", false)
			return
		}
		if a == nil || a.store == nil {
			writeError(w, http.StatusServiceUnavailable, "automation_auth_unavailable", "automation authentication is unavailable", true)
			return
		}

		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="openpoet-automation"`)
			writeError(w, http.StatusUnauthorized, "authentication_required", "a valid automation bearer token is required", false)
			return
		}
		prefix, ok := parseTokenPrefix(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid_token", "the automation bearer token is invalid", false)
			return
		}

		client, err := a.store.GetAutomationClientByTokenPrefix(r.Context(), prefix)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusUnauthorized, "invalid_token", "the automation bearer token is invalid", false)
				return
			}
			writeError(w, http.StatusServiceUnavailable, "automation_auth_unavailable", "automation authentication is unavailable", true)
			return
		}
		hash := sha256.Sum256([]byte(token))
		if !client.Enabled || len(client.TokenHash) != sha256.Size ||
			subtle.ConstantTimeCompare(hash[:], client.TokenHash) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_token", "the automation bearer token is invalid", false)
			return
		}

		scopes, err := ParseScopeSet(client.Scopes)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "automation_auth_invalid", "automation client configuration is invalid", false)
			return
		}
		actor := Actor{
			Type:     "automation_client",
			ID:       client.ID,
			Name:     client.Name,
			Scopes:   scopes,
			ClientID: client.ID,
		}
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
	})
}

func RequireScopes(scopes ...Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
				return
			}
			if !actor.Scopes.HasAll(scopes...) {
				writeError(w, http.StatusForbidden, "insufficient_scope", "the automation client lacks a required scope", false)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(header string) (string, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.Contains(token, " ") {
		return "", false
	}
	return token, true
}

func requestCameFromLoopback(r *http.Request) bool {
	peer, _ := r.Context().Value(peerAddressContextKey{}).(peerMetadata)
	if peer.tunnelRequest {
		return false
	}
	address := peer.address
	if address == "" {
		address = r.RemoteAddr
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
