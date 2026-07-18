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

// EnsureUICookieMiddleware plants the UI credential cookie on page/asset loads
// that don't already carry it, so any browser that has fetched an OpenPoet page
// can subsequently mutate. It never touches automation or hook endpoints.
func EnsureUICookieMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret != "" && r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/automation/") {
				if cookie, err := r.Cookie(uiCookieName); err != nil || cookie.Value != secret {
					SetUICookie(w, secret)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
