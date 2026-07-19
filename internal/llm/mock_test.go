package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMockProviderRepliesCannedAndRecordsPrompt(t *testing.T) {
	dir := t.TempDir()
	respFile := filepath.Join(dir, "resp.json")
	promptFile := filepath.Join(dir, "prompt.txt")
	t.Setenv("OPENPOET_TEST_MOCK_RESPONSE_FILE", respFile)
	t.Setenv("OPENPOET_TEST_MOCK_PROMPT_FILE", promptFile)

	canned := `{"action":"escalate_human","reason":"canned"}`
	if err := os.WriteFile(respFile, []byte(canned), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewMockProvider()
	var got strings.Builder
	resp, err := p.StreamMessage(context.Background(), &Request{
		System:   "SYS-PROMPT-MARKER",
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "BRIEF-MARKER"}}}},
	}, func(ev StreamEvent) error {
		if ev.Type == "content_block_delta" && ev.Delta != nil {
			got.WriteString(ev.Delta.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != canned {
		t.Fatalf("streamed = %q, want canned %q", got.String(), canned)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != canned {
		t.Fatalf("response content = %+v", resp.Content)
	}
	// Zero usage → no token_usage recorded (N1 zero-cost-at-rest).
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 || resp.CostUSD != 0 {
		t.Fatalf("mock reported nonzero usage/cost: %+v cost=%v", resp.Usage, resp.CostUSD)
	}
	// The prompt (system + messages) was recorded for hygiene checks.
	recorded, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "SYS-PROMPT-MARKER") || !strings.Contains(string(recorded), "BRIEF-MARKER") {
		t.Fatalf("recorded prompt missing markers: %s", recorded)
	}

	// Re-read on each call: swap the canned file mid-run.
	if err := os.WriteFile(respFile, []byte(`{"action":"dismiss"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resp2, _ := p.StreamMessage(context.Background(), &Request{System: "x"}, nil)
	if resp2.Content[0].Text != `{"action":"dismiss"}` {
		t.Fatalf("second call did not re-read the file: %q", resp2.Content[0].Text)
	}
}
