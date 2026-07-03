package session

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOpenCodeBackendBuildCLIArgs(t *testing.T) {
	backend := &OpenCodeBackend{}
	cfg := &SessionConfig{
		AppendSystemPrompt: "task context",
		BackendConfig:      `{"model":"anthropic/claude-sonnet-4-5","agent":"plan","auto_approve":true}`,
	}

	got := backend.BuildCLIArgs(cfg)
	want := []string{"--model", "anthropic/claude-sonnet-4-5", "--agent", "plan", "--prompt", "task context", "--auto"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestOpenCodeBackendBuildCLIArgsForResume(t *testing.T) {
	backend := &OpenCodeBackend{}
	cfg := &SessionConfig{
		IsReopen:          true,
		ProviderSessionID: "opencode-session-123",
		BackendConfig:     `{"model":"anthropic/claude-sonnet-4-5"}`,
	}

	got := backend.BuildCLIArgs(cfg)
	want := []string{"--session", "opencode-session-123", "--model", "anthropic/claude-sonnet-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestOpenCodeBackendBuildEnvVarsUsesBinaryOverride(t *testing.T) {
	backend := &OpenCodeBackend{}
	cfg := &SessionConfig{
		SessionID:     "sid",
		ServerAddr:    "127.0.0.1:8080",
		ExecPath:      "/tmp/openpoet",
		BackendConfig: `{"binary_path":"/opt/opencode"}`,
	}

	got := backend.BuildEnvVars(cfg)
	if got["OPENPOET_HOOK_URL"] != "http://127.0.0.1:8080" {
		t.Fatalf("OPENPOET_HOOK_URL = %q", got["OPENPOET_HOOK_URL"])
	}
	if got["OPENPOET_SESSION_ID"] != "sid" {
		t.Fatalf("OPENPOET_SESSION_ID = %q", got["OPENPOET_SESSION_ID"])
	}
	if got["OPENPOET_BIN"] != "/tmp/openpoet" {
		t.Fatalf("OPENPOET_BIN = %q", got["OPENPOET_BIN"])
	}
	if got["OPENPOET_BACKEND_BINARY"] != "/opt/opencode" {
		t.Fatalf("OPENPOET_BACKEND_BINARY = %q", got["OPENPOET_BACKEND_BINARY"])
	}
}

func TestOpenCodeBackendBuildEnvVarsInjectsMCPConfigContent(t *testing.T) {
	backend := &OpenCodeBackend{}
	cfg := &SessionConfig{
		SessionID:     "sid",
		ServerAddr:    "127.0.0.1:8080",
		ExecPath:      "/tmp/openpoet",
		BackendConfig: `{}`,
		MCPConfigJSON: `{"mcpServers":{"openpoet":{"command":"/tmp/openpoet","args":["mcp-serve","--session-id","sid"],"env":{"A":"B"}}}}`,
	}

	got := backend.BuildEnvVars(cfg)
	raw := got["OPENCODE_CONFIG_CONTENT"]
	if raw == "" {
		t.Fatal("OPENCODE_CONFIG_CONTENT was not set")
	}

	var content map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		t.Fatal(err)
	}
	mcp := content["mcp"].(map[string]interface{})
	openpoet := mcp["openpoet"].(map[string]interface{})
	if openpoet["type"] != "local" {
		t.Fatalf("openpoet type = %q", openpoet["type"])
	}
	command := openpoet["command"].([]interface{})
	if len(command) != 4 || command[0] != "/tmp/openpoet" || command[1] != "mcp-serve" || command[2] != "--session-id" || command[3] != "sid" {
		t.Fatalf("openpoet command = %#v", command)
	}
	env := openpoet["environment"].(map[string]interface{})
	if env["A"] != "B" {
		t.Fatalf("openpoet env A = %q", env["A"])
	}
}

func TestGetBackendReturnsOpenCode(t *testing.T) {
	if got := GetBackend("opencode").Type(); got != BackendOpenCode {
		t.Fatalf("backend type = %q, want %q", got, BackendOpenCode)
	}
}
