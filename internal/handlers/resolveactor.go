package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessiontoken"
	"openpoet/internal/tunnel"
)

// resolvedActorInfo is the verified principal behind a REST request. approved
// marks owner-tier callers (UI cookie, paired device, automation bearer) whose
// direct action authorizes destructive/env-gated operations; session-tier
// actors are NOT approved and must obtain a grant for destructive verbs.
type resolvedActorInfo struct {
	actor    application.Actor
	approved bool
}

type resolvedActorKeyType struct{}

var resolvedActorKey resolvedActorKeyType

func withResolvedActor(ctx context.Context, info resolvedActorInfo) context.Context {
	return context.WithValue(ctx, resolvedActorKey, info)
}

func resolvedActorFromContext(ctx context.Context) (resolvedActorInfo, bool) {
	info, ok := ctx.Value(resolvedActorKey).(resolvedActorInfo)
	return info, ok
}

// resolveActorMutatingMethod reports whether a method mutates state and must
// therefore carry a verified credential.
func resolveActorMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// resolveActorExemptPath lists /api paths that carry their own authentication
// (or are intentionally open) and must not be gated by the REST actor resolver.
func resolveActorExemptPath(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return true // only /api mutations are gated here
	}
	exemptPrefixes := []string{
		"/api/automation/", // own bearer auth + loopback/origin policy
		"/api/hooks/",      // own per-session hook token
		"/api/test/",       // test-mode only, env-gated at registration
		"/api/debug/",      // debug-only telemetry
	}
	for _, p := range exemptPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	switch path {
	case "/api/version", "/api/client-log":
		return true
	}
	return false
}

// ResolveActorMiddleware authenticates mutating REST requests against one of
// four credentials — a per-session opst1_ bearer, an opav1_ automation bearer,
// the per-install UI cookie, or a paired-device session cookie (the tunnel
// principal) — and threads the verified actor into the request context so the
// audit trail records who really acted. Reads stay open on loopback; mutations
// without any credential are rejected 401.
func ResolveActorMiddleware(db *database.DB, jwtSecret []byte, uiCookieSecret string) func(http.Handler) http.Handler {
	uiSecretDigest := sha256.Sum256([]byte(uiCookieSecret))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if info, ok := resolveRESTActor(db, jwtSecret, uiSecretDigest[:], uiCookieSecret, r); ok {
				next.ServeHTTP(w, r.WithContext(withResolvedActor(r.Context(), info)))
				return
			}
			if !resolveActorMutatingMethod(r.Method) || resolveActorExemptPath(r.URL.Path) {
				next.ServeHTTP(w, r) // reads and exempt paths pass through unresolved
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "an authenticated actor is required for this request",
			})
		})
	}
}

// resolveRESTActor attempts to identify the caller from any of the four
// credentials. It returns ok=false when no credential is present (the caller
// then decides whether the route requires one).
func resolveRESTActor(db *database.DB, jwtSecret, uiSecretDigest []byte, uiCookieSecret string, r *http.Request) (resolvedActorInfo, bool) {
	if bearer, ok := bearerToken(r.Header.Get("Authorization")); ok {
		if sessiontoken.IsMCPToken(bearer) {
			if sessionID, ok := sessiontoken.SessionIDFromMCPToken(bearer); ok {
				mcpHash, _, err := db.GetSessionTokenHashes(r.Context(), sessionID)
				if err == nil && sessiontoken.EqualHash(bearer, mcpHash) {
					return resolvedActorInfo{
						actor:    application.Actor{Type: "session", ID: sessionID},
						approved: false,
					}, true
				}
			}
			return resolvedActorInfo{}, false // opst1 presented but invalid
		}
		if strings.HasPrefix(bearer, "opav1_") {
			if client, ok := verifyAutomationBearer(db, bearer, r); ok {
				return resolvedActorInfo{
					actor:    application.Actor{Type: "automation_client", ID: client.ID},
					approved: true,
				}, true
			}
			return resolvedActorInfo{}, false
		}
		return resolvedActorInfo{}, false // unknown bearer scheme
	}
	if cookie, err := r.Cookie(uiCookieName); err == nil && cookie.Value != "" && uiCookieSecret != "" {
		presented := sha256.Sum256([]byte(cookie.Value))
		if subtle.ConstantTimeCompare(presented[:], uiSecretDigest) == 1 {
			return resolvedActorInfo{
				actor:    application.Actor{Type: "user", ID: platformUIActorID},
				approved: true,
			}, true
		}
	}
	if deviceID, ok := tunnel.AuthenticatedDevice(db, jwtSecret, r); ok {
		return resolvedActorInfo{
			actor:    application.Actor{Type: "user", ID: "device:" + deviceID},
			approved: true,
		}, true
	}
	return resolvedActorInfo{}, false
}

// verifyAutomationBearer validates an opav1_ token against automation_clients
// using the same prefix-lookup + constant-time digest compare as the automation
// plane, so the REST resolver honors automation identity without a second auth
// path drifting out of sync.
func verifyAutomationBearer(db *database.DB, token string, r *http.Request) (*database.AutomationClient, bool) {
	rest, ok := strings.CutPrefix(token, "opav1_")
	if !ok {
		return nil, false
	}
	prefix, secret, ok := strings.Cut(rest, ".")
	if !ok || prefix == "" || secret == "" {
		return nil, false
	}
	client, err := db.GetAutomationClientByTokenPrefix(r.Context(), prefix)
	if err != nil || client == nil || !client.Enabled {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	if len(client.TokenHash) != sha256.Size || subtle.ConstantTimeCompare(sum[:], client.TokenHash) != 1 {
		return nil, false
	}
	return client, true
}

// bearerToken extracts a well-formed Bearer token from an Authorization header.
func bearerToken(header string) (string, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" || strings.Contains(token, " ") {
		return "", false
	}
	return token, true
}
