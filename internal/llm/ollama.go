package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// OllamaProvider uses the OpenAI-compatible API to connect to Ollama servers.
type OllamaProvider struct {
	client *openai.Client
	model  string
}

// NewOllamaProvider creates a provider that connects to a remote Ollama server.
func NewOllamaProvider(baseURL, apiKey, model string) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "qwen3-coder"
	}

	config := openai.DefaultConfig(apiKey)
	config.BaseURL = baseURL + "/v1"

	return &OllamaProvider{
		client: openai.NewClientWithConfig(config),
		model:  model,
	}
}

func (p *OllamaProvider) Name() string { return "ollama" }

// StreamMessage sends a streaming request to Ollama using OpenAI-compatible API.
func (p *OllamaProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	// Convert messages to OpenAI format
	messages := convertMessagesToOpenAI(req.Messages)

	// Add system message if provided
	if req.System != "" {
		messages = append([]openai.ChatCompletionMessage{
			{Role: "system", Content: req.System},
		}, messages...)
	}

	// Build tools if provided
	var tools []openai.Tool
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			tools = append(tools, openai.Tool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Messages:    messages,
		Stream:      true,
		Tools:       tools,
		Temperature: 0.3,
	})
	if err != nil {
		return nil, fmt.Errorf("Ollama API error: %w", err)
	}
	defer stream.Close()

	var response Response
	var contentText strings.Builder
	toolCalls := make(map[int]*ContentBlock)     // index -> tool call block
	toolArgsJSON := make(map[int]*strings.Builder) // index -> accumulated JSON

	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("stream error: %w", err)
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			choice := chunk.Choices[0]

			// Handle content
			if delta.Content != "" {
				contentText.WriteString(delta.Content)
				callback(StreamEvent{
					Type:  "content_block_delta",
					Index: 0,
					Delta: &StreamDelta{
						Type: "text_delta",
						Text: delta.Content,
					},
				})
			}

			// Handle tool calls
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}

				if _, exists := toolCalls[idx]; !exists {
					// Generate fallback ID if the model doesn't provide one
					// (many Ollama models don't generate tool call IDs)
					callID := tc.ID
					if callID == "" {
						callID = fmt.Sprintf("call_%d_%d", idx, len(toolCalls))
					}
					toolCalls[idx] = &ContentBlock{
						Type:  "tool_use",
						ID:    callID,
						Name:  tc.Function.Name,
						Input: make(map[string]any),
					}
					toolArgsJSON[idx] = &strings.Builder{}
					callback(StreamEvent{
						Type:  "content_block_start",
						Index: idx,
						ContentBlock: &ContentBlock{
							Type: "tool_use",
							ID:   callID,
							Name: tc.Function.Name,
						},
					})
				} else {
					// Update ID/Name if they arrive in later chunks
					if tc.ID != "" {
						toolCalls[idx].ID = tc.ID
					}
					if tc.Function.Name != "" {
						toolCalls[idx].Name = tc.Function.Name
					}
				}

				if tc.Function.Arguments != "" {
					toolArgsJSON[idx].WriteString(tc.Function.Arguments)
					callback(StreamEvent{
						Type:  "content_block_delta",
						Index: idx,
						Delta: &StreamDelta{
							Type:        "input_json_delta",
							PartialJSON: tc.Function.Arguments,
						},
					})
				}
			}

			// Handle finish reason
			if choice.FinishReason != "" {
				response.StopReason = string(choice.FinishReason)
				if response.StopReason == "stop" {
					response.StopReason = "end_turn"
				} else if response.StopReason == "tool_calls" {
					response.StopReason = "tool_use"
				}
			}
		}
	}

	// Parse accumulated tool call arguments JSON.
	// Some Ollama models concatenate multiple JSON objects when they want to call
	// the same tool multiple times (e.g. {"project_id":"1"}{"project_id":"2"}).
	// We split these into separate tool call blocks.
	var extraToolCalls []ContentBlock
	for idx, tc := range toolCalls {
		if buf, ok := toolArgsJSON[idx]; ok && buf.Len() > 0 {
			rawJSON := buf.String()
			log.Printf("[Ollama] tool %s(id=%s) raw args JSON: %s", tc.Name, tc.ID, rawJSON)

			// Try parsing as single JSON first
			var parsed any
			if err := json.Unmarshal([]byte(rawJSON), &parsed); err == nil {
				tc.Input = parsed
			} else {
				// Try splitting concatenated JSON objects: {"a":"1"}{"a":"2"} → two calls
				objects := splitConcatenatedJSON(rawJSON)
				if len(objects) > 0 {
					// First object stays with the original tool call
					var first any
					if err := json.Unmarshal([]byte(objects[0]), &first); err == nil {
						tc.Input = first
					}
					// Additional objects become extra tool calls
					for i := 1; i < len(objects); i++ {
						var extra any
						if err := json.Unmarshal([]byte(objects[i]), &extra); err == nil {
							extraToolCalls = append(extraToolCalls, ContentBlock{
								Type:  "tool_use",
								ID:    fmt.Sprintf("%s_%d", tc.ID, i),
								Name:  tc.Name,
								Input: extra,
							})
						}
					}
					log.Printf("[Ollama] split concatenated JSON into %d tool calls for %s", len(objects), tc.Name)
				} else {
					log.Printf("[Ollama] ERROR parsing tool args for %s: %v", tc.Name, err)
				}
			}
		} else {
			log.Printf("[Ollama] tool %s(id=%s) has NO accumulated args JSON", tc.Name, tc.ID)
		}
	}

	// Build final response content
	response.Content = []ContentBlock{
		{Type: "text", Text: contentText.String()},
	}

	// Add tool calls to content
	for i := 0; i < len(toolCalls); i++ {
		if tc, exists := toolCalls[i]; exists {
			response.Content = append(response.Content, *tc)
		}
	}

	// Add extra tool calls from concatenated JSON splitting
	response.Content = append(response.Content, extraToolCalls...)

	response.Model = model

	return &response, nil
}

