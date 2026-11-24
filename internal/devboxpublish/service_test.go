package devboxpublish

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func testConfig() Config {
	cfg, err := parseConfig(mapEnv(map[string]string{
		EnvEnabled:      "1",
		EnvHostTemplate: "{app}-dev9.example.com",
		EnvSSHHosts:     "192.0.2.9, localhost",
		EnvContainers:   "alpha-devbox:alpha, beta-devbox:beta",
		EnvReserved:     "alpha,alpha-manual",
		EnvPortRange:    "28100-28102",
		EnvRoutesDir:    "/srv/devbox-proxy/routes",
	}))
	if err != nil {
		panic(err)
	}
	return cfg
}

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// memStore is an in-memory Store.
type memStore struct {
	mu   sync.Mutex
	rows map[string]Publication
}

func newMemStore() *memStore { return &memStore{rows: map[string]Publication{}} }

func (m *memStore) ListDevboxPublications(context.Context) ([]Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Publication
	for _, p := range m.rows {
		out = append(out, p)
	}
	return out, nil
}
func (m *memStore) GetDevboxPublication(_ context.Context, app string) (*Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.rows[app]; ok {
		return &p, nil
	}
	return nil, nil
}
func (m *memStore) InsertDevboxPublication(_ context.Context, p Publication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[p.App]; ok {
		return errors.New("duplicate")
	}
	m.rows[p.App] = p
	return nil
}
func (m *memStore) DeleteDevboxPublication(_ context.Context, app string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, app)
	return nil
}

// fakeHost records what the service did to the machine.
type fakeHost struct {
	sshPorts   map[int]string // host SSH port -> container
	gone       map[string]bool
	dockerDown bool
	listening  map[string]bool // "container:port"
	busy       map[int]bool    // host ports in use by something else
	forwards   map[string]string
	routes     map[string]string
	reloads    int
	failReload int // fail the next N reloads
	failAdd    bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		sshPorts:  map[int]string{2201: "alpha-devbox", 2202: "beta-devbox", 2203: "gamma-devbox"},
		gone:      map[string]bool{},
		listening: map[string]bool{"alpha-devbox:3000": true, "alpha-devbox:4000": true, "beta-devbox:3000": true},
		busy:      map[int]bool{},
		forwards:  map[string]string{},
		routes:    map[string]string{},
	}
}

func (h *fakeHost) ContainerForSSHPort(_ context.Context, port int) (string, error) {
	if c, ok := h.sshPorts[port]; ok {
		return c, nil
	}
	return "", errors.New("no running container publishes SSH on that port")
}
func (h *fakeHost) ContainerExists(_ context.Context, c string) error {
	if h.dockerDown {
		return errors.New("docker: cannot connect")
	}
	if h.gone[c] {
		return ErrNoSuchContainer
	}
	return nil
}
func (h *fakeHost) Probe(_ context.Context, c string, port int, _ string) error {
	if h.listening[c+":"+strconv.Itoa(port)] {
		return nil
	}
	return errors.New("nothing accepts connections")
}
func (h *fakeHost) PortFree(port int) bool { return !h.busy[port] }
func (h *fakeHost) AddForward(_ context.Context, name, c string, cp, hp int, mode string) error {
	if h.failAdd {
		return errors.New("systemctl failed")
	}
	if _, ok := h.forwards[name]; ok {
		return errors.New(name + " ja existe")
	}
	h.forwards[name] = c + ":" + strconv.Itoa(cp) + "<-" + strconv.Itoa(hp) + "/" + mode
	return nil
}
func (h *fakeHost) RemoveForward(_ context.Context, name string) error {
	delete(h.forwards, name)
	return nil
}
func (h *fakeHost) ForwardExists(name string) bool        { _, ok := h.forwards[name]; return ok }
func (h *fakeHost) WriteRoute(file, content string) error { h.routes[file] = content; return nil }
func (h *fakeHost) RemoveRoute(file string) error         { delete(h.routes, file); return nil }
func (h *fakeHost) RouteExists(file string) bool          { _, ok := h.routes[file]; return ok }
func (h *fakeHost) ReloadProxy(context.Context) error {
	h.reloads++
	if h.failReload > 0 {
		h.failReload--
		return errors.New("caddy: adapting config: bad")
	}
	return nil
}

