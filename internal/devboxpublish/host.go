package devboxpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrNoSuchContainer means Docker answered that the container does not exist
// (as opposed to Docker failing to answer).
var ErrNoSuchContainer = errors.New("no such container")

// Host is what the service needs from the machine. ExecHost is the real one;
// tests use a fake.
type Host interface {
	// ContainerForSSHPort returns the container whose port 22 is published on
	// hostPort.
	ContainerForSSHPort(ctx context.Context, hostPort int) (string, error)
	// ContainerExists returns ErrNoSuchContainer when Docker says it is gone.
	ContainerExists(ctx context.Context, container string) error
	// Probe checks that something accepts TCP connections on the container port.
	Probe(ctx context.Context, container string, port int, mode string) error
	// PortFree reports whether nothing listens on 127.0.0.1:port.
	PortFree(port int) bool
	AddForward(ctx context.Context, name, container string, containerPort, hostPort int, mode string) error
	RemoveForward(ctx context.Context, name string) error
	ForwardExists(name string) bool
	WriteRoute(file, content string) error
	RemoveRoute(file string) error
	RouteExists(file string) bool
	// ReloadProxy reloads the proxy's configuration without a restart.
	ReloadProxy(ctx context.Context) error
}

// ExecHost runs docker and devbox-forward as the OpenPoet user. Every command
// is a fixed executable with separate arguments: nothing goes through a shell.
type ExecHost struct {
	Cfg Config
	// ForwardConfDir is where devbox-forward keeps <name>.conf.
	ForwardConfDir string
}

// NewExecHost returns the host implementation for cfg.
func NewExecHost(cfg Config) *ExecHost {
	home, _ := os.UserHomeDir()
	return &ExecHost{Cfg: cfg, ForwardConfDir: filepath.Join(home, ".config", "devbox-forward")}
}

func run(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s %s: %s", filepath.Base(name), firstArg(args), lastLines(text, 4))
	}
	return text, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// ContainerForSSHPort implements Host.
func (h *ExecHost) ContainerForSSHPort(ctx context.Context, hostPort int) (string, error) {
	out, err := run(ctx, 15*time.Second, nil, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	if err != nil {
		return "", err
	}
	return containerForSSHPort(out, hostPort)
}

// containerForSSHPort parses `docker ps --format '{{.Names}}\t{{.Ports}}'`.
func containerForSSHPort(psOutput string, hostPort int) (string, error) {
	want := ":" + strconv.Itoa(hostPort) + "->22/tcp"
	var found []string
	for _, line := range strings.Split(psOutput, "\n") {
		name, ports, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		for _, p := range strings.Split(ports, ",") {
			if strings.HasSuffix(strings.TrimSpace(p), want) {
				found = append(found, name)
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no running container publishes SSH on port %d", hostPort)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("more than one container publishes SSH on port %d: %s", hostPort, strings.Join(found, ", "))
}

// ContainerExists implements Host.
func (h *ExecHost) ContainerExists(ctx context.Context, container string) error {
	out, err := run(ctx, 15*time.Second, nil, "docker", "inspect", "--type", "container", "--format", "{{.Name}}", container)
	if err != nil {
		if strings.Contains(strings.ToLower(out), "no such") {
			return ErrNoSuchContainer
		}
		return err
	}
	return nil
}

func (h *ExecHost) containerIPs(ctx context.Context, container string) ([]string, error) {
	out, err := run(ctx, 15*time.Second, nil, "docker", "inspect", "--type", "container", "--format", "{{json .NetworkSettings.Networks}}", container)
	if err != nil {
		return nil, err
	}
	var nets map[string]struct {
		IPAddress string `json:"IPAddress"`
	}
	if err := json.Unmarshal([]byte(out), &nets); err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var ips []string
	for _, n := range nets {
		if n.IPAddress != "" {
			ips = append(ips, n.IPAddress)
		}
	}
	return ips, nil
}

// Probe implements Host.
func (h *ExecHost) Probe(ctx context.Context, container string, port int, mode string) error {
	if mode == ModeExec {
		_, err := run(ctx, 15*time.Second, nil, "docker", "exec", container, "bash", "-c",
			`exec 3<>/dev/tcp/127.0.0.1/"$1"`, "probe", strconv.Itoa(port))
		if err != nil {
			return fmt.Errorf("nothing accepts connections on 127.0.0.1:%d inside the container; start the app first", port)
		}
		return nil
	}
	ips, err := h.containerIPs(ctx, container)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 3*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
	}
	return fmt.Errorf("nothing accepts connections on port %d of the container's network address; "+
		"start the app listening on 0.0.0.0:%d, or pass mode \"exec\" if it can only listen on 127.0.0.1", port, port)
}

// PortFree implements Host.
func (h *ExecHost) PortFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func (h *ExecHost) forwardEnv() []string {
	env := []string{"LISTEN_ADDR=127.0.0.1", "ALLOW=127.0.0.1/32"}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	}
	return env
}

// AddForward implements Host.
func (h *ExecHost) AddForward(ctx context.Context, name, container string, containerPort, hostPort int, mode string) error {
	_, err := run(ctx, 30*time.Second, h.forwardEnv(), h.Cfg.ForwardBin, "add", name, container,
		strconv.Itoa(containerPort), strconv.Itoa(hostPort), mode)
	return err
}

// RemoveForward implements Host.
func (h *ExecHost) RemoveForward(ctx context.Context, name string) error {
	_, err := run(ctx, 30*time.Second, h.forwardEnv(), h.Cfg.ForwardBin, "rm", name)
	return err
}

// ForwardExists implements Host.
func (h *ExecHost) ForwardExists(name string) bool {
	_, err := os.Stat(filepath.Join(h.ForwardConfDir, name+".conf"))
	return err == nil
}

// WriteRoute implements Host: write to a temporary file, then rename, so the
// proxy never imports a half-written file.
func (h *ExecHost) WriteRoute(file, content string) error {
	tmp, err := os.CreateTemp(h.Cfg.RoutesDir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(h.Cfg.RoutesDir, file))
}

// RemoveRoute implements Host.
func (h *ExecHost) RemoveRoute(file string) error {
	err := os.Remove(filepath.Join(h.Cfg.RoutesDir, file))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RouteExists implements Host.
func (h *ExecHost) RouteExists(file string) bool {
	_, err := os.Stat(filepath.Join(h.Cfg.RoutesDir, file))
	return err == nil
}

// ReloadProxy implements Host.
func (h *ExecHost) ReloadProxy(ctx context.Context) error {
	_, err := run(ctx, 30*time.Second, nil, "docker", "exec", h.Cfg.ProxyContainer,
		"caddy", "reload", "--config", h.Cfg.ProxyConfig, "--adapter", "caddyfile", "--address", h.Cfg.ProxyAdmin)
	return err
}
