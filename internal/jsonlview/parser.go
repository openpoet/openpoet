package jsonlview

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

// rawEvent is the raw JSON structure of a JSONL line.
type rawEvent struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	ParentUUID  *string         `json:"parentUuid"`
	Timestamp   string          `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
	Data        json.RawMessage `json:"data"`
}

// rawMessage is the raw JSON structure of a message field.
type rawMessage struct {
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	ID         string          `json:"id"`
	Content    json.RawMessage `json:"content"`
	Usage      *rawUsage       `json:"usage"`
	StopReason *string         `json:"stop_reason"`
}

type rawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// rawContentBlock is a single content block in the content array.
type rawContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	Input     any    `json:"input"`
	Content   any    `json:"content"`
	IsError   bool   `json:"is_error"`
}

type rawProgressData struct {
	Type      string `json:"type"`
	HookEvent string `json:"hookEvent"`
	HookName  string `json:"hookName"`
	Command   string `json:"command"`
}

// ParseLine parses a single JSONL line into a SessionEvent.
// Returns nil, nil for lines that should be skipped (snapshots, etc).
func ParseLine(line []byte) (*SessionEvent, error) {
	var raw rawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}

	// Skip event types we don't render
	switch raw.Type {
	case "file-history-snapshot", "queue-operation":
		return nil, nil
	}

	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)

	event := &SessionEvent{
		Type:        raw.Type,
		UUID:        raw.UUID,
		Timestamp:   ts,
		SessionID:   raw.SessionID,
		IsSidechain: raw.IsSidechain,
	}
	if raw.ParentUUID != nil {
		event.ParentUUID = *raw.ParentUUID
	}

	switch raw.Type {
	case "user", "assistant":
		msg, err := parseMessage(raw.Message)
		if err != nil {
			return nil, err
		}
		event.Message = msg

	case "progress":
		if len(raw.Data) > 0 {
			var pd rawProgressData
			if err := json.Unmarshal(raw.Data, &pd); err == nil {
				event.Progress = &ProgressData{
					Type:      pd.Type,
					HookEvent: pd.HookEvent,
					HookName:  pd.HookName,
					Command:   pd.Command,
				}
			}
		}
	}

	return event, nil
}

func parseMessage(data json.RawMessage) (*EventMessage, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var raw rawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	msg := &EventMessage{
		Role:      raw.Role,
		Model:     raw.Model,
		MessageID: raw.ID,
	}
	if raw.StopReason != nil {
		msg.StopReason = *raw.StopReason
	}

	if raw.Usage != nil {
		msg.Usage = &TokenUsage{
			InputTokens:         raw.Usage.InputTokens,
			OutputTokens:        raw.Usage.OutputTokens,
			CacheReadTokens:     raw.Usage.CacheReadInputTokens,
			CacheCreationTokens: raw.Usage.CacheCreationInputTokens,
		}
	}

	msg.ContentBlocks = parseContentBlocks(raw.Content)

	return msg, nil
}

func parseContentBlocks(data json.RawMessage) []ContentBlock {
	if len(data) == 0 {
		return nil
	}

	// Try as string first (user messages can be plain strings)
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		return []ContentBlock{{Type: "text", Text: str}}
	}

	// Try as array of content blocks
	var rawBlocks []rawContentBlock
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return nil
	}

	blocks := make([]ContentBlock, 0, len(rawBlocks))
	for _, rb := range rawBlocks {
		block := ContentBlock{Type: rb.Type}

		switch rb.Type {
		case "text":
			block.Text = rb.Text
		case "thinking":
			block.Text = rb.Thinking
		case "tool_use":
			block.ToolName = rb.Name
			block.ToolID = rb.ID
			block.ToolInput = rb.Input
		case "tool_result":
			block.ToolID = rb.ToolUseID
			block.Content = extractToolResultContent(rb.Content)
			block.IsError = rb.IsError
		}

		blocks = append(blocks, block)
	}

	return blocks
}

// extractToolResultContent handles tool_result content which can be a string
// or an array of content blocks.
func extractToolResultContent(v any) string {
	if v == nil {
		return ""
	}
	switch c := v.(type) {
	case string:
		return c
	default:
		b, _ := json.Marshal(c)
		return string(b)
	}
}

// mergeAssistantMessage merges content blocks from a newer partial message into
// the existing one. Claude Code streams assistant messages as partial chunks where
// each chunk may contain different block types (text, thinking, tool_use).
// This keeps all unique blocks and updates metadata (usage, stop_reason) from the newer version.
func mergeAssistantMessage(dst, src *EventMessage) {
	// Build a set of block types already present in dst
	existingTypes := make(map[string]bool)
	for _, b := range dst.ContentBlocks {
		existingTypes[b.Type] = true
	}

	// Append blocks from src that aren't already in dst
	for _, b := range src.ContentBlocks {
		if !existingTypes[b.Type] {
			dst.ContentBlocks = append(dst.ContentBlocks, b)
			existingTypes[b.Type] = true
		}
	}

	// Always take the latest metadata
	if src.Usage != nil {
		dst.Usage = src.Usage
	}
	if src.StopReason != "" {
		dst.StopReason = src.StopReason
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
}

// ParseFile reads an entire JSONL file and returns all parsed events.
// Merges assistant message chunks by combining content blocks across partials.
func ParseFile(path string) ([]*SessionEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []*SessionEvent
	// Track assistant messages by message ID for dedup
	msgIDIndex := make(map[string]int) // messageID → index in events slice

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		event, err := ParseLine(scanner.Bytes())
		if err != nil || event == nil {
			continue
		}

		// Deduplicate assistant messages: Claude Code streams partial messages
		// (stop_reason=null) followed by the final one (stop_reason=tool_use/end_turn).
		// Each partial chunk may contain different content blocks (text, thinking, tool_use).
		// Merge all unique blocks into the final event to avoid losing text/thinking.
		if event.Type == "assistant" && event.Message != nil && event.Message.MessageID != "" {
			msgID := event.Message.MessageID
			if idx, exists := msgIDIndex[msgID]; exists {
				prev := events[idx]
				mergeAssistantMessage(prev.Message, event.Message)
				continue
			}
			msgIDIndex[msgID] = len(events)
		}

		events = append(events, event)
	}

	return events, scanner.Err()
}

// ParseFileFromOffset reads a JSONL file starting from a byte offset and returns
// new events and the new offset. Used by the watcher for incremental reads.
func ParseFileFromOffset(path string, offset int64) ([]*SessionEvent, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}

	if info.Size() <= offset {
		return nil, offset, nil
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset, err
	}

	// Read the remaining bytes manually since bufio.Scanner doesn't track byte position
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}

	var events []*SessionEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		event, err := ParseLine(scanner.Bytes())
		if err != nil || event == nil {
			continue
		}
		events = append(events, event)
	}

	return events, offset + int64(len(data)), scanner.Err()
}
