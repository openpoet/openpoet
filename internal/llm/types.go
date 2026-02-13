package llm

// DefaultModel is the fallback model used when no model is configured.
const DefaultModel = "claude-sonnet-4-5-20250929"

// Message represents a conversation message.
type Message struct {
	Role    string         `json:"role"` // "user", "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock represents a block of content in a message.
type ContentBlock struct {
	Type  string `json:"type"`            // "text", "tool_use", "tool_result"
	Text  string `json:"text,omitempty"`  // for type="text"
	ID    string `json:"id,omitempty"`    // for type="tool_use"
	Name  string `json:"name,omitempty"`  // for type="tool_use"
	Input any    `json:"input,omitempty"` // for type="tool_use"

	// For tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"` // for type="tool_result" (stringified)
}

// TextContent returns the concatenated text from all text blocks.
func (m *Message) TextContent() string {
	var text string
	for _, b := range m.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text
}

// NewTextMessage creates a message with a single text block.
func NewTextMessage(role, text string) Message {
	return Message{
		Role:    role,
		Content: []ContentBlock{{Type: "text", Text: text}},
	}
}

// ToolDefinition defines a tool available to the model.
type ToolDefinition struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	InputSchema ToolDefinitionInput `json:"input_schema"`
}

// ToolDefinitionInput is the JSON Schema for a tool's input.
type ToolDefinitionInput struct {
	Type       string                        `json:"type"` // always "object"
	Properties map[string]ToolPropertySchema `json:"properties"`
	Required   []string                      `json:"required,omitempty"`
}

// ToolPropertySchema is a JSON Schema property for a tool input.
type ToolPropertySchema struct {
	Type        string                        `json:"type"`
	Description string                        `json:"description,omitempty"`
	Enum        []string                      `json:"enum,omitempty"`
	Properties  map[string]ToolPropertySchema `json:"properties,omitempty"`
	Items       *ToolPropertySchema           `json:"items,omitempty"`
	Required    []string                      `json:"required,omitempty"`
	MaxItems    int                           `json:"maxItems,omitempty"`
}

// StreamEvent represents a streaming event from the model.
type StreamEvent struct {
	Type  string `json:"type"` // "content_block_start", "content_block_delta", "content_block_stop", "message_start", "message_delta", "message_stop", "error"
	Index int    `json:"index,omitempty"`

	// For content_block_start
	ContentBlock *ContentBlock `json:"content_block,omitempty"`

	// For content_block_delta
	Delta *StreamDelta `json:"delta,omitempty"`

	// For message_start
	Message *StreamMessage `json:"message,omitempty"`

	// For message_delta (top-level usage with output_tokens)
	Usage *Usage `json:"usage,omitempty"`

	// For error
	Error *StreamError `json:"error,omitempty"`
}

// StreamDelta contains incremental content.
type StreamDelta struct {
	Type        string `json:"type,omitempty"`         // "text_delta", "input_json_delta"
	Text        string `json:"text,omitempty"`         // for text_delta
	PartialJSON string `json:"partial_json,omitempty"` // for input_json_delta
	StopReason  string `json:"stop_reason,omitempty"`  // for message_delta
}

// StreamMessage is a partial message in a stream start event.
type StreamMessage struct {
	ID         string `json:"id"`
	Model      string `json:"model,omitempty"`
	Role       string `json:"role"`
	StopReason string `json:"stop_reason,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`
}

// StreamError represents an error in the stream.
type StreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Usage tracks token usage.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// Request is the input to a provider call.
type Request struct {
	System         string
	Messages       []Message
	Tools          []ToolDefinition
	MaxTokens      int
	Model          string
	ConversationID int64 // Optional: passed to MCP subprocess for correlation
}

// Response is the output of a non-streaming provider call.
type Response struct {
	Content    []ContentBlock
	StopReason string  // "end_turn", "tool_use", "max_tokens"
	Usage      Usage
	Model      string  // model used (populated by provider)
	SessionID  string  // SDK session ID for resume (populated by SDK providers)
	CostUSD    float64 // total cost reported by SDK providers (0 if not available)
}

// StreamCallback is called for each streaming event.
type StreamCallback func(event StreamEvent) error
