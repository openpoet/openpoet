package sessionmeta

import (
	"encoding/json"
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
	if claude.Model != "default" || claude.Effort != "default" || claude.Harness != "claude_code" {
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
