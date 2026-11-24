package devboxpublish

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// labelRE is a lowercase DNS label made of words joined by single hyphens.
	labelRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// containerRE is a Docker container name.
	containerRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// devSuffixRE catches names that would read as another host's suffix.
	devSuffixRE = regexp.MustCompile(`-dev[0-9]+$`)
)

// MaxAppLen keeps the app label well under the 63-character DNS label limit.
const MaxAppLen = 40

// Valid modes of the relay (see devbox-forward).
const (
	ModeIP   = "ip"   // the app listens on 0.0.0.0 in the container
	ModeExec = "exec" // the app listens on 127.0.0.1 only; docker exec per connection
)

// ValidateApp checks an app name against the rules and the container's prefix.
func (c Config) ValidateApp(app, prefix string) error {
	switch {
	case app == "":
		return fmt.Errorf("app is required")
	case len(app) > MaxAppLen:
		return fmt.Errorf("app %q is longer than %d characters", app, MaxAppLen)
	case !labelRE.MatchString(app):
		return fmt.Errorf("app %q is invalid: use lowercase letters, digits and single hyphens (e.g. %s-web)", app, prefix)
	case devSuffixRE.MatchString(app):
		return fmt.Errorf("app %q must not end in -dev<N>: the URL is %s", app, c.Host("<app>"))
	case !validHostName(c.Host(app)):
		return fmt.Errorf("app %q does not make a valid host name (%s)", app, c.Host(app))
	case app != prefix && !strings.HasPrefix(app, prefix+"-"):
		return fmt.Errorf("app %q must be %q or start with %q in this devbox", app, prefix, prefix+"-")
	case c.Reserved[app]:
		return fmt.Errorf("app %q is managed by hand on the host and cannot be changed by this tool", app)
	}
	return nil
}

// ValidatePort checks a container port.
func ValidatePort(port int) error {
	if port < 1024 || port > 65535 {
		return fmt.Errorf("port %d is out of range: use a port between 1024 and 65535", port)
	}
	return nil
}

// NormalizeMode returns the relay mode, defaulting to ip.
func NormalizeMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeIP:
		return ModeIP, nil
	case ModeExec:
		return ModeExec, nil
	}
	return "", fmt.Errorf("mode %q is invalid: use ip (app listens on 0.0.0.0) or exec (app listens on 127.0.0.1 only)", mode)
}

// matcherName is the Caddy named matcher of an app (letters, digits, underscore).
func matcherName(app string) string {
	return "op_" + strings.ReplaceAll(app, "-", "_")
}

// routeFileName is the file, in the routes directory, that holds an app's route.
func routeFileName(app string) string {
	return "op-" + app + ".caddy"
}

// forwardName is the devbox-forward relay name of an app.
func forwardName(app string) string {
	return "op-" + app
}

// RouteSnippet is the Caddyfile fragment of one published app. It is imported
// inside the proxy's route block, after the origin check and before the final
// abort, so an unknown Host is still dropped.
func (c Config) RouteSnippet(app string, hostPort int) string {
	m := matcherName(app)
	return fmt.Sprintf("# Managed by OpenPoet (devbox publish). Do not edit.\n@%s host %s\nreverse_proxy @%s 127.0.0.1:%d\n",
		m, c.Host(app), m, hostPort)
}
