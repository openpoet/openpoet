package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
)

// AnthropicProvider makes direct HTTP calls to the Anthropic Messages API.
type AnthropicProvider struct {
	apiKey string
	client *http.Client
}

// NewAnthropicProvider creates a provider that uses an API key.
func NewAnthropicProvider(apiKey string) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *AnthropicProvider) Name() string { return "apikey" }

// anthropicRequest is the request body for the Messages API.
type anthropicRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []anthropicMsg   `json:"messages"`
	Stream    bool             `json:"stream,omitempty"`
	Tools     []ToolDefinition `json:"tools,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentBlock
}

func (p *AnthropicProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	model := req.Model
	if model == "" {
		model = "claude-sonnet-4-5-20250929"
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	// Convert messages
	msgs := make([]anthropicMsg, len(req.Messages))
	for i, m := range req.Messages {
		if len(m.Content) == 1 && m.Content[0].Type == "text" {
			msgs[i] = anthropicMsg{Role: m.Role, Content: m.Content[0].Text}
		} else {
			msgs[i] = anthropicMsg{Role: m.Role, Content: m.Content}
		}
	}

	body := anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  msgs,
		Stream:    true,
	}
	if len(req.Tools) > 0 {
		body.Tools = req.Tools
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	return parseSSEStream(resp.Body, callback)
}

// parseSSEStream parses Anthropic SSE format and calls callback for each event.
func parseSSEStream(r io.Reader, callback StreamCallback) (*Response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var (
		result     Response
		eventType  string
		blocks     []ContentBlock
		curBlockIdx int = -1
		inputJSONBuf strings.Builder
	)

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event StreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue // skip malformed events
		}
		event.Type = eventType

		switch eventType {
		case "message_start":
			if event.Message != nil && event.Message.Usage != nil {
				result.Usage.InputTokens = event.Message.Usage.InputTokens
			}

		case "content_block_start":
			if event.ContentBlock != nil {
				blocks = append(blocks, *event.ContentBlock)
				curBlockIdx = len(blocks) - 1
				inputJSONBuf.Reset()
			}

		case "content_block_delta":
			if event.Delta != nil {
				switch event.Delta.Type {
				case "text_delta":
					if curBlockIdx >= 0 && curBlockIdx < len(blocks) {
						blocks[curBlockIdx].Text += event.Delta.Text
					}
				case "input_json_delta":
					inputJSONBuf.WriteString(event.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			// If we accumulated tool input JSON, parse it
			if curBlockIdx >= 0 && curBlockIdx < len(blocks) && blocks[curBlockIdx].Type == "tool_use" && inputJSONBuf.Len() > 0 {
				var input any
				if err := json.Unmarshal([]byte(inputJSONBuf.String()), &input); err == nil {
					blocks[curBlockIdx].Input = input
				}
				inputJSONBuf.Reset()
			}

		case "message_delta":
			if event.Delta != nil {
				result.StopReason = event.Delta.StopReason
			}

		case "message_stop":
			// done

		case "error":
			if event.Error != nil {
				return nil, fmt.Errorf("stream error: %s: %s", event.Error.Type, event.Error.Message)
			}
		}

		if callback != nil {
			if err := callback(event); err != nil {
				return nil, err
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	result.Content = blocks
	return &result, nil
}
