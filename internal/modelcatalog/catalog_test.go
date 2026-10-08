package modelcatalog

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestClaudeModelsDropsDefaultAndPinsAliases(t *testing.T) {
	got := claudeModels([]claudeModel{
		{Value: "default", ResolvedModel: "claude-opus-5-5", DisplayName: "Default (recommended)"},
		{Value: "opus", ResolvedModel: "claude-opus-5-5", DisplayName: "Opus 5.5", Description: "Most capable"},
		{Value: "claude-fable-5-1", ResolvedModel: "claude-fable-5-1", DisplayName: "Fable 5.1"},
		{Value: "sonnet", ResolvedModel: "claude-sonnet-5", DisplayName: "Sonnet 5"},
		{Value: "claude-sonnet-5", ResolvedModel: "claude-sonnet-5", DisplayName: "Sonnet 5"},
	})
	ids := []string{}
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	want := []string{"opus", "claude-opus-5-5", "claude-fable-5-1", "sonnet", "claude-sonnet-5"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	if got[0].ResolvedID != "claude-opus-5-5" || got[0].Label != "Opus 5.5 (latest)" {
		t.Fatalf("alias entry = %+v", got[0])
	}
	if got[1].ResolvedID != "" || got[1].Label != "Opus 5.5" {
		t.Fatalf("pinned entry = %+v", got[1])
	}
}

func TestParseOpenCodeModels(t *testing.T) {
	got := parseOpenCodeModels("anthropic/claude-sonnet-5\n\nopenai/gpt-6-astra\nSome banner text\nanthropic/claude-sonnet-5\n")
	if len(got) != 2 || got[0].ID != "anthropic/claude-sonnet-5" || got[1].ID != "openai/gpt-6-astra" {
		t.Fatalf("got %+v", got)
	}
}

func TestServiceCachesAndReportsErrors(t *testing.T) {
	var calls int32
	s := &Service{
		cache:   map[string]entry{},
		pending: map[string]chan struct{}{},
		probes: map[string]func(context.Context) ([]Model, error){
			"ok":   func(context.Context) ([]Model, error) { atomic.AddInt32(&calls, 1); return []Model{{ID: "m"}}, nil },
			"fail": func(context.Context) ([]Model, error) { return nil, errors.New("boom") },
		},
	}
	s.Get(context.Background(), "ok", false)
	if c := s.Get(context.Background(), "ok", false); len(c.Models) != 1 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected cached result, calls=%d catalog=%+v", calls, c)
	}
	s.Get(context.Background(), "ok", true)
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("refresh should re-probe, calls=%d", calls)
	}
	if c := s.Get(context.Background(), "fail", false); c.Error != "boom" || c.Models == nil {
		t.Fatalf("failure catalog = %+v", c)
	}
}

func TestClaudeModelsCarryEffortLevels(t *testing.T) {
	got := claudeModels([]claudeModel{
		{Value: "opus", ResolvedModel: "claude-opus-5-5", DisplayName: "Opus 5.5", SupportsEffort: true, SupportedEffortLevels: []string{"low", "High", "max"}},
		{Value: "claude-haiku-4-5-20251001", ResolvedModel: "claude-haiku-4-5-20251001", DisplayName: "Haiku 4.5"},
	})
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	for _, m := range got[:2] { // the alias and the pinned id it resolves to
		if len(m.Efforts) != 3 || m.Efforts[1] != "high" {
			t.Fatalf("%s efforts = %v", m.ID, m.Efforts)
		}
	}
	if got[2].Efforts != nil {
		t.Fatalf("a model without effort support must list none, got %v", got[2].Efforts)
	}
}

func TestParseCodexModelList(t *testing.T) {
	line := []byte(`{"id":2,"result":{"data":[` +
		`{"id":"gpt-6.1-sol","model":"gpt-6.1-sol","displayName":"GPT-6.1-Sol","isDefault":true,"defaultReasoningEffort":"low","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"ultra"}]},` +
		`{"id":"secret","model":"secret","hidden":true}]}}`)
	models, done, err := parseCodexModelList(line)
	if err != nil || !done || len(models) != 1 {
		t.Fatalf("models=%+v done=%v err=%v", models, done, err)
	}
	m := models[0]
	if m.ID != "gpt-6.1-sol" || !m.IsDefault || m.DefaultEffort != "low" || len(m.Efforts) != 2 || m.Efforts[1] != "ultra" {
		t.Fatalf("model = %+v", m)
	}
	if _, done, _ := parseCodexModelList([]byte(`{"id":1,"result":{}}`)); done {
		t.Fatal("a reply to another request must not end the scan")
	}
}

func TestCatalogFindAndKnownEfforts(t *testing.T) {
	c := Catalog{Models: []Model{{ID: "opus", ResolvedID: "claude-opus-5-5", Efforts: []string{"low", "high"}}, {ID: "x", Efforts: []string{"high", "max"}}}}
	if m, ok := c.Find("claude-opus-5-5"); !ok || m.ID != "opus" {
		t.Fatalf("resolved-id lookup = %+v %v", m, ok)
	}
	if _, ok := c.Find("nope"); ok {
		t.Fatal("unknown id found")
	}
	if got := KnownEfforts("claude_code", c); len(got) != 3 {
		t.Fatalf("union = %v", got)
	}
	if got := KnownEfforts("codex", Catalog{}); len(got) == 0 || got[len(got)-1] != "ultra" {
		t.Fatalf("fallback = %v", got)
	}
}
