package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/sessionmeta"
	"openpoet/internal/websocket"
)

func TestClaudeScreenEffortPrefersFooter(t *testing.T) {
	screen := "Claude Code v2.1.295\nOpus 5.5 with high effort · Claude Team\n> \n● max · /effort"
	if got := claudeScreenEffort(screen); got != "max" {
		t.Fatalf("effort = %q, want the footer's max", got)
	}
	if got := claudeScreenEffort("Opus 5.5 with xhigh effort"); got != "xhigh" {
		t.Fatalf("banner effort = %q", got)
	}
	if got := claudeScreenEffort("nothing about effort here, high hopes"); got != "" {
		t.Fatalf("false positive %q", got)
	}
}

func TestClaudeCodeBackendPassesExplicitEffort(t *testing.T) {
	got := (&ClaudeCodeBackend{}).BuildCLIArgs(&SessionConfig{
		SessionID:     "openpoet-session",
		BackendConfig: sessionmeta.ApplyRuntimeValues(`{}`, "opus", "high"),
	})
	want := []string{"--session-id", "openpoet-session", "--model", "opus", "--effort", "high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCLIArgs() = %#v, want %#v", got, want)
	}
	legacy := (&ClaudeCodeBackend{}).BuildCLIArgs(&SessionConfig{SessionID: "s", BackendConfig: `{"provider":"anthropic","model":"fable","effort":"max"}`})
	if !reflect.DeepEqual(legacy[len(legacy)-2:], []string{"--effort", "max"}) {
		t.Fatalf("legacy effort key not passed: %#v", legacy)
	}
}

func TestCodexBackendPassesExplicitModelAndEffort(t *testing.T) {
	args := (&CodexBackend{}).BuildCLIArgs(&SessionConfig{BackendConfig: sessionmeta.ApplyRuntimeValues(`{"runtime":"tui"}`, "gpt-6-astra", "xhigh")})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model gpt-6-astra") || !strings.Contains(joined, `-c model_reasoning_effort="xhigh"`) {
		t.Fatalf("args = %s", joined)
	}
}

func TestCodexThreadParamsCarryEffortAsConfigOverride(t *testing.T) {
	r := &CodexRunner{workDir: "/tmp/w", cfg: &SessionConfig{
		BackendConfig: `{"model":"gpt-6-astra","reasoning_effort":"ultra"}`,
		MCPConfigJSON: `{"mcpServers":{"openpoet":{"command":"x"}}}`,
	}}
	params := r.threadParams()
	if params["model"] != "gpt-6-astra" {
		t.Fatalf("model = %#v", params["model"])
	}
	config, _ := params["config"].(map[string]interface{})
	if config["model_reasoning_effort"] != "ultra" || config["mcp_servers"] == nil {
		t.Fatalf("config = %#v", params["config"])
	}
}

func TestCodexThreadSettingsFromResult(t *testing.T) {
	model, effort := codexThreadSettingsFromResult([]byte(`{"thread":{"id":"t1","model":"x"},"model":"gpt-6-astra","reasoningEffort":"ultra"}`))
	if model != "gpt-6-astra" || effort != "ultra" {
		t.Fatalf("top-level = %q %q", model, effort)
	}
	model, effort = codexThreadSettingsFromResult([]byte(`{"thread":{"id":"t1","model":"gpt-6-luna","reasoningEffort":"low"}}`))
	if model != "gpt-6-luna" || effort != "low" {
		t.Fatalf("thread fallback = %q %q", model, effort)
	}
	var reported [][2]string
	r := &CodexRunner{cfg: &SessionConfig{OnEffectiveSettings: func(m, e string) { reported = append(reported, [2]string{m, e}) }}}
	r.reportEffectiveSettings("gpt-6-astra", "")
	r.reportEffectiveSettings("", "")
	if len(reported) != 1 || reported[0][0] != "gpt-6-astra" {
		t.Fatalf("reported = %#v", reported)
	}
}

func TestReadCodexRolloutTurnContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []string{
		`{"type":"session_meta","payload":{"id":"abc"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-6-luna","effort":"low"}}`,
		`{"type":"response_item","payload":{"text":"turn_context mentioned in text"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-6.1-sol","reasoning_effort":null,"collaboration_mode":{"settings":{"reasoning_effort":"high"}}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model, effort, ok := readCodexRolloutTurnContext(path)
	if !ok || model != "gpt-6.1-sol" || effort != "high" {
		t.Fatalf("last turn_context = %q %q %v", model, effort, ok)
	}
	if _, _, ok := readCodexRolloutTurnContext(filepath.Join(t.TempDir(), "missing.jsonl")); ok {
		t.Fatal("missing rollout reported settings")
	}
}

func newSettingsTestManager(t *testing.T, defaults map[string]string) *Manager {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for key, value := range defaults {
		if err := db.SetSetting(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	return NewManager(db, websocket.NewHub(), "localhost:0")
}

func TestStartSessionRefusesWithoutExplicitSettings(t *testing.T) {
	m := newSettingsTestManager(t, nil)
	project := &database.Project{ID: 1, Name: "p", Path: t.TempDir(), Type: "local", Backend: "codex", BackendConfig: `{"runtime":"tui"}`}
	_, err := m.StartSession(context.Background(), project, nil)
	if !errors.Is(err, ErrSessionSettingsUnresolved) || !strings.Contains(err.Error(), "default_model_codex") {
		t.Fatalf("StartSession err = %v, want unresolved naming the setting", err)
	}
	sessions, _ := m.db.ListSessions(context.Background())
	if len(sessions) != 0 {
		t.Fatalf("a refused start must not create a session row, got %d", len(sessions))
	}
	_, err = m.StartRemoteSession(context.Background(), &database.Project{ID: 1, Type: "remote", Backend: "claude_code", BackendConfig: `{}`}, nil, nil)
	if err == nil {
		t.Fatal("remote start without settings must be refused")
	}
}

func TestStartSessionRejectsRequestedDefaultEffortSyntax(t *testing.T) {
	m := newSettingsTestManager(t, map[string]string{"default_model_claude_code": "opus", "default_effort_claude_code": "high"})
	project := &database.Project{ID: 1, Name: "p", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: `{}`}
	_, err := m.StartSession(context.Background(), project, map[string]string{RequestEffortEnv: "turbo"})
	if !errors.Is(err, ErrInvalidSessionSetting) {
		t.Fatalf("err = %v, want invalid setting", err)
	}
}

func TestResolveReopenSettingsKeepsSessionThenFallsBack(t *testing.T) {
	m := newSettingsTestManager(t, map[string]string{"default_model_codex": "gpt-6.1-sol", "default_effort_codex": "medium"})
	ctx := context.Background()

	kept := &database.Session{Backend: "codex", RequestedModel: "gpt-6-astra", Effort: "high", Model: "gpt-6-astra"}
	got, err := m.resolveReopenSettings(ctx, kept, `{"model":"gpt-6-luna"}`, "", "")
	if err != nil || got.Model != "gpt-6-astra" || got.ModelSource != sessionmeta.SourceSession || got.Effort != "high" || got.EffortSource != sessionmeta.SourceSession {
		t.Fatalf("kept = %+v err=%v", got, err)
	}

	got, err = m.resolveReopenSettings(ctx, kept, `{}`, "gpt-6-luna", "low")
	if err != nil || got.Model != "gpt-6-luna" || got.ModelSource != sessionmeta.SourceRequest || got.EffortSource != sessionmeta.SourceRequest {
		t.Fatalf("request = %+v err=%v", got, err)
	}

	// A session from before explicit settings: "default" is resolved again.
	legacy := &database.Session{Backend: "codex", RequestedModel: "default", Effort: "default", Model: "default"}
	got, err = m.resolveReopenSettings(ctx, legacy, `{"reasoning_effort":"xhigh"}`, "", "")
	if err != nil || got.Model != "gpt-6.1-sol" || got.ModelSource != sessionmeta.SourceGlobal || got.Effort != "xhigh" || got.EffortSource != sessionmeta.SourceProject {
		t.Fatalf("legacy = %+v err=%v", got, err)
	}

	empty := newSettingsTestManager(t, nil)
	if _, err := empty.resolveReopenSettings(ctx, legacy, `{}`, "", ""); !errors.Is(err, ErrSessionSettingsUnresolved) {
		t.Fatalf("legacy without config err = %v", err)
	}
}

func TestSetSessionModelAndEffortRefuseDefault(t *testing.T) {
	m := newSettingsTestManager(t, nil)
	if _, err := m.SetSessionModel(context.Background(), "nope", "default", 0); !errors.Is(err, ErrInvalidSessionSetting) {
		t.Fatalf("model default err = %v", err)
	}
	if _, err := m.SetSessionEffort(context.Background(), "nope", "reset", 0); !errors.Is(err, ErrInvalidSessionSetting) {
		t.Fatalf("effort reset err = %v", err)
	}
	if _, err := validateSessionEffort("ultra"); err != nil {
		t.Fatalf("ultra must be a known level: %v", err)
	}
}

func TestTakeRuntimeRequestRemovesMarkers(t *testing.T) {
	env := map[string]string{RequestModelEnv: " opus ", RequestEffortEnv: "high", "KEEP": "1"}
	model, effort := takeRuntimeRequest(env)
	if model != "opus" || effort != "high" || len(env) != 1 {
		t.Fatalf("model=%q effort=%q env=%v", model, effort, env)
	}
}
