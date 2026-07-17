package application

import (
	"encoding/json"
	"testing"
)

func TestNormalizeClaudeCodeOpenAIProviderConfig(t *testing.T) {
	raw := normalizeProjectBackendConfig("claude_code", `{
		"provider":"openai",
		"provider_config_id":42,
		"model":" gpt-5.6-sol[1m] ",
		"small_model":"gpt-5.6-luna[1m]",
		"approval_policy":"never"
	}`)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["provider"] != "openai_oauth" || got["provider_config_id"] != float64(42) || got["model"] != "gpt-5.6-sol[1m]" {
		t.Fatalf("normalized config = %#v", got)
	}
	if _, leaked := got["approval_policy"]; leaked {
		t.Fatalf("foreign backend setting leaked: %#v", got)
	}
}

func TestNormalizeClaudeCodeRejectsImplicitOrIncompleteProviderConfig(t *testing.T) {
	for _, raw := range []string{
		`{"model":"gpt-5.6-sol","runtime":"app-server"}`,
		`{"provider":"openai_oauth","model":"gpt-5.6-sol"}`,
		`{"provider":"openai_oauth","provider_config_id":1}`,
		`{"provider":"unknown","model":"anything"}`,
	} {
		if got := normalizeProjectBackendConfig("claude_code", raw); got != "{}" {
			t.Errorf("normalizeProjectBackendConfig(%s) = %s, want {}", raw, got)
		}
	}
}

func TestNormalizeClaudeCodeAnthropicConfigIsExplicit(t *testing.T) {
	raw := normalizeProjectBackendConfig("claude_code", `{"provider":"anthropic","model":"sonnet","runtime":"tui"}`)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["provider"] != "anthropic" || got["model"] != "sonnet" || len(got) != 2 {
		t.Fatalf("normalized config = %#v", got)
	}
}
