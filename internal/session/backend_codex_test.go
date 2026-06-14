package session

import (
	"reflect"
	"testing"
)

func TestCodexBuildCLIArgsUsesNativeTUI(t *testing.T) {
	backend := &CodexBackend{}
	cfg := &SessionConfig{
		BackendConfig: `{
			"runtime":"tui",
			"model":"gpt-5.5",
			"reasoning_effort":"high",
			"service_tier":"fast",
			"approval_policy":"never",
			"sandbox_mode":"danger-full-access"
		}`,
	}

	got := backend.BuildCLIArgs(cfg)
	want := []string{
		"--no-alt-screen",
		"--ask-for-approval", "never",
		"--sandbox", "danger-full-access",
		"--model", "gpt-5.5",
		"-c", `model_reasoning_effort="high"`,
		"-c", `service_tier="fast"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}

func TestCodexBuildCLIArgsResumeKeepsOptionsBeforeSessionID(t *testing.T) {
	backend := &CodexBackend{}
	cfg := &SessionConfig{
		IsReopen:          true,
		ProviderSessionID: "session-123",
		BackendConfig:     `{"runtime":"tui","approval_policy":"on-request","sandbox_mode":"workspace-write"}`,
	}

	got := backend.BuildCLIArgs(cfg)
	want := []string{
		"resume",
		"--no-alt-screen",
		"--ask-for-approval", "on-request",
		"--sandbox", "workspace-write",
		"session-123",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}

func TestCodexBuildCLIArgsResumeWithoutProviderIDDoesNotUseLast(t *testing.T) {
	backend := &CodexBackend{}
	cfg := &SessionConfig{
		IsReopen:      true,
		BackendConfig: `{"runtime":"tui","approval_policy":"on-request","sandbox_mode":"workspace-write"}`,
	}

	got := backend.BuildCLIArgs(cfg)
	want := []string{
		"resume",
		"--no-alt-screen",
		"--ask-for-approval", "on-request",
		"--sandbox", "workspace-write",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
}

func TestCodexRuntimeDefaultsToAppServer(t *testing.T) {
	if !useCodexAppServer("codex", "") {
		t.Fatalf("Codex should default to app-server runtime")
	}
	if !useCodexAppServer("codex", `{"runtime":"app-server"}`) {
		t.Fatalf("Codex app-server runtime should be accepted when explicit")
	}
	if useCodexAppServer("codex", `{"runtime":"tui"}`) {
		t.Fatalf("Codex native TUI runtime should be explicit opt-in")
	}
	if useCodexAppServer("claude_code", `{"runtime":"app-server"}`) {
		t.Fatalf("non-Codex backend must not use Codex app-server")
	}
}
