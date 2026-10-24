package application

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// composeRule is one refusal: a pattern plus why it is refused.
type composeRule struct {
	pattern *regexp.Regexp
	code    string
	reason  string
}

// composeRules reject the directives that turn `docker compose up` on a
// generated file into host compromise. This is a deliberately blunt textual
// screen, not a YAML semantic analysis — it runs before the file is ever
// handed to Docker, and a refusal is always the safe answer.
var composeRules = []composeRule{
	{
		pattern: regexp.MustCompile(`(?mi)^\s*privileged\s*:\s*(true|yes)\b`),
		code:    "compose_privileged",
		reason:  "`privileged: true` gives the container full host capabilities",
	},
	{
		pattern: regexp.MustCompile(`(?mi)^\s*(network_mode|pid|ipc|uts|userns_mode)\s*:\s*["']?host\b`),
		code:    "compose_host_namespace",
		reason:  "sharing a host namespace (network_mode/pid/ipc/uts/userns: host) removes container isolation",
	},
	{
		pattern: regexp.MustCompile(`/var/run/docker\.sock|/run/docker\.sock`),
		code:    "compose_docker_socket",
		reason:  "mounting the Docker socket is equivalent to handing over root on the host",
	},
	{
		pattern: regexp.MustCompile(`(?mi)^\s*-?\s*["']?(/|/etc|/root|/home|/usr|/boot|/sys|/proc|/dev|/var)(/[^:"'\s]*)?:`),
		code:    "compose_host_bind_mount",
		reason:  "binding an absolute host path escapes the project directory — use a relative path or a named volume",
	},
	{
		pattern: regexp.MustCompile(`(?mi)^\s*cap_add\s*:`),
		code:    "compose_cap_add",
		reason:  "`cap_add` grants extra kernel capabilities",
	},
	{
		pattern: regexp.MustCompile(`(?mi)^\s*-?\s*["']?(apparmor|seccomp)\s*[:=]\s*unconfined`),
		code:    "compose_unconfined",
		reason:  "disabling AppArmor/seccomp removes the syscall sandbox",
	},
}

// reservedComposePorts are the host ports the compose file must never publish
// on any installation: 8080 is OpenPoet's default port and 8081 the
// conventional port of a production instance next to a development one.
// Installation-specific reservations live in the database (environment_resources
// rows with status 'reserved') and are enforced by LintComposeReserved.
var reservedComposePorts = map[string]string{
	"8080": "the OpenPoet development server",
	"8081": "the OpenPoet PRODUCTION server",
}

// ReservedPortChecker is implemented by stores that record reserved host ports
// (database.DB does, through environment_resources).
type ReservedPortChecker interface {
	IsPortReserved(ctx context.Context, port int) (bool, error)
}

// publishedHostPorts returns the host-side ports (both ends of a range) the
// compose body publishes.
func publishedHostPorts(body string) []string {
	var ports []string
	for _, match := range publishedPortPattern.FindAllStringSubmatch(body, -1) {
		for _, port := range match[1:] {
			if port != "" {
				ports = append(ports, port)
			}
		}
	}
	return ports
}

// LintComposeReserved runs LintCompose and, when store records reserved ports
// (see ReservedPortChecker), also refuses host ports reserved on this
// installation. A store without that capability adds no extra check.
func LintComposeReserved(ctx context.Context, body string, store any) error {
	if err := LintCompose(body); err != nil {
		return err
	}
	checker, ok := store.(ReservedPortChecker)
	if !ok || checker == nil {
		return nil
	}
	for _, port := range publishedHostPorts(body) {
		number, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		reserved, err := checker.IsPortReserved(ctx, number)
		if err != nil {
			return fmt.Errorf("check reserved port %d: %w", number, err)
		}
		if reserved {
			return validationError("compose_reserved_port", fmt.Sprintf(
				"Refused: host port %s is reserved on this installation. Publish a different port.", port))
		}
	}
	return nil
}

// publishedPortPattern captures the HOST side of a `ports:` entry, i.e. the
// left-hand number in "8080:80", "127.0.0.1:8080:80" or "8080-8090:80".
var publishedPortPattern = regexp.MustCompile(`(?m)^\s*-\s*["']?(?:\d{1,3}(?:\.\d{1,3}){3}:)?(\d{2,5})(?:-(\d{2,5}))?:\d`)

// LintCompose screens a docker-compose body for directives that would hand the
// container control of the host, and for host ports that are already spoken
// for. It returns a validation error naming the first problem found.
func LintCompose(body string) error {
	if strings.TrimSpace(body) == "" {
		return validationError("compose_required", "The docker-compose body is empty")
	}
	for _, rule := range composeRules {
		if loc := rule.pattern.FindString(body); loc != "" {
			return validationError(rule.code, fmt.Sprintf(
				"Refused: %s (found %q). Edit the compose file to remove it.",
				rule.reason, strings.TrimSpace(loc)))
		}
	}
	for _, port := range publishedHostPorts(body) {
		if why, reserved := reservedComposePorts[port]; reserved {
			return validationError("compose_reserved_port", fmt.Sprintf(
				"Refused: host port %s is reserved for %s. Publish a different port.", port, why))
		}
	}
	return nil
}