var alpha = Caller{SessionID: "s-alpha", ProjectID: 1, SSHHost: "192.0.2.9", SSHPort: 2201}
var beta = Caller{SessionID: "s-beta", ProjectID: 2, SSHHost: "192.0.2.9", SSHPort: 2202}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("expected a user error, got %v", err)
	}
	return ue.Status
}

func TestPublishCreatesRelayRouteAndRecord(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	s := NewService(testConfig(), store, host, nil, nil)
	v, err := s.Publish(context.Background(), alpha, "alpha-web", 3000, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "created" || v.URL != "https://alpha-web-dev9.example.com/" || v.Container != "alpha-devbox" || v.Mode != ModeIP {
		t.Fatalf("unexpected view %+v", v)
	}
	if host.forwards["op-alpha-web"] != "alpha-devbox:3000<-28100/ip" {
		t.Fatalf("relay: %v", host.forwards)
	}
	want := "@op_alpha_web host alpha-web-dev9.example.com\nreverse_proxy @op_alpha_web 127.0.0.1:28100\n"
	if !strings.HasSuffix(host.routes["op-alpha-web.caddy"], want) {
		t.Fatalf("route file:\n%s", host.routes["op-alpha-web.caddy"])
	}
	if host.reloads != 1 {
		t.Fatalf("reloads = %d", host.reloads)
	}
	if p, _ := store.GetDevboxPublication(context.Background(), "alpha-web"); p == nil || p.SessionID != "s-alpha" || p.HostPort != 28100 {
		t.Fatalf("record: %+v", p)
	}
}

func TestPublishIsIdempotentAndRepairs(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	s := NewService(testConfig(), store, host, nil, nil)
	ctx := context.Background()
	if _, err := s.Publish(ctx, alpha, "alpha-web", 3000, "ip"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Publish(ctx, alpha, "alpha-web", 3000, "")
	if err != nil || v.Status != "unchanged" || host.reloads != 1 {
		t.Fatalf("second publish: %+v %v reloads=%d", v, err, host.reloads)
	}
	// Somebody removed the relay and the route by hand.
	delete(host.forwards, "op-alpha-web")
	delete(host.routes, "op-alpha-web.caddy")
	v, err = s.Publish(ctx, alpha, "alpha-web", 3000, "")
	if err != nil || v.Status != "repaired" || host.forwards["op-alpha-web"] == "" || host.routes["op-alpha-web.caddy"] == "" {
		t.Fatalf("repair: %+v %v", v, err)
	}
}

func TestPublishConflicts(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	s := NewService(testConfig(), store, host, nil, nil)
	ctx := context.Background()
	if _, err := s.Publish(ctx, alpha, "alpha-web", 3000, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, alpha, "alpha-web", 4000, ""); statusOf(t, err) != 409 {
		t.Fatalf("other port: %v", err)
	}
	if _, err := s.Publish(ctx, alpha, "alpha-web", 3000, "exec"); statusOf(t, err) != 409 {
		t.Fatalf("other mode: %v", err)
	}
	// beta cannot take a name of alpha's prefix at all.
	if _, err := s.Publish(ctx, beta, "alpha-web", 3000, ""); statusOf(t, err) != 400 {
		t.Fatalf("beta with alpha prefix: %v", err)
	}
}

func TestPublishValidation(t *testing.T) {
	s := NewService(testConfig(), newMemStore(), newFakeHost(), nil, nil)
	ctx := context.Background()
	cases := []struct {
		app  string
		port int
		mode string
		code int
	}{
		{"", 3000, "", 400},
		{"Alpha-Web", 3000, "", 0}, // lowercased
		{"alpha_web", 3000, "", 400},
		{"alpha--web", 3000, "", 400},
		{"alpha-web-", 3000, "", 400},
		{"alphaweb", 3000, "", 400}, // prefix must be followed by a hyphen
		{"beta-web", 3000, "", 400},
		{"alpha-web-dev12", 3000, "", 400},
		{"alpha-" + strings.Repeat("x", 40), 3000, "", 400},
		{"alpha", 3000, "", 400},        // reserved
		{"alpha-manual", 3000, "", 400}, // reserved
		{"alpha-api", 22, "", 400},
		{"alpha-api", 70000, "", 400},
		{"alpha-api", 3000, "ssh", 400},
		{"alpha-api", 5000, "", 422}, // nothing listening
	}
	for _, c := range cases {
		_, err := s.Publish(ctx, alpha, c.app, c.port, c.mode)
		if c.code == 0 {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.app, err)
			}
			continue
		}
		if got := statusOf(t, err); got != c.code {
			t.Errorf("%q port %d mode %q: status %d, want %d (%v)", c.app, c.port, c.mode, got, c.code, err)
		}
	}
}

