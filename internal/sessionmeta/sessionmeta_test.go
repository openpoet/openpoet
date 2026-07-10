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
