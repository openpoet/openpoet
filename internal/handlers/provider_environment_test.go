package handlers

import "testing"

func TestOpenAIClaudeEnvironmentUsesLoopbackBridgeModels(t *testing.T) {
	env := openAIClaudeEnvironment("http://127.0.0.1:18765/", "gpt-main", "gpt-fast")
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:18765" {
		t.Fatalf("base URL = %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_MODEL"] != "gpt-main" || env["ANTHROPIC_SMALL_FAST_MODEL"] != "gpt-fast" {
		t.Fatalf("model environment = %#v", env)
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "unused" || env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Fatalf("bridge safeguards = %#v", env)
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_ACCESS_TOKEN", "CODEX_API_KEY"} {
		if env[key] != "" {
			t.Fatalf("inherited credential %s was not cleared: %#v", key, env)
		}
	}
}

func TestOpenAIClaudeEnvironmentDefaultsSmallModel(t *testing.T) {
	env := openAIClaudeEnvironment("http://127.0.0.1:1", "gpt-main", "")
	if env["ANTHROPIC_SMALL_FAST_MODEL"] != "gpt-main" || env["CLAUDE_CODE_SUBAGENT_MODEL"] != "gpt-main" {
		t.Fatalf("small model fallback = %#v", env)
	}
}
