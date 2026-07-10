package session

import (
	"reflect"
	"testing"
)

func TestClaudeCodeBackendReopensProviderSession(t *testing.T) {
	backend := &ClaudeCodeBackend{}
	got := backend.BuildCLIArgs(&SessionConfig{
		SessionID:         "openpoet-session",
		ProviderSessionID: "claude-resumed-session",
		IsReopen:          true,
	})
	want := []string{"--resume", "claude-resumed-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}

func TestClaudeCodeBackendReopenFallsBackToOpenPoetSession(t *testing.T) {
	backend := &ClaudeCodeBackend{}
	got := backend.BuildCLIArgs(&SessionConfig{
		SessionID: "openpoet-session",
		IsReopen:  true,
	})
	want := []string{"--resume", "openpoet-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}
