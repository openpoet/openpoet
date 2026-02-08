package llm

import "context"

// Provider abstracts different LLM backends.
type Provider interface {
	// StreamMessage sends a streaming request, calling callback for each event.
	StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error)
	// Name returns the provider identifier ("apikey" or "claudecode").
	Name() string
}
