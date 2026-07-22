package session

import (
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