// convertMessagesToOpenAI converts internal message format to OpenAI chat messages.
// Handles text, tool_use (assistant), and tool_result (tool) messages correctly.
func convertMessagesToOpenAI(msgs []Message) []openai.ChatCompletionMessage {
	var result []openai.ChatCompletionMessage

	for _, m := range msgs {
		// Check if this message contains tool_result blocks
		if hasToolResults(m.Content) {
			// Each tool_result becomes a separate "tool" role message
			for _, c := range m.Content {
				if c.Type == "tool_result" {
					result = append(result, openai.ChatCompletionMessage{
						Role:       "tool",
						Content:    c.Content,
						ToolCallID: c.ToolUseID,
					})
				}
			}
			continue
		}

		// Check if this is an assistant message with tool_use blocks
		if m.Role == "assistant" && hasToolUse(m.Content) {
			msg := openai.ChatCompletionMessage{
				Role: "assistant",
			}

			// Extract text content
			var textParts []string
			for _, c := range m.Content {
				if c.Type == "text" && c.Text != "" {
					textParts = append(textParts, c.Text)
				}
			}
			if len(textParts) > 0 {
				msg.Content = strings.Join(textParts, "\n")
			}

			// Convert tool_use blocks to OpenAI ToolCalls
			for _, c := range m.Content {
				if c.Type == "tool_use" {
					argsJSON, _ := json.Marshal(c.Input)
					msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
						ID:   c.ID,
						Type: "function",
						Function: openai.FunctionCall{
							Name:      c.Name,
							Arguments: string(argsJSON),
						},
					})
				}
			}

			result = append(result, msg)
			continue
		}

		// Regular text message
		var content string
		if len(m.Content) == 1 && m.Content[0].Type == "text" {
			content = m.Content[0].Text
		} else {
			var parts []string
			for _, c := range m.Content {
				if c.Type == "text" {
					parts = append(parts, c.Text)
				}
			}
			content = strings.Join(parts, "\n")
		}
		result = append(result, openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: content,
		})
	}

	return result
}

// hasToolResults checks if any content block is a tool_result.
func hasToolResults(blocks []ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// hasToolUse checks if any content block is a tool_use.
func hasToolUse(blocks []ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// splitConcatenatedJSON splits concatenated JSON objects like {"a":"1"}{"a":"2"}
// into separate strings. Returns nil if the input doesn't match this pattern.
func splitConcatenatedJSON(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return nil
	}

	var results []string
	decoder := json.NewDecoder(strings.NewReader(raw))
	for decoder.More() {
		var obj json.RawMessage
		if err := decoder.Decode(&obj); err != nil {
			return nil
		}
		results = append(results, string(obj))
	}

	if len(results) <= 1 {
		return nil // Not concatenated, let caller handle normally
	}
	return results
}
