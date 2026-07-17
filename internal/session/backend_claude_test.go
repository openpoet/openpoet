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

func TestClaudeCodeBackendPassesConfiguredModel(t *testing.T) {
	backend := &ClaudeCodeBackend{}
	got := backend.BuildCLIArgs(&SessionConfig{
		SessionID:     "openpoet-session",
		BackendConfig: `{"model":"claude-fable-5"}`,
	})
	want := []string{"--session-id", "openpoet-session", "--model", "claude-fable-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}

func TestClaudeCodeBackendPassesOpenAIModelToClaudeHarness(t *testing.T) {
	backend := &ClaudeCodeBackend{}
	got := backend.BuildCLIArgs(&SessionConfig{
		SessionID:     "openpoet-session",
		BackendConfig: `{"provider":"openai_oauth","provider_config_id":3,"model":"gpt-5.6-sol[1m]"}`,
	})
	want := []string{"--session-id", "openpoet-session", "--model", "gpt-5.6-sol[1m]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}
