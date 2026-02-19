package tunnel

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"devmanager/internal/database"

	"github.com/golang-jwt/jwt/v5"
)

// DeviceClaims represents the JWT claims for a paired device session.
type DeviceClaims struct {
	DeviceID string `json:"device_id"`
	jwt.RegisteredClaims
}

const (
	sessionCookieName = "devmanager_session"
	sessionMaxAge     = 30 * 24 * time.Hour // 30 days
)

// AuthMiddleware returns chi-compatible middleware that authenticates tunnel requests.
// Local requests (without X-Tunnel-Request header) bypass auth entirely.
func AuthMiddleware(db *database.DB, jwtSecret []byte) func(http.Handler) http.Handler {
	// Debounce last_seen updates per device (at most once per minute)
	lastSeenCache := &sync.Map{}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only authenticate tunnel-originated requests
			if r.Header.Get(TunnelHeader) != "true" {
				next.ServeHTTP(w, r)
				return
			}

			// Remove the tunnel header so app code doesn't see it
			r.Header.Del(TunnelHeader)

			// Exempt paths that must be accessible without auth
			path := r.URL.Path
			if isExemptPath(path) {
				next.ServeHTTP(w, r)
				return
			}

			// Check session cookie
			cookie, err := r.Cookie(sessionCookieName)
			if err != nil || cookie.Value == "" {
				redirectToPairing(w, r)
				return
			}

			// Validate JWT
			claims := &DeviceClaims{}
			token, err := jwt.ParseWithClaims(cookie.Value, claims, func(token *jwt.Token) (interface{}, error) {
				return jwtSecret, nil
			})
			if err != nil || !token.Valid {
				clearSessionCookie(w)
				redirectToPairing(w, r)
				return
			}

			// Check device is not revoked
			device, err := db.GetPairedDevice(r.Context(), claims.DeviceID)
			if err != nil || device.Revoked {
				clearSessionCookie(w)
				redirectToPairing(w, r)
				return
			}

			// Debounced last_seen update (at most once per minute per device)
			if _, loaded := lastSeenCache.LoadOrStore(claims.DeviceID, time.Now()); !loaded {
				go func() {
					db.UpdatePairedDeviceLastSeen(r.Context(), claims.DeviceID)
					time.AfterFunc(time.Minute, func() {
						lastSeenCache.Delete(claims.DeviceID)
					})
				}()
			}

			next.ServeHTTP(w, r)
		})
	}
}

// CreateSessionToken creates a JWT session token for a paired device.
func CreateSessionToken(deviceID string, jwtSecret []byte) (string, error) {
	claims := &DeviceClaims{
		DeviceID: deviceID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionMaxAge)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// SetSessionCookie sets the authenticated session cookie on the response.
func SetSessionCookie(w http.ResponseWriter, tokenStr string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tokenStr,
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Not setting Secure: the cookie must work on both HTTP (localhost)
		// and HTTPS (production relay). Transport security is provided by
		// the tunnel's WebSocket connection, not the cookie flag.
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
}

func isExemptPath(path string) bool {
	exemptPrefixes := []string{"/pair", "/static/", "/favicon"}
	exemptExact := []string{"/sw.js", "/manifest.json"}

	for _, p := range exemptExact {
		if path == p {
			return true
		}
	}
	for _, p := range exemptPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func redirectToPairing(w http.ResponseWriter, r *http.Request) {
	// For API requests, return 401 JSON
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"authentication required","redirect":"/pair"}`))
		return
	}

	// For page requests, redirect to pairing page
	http.Redirect(w, r, "/pair", http.StatusTemporaryRedirect)
	log.Printf("[TUNNEL-AUTH] Unauthenticated request redirected to /pair: %s", r.URL.Path)
}
