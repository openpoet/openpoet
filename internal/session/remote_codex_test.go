package session

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestBuildWindowsBatchScriptUsesSelectedBackendCommand(t *testing.T) {
	script := buildWindowsBatchScript(
		map[string]string{"OPENPOET_SESSION_ID": "session-1"},
		"/C:/Users/ADM/project",
		"codex",
		[]string{"--no-alt-screen"},
	)

	if !strings.Contains(script, `"codex" "--no-alt-screen"`) {
		t.Fatalf("expected launcher to execute codex, got:\n%s", script)
	}
	if strings.Contains(script, "\r\nclaude ") {
		t.Fatalf("launcher should not hard-code claude:\n%s", script)
	}
}

func TestShellCommandWordQuotesCustomPath(t *testing.T) {
	got := shellCommandWord("/opt/OpenAI Codex/codex")
	want := "'/opt/OpenAI Codex/codex'"
	if got != want {
		t.Fatalf("shellCommandWord() = %q, want %q", got, want)
	}
}

func TestRemoteCodexUsesConfiguredBinaryOverride(t *testing.T) {
	runner := &RemoteRunner{
		backend: &CodexBackend{},
		envVars: map[string]string{
			"OPENPOET_BACKEND_BINARY": `C:\Users\ADM\AppData\Roaming\npm\codex.cmd`,
		},
	}

	got := runner.backendCommand()
	want := `C:\Users\ADM\AppData\Roaming\npm\codex.cmd`
	if got != want {
		t.Fatalf("backendCommand() = %q, want %q", got, want)
	}
}

func TestBuildRemoteCodexPOSIXCommandLoadsUserPath(t *testing.T) {
	cmd := buildRemoteCodexPOSIXCommand(
		map[string]string{"OPENPOET_SESSION_ID": "session-1"},
		"/home/user/projects/example",
		"codex",
	)

	for _, want := range []string{
		"bash -c ",
		"$HOME/.profile",
		"$HOME/.bash_profile",
		"$HOME/.bashrc",
		".nvm/versions/node/*/bin",
		"$HOME/.bun/bin",
		"export OPENPOET_SESSION_ID=",
		"OPENPOET_CODEX_BIN=",
		"codex",
		"command -v \"$OPENPOET_CODEX_BIN\"",
		"non-interactive SSH shell",
		"exec \"$OPENPOET_CODEX_BIN\" app-server",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected command to contain %q, got:\n%s", want, cmd)
		}
	}
}

func TestCodexRunnerStderrSummaryKeepsTail(t *testing.T) {
	runner := &CodexRunner{}
	for _, line := range []string{"one", "two", "three", "four", "five", "six"} {
		runner.recordStderrLine(line)
	}

	got := runner.stderrSummary()
	if strings.Contains(got, "one") {
		t.Fatalf("stderrSummary() should drop old lines, got %q", got)
	}
	for _, want := range []string{"two", "three", "four", "five", "six"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderrSummary() missing %q in %q", want, got)
		}
	}
}

func TestRemoteRunnerInjectsCodexOpenPoetMCPThroughTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	runner := &RemoteRunner{
		backend:        &CodexBackend{},
		tunnelListener: listener,
		envVars: map[string]string{
			"OPENPOET_SESSION_ID":      "session-1",
			"OPENPOET_MCP_CONFIG_JSON": `{"mcpServers":{"openpoet":{"command":"openpoet"}}}`,
		},
		cliArgs: []string{"--no-alt-screen"},
	}

	runner.rewriteMCPConfigForRemote()

	if _, ok := runner.envVars["OPENPOET_MCP_CONFIG_JSON"]; ok {
		t.Fatal("internal MCP config env var should not be exported to the remote process")
	}
	got := strings.Join(runner.cliArgs, " ")
	if !strings.Contains(got, "-c mcp_servers.openpoet.url=") {
		t.Fatalf("expected Codex -c MCP override, got %#v", runner.cliArgs)
	}
	if !strings.Contains(got, "/mcp?session_id=session-1") {
		t.Fatalf("expected session-scoped MCP URL, got %#v", runner.cliArgs)
	}
	if strings.Contains(got, "bearer_token_env_var") {
		t.Fatalf("bearer override should be omitted when no session token is present, got %#v", runner.cliArgs)
	}
}

