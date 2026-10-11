package llm

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Claude Haiku 5.5 thinks by default: the stream starts with a thinking block
// whose text is omitted, and that block (with its signature) has to go back to
// the API unchanged in the tool loop.
func TestParseSSEStreamKeepsThinkingBlockForReplay(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-haiku-5-5","role":"assistant","usage":{"input_tokens":10}}}`,
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"list_projects","input":{}}}`,
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		"",
	}, "\n")

	resp, err := parseSSEStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != "thinking" || resp.Content[0].Signature != "sig-abc" {
		t.Fatalf("thinking block not captured: %+v", resp.Content)
	}

	raw, err := json.Marshal(resp.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"type":"thinking","thinking":"","signature":"sig-abc"}` {
		t.Fatalf("replayed thinking block = %s", got)
	}
}

func TestParseSSEStreamAccumulatesThinkingText(t *testing.T) {
	stream := strings.Join([]string{
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`,
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check."}}`,
		"",
	}, "\n")
	resp, err := parseSSEStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if th := resp.Content[0].Thinking; th == nil || *th != "Let me check." {
		t.Fatalf("thinking text = %v", th)
	}
}

func TestCalculateCostHaiku55LongPromptRate(t *testing.T) {
	short := CalculateCost("claude-haiku-5-5", 100_000, 1_000_000)
	if math.Abs(short-(0.01+0.50)) > 1e-9 {
		t.Fatalf("short-prompt cost = %v, want 0.51", short)
	}
	long := CalculateCost("claude-haiku-5-5", 200_000, 1_000_000)
	if want := 0.2*0.50 + 2.50; math.Abs(long-want) > 1e-9 {
		t.Fatalf("long-prompt cost = %v, want %v", long, want)
	}
}
