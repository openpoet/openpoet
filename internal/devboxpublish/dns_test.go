package devboxpublish

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeDNS records the names it was asked to create and remove.
type fakeDNS struct {
	records   map[string]bool
	failApply bool
	calls     []string
}

func (d *fakeDNS) Ensure(_ context.Context, host string) error {
	d.calls = append(d.calls, "ensure "+host)
	if d.failApply {
		return errors.New("edge refused")
	}
	d.records[host] = true
	return nil
}

func (d *fakeDNS) Remove(_ context.Context, host string) error {
	d.calls = append(d.calls, "remove "+host)
	delete(d.records, host)
	return nil
}

// cachingResolver behaves like dnsmasq: it caches a negative answer until it
// is flushed, and only sees records the fake DNS holds.
type cachingResolver struct {
	dns       *fakeDNS
	negCached map[string]bool
	flushes   int
	queries   int
	// lagQueries: the record is invisible for this many queries after it
	// appears (propagation).
	lagQueries int
}

func (r *cachingResolver) Flush(context.Context) error {
	r.flushes++
	r.negCached = map[string]bool{}
	return nil
}

func (r *cachingResolver) Resolves(_ context.Context, host string) (bool, error) {
	r.queries++
	if r.negCached[host] {
		return false, nil
	}
	if !r.dns.records[host] || r.lagQueries > 0 {
		r.lagQueries--
		r.negCached[host] = true
		return false, nil
	}
	return true, nil
}

func dnsService(t *testing.T) (*Service, *memStore, *fakeHost, *fakeDNS, *cachingResolver) {
	t.Helper()
	store, host := newMemStore(), newFakeHost()
	d := &fakeDNS{records: map[string]bool{}}
	r := &cachingResolver{dns: d, negCached: map[string]bool{}}
	cfg := testConfig()
	cfg.DNSWait = 2 * time.Second
	s := NewService(cfg, store, host, d, r)
	s.pollEvery = time.Millisecond
	return s, store, host, d, r
}

func TestPublishCreatesDNSAndWaitsForTheResolver(t *testing.T) {
	s, _, _, d, r := dnsService(t)
	// A client asked for the name before it existed: the resolver cached "no such name".
	r.negCached["alpha-web-dev9.example.com"] = true
	r.lagQueries = 2

	v, err := s.Publish(context.Background(), alpha, "alpha-web", 3000, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "created" || !d.records["alpha-web-dev9.example.com"] {
		t.Fatalf("view %+v records %v", v, d.records)
	}
	if r.flushes < 3 {
		t.Fatalf("the resolver must be flushed until the name resolves (flushes=%d)", r.flushes)
	}
	if ok, _ := r.Resolves(context.Background(), "alpha-web-dev9.example.com"); !ok {
		t.Fatal("name does not resolve after publish returned")
	}
}

func TestPublishRollsBackWhenTheNameNeverResolves(t *testing.T) {
	s, store, host, d, r := dnsService(t)
	s.cfg.DNSWait = 20 * time.Millisecond
	r.lagQueries = 1 << 30
	_, err := s.Publish(context.Background(), alpha, "alpha-web", 3000, "")
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("err = %v", err)
	}
	if len(store.rows) != 0 || len(host.forwards) != 0 || len(host.routes) != 0 || len(d.records) != 0 {
		t.Fatalf("left behind: rows=%v forwards=%v routes=%v dns=%v", store.rows, host.forwards, host.routes, d.records)
	}
}

func TestPublishRollsBackWhenDNSFails(t *testing.T) {
	s, store, host, d, _ := dnsService(t)
	d.failApply = true
	_, err := s.Publish(context.Background(), alpha, "alpha-web", 3000, "")
	if err == nil || !strings.Contains(err.Error(), "creating the DNS record") {
		t.Fatalf("err = %v", err)
	}
	if len(store.rows) != 0 || len(host.forwards) != 0 || len(host.routes) != 0 {
		t.Fatalf("left behind: rows=%v forwards=%v routes=%v", store.rows, host.forwards, host.routes)
	}
}

func TestUnpublishRemovesDNSAndFlushes(t *testing.T) {
	s, _, _, d, r := dnsService(t)
	ctx := context.Background()
	if _, err := s.Publish(ctx, alpha, "alpha-web", 3000, ""); err != nil {
		t.Fatal(err)
	}
	before := r.flushes
	if _, err := s.Unpublish(ctx, alpha, "alpha-web"); err != nil {
		t.Fatal(err)
	}
	if d.records["alpha-web-dev9.example.com"] || r.flushes != before+1 {
		t.Fatalf("records %v flushes %d->%d", d.records, before, r.flushes)
	}
	if got := d.calls[len(d.calls)-1]; got != "remove alpha-web-dev9.example.com" {
		t.Fatalf("last DNS call %q", got)
	}
}

