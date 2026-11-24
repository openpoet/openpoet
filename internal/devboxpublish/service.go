package devboxpublish

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Publication is one published port.
type Publication struct {
	App           string    `db:"app" json:"app"`
	Container     string    `db:"container" json:"container"`
	ProjectID     int64     `db:"project_id" json:"project_id"`
	SessionID     string    `db:"session_id" json:"session_id"`
	ContainerPort int       `db:"container_port" json:"container_port"`
	HostPort      int       `db:"host_port" json:"host_port"`
	Mode          string    `db:"mode" json:"mode"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}

// Store persists publications. *database.DB implements it.
type Store interface {
	ListDevboxPublications(ctx context.Context) ([]Publication, error)
	GetDevboxPublication(ctx context.Context, app string) (*Publication, error) // nil, nil when absent
	InsertDevboxPublication(ctx context.Context, p Publication) error
	DeleteDevboxPublication(ctx context.Context, app string) error
}

// Caller is the session asking, with the SSH settings of its project.
type Caller struct {
	SessionID string
	ProjectID int64
	SSHHost   string
	SSHPort   int
}

// Error is a failure the caller can act on (bad input, not allowed, conflict).
// Anything else is a host failure.
type Error struct {
	Status int // HTTP status to answer with
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func userErr(status int, format string, args ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// View is a publication as the agent sees it.
type View struct {
	App           string `json:"app"`
	URL           string `json:"url"`
	ContainerPort int    `json:"container_port"`
	Mode          string `json:"mode"`
	Container     string `json:"container"`
	Status        string `json:"status,omitempty"` // created, unchanged, repaired, removed
	CreatedAt     string `json:"created_at,omitempty"`
}

// Service publishes, lists and removes ports. One mutex serializes every
// change: they share the port pool, the routes directory and the proxy.
type Service struct {
	cfg      Config
	store    Store
	host     Host
	dns      DNS
	resolver Resolver // nil: do not wait for DNS
	// pollEvery is how often the resolver is asked again while waiting.
	pollEvery time.Duration
	mu        sync.Mutex
}

// NewService returns the service. dns nil means StaticDNS; resolver nil means
// a publish does not wait for the name to resolve.
func NewService(cfg Config, store Store, host Host, dns DNS, resolver Resolver) *Service {
	if dns == nil {
		dns = StaticDNS{}
	}
	return &Service{cfg: cfg, store: store, host: host, dns: dns, resolver: resolver, pollEvery: 2 * time.Second}
}

// publishName gives the host its DNS record and waits until the private
// network's resolver answers for it, so the URL works when it is handed out.
func (s *Service) publishName(ctx context.Context, app string) error {
	host := s.cfg.Host(app)
	if err := s.dns.Ensure(ctx, host); err != nil {
		return fmt.Errorf("creating the DNS record of %s failed: %w", host, err)
	}
	return waitResolves(ctx, s.resolver, host, s.cfg.DNSWait, s.pollEvery)
}

// Enabled reports whether the feature is configured.
func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled }

func (s *Service) view(p Publication, status string) View {
	return View{App: p.App, URL: s.cfg.URL(p.App), ContainerPort: p.ContainerPort, Mode: p.Mode,
		Container: p.Container, Status: status, CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339)}
}

// ResolveContainer derives the caller's container from its project's SSH
// settings and checks that it may publish. It returns the container and its
// app prefix.
func (s *Service) ResolveContainer(ctx context.Context, c Caller) (string, string, error) {
	if !s.Enabled() {
		return "", "", userErr(503, "publishing devbox ports is not configured on this OpenPoet instance")
	}
	host := strings.ToLower(strings.TrimSpace(c.SSHHost))
	local := false
	for _, h := range s.cfg.SSHHosts {
		if h == host {
			local = true
			break
		}
	}
	if !local || c.SSHPort <= 0 {
		return "", "", userErr(403, "this session's project is not a devbox container on the OpenPoet host, so it cannot publish ports")
	}
	container, err := s.host.ContainerForSSHPort(ctx, c.SSHPort)
	if err != nil {
		return "", "", userErr(403, "cannot tell which container this session runs in: %v", err)
	}
	prefix, ok := s.cfg.Prefixes[container]
	if !ok {
		return "", "", userErr(403, "container %s is not enabled for publishing ports; ask the user to add it on the host", container)
	}
	return container, prefix, nil
}

// Publish publishes containerPort of the caller's container as app.
func (s *Service) Publish(ctx context.Context, c Caller, app string, containerPort int, mode string) (View, error) {
	container, prefix, err := s.ResolveContainer(ctx, c)
	if err != nil {
		return View{}, err
	}
	app = strings.ToLower(strings.TrimSpace(app))
	if err := s.cfg.ValidateApp(app, prefix); err != nil {
		return View{}, userErr(400, "%v", err)
	}
	if err := ValidatePort(containerPort); err != nil {
		return View{}, userErr(400, "%v", err)
	}
	if mode, err = NormalizeMode(mode); err != nil {
		return View{}, userErr(400, "%v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.store.GetDevboxPublication(ctx, app)
	if err != nil {
		return View{}, err
	}
	if existing != nil {
		if existing.Container != container {
			return View{}, userErr(409, "app %q is already published by another devbox; choose another name", app)
		}
		if existing.ContainerPort != containerPort || existing.Mode != mode {
			return View{}, userErr(409, "app %q is already published to port %d (mode %s); unpublish it first to change it",
				app, existing.ContainerPort, existing.Mode)
		}
		status, err := s.repair(ctx, *existing)
		if err != nil {
			return View{}, err
		}
		if err := s.publishName(ctx, app); err != nil {
			return View{}, err
		}
		return s.view(*existing, status), nil
	}

	if err := s.host.Probe(ctx, container, containerPort, mode); err != nil {
		return View{}, userErr(422, "%v", err)
	}
	hostPort, err := s.allocatePort(ctx)
	if err != nil {
		return View{}, err
	}
	p := Publication{App: app, Container: container, ProjectID: c.ProjectID, SessionID: c.SessionID,
		ContainerPort: containerPort, HostPort: hostPort, Mode: mode, CreatedAt: time.Now().UTC()}
	if err := s.store.InsertDevboxPublication(ctx, p); err != nil {
		return View{}, err
	}
	if err := s.apply(ctx, p); err != nil {
		_ = s.store.DeleteDevboxPublication(ctx, app)
		return View{}, err
	}
	if err := s.publishName(ctx, app); err != nil {
		if rerr := s.remove(ctx, p); rerr != nil {
			log.Printf("[devbox-publish] undoing %s after a DNS failure: %v", app, rerr)
		}
		return View{}, err
	}
	log.Printf("[devbox-publish] published %s -> %s:%d (host 127.0.0.1:%d, %s) by session %s",
		s.cfg.Host(app), container, containerPort, hostPort, mode, c.SessionID)
	return s.view(p, "created"), nil
}

// apply creates the relay and the route of a new publication, undoing what it
// did when a step fails.
func (s *Service) apply(ctx context.Context, p Publication) error {
	fwd := forwardName(p.App)
	if err := s.host.AddForward(ctx, fwd, p.Container, p.ContainerPort, p.HostPort, p.Mode); err != nil {
		_ = s.host.RemoveForward(ctx, fwd)
		return fmt.Errorf("creating the relay failed: %w", err)
	}
	if err := s.writeRouteAndReload(ctx, p); err != nil {
		_ = s.host.RemoveForward(ctx, fwd)
		return err
	}
	return nil
}

func (s *Service) writeRouteAndReload(ctx context.Context, p Publication) error {
	file := routeFileName(p.App)
	if err := s.host.WriteRoute(file, s.cfg.RouteSnippet(p.App, p.HostPort)); err != nil {
		return fmt.Errorf("writing the proxy route failed: %w", err)
	}
	if err := s.host.ReloadProxy(ctx); err != nil {
		_ = s.host.RemoveRoute(file)
		if rerr := s.host.ReloadProxy(ctx); rerr != nil {
			log.Printf("[devbox-publish] reload after removing the failed route %s also failed: %v", file, rerr)
		}
		return fmt.Errorf("reloading the proxy failed (route removed, previous configuration kept): %w", err)
	}
	return nil
}

// repair recreates the relay or the route of an existing publication when one
// of them is missing (host cleaned by hand, files lost).
func (s *Service) repair(ctx context.Context, p Publication) (string, error) {
	status := "unchanged"
	if !s.host.ForwardExists(forwardName(p.App)) {
		if err := s.host.AddForward(ctx, forwardName(p.App), p.Container, p.ContainerPort, p.HostPort, p.Mode); err != nil {
			return "", fmt.Errorf("recreating the relay failed: %w", err)
		}
		status = "repaired"
	}
	if !s.host.RouteExists(routeFileName(p.App)) {
		if err := s.writeRouteAndReload(ctx, p); err != nil {
			return "", err
		}
		status = "repaired"
	}
	return status, nil
}

func (s *Service) allocatePort(ctx context.Context) (int, error) {
	all, err := s.store.ListDevboxPublications(ctx)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, p := range all {
		used[p.HostPort] = true
	}
	for port := s.cfg.PortMin; port <= s.cfg.PortMax; port++ {
		if !used[port] && s.host.PortFree(port) {
			return port, nil
		}
	}
	return 0, userErr(503, "no free host port left in the pool %d-%d; unpublish an app first", s.cfg.PortMin, s.cfg.PortMax)
}

// List returns the publications of the caller's container.
func (s *Service) List(ctx context.Context, c Caller) ([]View, error) {
	container, _, err := s.ResolveContainer(ctx, c)
	if err != nil {
		return nil, err
	}
	all, err := s.store.ListDevboxPublications(ctx)
	if err != nil {
		return nil, err
	}
	views := []View{}
	for _, p := range all {
		if p.Container == container {
			views = append(views, s.view(p, ""))
		}
	}
	return views, nil
}

// Unpublish removes an app of the caller's container.
func (s *Service) Unpublish(ctx context.Context, c Caller, app string) (View, error) {
	container, _, err := s.ResolveContainer(ctx, c)
	if err != nil {
		return View{}, err
	}
	app = strings.ToLower(strings.TrimSpace(app))
	if app == "" {
		return View{}, userErr(400, "app is required")
	}
	if s.cfg.Reserved[app] {
		return View{}, userErr(403, "app %q is managed by hand on the host and cannot be changed by this tool", app)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.store.GetDevboxPublication(ctx, app)
	if err != nil {
		return View{}, err
	}
	if p == nil {
		return View{}, userErr(404, "app %q is not published", app)
	}
	if p.Container != container {
		return View{}, userErr(403, "app %q belongs to another devbox", app)
	}
	if err := s.remove(ctx, *p); err != nil {
		return View{}, err
	}
	log.Printf("[devbox-publish] unpublished %s by session %s", s.cfg.Host(app), c.SessionID)
	return s.view(*p, "removed"), nil
}

// remove takes the route out first (the URL stops answering), then the relay,
// then the record. A failure leaves the record so the removal can be retried.
func (s *Service) remove(ctx context.Context, p Publication) error {
	file := routeFileName(p.App)
	if s.host.RouteExists(file) {
		if err := s.host.RemoveRoute(file); err != nil {
			return fmt.Errorf("removing the proxy route failed: %w", err)
		}
		if err := s.host.ReloadProxy(ctx); err != nil {
			return fmt.Errorf("reloading the proxy failed: %w", err)
		}
	}
	if err := s.host.RemoveForward(ctx, forwardName(p.App)); err != nil {
		return fmt.Errorf("removing the relay failed: %w", err)
	}
	if err := s.dns.Remove(ctx, s.cfg.Host(p.App)); err != nil {
		return fmt.Errorf("removing the DNS record of %s failed: %w", s.cfg.Host(p.App), err)
	}
	if s.resolver != nil {
		if err := s.resolver.Flush(ctx); err != nil {
			log.Printf("[devbox-publish] flushing the resolver after removing %s: %v", p.App, err)
		}
	}
	return s.store.DeleteDevboxPublication(ctx, p.App)
}

// CollectGarbage removes the publications whose container no longer exists.
// A Docker error that is not "no such container" removes nothing.
func (s *Service) CollectGarbage(ctx context.Context) (int, error) {
	if !s.Enabled() {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.store.ListDevboxPublications(ctx)
	if err != nil {
		return 0, err
	}
	gone := map[string]bool{}
	removed := 0
	var errs []error
	for _, p := range all {
		isGone, seen := gone[p.Container]
		if !seen {
			err := s.host.ContainerExists(ctx, p.Container)
			if err != nil && !errors.Is(err, ErrNoSuchContainer) {
				errs = append(errs, err)
				continue
			}
			isGone = errors.Is(err, ErrNoSuchContainer)
			gone[p.Container] = isGone
		}
		if !isGone {
			continue
		}
		if err := s.remove(ctx, p); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
		log.Printf("[devbox-publish] container %s is gone: unpublished %s", p.Container, s.cfg.Host(p.App))
	}
	return removed, errors.Join(errs...)
}

// RunGarbageCollector calls CollectGarbage every interval until ctx ends.
func (s *Service) RunGarbageCollector(ctx context.Context, interval time.Duration) {
	if !s.Enabled() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.CollectGarbage(ctx); err != nil {
				log.Printf("[devbox-publish] garbage collection: %v", err)
			}
		}
	}
}