func TestRemoteRunnerInjectsCodexBearerEnvVarWhenTokenPresent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	runner := &RemoteRunner{
		backend:        &CodexBackend{},
		tunnelListener: listener,
		envVars: map[string]string{
			"OPENPOET_SESSION_ID":      "session-1",
			"OPENPOET_SESSION_TOKEN":   "opst1_session-1.secret",
			"OPENPOET_MCP_CONFIG_JSON": `{"mcpServers":{"openpoet":{"command":"openpoet"}}}`,
		},
		cliArgs: []string{"--no-alt-screen"},
	}

	runner.rewriteMCPConfigForRemote()

	got := strings.Join(runner.cliArgs, " ")
	if !strings.Contains(got, `-c mcp_servers.openpoet.bearer_token_env_var="OPENPOET_SESSION_TOKEN"`) {
		t.Fatalf("expected bearer_token_env_var override pointing at the env var name, got %#v", runner.cliArgs)
	}
	if strings.Contains(got, "opst1_session-1.secret") {
		t.Fatalf("the token value itself must never appear in CLI args, got %#v", runner.cliArgs)
	}
}

// decodeRemoteCodexOpenPoetEntry runs the app-server MCP rewrite and returns the
// resulting "openpoet" server entry as Codex would parse it.
func decodeRemoteCodexOpenPoetEntry(t *testing.T, cfg *SessionConfig, listener net.Listener) map[string]interface{} {
	t.Helper()
	rewriteCodexMCPConfigForRemote(cfg, listener)
	var config struct {
		MCPServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg.MCPConfigJSON), &config); err != nil {
		t.Fatalf("rewritten MCP config is not valid JSON: %v (%s)", err, cfg.MCPConfigJSON)
	}
	entry, ok := config.MCPServers["openpoet"]
	if !ok {
		t.Fatalf("openpoet server missing from rewritten config: %s", cfg.MCPConfigJSON)
	}
	return entry
}

// A remote codex/app-server session used to publish its bearer under a "headers"
// key, which Codex's mcp_servers schema does not define — it was dropped, the
// MCP client called the tunnel anonymously, and every write (openpoet_create_document
// among them) came back 401 while reads kept working.
func TestRewriteCodexMCPConfigForRemoteUsesBearerTokenEnvVar(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := &SessionConfig{
		SessionID:     "session-1",
		MCPToken:      "opst1_session-1.secret",
		MCPConfigJSON: `{"mcpServers":{"openpoet":{"command":"openpoet","args":["mcp-serve"]}}}`,
	}
	entry := decodeRemoteCodexOpenPoetEntry(t, cfg, listener)

	if got := entry["bearer_token_env_var"]; got != sessionTokenEnvVar {
		t.Fatalf("expected bearer_token_env_var=%q, got %#v (entry %#v)", sessionTokenEnvVar, got, entry)
	}
	if _, ok := entry["headers"]; ok {
		t.Fatal(`"headers" is not a Codex mcp_servers key — Codex ignores it and the session calls the tunnel anonymously`)
	}
	if strings.Contains(cfg.MCPConfigJSON, cfg.MCPToken) {
		t.Fatalf("the token value itself must never appear in the config blob, got %s", cfg.MCPConfigJSON)
	}
	if url, _ := entry["url"].(string); !strings.HasSuffix(url, "/mcp?session_id=session-1") {
		t.Fatalf("expected session-scoped MCP URL, got %#v", entry["url"])
	}
}

func TestRewriteCodexMCPConfigForRemoteOmitsBearerWithoutToken(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := &SessionConfig{
		SessionID:     "session-1",
		MCPConfigJSON: `{"mcpServers":{"openpoet":{"command":"openpoet"}}}`,
	}
	entry := decodeRemoteCodexOpenPoetEntry(t, cfg, listener)

	if _, ok := entry["bearer_token_env_var"]; ok {
		t.Fatalf("bearer override should be omitted when no session token is minted, got %#v", entry)
	}
}

// The env var name shipped in the Codex config only authenticates anything if the
// backend actually exports a value under that same name.
func TestCodexBackendExportsSessionTokenUnderTheNameCodexReads(t *testing.T) {
	env := (&CodexBackend{}).BuildEnvVars(&SessionConfig{
		ServerAddr: "127.0.0.1:8080",
		SessionID:  "session-1",
		MCPToken:   "opst1_session-1.secret",
	})

	if env[sessionTokenEnvVar] != "opst1_session-1.secret" {
		t.Fatalf("expected the bearer exported as %s, got %#v", sessionTokenEnvVar, env)
	}
}
