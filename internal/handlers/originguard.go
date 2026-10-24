package handlers

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"openpoet/internal/tunnel"
)

// originGuardedPrefixes are the machine-facing transports a foreign web page
// must never drive: the REST API, the MCP endpoint, the WebSocket streams and
// the OTLP ingestion. The automation API keeps its own stricter policy (it
// rejects any Origin outright) and is not handled here.
var originGuardedPrefixes = []string{"/api/", "/mcp", "/ws/", "/v1/"}

// HostOriginGuard protects a localhost tool from the browser it runs next to.
//
//   - Host allowlist: a request whose Host header is not this machine (an IP
//     literal, localhost, the OS hostname, or an entry in extraHosts) is
//     refused. That is what defeats DNS rebinding, where a hostile domain is
//     re-pointed at 127.0.0.1 so the browser treats OpenPoet as same-origin.
//   - Origin check: a request that carries an Origin header must come from the
//     same host:port it is addressed to. Browsers always send Origin on
//     cross-origin fetches, form posts and WebSocket handshakes, so this blocks
//     a foreign page from driving the API, /mcp or /ws even with no-cors or
//     text/plain tricks that skip a CORS preflight.
//
// Tunnel-forwarded requests are skipped: they arrive from the local tunnel
// client and tunnel.AuthMiddleware (registered after this guard) requires a
// paired-device credential on every non-public path. A browser cannot forge
// the tunnel header cross-origin without a preflight, which OpenPoet no longer
// answers. extraHosts may contain "*" to disable the Host allowlist.
func HostOriginGuard(extraHosts []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true}
	if name, err := os.Hostname(); err == nil && name != "" {
		name = strings.ToLower(name)
		allowed[name] = true
		allowed[name+".local"] = true
	}
	anyHost := false
	for _, h := range extraHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "*" {
			anyHost = true
		} else if h != "" {
			allowed[h] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(tunnel.TunnelHeader) == "true" || strings.HasPrefix(r.URL.Path, "/api/automation/") {
				next.ServeHTTP(w, r)
				return
			}
			if !anyHost && !hostAllowed(r.Host, allowed) {
				writeGuardError(w, "host not allowed: add it to OPENPOET_ALLOWED_HOSTS to reach OpenPoet by this name")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && originGuarded(r) && !sameOrigin(origin, r.Host) {
				writeGuardError(w, "cross-origin requests are not allowed")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func originGuarded(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return true
	}
	for _, p := range originGuardedPrefixes {
		if strings.HasPrefix(r.URL.Path, p) {
			return true
		}
	}
	return false
}

func hostAllowed(hostport string, allowed map[string]bool) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || strings.HasSuffix(host, ".localhost") {
		return true
	}
	return allowed[host]
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // includes the opaque "null" origin
	}
	return strings.EqualFold(u.Host, host)
}

func writeGuardError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requestFromLoopback reports whether the TCP peer is this machine. It reads
// r.RemoteAddr, which is trustworthy because the router does not rewrite it
// from client-supplied forwarding headers.
func requestFromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