func TestCallerMustBeAnEnabledLocalDevbox(t *testing.T) {
	s := NewService(testConfig(), newMemStore(), newFakeHost(), nil, nil)
	ctx := context.Background()
	for name, c := range map[string]Caller{
		"remote host":           {SSHHost: "198.51.100.7", SSHPort: 2201},
		"no port":               {SSHHost: "192.0.2.9"},
		"unknown ssh port":      {SSHHost: "192.0.2.9", SSHPort: 2299},
		"container not in list": {SSHHost: "192.0.2.9", SSHPort: 2203},
	} {
		if _, err := s.Publish(ctx, c, "alpha-web", 3000, ""); statusOf(t, err) != 403 {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := s.List(ctx, c); statusOf(t, err) != 403 {
			t.Errorf("%s list: %v", name, err)
		}
	}
	if _, err := s.Publish(ctx, Caller{SSHHost: "LOCALHOST", SSHPort: 2201}, "alpha-web", 3000, ""); err != nil {
		t.Errorf("host match is case-insensitive: %v", err)
	}
	off := NewService(Config{}, newMemStore(), newFakeHost(), nil, nil)
	if _, err := off.Publish(ctx, alpha, "alpha-web", 3000, ""); statusOf(t, err) != 503 {
		t.Errorf("disabled: %v", err)
	}
}

func TestReloadFailureRollsEverythingBack(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	host.failReload = 1
	s := NewService(testConfig(), store, host, nil, nil)
	_, err := s.Publish(context.Background(), alpha, "alpha-web", 3000, "")
	if err == nil || !strings.Contains(err.Error(), "reloading the proxy failed") {
		t.Fatalf("err = %v", err)
	}
	if len(host.forwards) != 0 || len(host.routes) != 0 || len(store.rows) != 0 {
		t.Fatalf("left behind: forwards=%v routes=%v rows=%v", host.forwards, host.routes, store.rows)
	}
	if host.reloads != 2 {
		t.Fatalf("expected a second reload to restore the previous config, got %d", host.reloads)
	}
}

func TestRelayFailureRollsBack(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	host.failAdd = true
	s := NewService(testConfig(), store, host, nil, nil)
	if _, err := s.Publish(context.Background(), alpha, "alpha-web", 3000, ""); err == nil {
		t.Fatal("expected an error")
	}
	if len(store.rows) != 0 || len(host.routes) != 0 || host.reloads != 0 {
		t.Fatalf("left behind: rows=%v routes=%v reloads=%d", store.rows, host.routes, host.reloads)
	}
}

func TestPortPool(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	host.busy[28100] = true // something else listens there
	host.listening["alpha-devbox:3001"] = true
	host.listening["alpha-devbox:3002"] = true
	s := NewService(testConfig(), store, host, nil, nil)
	ctx := context.Background()
	v1, err := s.Publish(ctx, alpha, "alpha-a", 3000, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, alpha, "alpha-b", 3001, ""); err != nil {
		t.Fatal(err)
	}
	p1, _ := store.GetDevboxPublication(ctx, v1.App)
	if p1.HostPort != 28101 {
		t.Fatalf("first port = %d, want 28101 (28100 busy)", p1.HostPort)
	}
	if _, err := s.Publish(ctx, alpha, "alpha-c", 3002, ""); statusOf(t, err) != 503 {
		t.Fatalf("pool exhausted: %v", err)
	}
	if _, err := s.Unpublish(ctx, alpha, "alpha-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, alpha, "alpha-c", 3002, ""); err != nil {
		t.Fatalf("port freed by unpublish should be reused: %v", err)
	}
}

func TestListAndUnpublishOnlyOwnContainer(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	s := NewService(testConfig(), store, host, nil, nil)
	ctx := context.Background()
	if _, err := s.Publish(ctx, alpha, "alpha-web", 3000, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, beta, "beta-web", 3000, ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, alpha)
	if err != nil || len(list) != 1 || list[0].App != "alpha-web" {
		t.Fatalf("alpha list: %+v %v", list, err)
	}
	if _, err := s.Unpublish(ctx, beta, "alpha-web"); statusOf(t, err) != 403 {
		t.Fatalf("beta removing alpha's app: %v", err)
	}
	if _, err := s.Unpublish(ctx, alpha, "alpha"); statusOf(t, err) != 403 {
		t.Fatalf("reserved: %v", err)
	}
	if _, err := s.Unpublish(ctx, alpha, "alpha-nope"); statusOf(t, err) != 404 {
		t.Fatalf("missing: %v", err)
	}
	v, err := s.Unpublish(ctx, alpha, "alpha-web")
	if err != nil || v.Status != "removed" {
		t.Fatalf("unpublish: %+v %v", v, err)
	}
	if host.ForwardExists("op-alpha-web") || host.RouteExists("op-alpha-web.caddy") {
		t.Fatal("relay or route left behind")
	}
	if p, _ := store.GetDevboxPublication(ctx, "alpha-web"); p != nil {
		t.Fatal("record left behind")
	}
	if !host.ForwardExists("op-beta-web") {
		t.Fatal("beta's relay was touched")
	}
}

func TestGarbageCollection(t *testing.T) {
	store, host := newMemStore(), newFakeHost()
	s := NewService(testConfig(), store, host, nil, nil)
	ctx := context.Background()
	s.Publish(ctx, alpha, "alpha-web", 3000, "")
	s.Publish(ctx, beta, "beta-web", 3000, "")

	host.dockerDown = true
	if n, err := s.CollectGarbage(ctx); n != 0 || err == nil {
		t.Fatalf("docker down must remove nothing: n=%d err=%v", n, err)
	}
	host.dockerDown = false
	if n, err := s.CollectGarbage(ctx); n != 0 || err != nil {
		t.Fatalf("all alive: n=%d err=%v", n, err)
	}
	host.gone["beta-devbox"] = true
	if n, err := s.CollectGarbage(ctx); n != 1 || err != nil {
		t.Fatalf("beta gone: n=%d err=%v", n, err)
	}
	if p, _ := store.GetDevboxPublication(ctx, "beta-web"); p != nil || host.ForwardExists("op-beta-web") {
		t.Fatal("beta not cleaned")
	}
	if p, _ := store.GetDevboxPublication(ctx, "alpha-web"); p == nil {
		t.Fatal("alpha removed")
	}
}

func TestConfigParsing(t *testing.T) {
	if cfg, err := parseConfig(mapEnv(nil)); err != nil || cfg.Enabled {
		t.Fatalf("unset: %+v %v", cfg, err)
	}
	base := map[string]string{
		EnvEnabled: "1", EnvHostTemplate: "{app}-dev9.example.com", EnvSSHHosts: "192.0.2.9",
		EnvContainers: "a-devbox:a", EnvPortRange: "28100-28199", EnvRoutesDir: "/srv/routes",
	}
	if _, err := parseConfig(mapEnv(base)); err != nil {
		t.Fatal(err)
	}
	for key, bad := range map[string]string{
		EnvHostTemplate: "dev9.example.com", EnvSSHHosts: "", EnvContainers: "a-devbox",
		EnvPortRange: "80-90", EnvRoutesDir: "routes",
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[key] = bad
		if _, err := parseConfig(mapEnv(env)); err == nil {
			t.Errorf("%s=%q accepted", key, bad)
		}
	}
}

func TestContainerForSSHPort(t *testing.T) {
	ps := "alpha-devbox\t192.0.2.9:2201->22/tcp\n" +
		"beta-devbox\t0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp, 0.0.0.0:2202->22/tcp, [::]:2202->22/tcp\n" +
		"web\t0.0.0.0:22010->80/tcp\n" +
		"db\t\n"
	if c, err := containerForSSHPort(ps, 2201); err != nil || c != "alpha-devbox" {
		t.Fatalf("2201: %q %v", c, err)
	}
	if c, err := containerForSSHPort(ps, 2202); err != nil || c != "beta-devbox" {
		t.Fatalf("2202: %q %v", c, err)
	}
	if _, err := containerForSSHPort(ps, 220); err == nil {
		t.Fatal("220 must not match 2201 or 22010")
	}
	if _, err := containerForSSHPort(ps+"twin\t0.0.0.0:2201->22/tcp\n", 2201); err == nil {
		t.Fatal("two containers on one port must be an error")
	}
}
