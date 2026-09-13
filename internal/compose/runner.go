// Package compose runs `docker compose` against a project directory. It is a
// thin, deliberately boring wrapper: no state of its own, because the Docker
// daemon is already the source of truth for what is running.
package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the manifest compose is driven from.
const FileName = "docker-compose.yml"

// Timeouts — `up` pulls images, the rest are local daemon round-trips.
const (
	UpTimeout      = 5 * time.Minute
	DefaultTimeout = 60 * time.Second
)

// ErrNotInstalled is returned when the docker CLI is not on PATH.
var ErrNotInstalled = errors.New("docker is not installed or not on PATH")

// Service is one entry of `docker compose ps`.
type Service struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Health  string `json:"health,omitempty"`
	Ports   string `json:"ports,omitempty"`
}

// Runner executes compose commands in a project directory.
type Runner struct {
	// Binary is the docker executable; empty means "docker" resolved on PATH.
	Binary string
}

func New() *Runner { return &Runner{} }

func (r *Runner) binary() string {
	if strings.TrimSpace(r.Binary) != "" {
		return r.Binary
	}
	return "docker"
}

// Available reports whether the docker CLI can be found.
func (r *Runner) Available() error {
	if _, err := exec.LookPath(r.binary()); err != nil {
		return ErrNotInstalled
	}
	return nil
}

// ManifestPath is the compose file for a project directory.
func ManifestPath(projectPath string) string {
	return filepath.Join(projectPath, FileName)
}

// HasManifest reports whether the project carries a compose file.
func HasManifest(projectPath string) bool {
	info, err := os.Stat(ManifestPath(projectPath))
	return err == nil && !info.IsDir()
}

// ReadManifest returns the compose file contents.
func ReadManifest(projectPath string) (string, error) {
	body, err := os.ReadFile(ManifestPath(projectPath))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ProjectName derives the compose project name. Prefixing keeps OpenPoet-managed
// stacks from colliding with anything else already running on the host.
func ProjectName(projectPath string) string {
	base := strings.ToLower(filepath.Base(projectPath))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" {
		name = "project"
	}
	return "openpoet-" + name
}

// run executes `docker compose -p <name> <args...>` in projectPath with a
// scrubbed environment, so nothing from the OpenPoet process leaks into the
// container runtime.
func (r *Runner) run(ctx context.Context, projectPath string, timeout time.Duration, args ...string) (string, error) {
	if err := r.Available(); err != nil {
		return "", err
	}
	if !HasManifest(projectPath) {
		return "", fmt.Errorf("no %s in %s", FileName, projectPath)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{"compose", "-p", ProjectName(projectPath), "-f", ManifestPath(projectPath)}, args...)
	cmd := exec.CommandContext(ctx, r.binary(), full...)
	cmd.Dir = projectPath
	cmd.Env = scrubbedEnv()

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("docker compose %s timed out after %s", strings.Join(args, " "), timeout)
	}
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("docker compose %s: %w", strings.Join(args, " "), err)
		}
		return text, fmt.Errorf("docker compose %s failed: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

// scrubbedEnv gives the child the minimum it needs to talk to the daemon —
// the same discipline as the workspace ProcessDriver.
func scrubbedEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	// DOCKER_HOST and friends decide WHICH daemon is used; dropping them would
	// silently target the wrong one on a non-default setup.
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "XDG_RUNTIME_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// Status lists the services of the project's stack. An empty slice means the
// stack is defined but nothing is running.
func (r *Runner) Status(ctx context.Context, projectPath string) ([]Service, error) {
	out, err := r.run(ctx, projectPath, DefaultTimeout, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	services := make([]Service, 0, 4)
	// `docker compose ps --format json` emits one JSON object per line.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[]" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			var batch []Service
			if err := json.Unmarshal([]byte(line), &batch); err != nil {
				return nil, fmt.Errorf("parse compose ps output: %w", err)
			}
			services = append(services, batch...)
			continue
		}
		var one Service
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			return nil, fmt.Errorf("parse compose ps output: %w", err)
		}
		services = append(services, one)
	}
	return services, nil
}

// Up starts the stack detached, recreating containers whose definition changed.
func (r *Runner) Up(ctx context.Context, projectPath string) (string, error) {
	return r.run(ctx, projectPath, UpTimeout, "up", "-d", "--remove-orphans")
}

// Down stops and removes the stack's containers and networks. Named volumes are
// deliberately kept — losing a database to a stop button is not recoverable.
func (r *Runner) Down(ctx context.Context, projectPath string) (string, error) {
	return r.run(ctx, projectPath, DefaultTimeout, "down", "--remove-orphans")
}

// Restart restarts the running services.
func (r *Runner) Restart(ctx context.Context, projectPath string) (string, error) {
	return r.run(ctx, projectPath, UpTimeout, "restart")
}

// Logs returns the tail of the stack's logs, optionally for one service.
func (r *Runner) Logs(ctx context.Context, projectPath, service string, tail int) (string, error) {
	if tail <= 0 || tail > 5000 {
		tail = 200
	}
	args := []string{"logs", "--no-color", "--tail", fmt.Sprintf("%d", tail)}
	if s := strings.TrimSpace(service); s != "" {
		args = append(args, s)
	}
	return r.run(ctx, projectPath, DefaultTimeout, args...)
}

// Config validates the compose file through docker itself, catching the schema
// errors a textual lint cannot see.
func (r *Runner) Config(ctx context.Context, projectPath string) (string, error) {
	return r.run(ctx, projectPath, DefaultTimeout, "config", "--quiet")
}
