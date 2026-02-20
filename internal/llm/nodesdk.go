package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NodeSDKProvider manages a Node.js sidecar process running the official
// @anthropic-ai/claude-agent-sdk. Communication happens via HTTP.
type NodeSDKProvider struct {
	apiURL     string // OpenPoet API URL (for tool callbacks from sidecar)
	sidecarURL string // http://127.0.0.1:{port}
	sidecarCmd *exec.Cmd
	sessions   map[int64]string // conversationID -> sessionID
	mu         sync.RWMutex
	port       int
	started    bool
	client     *http.Client
}

// NewNodeSDKProvider creates a new Node.js SDK sidecar provider.
func NewNodeSDKProvider(apiURL string) *NodeSDKProvider {
	return &NodeSDKProvider{
		apiURL:   apiURL,
		sessions: make(map[int64]string),
		client:   &http.Client{Timeout: 180 * time.Second},
	}
}

// Name returns the provider identifier.
func (p *NodeSDKProvider) Name() string {
	return "nodesdk"
}

// sidecarDir returns the path to the sidecar scripts directory.
func (p *NodeSDKProvider) sidecarDir() string {
	// Look for the sidecar relative to the executable
	exePath, err := os.Executable()
	if err != nil {
		return "scripts/nodesdk-sidecar"
	}
	dir := filepath.Dir(exePath)
	// Try: same dir as executable -> ../scripts/nodesdk-sidecar -> ./scripts/nodesdk-sidecar
	candidates := []string{
		filepath.Join(dir, "scripts", "nodesdk-sidecar"),
		filepath.Join(dir, "..", "scripts", "nodesdk-sidecar"),
		"scripts/nodesdk-sidecar",
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "index.js")); err == nil {
			return c
		}
	}
	return "scripts/nodesdk-sidecar"
}

// ensureDependencies runs npm install if node_modules is missing.
func (p *NodeSDKProvider) ensureDependencies() error {
	dir := p.sidecarDir()
	nodeModules := filepath.Join(dir, "node_modules")
	if _, err := os.Stat(nodeModules); err == nil {
		return nil // already installed
	}
	log.Printf("[NodeSDK] Installing sidecar dependencies...")
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("npm not found in PATH: %w", err)
	}
	cmd := exec.Command(npmPath, "install", "--production")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if out, err := cmd.Output(); err != nil {
		return fmt.Errorf("npm install failed: %w (output: %s)", err, string(out))
	}
	log.Printf("[NodeSDK] Dependencies installed")
	return nil
}

// Start launches the Node.js sidecar process.
func (p *NodeSDKProvider) Start() error {
	if p.started {
		return nil
	}

	// Find node
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("node not found in PATH: %w", err)
	}

	// Ensure npm dependencies are installed
	if err := p.ensureDependencies(); err != nil {
		return fmt.Errorf("sidecar dependencies: %w", err)
	}

	// Allocate a random port
	p.port = 10000 + rand.Intn(50000)
	sidecarPath := filepath.Join(p.sidecarDir(), "index.js")

	if _, err := os.Stat(sidecarPath); err != nil {
		return fmt.Errorf("sidecar script not found at %s: %w", sidecarPath, err)
	}

	p.sidecarCmd = exec.Command(nodePath, sidecarPath,
		"--port", fmt.Sprintf("%d", p.port),
		"--api-url", p.apiURL,
	)
	p.sidecarCmd.Stderr = os.Stderr // Pipe sidecar logs to OpenPoet stderr

	if err := p.sidecarCmd.Start(); err != nil {
		return fmt.Errorf("failed to start sidecar: %w", err)
	}

	p.sidecarURL = fmt.Sprintf("http://127.0.0.1:%d", p.port)

	// Wait for health check
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		resp, err := p.client.Get(p.sidecarURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				p.started = true
				log.Printf("[NodeSDK] Sidecar started on port %d (PID: %d)", p.port, p.sidecarCmd.Process.Pid)
				return nil
			}
		}
	}

	// Failed to start — kill the process
	if p.sidecarCmd.Process != nil {
		p.sidecarCmd.Process.Kill()
	}
	return fmt.Errorf("sidecar failed to start (health check timeout on port %d)", p.port)
}

// Stop gracefully shuts down the sidecar process.
func (p *NodeSDKProvider) Stop() {
	if !p.started || p.sidecarCmd == nil {
		return
	}

	// Try graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", p.sidecarURL+"/shutdown", nil)
	if req != nil {
		p.client.Do(req)
	}

	// Wait for process to exit
	done := make(chan error, 1)
	go func() { done <- p.sidecarCmd.Wait() }()

	select {
	case <-done:
		// Clean exit
	case <-time.After(5 * time.Second):
		// Force kill
		if p.sidecarCmd.Process != nil {
			p.sidecarCmd.Process.Kill()
		}
	}

	p.started = false
	log.Printf("[NodeSDK] Sidecar stopped")
}

