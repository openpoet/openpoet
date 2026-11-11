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

// A Claude session is told where the openpoet binary is, so the CLI the docs
// steering and the html-artifact skill name ($OPENPOET_BIN cli call ...) works.
func TestClaudeCodeBackendExportsTheOpenPoetBinary(t *testing.T) {
	backend := &ClaudeCodeBackend{}
	env := backend.BuildEnvVars(&SessionConfig{SessionID: "s1", ServerAddr: "localhost:8080", ExecPath: "/opt/openpoet/openpoet"})
	if env["OPENPOET_BIN"] != "/opt/openpoet/openpoet" || env["OPENPOET_HOOK_URL"] != "http://localhost:8080" || env["OPENPOET_SESSION_ID"] != "s1" {
		t.Fatalf("env = %v", env)
	}
	if _, set := backend.BuildEnvVars(&SessionConfig{SessionID: "s1", ServerAddr: "localhost:8080"})["OPENPOET_BIN"]; set {
		t.Fatal("OPENPOET_BIN set without a resolved binary path")
	}
}
