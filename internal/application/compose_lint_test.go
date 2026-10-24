package application

import (
	"context"
	"errors"
	"testing"
)

// fakeReservedPorts records installation-specific reserved ports, like the
// environment_resources rows database.DB.IsPortReserved reads.
type fakeReservedPorts struct {
	ports map[int]bool
	err   error
}

func (f fakeReservedPorts) IsPortReserved(_ context.Context, port int) (bool, error) {
	return f.ports[port], f.err
}

func TestLintComposeReserved(t *testing.T) {
	ctx := context.Background()
	compose := func(port string) string {
		return "services:\n  a:\n    ports:\n      - \"" + port + ":80\"\n"
	}
	store := fakeReservedPorts{ports: map[int]bool{8090: true}}

	if err := LintCompose(compose("8090")); err != nil {
		t.Fatalf("static lint must not hardcode installation-specific ports: %v", err)
	}
	if err := LintComposeReserved(ctx, compose("8090"), store); ErrorCode(err) != "compose_reserved_port" {
		t.Fatalf("port reserved in the store: got %v, want compose_reserved_port", err)
	}
	if err := LintComposeReserved(ctx, compose("127.0.0.1:8085-8090"), store); ErrorCode(err) != "compose_reserved_port" {
		t.Fatalf("range ending on a reserved port: got %v, want compose_reserved_port", err)
	}
	if err := LintComposeReserved(ctx, compose("8091"), store); err != nil {
		t.Fatalf("free port refused: %v", err)
	}
	if err := LintComposeReserved(ctx, compose("8081"), store); ErrorCode(err) != "compose_reserved_port" {
		t.Fatalf("static reservations still apply: got %v", err)
	}
	if err := LintComposeReserved(ctx, compose("8090"), fakeSettings{}); err != nil {
		t.Fatalf("a store without reserved ports adds no check: %v", err)
	}
	failing := fakeReservedPorts{err: errors.New("db down")}
	if err := LintComposeReserved(ctx, compose("3000"), failing); err == nil {
		t.Fatal("a failing reserved-port lookup must refuse (fail closed)")
	}
}