// nodeQueryRequest is the JSON body sent to the sidecar's /query endpoint.
type nodeQueryRequest struct {
	ConversationID int64  `json:"conversation_id"`
	Message        string `json:"message"`
	SystemPrompt   string `json:"system_prompt,omitempty"`
	Model          string `json:"model,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
}

// nodeSSEEvent is a single SSE event from the sidecar.
type nodeSSEEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// StreamMessage sends a message via the Node.js SDK sidecar.
func (p *NodeSDKProvider) StreamMessage(ctx context.Context, req *Request, callback StreamCallback) (*Response, error) {
	// Lazy start
	if !p.started {
		if err := p.Start(); err != nil {
			return nil, err
		}
	}

	convID := req.ConversationID

	// Get latest user message
	var prompt string
	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]
		prompt = lastMsg.TextContent()
	}
	if prompt == "" {
		return nil, fmt.Errorf("no message to send")
	}

	// Build request
	queryReq := nodeQueryRequest{
		ConversationID: convID,
		Message:        prompt,
		SystemPrompt:   req.System,
		Model:          req.Model,
	}

	// Resume existing session
	p.mu.RLock()
	if sid, ok := p.sessions[convID]; ok {
		queryReq.SessionID = sid
	}
	p.mu.RUnlock()

	// POST to sidecar
	body, _ := json.Marshal(queryReq)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.sidecarURL+"/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse SSE stream from sidecar
	return p.parseSSEStream(ctx, resp.Body, callback, convID)
}

// parseSSEStream reads SSE events from the sidecar and translates them to OpenPoet StreamEvents.
func (p *NodeSDKProvider) parseSSEStream(ctx context.Context, reader io.Reader, callback StreamCallback, convID int64) (*Response, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 512*1024), 512*1024)

	var response Response
	response.StopReason = "end_turn"
	var fullText strings.Builder
	blockStarted := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			continue
		}

		var event nodeSSEEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text":
			var textData struct {
				Text string `json:"text"`
			}
			json.Unmarshal(event.Data, &textData)

			if !blockStarted {
				callback(StreamEvent{
					Type:         "content_block_start",
					Index:        0,
					ContentBlock: &ContentBlock{Type: "text", Text: ""},
				})
				blockStarted = true
			}
			callback(StreamEvent{
				Type:  "content_block_delta",
				Index: 0,
				Delta: &StreamDelta{Type: "text_delta", Text: textData.Text},
			})
			fullText.WriteString(textData.Text)

		case "tool_start":
			var toolData struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			json.Unmarshal(event.Data, &toolData)
			callback(StreamEvent{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: &ContentBlock{Type: "tool_use", ID: toolData.ID, Name: toolData.Name},
			})

		case "tool_result":
			// Tool results are handled by the sidecar; we just forward the SSE event
			// The ai.go handler will pick this up via the callback

		case "done":
			var doneData struct {
				ConversationID int64          `json:"conversation_id"`
				SessionID      string         `json:"session_id"`
				Usage          map[string]any `json:"usage"`
				CostUSD        float64        `json:"cost_usd"`
				Model          string         `json:"model"`
			}
			json.Unmarshal(event.Data, &doneData)

			if doneData.SessionID != "" {
				p.mu.Lock()
				p.sessions[convID] = doneData.SessionID
				p.mu.Unlock()
				response.SessionID = doneData.SessionID
			}

			response.CostUSD = doneData.CostUSD
			if doneData.Model != "" {
				response.Model = doneData.Model
			}

			// Extract usage from map
			if doneData.Usage != nil {
				if v, ok := doneData.Usage["input_tokens"].(float64); ok {
					response.Usage.InputTokens = int(v)
				}
				if v, ok := doneData.Usage["output_tokens"].(float64); ok {
					response.Usage.OutputTokens = int(v)
				}
				if v, ok := doneData.Usage["cache_read_input_tokens"].(float64); ok {
					response.Usage.CacheReadTokens = int(v)
				}
				if v, ok := doneData.Usage["cache_creation_input_tokens"].(float64); ok {
					response.Usage.CacheCreationTokens = int(v)
				}
			}

		case "error":
			var errData struct {
				Message string `json:"message"`
			}
			json.Unmarshal(event.Data, &errData)
			if blockStarted {
				callback(StreamEvent{Type: "content_block_stop", Index: 0})
			}
			return nil, fmt.Errorf("sidecar error: %s", errData.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE stream read error: %w", err)
	}

	// Close text block
	if blockStarted {
		callback(StreamEvent{Type: "content_block_stop", Index: 0})
	}

	// Emit message_delta with stop_reason
	callback(StreamEvent{
		Type:  "message_delta",
		Delta: &StreamDelta{StopReason: "end_turn"},
	})

	response.Content = []ContentBlock{{Type: "text", Text: fullText.String()}}
	return &response, nil
}

// HasActiveSession returns true if a session exists for the given conversation.
func (p *NodeSDKProvider) HasActiveSession(conversationID int64) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, exists := p.sessions[conversationID]
	return exists
}

// CloseSession terminates the session for a conversation.
func (p *NodeSDKProvider) CloseSession(conversationID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started && p.sidecarURL != "" {
		url := fmt.Sprintf("%s/sessions/%d/close", p.sidecarURL, conversationID)
		req, _ := http.NewRequest("POST", url, nil)
		if req != nil {
			p.client.Do(req)
		}
	}

	delete(p.sessions, conversationID)
	return nil
}

// CloseAllSessions terminates all active sessions and stops the sidecar.
func (p *NodeSDKProvider) CloseAllSessions() {
	p.mu.Lock()
	for id := range p.sessions {
		delete(p.sessions, id)
	}
	p.mu.Unlock()

	p.Stop()
	log.Printf("[NodeSDK] Closed all sessions and stopped sidecar")
}
