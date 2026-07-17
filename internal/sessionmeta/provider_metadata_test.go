package sessionmeta

import "testing"

func TestOpenAIProviderMetadata(t *testing.T) {
	meta := FromProjectConfig("claude_code", `{"provider":"openai_oauth","provider_config_id":7,"model":"gpt-5.6-sol[1m]"}`)
	if meta.Model != "gpt-5.6-sol[1m]" || meta.Harness != "claude_code/openai" {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.HarnessDetails == "" {
		t.Fatal("OpenAI provider details are missing")
	}
}
