package llm

import (
	"context"
	"os"
	"strings"
)

// MockProvider is a test-only canned LLM used by the Phase 4 brain E2E. It
// replays the JSON in OPENPOET_TEST_MOCK_RESPONSE_FILE (re-read on EVERY call so
// the harness can swap the canned action between asserts) and appends the
// received prompt (system + messages) to OPENPOET_TEST_MOCK_PROMPT_FILE so the
// check can verify brief hygiene (no raw ANSI, no leaked tokens). It records
// ZERO usage/cost, so "no LLM usage at rest" holds. It is registered only under
// OPENPOET_TEST_MODE and never reaches production.
type MockProvider struct{}

func NewMockProvider() *MockProvider { return &MockProvider{} }

func (p *MockProvider) Name() string { return "mock" }

func (p *MockProvider) StreamMessage(_ context.Context, req *Request, callback StreamCallback) (*Response, error) {
	p.recordPrompt(req)
	canned := p.cannedResponse()

	if callback != nil {
		_ = callback(StreamEvent{Type: "content_block_start", ContentBlock: &ContentBlock{Type: "text"}})
		_ = callback(StreamEvent{Type: "content_block_delta", Delta: &StreamDelta{Type: "text_delta", Text: canned}})
		_ = callback(StreamEvent{Type: "content_block_stop"})
	}
	return &Response{
		Content:    []ContentBlock{{Type: "text", Text: canned}},
		StopReason: "end_turn",
		Model:      "mock",
		// Usage/CostUSD deliberately zero: the brain must not record token_usage
		// for a mock consult (N1 zero-cost-at-rest).
	}, nil
}

func (p *MockProvider) cannedResponse() string {
	path := os.Getenv("OPENPOET_TEST_MOCK_RESPONSE_FILE")
	if path == "" {
		return "{}"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func (p *MockProvider) recordPrompt(req *Request) {
	path := os.Getenv("OPENPOET_TEST_MOCK_PROMPT_FILE")
	if path == "" || req == nil {
		return
	}
	var b strings.Builder
	b.WriteString("=== SYSTEM ===\n")
	b.WriteString(req.System)
	b.WriteString("\n=== MESSAGES ===\n")
	for i := range req.Messages {
		b.WriteString(req.Messages[i].TextContent())
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}