func TestSeveralAppsInOneContainer(t *testing.T) {
	s, store, host, d, _ := dnsService(t)
	ctx := context.Background()
	a, err := s.Publish(ctx, alpha, "alpha-web", 3000, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Publish(ctx, alpha, "alpha-api", 4000, "")
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := store.GetDevboxPublication(ctx, "alpha-web")
	pb, _ := store.GetDevboxPublication(ctx, "alpha-api")
	if a.URL == b.URL || pa.HostPort == pb.HostPort {
		t.Fatalf("apps share a URL or a host port: %+v %+v", pa, pb)
	}
	if !strings.Contains(host.routes["op-alpha-web.caddy"], "127.0.0.1:28100") ||
		!strings.Contains(host.routes["op-alpha-api.caddy"], "127.0.0.1:28101") {
		t.Fatalf("routes: %v", host.routes)
	}
	list, _ := s.List(ctx, alpha)
	if len(list) != 2 {
		t.Fatalf("list: %+v", list)
	}
	// The same port twice under two names is allowed too (two hosts, one app).
	if _, err := s.Publish(ctx, alpha, "alpha-web2", 3000, ""); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"alpha-web", "alpha-api", "alpha-web2"} {
		if _, err := s.Unpublish(ctx, alpha, app); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.rows)+len(host.forwards)+len(host.routes)+len(d.records) != 0 {
		t.Fatalf("left behind: %v %v %v %v", store.rows, host.forwards, host.routes, d.records)
	}
}

func TestHostTemplate(t *testing.T) {
	base := map[string]string{
		EnvEnabled: "1", EnvSSHHosts: "192.0.2.9", EnvContainers: "a-devbox:a",
		EnvPortRange: "28100-28199", EnvRoutesDir: "/srv/routes",
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for key, val := range base {
			m[key] = val
		}
		m[k] = v
		return m
	}
	for tmpl, want := range map[string]string{
		"{app}-dev9.example.com":  "a-web-dev9.example.com",
		"{app}.dev9.example.com.": "a-web.dev9.example.com",
		"{APP}.Dev9.Example.com":  "a-web.dev9.example.com",
	} {
		cfg, err := parseConfig(mapEnv(with(EnvHostTemplate, tmpl)))
		if err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		if got := cfg.Host("a-web"); got != want {
			t.Errorf("%s: host %q, want %q", tmpl, got, want)
		}
		if cfg.DNSMode != DNSStatic || cfg.Resolver != "" || cfg.DNSWait != time.Minute {
			t.Errorf("defaults: %+v", cfg)
		}
	}
	for _, bad := range []string{"", "{app}", "example.com", "{app}.{app}.example.com", "{app}_x.example.com", "{app}..example.com"} {
		if _, err := parseConfig(mapEnv(with(EnvHostTemplate, bad))); err == nil {
			t.Errorf("template %q accepted", bad)
		}
	}
	for k, v := range map[string]string{
		EnvDNS: "cloud", EnvResolver: "192.0.2.9", EnvResolverCtr: "bad name", EnvDNSWait: "0",
	} {
		m := with(EnvHostTemplate, "{app}-dev9.example.com")
		m[k] = v
		if _, err := parseConfig(mapEnv(m)); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
	m := with(EnvHostTemplate, "{app}-dev9.example.com")
	m[EnvDNS], m[EnvDNSCommand] = "command", "relative/bin"
	if _, err := parseConfig(mapEnv(m)); err == nil {
		t.Error("relative DNS command accepted")
	}
	m[EnvDNSCommand], m[EnvResolver], m[EnvResolverCtr], m[EnvDNSWait] = "/usr/local/bin/devbox-dns", "192.0.2.9:53", "dns-cache", "30"
	cfg, err := parseConfig(mapEnv(m))
	if err != nil || cfg.DNSMode != DNSCommand || cfg.Resolver != "192.0.2.9:53" || cfg.ResolverContainer != "dns-cache" || cfg.DNSWait != 30*time.Second {
		t.Fatalf("full config: %+v %v", cfg, err)
	}
	if _, ok := NewDNS(cfg).(CommandDNS); !ok || NewResolver(cfg) == nil {
		t.Fatal("factories")
	}
}

// TestDNSResolverAgainstALocalServer checks the real resolver client against
// a tiny DNS server on loopback: an A answer resolves, NXDOMAIN does not.
func TestDNSResolverAgainstALocalServer(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no UDP on loopback")
	}
	defer pc.Close()
	go serveTinyDNS(pc, "known.example.com.")
	r := &DNSResolver{Addr: pc.LocalAddr().String()}
	if ok, err := r.Resolves(context.Background(), "known.example.com"); !ok || err != nil {
		t.Fatalf("known: %v %v", ok, err)
	}
	if ok, err := r.Resolves(context.Background(), "unknown.example.com"); ok || err != nil {
		t.Fatalf("unknown: %v %v", ok, err)
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("flush without a container is a no-op: %v", err)
	}
}

// serveTinyDNS answers A queries for name with 192.0.2.1 and NXDOMAIN
// otherwise; AAAA for name gets an empty answer.
func serveTinyDNS(pc net.PacketConn, name string) {
	buf := make([]byte, 512)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := buf[:n]
		if len(q) < 12 {
			continue
		}
		// Parse the question name.
		i, labels := 12, []string{}
		for i < len(q) && q[i] != 0 {
			l := int(q[i])
			labels = append(labels, string(q[i+1:i+1+l]))
			i += 1 + l
		}
		qEnd := i + 5 // zero byte + type + class
		if qEnd > len(q) {
			continue
		}
		qtype := int(q[i+1])<<8 | int(q[i+2])
		asked := strings.ToLower(strings.Join(labels, ".") + ".")
		resp := append([]byte{}, q[:qEnd]...)
		resp[2] = 0x81 // response, recursion desired
		resp[3] = 0x80 // recursion available, NOERROR
		resp[6], resp[7] = 0, 0
		resp[8], resp[9], resp[10], resp[11] = 0, 0, 0, 0
		if asked != name {
			resp[3] = 0x83 // NXDOMAIN
		} else if qtype == 1 {
			resp[7] = 1
			resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1)
		}
		pc.WriteTo(resp, addr)
	}
}
