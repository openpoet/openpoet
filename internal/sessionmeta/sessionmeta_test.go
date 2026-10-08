package sessionmeta

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestApplyRuntimeValues(t *testing.T) {
	raw := ApplyRuntimeValues(`{"model":"gpt-project","reasoning_effort":"low","runtime":"app-server"}`, "gpt-session", "high")
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "gpt-session" || cfg["reasoning_effort"] != "high" || cfg["runtime"] != "app-server" {
		t.Fatalf("merged config = %#v", cfg)
	}

	raw = ApplyRuntimeValues(raw, "default", "default")
	cfg = make(map[string]interface{})
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["model"]; ok {
		t.Fatalf("default model should remove override: %#v", cfg)
	}
	if _, ok := cfg["reasoning_effort"]; ok {
		t.Fatalf("default effort should remove override: %#v", cfg)
	}
}

func TestFromProjectConfigKeepsBackendMetadataIsolated(t *testing.T) {
	foreign := `{"runtime":"app-server","model":"gpt-5.6-sol","reasoning_effort":"xhigh","approval_policy":"never"}`
	claude := FromProjectConfig("claude_code", foreign)
	if claude.Model != "" || claude.Effort != "" || claude.Harness != "claude_code" {
		t.Fatalf("foreign Codex metadata leaked into Claude Code: %+v", claude)
	}

	codex := FromProjectConfig("codex", foreign)
	if codex.Model != "gpt-5.6-sol" || codex.Effort != "xhigh" || codex.Harness != "codex/app-server" {
		t.Fatalf("Codex metadata was not preserved: %+v", codex)
	}
}

func TestFromProjectConfigDescribesClaudeCodeOpenAIProvider(t *testing.T) {
	meta := FromProjectConfig("claude_code", `{"provider":"openai_oauth","provider_config_id":7,"model":"gpt-5.6-sol[1m]"}`)
	if meta.Model != "gpt-5.6-sol[1m]" || meta.Harness != "claude_code/openai" {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.HarnessDetails == "" {
		t.Fatal("OpenAI provider details are missing")
	}
}

func TestFromProjectConfigReadsClaudeCodeEffort(t *testing.T) {
	meta := FromProjectConfig("claude_code", `{"provider":"anthropic","model":"opus","reasoning_effort":"high"}`)
	if meta.Model != "opus" || meta.Effort != "high" {
		t.Fatalf("metadata = %+v", meta)
	}
	if legacy := FromProjectConfig("claude_code", `{"provider":"anthropic","effort":"max"}`); legacy.Effort != "max" {
		t.Fatalf("legacy effort key = %+v", legacy)
	}
}

func TestResolvePrefersRequestThenProjectThenGlobal(t *testing.T) {
	global := Defaults{Model: "gpt-6-luna", Effort: "low"}
	project := `{"runtime":"app-server","model":"gpt-6-astra"}`

	got, err := Resolve("codex", project, "", "", global)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-6-astra" || got.ModelSource != SourceProject || got.Effort != "low" || got.EffortSource != SourceGlobal {
		t.Fatalf("project/global = %+v", got)
	}

	got, err = Resolve("codex", project, "gpt-6.1-sol", "xhigh", global)
	if err != nil || got.Model != "gpt-6.1-sol" || got.ModelSource != SourceRequest || got.Effort != "xhigh" || got.EffortSource != SourceRequest {
		t.Fatalf("request = %+v err=%v", got, err)
	}

	// "default" is never an explicit value: it falls through to the next source.
	got, err = Resolve("codex", `{"model":"default","reasoning_effort":"default"}`, "default", "reset", global)
	if err != nil || got.Model != "gpt-6-luna" || got.ModelSource != SourceGlobal || got.EffortSource != SourceGlobal {
		t.Fatalf("default fallthrough = %+v err=%v", got, err)
	}
}

func TestResolveRefusesWithoutExplicitSettings(t *testing.T) {
	_, err := Resolve("claude_code", `{}`, "", "", Defaults{Model: "opus"})
	if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "effort") || strings.Contains(err.Error(), "model and") {
		t.Fatalf("missing effort error = %v", err)
	}
	if !strings.Contains(err.Error(), "default_effort_claude_code") {
		t.Fatalf("error must name the setting to configure: %v", err)
	}
	_, err = Resolve("codex", `{}`, "", "", Defaults{})
	if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "model and effort") {
		t.Fatalf("missing both = %v", err)
	}
}

func TestResolveSkipsGlobalModelForClaudeCodeOnOpenAI(t *testing.T) {
	_, err := Resolve("claude_code", `{"provider":"openai_oauth","provider_config_id":3}`, "", "high", Defaults{Model: "opus", Effort: "high"})
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("an Anthropic global model must not serve the OpenAI provider, err=%v", err)
	}
}

func TestResolveBackendsWithoutSelectableSettings(t *testing.T) {
	got, err := Resolve("copilot", `{}`, "", "", Defaults{})
	if err != nil || got != (RuntimeSettings{}) {
		t.Fatalf("copilot = %+v err=%v", got, err)
	}
	got, err = Resolve("opencode", `{"model":"anthropic/claude-sonnet-5"}`, "", "", Defaults{})
	if err != nil || got.Model != "anthropic/claude-sonnet-5" || got.Effort != "" {
		t.Fatalf("opencode = %+v err=%v", got, err)
	}
}

func TestSessionValuesLinesNeverSayDefault(t *testing.T) {
	legacy := SessionValues{Backend: "codex", Model: "gpt-6.1-sol", RequestedModel: "default", Effort: "default"}
	if got := legacy.ModelLine(); got != "gpt-6.1-sol (reported by the runtime; session predates explicit settings)" {
		t.Fatalf("model line = %q", got)
	}
	if got := legacy.EffortLine(); strings.HasPrefix(got, "default") || !strings.HasPrefix(got, "unknown") {
		t.Fatalf("effort line = %q", got)
	}
	explicit := SessionValues{Backend: "claude_code", Model: "unknown", RequestedModel: "opus", Effort: "high", EffectiveEffort: "high", ModelSource: "global", EffortSource: "global"}
	if got := explicit.ModelLine(); got != "opus (configured opus from global; not yet reported by the runtime)" {
		t.Fatalf("model line = %q", got)
	}
	if got := explicit.EffortLine(); got != "high (configured high from global; reported by the runtime)" {
		t.Fatalf("effort line = %q", got)
	}
}
