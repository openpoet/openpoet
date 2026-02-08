package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeCLIProvider spawns the Claude CLI as a subprocess.
// It inherits OAuth auth from the keychain (Max plan, zero config).
type ClaudeCLIProvider struct{}

// NewClaudeCLIProvider creates a provider that uses the Claude CLI.
func NewClaudeCLIProvider() *ClaudeCLIProvider {
	return &ClaudeCLIProvider{}
}

func (p *ClaudeCLIProvider) Name() string { return "claudecode" }

// claudeCLIEvent represents one line of claude --output-format stream-json.
// The actual format from the CLI is:
//
//	{"type":"system", "subtype":"init", ...}
//	{"type":"assistant", "message":{"content":[{"type":"text","text":"..."}], ...}}
//	{"type":"result", "subtype":"success", "result":"full text", ...}
type claudeCLIEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"` // for "assistant"
	Result  string          `json:"result,omitempty"`  // for "result"
}

// claudeCLIMessage is the message object inside an "assistant" event.
type claudeCLIMessage struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (p *ClaudeCLIProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	// Build the prompt from the last user message
	var prompt string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			prompt = req.Messages[i].TextContent()
			break
		}
	}
	if prompt == "" {
		return nil, fmt.Errorf("no user message found")
	}

	// Prepend system prompt as context if provided
	if req.System != "" {
		prompt = req.System + "\n\n---\n\n" + prompt
	}

	// --tools "" disables all built-in tools, --strict-mcp-config with no config disables MCP tools.
	// This ensures the CLI only generates text responses — tool execution is handled by DevManager.
	args := []string{"--print", "--verbose", "--output-format", "stream-json", "--tools", "", "--strict-mcp-config"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude CLI: %w", err)
	}

	var fullText strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	blockStarted := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var cliEvent claudeCLIEvent
		if err := json.Unmarshal([]byte(line), &cliEvent); err != nil {
			continue
		}

		switch cliEvent.Type {
		case "assistant":
			// Parse the message to extract text content
			if cliEvent.Message == nil {
				continue
			}
			var msg claudeCLIMessage
			if err := json.Unmarshal(cliEvent.Message, &msg); err != nil {
				continue
			}

			for _, block := range msg.Content {
				if block.Type != "text" || block.Text == "" {
					continue
				}

				if !blockStarted {
					blockStarted = true
					if callback != nil {
						callback(StreamEvent{
							Type:         "content_block_start",
							Index:        0,
							ContentBlock: &ContentBlock{Type: "text", Text: ""},
						})
					}
				}

				fullText.WriteString(block.Text)
				if callback != nil {
					callback(StreamEvent{
						Type:  "content_block_delta",
						Index: 0,
						Delta: &StreamDelta{
							Type: "text_delta",
							Text: block.Text,
						},
					})
				}
			}

		case "result":
			// The result event contains the full final text.
			// If we already streamed from assistant events, just close the block.
			// Otherwise, send the result as a single chunk.
			if !blockStarted && cliEvent.Result != "" {
				blockStarted = true
				fullText.WriteString(cliEvent.Result)
				if callback != nil {
					callback(StreamEvent{
						Type:         "content_block_start",
						Index:        0,
						ContentBlock: &ContentBlock{Type: "text", Text: ""},
					})
					callback(StreamEvent{
						Type:  "content_block_delta",
						Index: 0,
						Delta: &StreamDelta{
							Type: "text_delta",
							Text: cliEvent.Result,
						},
					})
				}
			}
			if callback != nil {
				callback(StreamEvent{
					Type:  "content_block_stop",
					Index: 0,
				})
			}

		case "error":
			errMsg := cliEvent.Result
			if errMsg == "" {
				errMsg = "unknown CLI error"
			}
			return nil, fmt.Errorf("claude CLI error: %s", errMsg)

		case "system":
			// init event, skip
			continue
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("claude CLI exited with error: %w", err)
	}

	return &Response{
		Content:    []ContentBlock{{Type: "text", Text: fullText.String()}},
		StopReason: "end_turn",
	}, nil
}

// IsClaudeCLIAvailable checks if the claude CLI is installed and accessible.
func IsClaudeCLIAvailable() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}
