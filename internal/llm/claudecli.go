package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ClaudeCLIProvider spawns the Claude CLI as a subprocess.
// It inherits OAuth auth from the keychain (Max plan, zero config).
type ClaudeCLIProvider struct {
	apiURL string // DevManager API URL for MCP server
}

// NewClaudeCLIProvider creates a provider that uses the Claude CLI.
// apiURL is the DevManager HTTP API URL used by the MCP server subprocess.
func NewClaudeCLIProvider(apiURL string) *ClaudeCLIProvider {
	return &ClaudeCLIProvider{apiURL: apiURL}
}

func (p *ClaudeCLIProvider) Name() string { return "claudecode" }

// claudeCLIEvent represents one line of claude --output-format stream-json.
// The actual format from the CLI is:
//
//	{"type":"system", "subtype":"init", ...}
//	{"type":"assistant", "message":{"content":[{"type":"text","text":"..."}], ...}}
//	{"type":"result", "subtype":"success", "result":"full text", "model":"...", "cost_usd":0.01, "usage":{...}}
type claudeCLIEvent struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"` // for "assistant"
	Result     string          `json:"result,omitempty"`  // for "result"
	CostUSD    float64         `json:"cost_usd,omitempty"`
	Usage      *cliResultUsage `json:"usage,omitempty"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage,omitempty"` // for "result": model -> usage
}

// cliResultUsage is the usage object inside a Claude CLI "result" event.
type cliResultUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// claudeCLIMessage is the message object inside an "assistant" event.
type claudeCLIMessage struct {
	Model   string `json:"model,omitempty"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (p *ClaudeCLIProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	// Build prompt with full conversation history so the model has context.
	// Claude CLI --print is stateless (single-turn), so we serialize all
	// previous messages into the prompt to preserve conversational context.
	var promptBuilder strings.Builder

	if req.System != "" {
		promptBuilder.WriteString(req.System)
		promptBuilder.WriteString("\n\n---\n\n")
	}

	if len(req.Messages) > 1 {
		promptBuilder.WriteString("## Conversation History\n\n")
		for _, msg := range req.Messages[:len(req.Messages)-1] {
			text := msg.TextContent()
			if text == "" {
				continue
			}
			if msg.Role == "user" {
				promptBuilder.WriteString("**User**: ")
			} else {
				promptBuilder.WriteString("**Assistant**: ")
			}
			promptBuilder.WriteString(text)
			promptBuilder.WriteString("\n\n")
		}
		promptBuilder.WriteString("---\n\n")
	}

	// Append the latest user message
	lastMsg := req.Messages[len(req.Messages)-1]
	lastText := lastMsg.TextContent()
	if lastText == "" {
		return nil, fmt.Errorf("no user message found")
	}
	promptBuilder.WriteString("**User**: ")
	promptBuilder.WriteString(lastText)

	prompt := promptBuilder.String()

	// Build MCP config JSON that tells Claude CLI to launch our MCP server subprocess.
	// The MCP server calls back into the DevManager HTTP API for tool execution.
	execPath, _ := os.Executable()
	mcpConfig := fmt.Sprintf(`{"mcpServers":{"devmanager":{"command":"%s","args":["mcp-serve"],"env":{"DEVMANAGER_API_URL":"%s"}}}}`, execPath, p.apiURL)

	args := []string{
		"--print", "--verbose", "--output-format", "stream-json",
		"--strict-mcp-config",                  // only use MCP servers from --mcp-config
		"--mcp-config", mcpConfig,              // inject DevManager MCP server
		"--allowedTools", "mcp__devmanager__*", // auto-approve all devmanager MCP tools
	}
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	args = append(args, "--model", model)
	// Pass prompt via stdin (more robust than CLI argument for long prompts)
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, "claude", args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude CLI: %w", err)
	}

	// Write prompt to stdin and close it
	go func() {
		stdin.Write([]byte(prompt))
		stdin.Close()
	}()

	var fullText strings.Builder
	var resultUsage Usage
	var resultModel string
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

			// Capture model from assistant message
			if msg.Model != "" && resultModel == "" {
				resultModel = msg.Model
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
			// Capture token usage from result event
			if cliEvent.Usage != nil {
				resultUsage.InputTokens = cliEvent.Usage.InputTokens
				resultUsage.OutputTokens = cliEvent.Usage.OutputTokens
				resultUsage.CacheReadTokens = cliEvent.Usage.CacheReadTokens
				resultUsage.CacheCreationTokens = cliEvent.Usage.CacheCreationTokens
			}
			// Extract model from modelUsage map keys (e.g. {"claude-opus-4-6": {...}})
			if resultModel == "" && len(cliEvent.ModelUsage) > 0 {
				for model := range cliEvent.ModelUsage {
					resultModel = model
					break
				}
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
		stderr := stderrBuf.String()
		if stderr != "" {
			return nil, fmt.Errorf("claude CLI exited with error: %w\nstderr: %s", err, stderr)
		}
		return nil, fmt.Errorf("claude CLI exited with error: %w", err)
	}

	return &Response{
		Content:    []ContentBlock{{Type: "text", Text: fullText.String()}},
		StopReason: "end_turn",
		Usage:      resultUsage,
		Model:      resultModel,
	}, nil
}

// IsClaudeCLIAvailable checks if the claude CLI is installed and accessible.
func IsClaudeCLIAvailable() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}
