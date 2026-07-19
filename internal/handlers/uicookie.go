package handlers

import (
	"net/http"
	"strings"
)

// uiCookieName is the per-install UI credential set at page load. Its value is
// a server-side secret; presenting it proves the caller loaded an OpenPoet page
// from this install (a same-origin browser), which authorizes UI mutations.
const uiCookieName = "openpoet_ui"

// SetUICookie writes the per-install UI credential cookie. HttpOnly keeps it out
// of JS; SameSite=Strict blocks cross-site (CSRF) submission; no Secure flag so
// it works over plain-HTTP localhost and the HTTPS relay alike.
func SetUICookie(w http.ResponseWriter, secret string) {
	if secret == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     uiCookieName,
		Value:    secret,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// uiCookiePlantExcludedPrefixes are paths that must NOT hand out the UI secret:
// API/MCP/WS transports (not page loads) and the pre-auth, tunnel-exempt paths
// (/pair, /static, /sw.js, /manifest.json, /favicon). Serving the secret on a
// pre-auth path would let an unpaired remote client over the tunnel obtain
// owner-tier credentials without pairing.
var uiCookiePlantExcludedPrefixes = []string{"/api/", "/mcp", "/ws/", "/pair", "/static/", "/favicon"}

func uiCookiePlantExcluded(path string) bool {
	switch path {
	case "/sw.js", "/manifest.json":
		return true
	}
	for _, p := range uiCookiePlantExcludedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// EnsureUICookieMiddleware plants the UI credential cookie only on genuine HTML
// page navigations (Accept: text/html) to non-exempt paths, so a browser that
// loads an OpenPoet page can subsequently mutate. It deliberately does NOT plant
// on API/asset/pairing requests: those either aren't page loads or are reachable
// pre-authentication, and the secret must not leak there. A local same-uid
// process can still request a page to obtain the secret — an accepted limitation
// of a cookie credential on a single-user host (full REST-side authorization is
// deferred to the Phase 3 capability-pipeline unification).
func EnsureUICookieMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Remote leak is closed structurally: uiCookiePlantExcluded skips the
			// tunnel-exempt paths (/static, /sw.js, /manifest.json, /pair,
			// /favicon), and tunnel.AuthMiddleware (registered earlier) blocks
			// unpaired tunnel clients on every other path before they reach here —
			// so only a same-origin local browser or an already-paired device
			// (both owner-equivalent) ever receives the cookie on a page load.
			if secret != "" && r.Method == http.MethodGet &&
				strings.Contains(r.Header.Get("Accept"), "text/html") &&
				r.Header.Get("Authorization") == "" && // never hand it to a bearer-authenticated (session/automation) caller
				!uiCookiePlantExcluded(r.URL.Path) {
				if cookie, err := r.Cookie(uiCookieName); err != nil || cookie.Value != secret {
					SetUICookie(w, secret)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
