package handlers

import "testing"

func TestValidateProjectBackendAcceptsRemoteCodex(t *testing.T) {
	if got := validateProjectBackend("remote", "codex"); got != "" {
		t.Fatalf("expected remote Codex backend to be accepted, got %q", got)
	}
}

func TestValidateProjectBackendAcceptsLocalCodex(t *testing.T) {
	if got := validateProjectBackend("local", "codex"); got != "" {
		t.Fatalf("expected local Codex backend to be accepted, got %q", got)
	}
}

func TestValidateProjectBackendRejectsUnknownBackend(t *testing.T) {
	if got := validateProjectBackend("local", "unknown"); got == "" {
		t.Fatal("expected unknown backend to be rejected")
	}
}
