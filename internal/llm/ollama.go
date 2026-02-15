package llm

import (
	"context"
	"fmt"
	"io"
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
	var messages []openai.ChatCompletionMessage
	for _, m := range req.Messages {
		var content string
		if len(m.Content) == 1 && m.Content[0].Type == "text" {
			content = m.Content[0].Text
		} else {
			// For multimodal content, just use the text parts
			var parts []string
			for _, c := range m.Content {
				if c.Type == "text" {
					parts = append(parts, c.Text)
				}
			}
			content = strings.Join(parts, "\n")
		}
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: content,
		})
	}

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
		Temperature: 0.7,
	})
	if err != nil {
		return nil, fmt.Errorf("Ollama API error: %w", err)
	}
	defer stream.Close()

	var response Response
	var contentText strings.Builder
	toolCalls := make(map[int]*ContentBlock) // index -> tool call block

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
					Type: "content_block_delta",
					Index: 0,
					Delta: &StreamDelta{
						Type: "text_delta",
						Text: delta.Content,
					},
				})
			}

			// Handle tool calls
			for i, tc := range delta.ToolCalls {
				if _, exists := toolCalls[i]; !exists {
					toolCalls[i] = &ContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: make(map[string]any),
					}
					callback(StreamEvent{
						Type:  "content_block_start",
						Index: i,
						ContentBlock: &ContentBlock{
							Type: "tool_use",
							ID:   tc.ID,
							Name: tc.Function.Name,
						},
					})
				}
				if tc.Function.Arguments != "" {
					callback(StreamEvent{
						Type:  "content_block_delta",
						Index: i,
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

	response.Model = model

	return &response, nil
}