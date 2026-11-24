package devboxpublish

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// DNS gives a published host its DNS record and takes it away. It is the only
// part that depends on how names are published (a static wildcard, a record
// per app made by an external tool...), so changing that scheme means another
// implementation and nothing else.
type DNS interface {
	// Ensure makes host resolve to the edge. Idempotent.
	Ensure(ctx context.Context, host string) error
	// Remove takes the record of host away. Idempotent.
	Remove(ctx context.Context, host string) error
}

// Resolver is the resolver the private network's clients use.
type Resolver interface {
	// Flush drops the resolver's cache, so a negative answer cached before the
	// record existed (or a positive one after it is gone) is forgotten.
	Flush(ctx context.Context) error
	// Resolves reports whether the resolver answers with an address for host.
	// (false, nil) is a definite "no such name"; an error is a failed query.
	Resolves(ctx context.Context, host string) (bool, error)
}

// NewDNS returns the DNS implementation the configuration names.
func NewDNS(cfg Config) DNS {
	if cfg.DNSMode == DNSCommand {
		return CommandDNS{Bin: cfg.DNSCommand}
	}
	return StaticDNS{}
}

// NewResolver returns the resolver of the configuration, or nil when none is
// configured (publishing then does not wait for DNS).
func NewResolver(cfg Config) Resolver {
	if cfg.Resolver == "" {
		return nil
	}
	return &DNSResolver{Addr: cfg.Resolver, Container: cfg.ResolverContainer}
}

// StaticDNS is a wildcard record that already covers every published host.
type StaticDNS struct{}

// Ensure implements DNS.
func (StaticDNS) Ensure(context.Context, string) error { return nil }

// Remove implements DNS.
func (StaticDNS) Remove(context.Context, string) error { return nil }

// CommandDNS runs an executable: "<bin> ensure <host>" and "<bin> remove <host>".
// The executable owns the credentials and the scope of what it may change.
type CommandDNS struct{ Bin string }

// Ensure implements DNS.
func (d CommandDNS) Ensure(ctx context.Context, host string) error {
	_, err := run(ctx, 90*time.Second, nil, d.Bin, "ensure", host)
	return err
}

// Remove implements DNS.
func (d CommandDNS) Remove(ctx context.Context, host string) error {
	_, err := run(ctx, 90*time.Second, nil, d.Bin, "remove", host)
	return err
}

// DNSResolver queries one resolver directly (like `dig @addr`) and, when
// Container is set, flushes its cache by sending SIGHUP to that container
// (dnsmasq drops its cache on SIGHUP).
type DNSResolver struct {
	Addr      string
	Container string
}

// Flush implements Resolver.
func (r *DNSResolver) Flush(ctx context.Context) error {
	if r.Container == "" {
		return nil
	}
	_, err := run(ctx, 15*time.Second, nil, "docker", "kill", "--signal", "HUP", r.Container)
	return err
}

// Resolves implements Resolver.
func (r *DNSResolver) Resolves(ctx context.Context, host string) (bool, error) {
	res := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, r.Addr)
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := res.LookupHost(ctx, host+".") // absolute: no search domains
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return false, nil
		}
		return false, err
	}
	return len(addrs) > 0, nil
}

// waitResolves flushes the resolver and polls it until it answers for host,
// flushing again between attempts (a client may have asked, and re-cached a
// negative answer, while the record was propagating).
func waitResolves(ctx context.Context, r Resolver, host string, timeout, every time.Duration) error {
	if r == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := r.Flush(ctx); err != nil {
			lastErr = fmt.Errorf("flushing the resolver cache failed: %w", err)
		}
		ok, err := r.Resolves(ctx, host)
		if ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().Add(every).After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("%s does not resolve on the private network's resolver after %s: %v", host, timeout, lastErr)
			}
			return fmt.Errorf("%s does not resolve on the private network's resolver after %s (no such name)", host, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
