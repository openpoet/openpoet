// Package devboxpublish lets an agent running in a devbox container publish a
// port of its own container as an HTTPS URL reachable only from the private
// network. OpenPoet runs on the container host and does the work with its own
// identity:
//
//  1. a loopback TCP relay on the host (devbox-forward) from a host port taken
//     from a pool to the container port;
//  2. one route file per app for the reverse proxy in front of the relays
//     (Caddy, routing by Host), followed by a graceful reload through the
//     proxy's admin socket, which exists only inside the proxy container.
//
// TLS, DNS and the edge route to the proxy are outside this package: they are
// static (a wildcard), so publishing needs no access to the edge server.
//
// The agent never names the container: it is derived from the calling
// session's project (its SSH host and port). Every value that identifies the
// installation (domain, host suffix, containers, ports, paths) comes from the
// environment; with OPENPOET_DEVBOX_PUBLISH unset the feature is off.
package devboxpublish

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the installation-specific configuration of the feature.
type Config struct {
	Enabled bool
	// HostTemplate is the published host name with {app} where the app name
	// goes, e.g. "{app}-dev9.example.com" or "{app}.dev9.example.com".
	HostTemplate string
	// SSHHosts are the SSH host names/addresses that mean "a container on this
	// machine" in a project's SSH settings.
	SSHHosts []string
	// Prefixes maps each container allowed to publish to the prefix its app
	// names must carry (<prefix> or <prefix>-<rest>).
	Prefixes map[string]string
	// Reserved are app names managed by hand: never created or removed here.
	Reserved map[string]bool
	// PortMin..PortMax is the pool of loopback host ports for the relays.
	PortMin, PortMax int
	// RoutesDir is the host directory the proxy imports route files from.
	RoutesDir string
	// ProxyContainer is the reverse proxy container; ProxyConfig its config
	// path and ProxyAdmin its admin address, both as seen inside it.
	ProxyContainer string
	ProxyConfig    string
	ProxyAdmin     string
	// ForwardBin is the devbox-forward executable.
	ForwardBin string
	// DNSMode is how a published host gets its DNS record: "static" (a
	// wildcard already covers it; nothing to do) or "command" (DNSCommand is
	// run with "ensure <host>" / "remove <host>").
	DNSMode    string
	DNSCommand string
	// Resolver is the host:port of the resolver the private network uses
	// (optional). When set, a publish waits until it answers for the host.
	// ResolverContainer, when set, is sent SIGHUP to drop its cache (a
	// negative answer cached before the record existed) before waiting.
	Resolver          string
	ResolverContainer string
	DNSWait           time.Duration
}

// Env variable names.
const (
	EnvEnabled        = "OPENPOET_DEVBOX_PUBLISH"
	EnvHostTemplate   = "OPENPOET_DEVBOX_HOST_TEMPLATE"
	EnvSSHHosts       = "OPENPOET_DEVBOX_SSH_HOSTS"
	EnvContainers     = "OPENPOET_DEVBOX_CONTAINERS"
	EnvReserved       = "OPENPOET_DEVBOX_RESERVED"
	EnvPortRange      = "OPENPOET_DEVBOX_PORT_RANGE"
	EnvRoutesDir      = "OPENPOET_DEVBOX_ROUTES_DIR"
	EnvProxyContainer = "OPENPOET_DEVBOX_PROXY_CONTAINER"
	EnvProxyConfig    = "OPENPOET_DEVBOX_PROXY_CONFIG"
	EnvProxyAdmin     = "OPENPOET_DEVBOX_PROXY_ADMIN"
	EnvForwardBin     = "OPENPOET_DEVBOX_FORWARD_BIN"
	EnvDNS            = "OPENPOET_DEVBOX_DNS"
	EnvDNSCommand     = "OPENPOET_DEVBOX_DNS_COMMAND"
	EnvResolver       = "OPENPOET_DEVBOX_RESOLVER"
	EnvResolverCtr    = "OPENPOET_DEVBOX_RESOLVER_CONTAINER"
	EnvDNSWait        = "OPENPOET_DEVBOX_DNS_WAIT_SECONDS"
)

// ConfigFromEnv reads the configuration. A disabled feature returns
// Enabled=false and no error; an enabled one with a bad value returns the error.
func ConfigFromEnv() (Config, error) {
	return parseConfig(os.Getenv)
}

func parseConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Prefixes:       map[string]string{},
		Reserved:       map[string]bool{},
		ProxyContainer: "devbox-proxy",
		ProxyConfig:    "/etc/caddy/Caddyfile",
		ProxyAdmin:     "unix//run/caddy-admin/admin.sock",
	}
	if v := strings.TrimSpace(getenv(EnvEnabled)); v != "1" && !strings.EqualFold(v, "true") {
		return cfg, nil
	}
	cfg.Enabled = true
	cfg.HostTemplate = strings.ToLower(strings.Trim(strings.TrimSpace(getenv(EnvHostTemplate)), "."))
	if strings.Count(cfg.HostTemplate, "{app}") != 1 || !validHostName(strings.Replace(cfg.HostTemplate, "{app}", "a", 1)) ||
		!strings.Contains(cfg.HostTemplate, ".") {
		return cfg, fmt.Errorf("%s must be a host name with one {app} (e.g. {app}-dev9.example.com)", EnvHostTemplate)
	}
	for _, h := range splitList(getenv(EnvSSHHosts)) {
		cfg.SSHHosts = append(cfg.SSHHosts, strings.ToLower(h))
	}
	if len(cfg.SSHHosts) == 0 {
		return cfg, fmt.Errorf("%s must list the SSH hosts of this machine", EnvSSHHosts)
	}
	for _, item := range splitList(getenv(EnvContainers)) {
		name, prefix, ok := strings.Cut(item, ":")
		name, prefix = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(prefix))
		if !ok || !containerRE.MatchString(name) || !labelRE.MatchString(prefix) {
			return cfg, fmt.Errorf("%s: %q must be <container>:<app-prefix>", EnvContainers, item)
		}
		cfg.Prefixes[name] = prefix
	}
	if len(cfg.Prefixes) == 0 {
		return cfg, fmt.Errorf("%s must list at least one <container>:<app-prefix>", EnvContainers)
	}
	for _, name := range splitList(getenv(EnvReserved)) {
		cfg.Reserved[strings.ToLower(name)] = true
	}
	lo, hi, ok := strings.Cut(strings.TrimSpace(getenv(EnvPortRange)), "-")
	minPort, err1 := strconv.Atoi(strings.TrimSpace(lo))
	maxPort, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if !ok || err1 != nil || err2 != nil || minPort < 1024 || maxPort > 65535 || minPort > maxPort {
		return cfg, fmt.Errorf("%s must be <min>-<max> within 1024-65535", EnvPortRange)
	}
	cfg.PortMin, cfg.PortMax = minPort, maxPort
	cfg.RoutesDir = strings.TrimSpace(getenv(EnvRoutesDir))
	if cfg.RoutesDir == "" || !filepath.IsAbs(cfg.RoutesDir) {
		return cfg, fmt.Errorf("%s must be an absolute directory", EnvRoutesDir)
	}
	if v := strings.TrimSpace(getenv(EnvProxyContainer)); v != "" {
		cfg.ProxyContainer = v
	}
	if !containerRE.MatchString(cfg.ProxyContainer) {
		return cfg, fmt.Errorf("%s is not a container name", EnvProxyContainer)
	}
	if v := strings.TrimSpace(getenv(EnvProxyConfig)); v != "" {
		cfg.ProxyConfig = v
	}
	if v := strings.TrimSpace(getenv(EnvProxyAdmin)); v != "" {
		cfg.ProxyAdmin = v
	}
	cfg.ForwardBin = strings.TrimSpace(getenv(EnvForwardBin))
	if cfg.ForwardBin == "" {
		cfg.ForwardBin = "devbox-forward"
	}
	cfg.DNSMode = strings.ToLower(strings.TrimSpace(getenv(EnvDNS)))
	switch cfg.DNSMode {
	case "", DNSStatic:
		cfg.DNSMode = DNSStatic
	case DNSCommand:
		cfg.DNSCommand = strings.TrimSpace(getenv(EnvDNSCommand))
		if !filepath.IsAbs(cfg.DNSCommand) {
			return cfg, fmt.Errorf("%s=command needs %s, an absolute path", EnvDNS, EnvDNSCommand)
		}
	default:
		return cfg, fmt.Errorf("%s must be static or command", EnvDNS)
	}
	cfg.Resolver = strings.TrimSpace(getenv(EnvResolver))
	if cfg.Resolver != "" {
		if _, port, err := net.SplitHostPort(cfg.Resolver); err != nil || port == "" {
			return cfg, fmt.Errorf("%s must be host:port (e.g. 192.0.2.9:53)", EnvResolver)
		}
	}
	cfg.ResolverContainer = strings.TrimSpace(getenv(EnvResolverCtr))
	if cfg.ResolverContainer != "" && !containerRE.MatchString(cfg.ResolverContainer) {
		return cfg, fmt.Errorf("%s is not a container name", EnvResolverCtr)
	}
	cfg.DNSWait = 60 * time.Second
	if v := strings.TrimSpace(getenv(EnvDNSWait)); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 1 || secs > 600 {
			return cfg, fmt.Errorf("%s must be 1-600", EnvDNSWait)
		}
		cfg.DNSWait = time.Duration(secs) * time.Second
	}
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// DNS modes.
const (
	DNSStatic  = "static"
	DNSCommand = "command"
)

// validHostName reports whether h is a lowercase host name of DNS labels.
func validHostName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) > 63 || !labelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// Host returns the published host name of an app.
func (c Config) Host(app string) string {
	return strings.Replace(c.HostTemplate, "{app}", app, 1)
}

// URL returns the published URL of an app.
func (c Config) URL(app string) string {
	return "https://" + c.Host(app) + "/"
}
