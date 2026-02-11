package llm

import (
	"context"
	"strings"
	"testing"
)

// --- Provider Interface Compliance ---

func TestAnthropicProviderImplementsProvider(t *testing.T) {
	var _ Provider = (*AnthropicProvider)(nil)
}

func TestGoSDKProviderImplementsSessionProvider(t *testing.T) {
	var _ SessionProvider = (*GoSDKProvider)(nil)
}

func TestNodeSDKProviderImplementsSessionProvider(t *testing.T) {
	var _ SessionProvider = (*NodeSDKProvider)(nil)
}

// --- Provider Construction ---

func TestNewAnthropicProvider(t *testing.T) {
	p := NewAnthropicProvider("test-key")
	if p == nil {
		t.Fatal("NewAnthropicProvider returned nil")
	}
	if p.Name() != "apikey" {
		t.Errorf("Name() = %q, want %q", p.Name(), "apikey")
	}
}

func TestNewGoSDKProvider(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	if p == nil {
		t.Fatal("NewGoSDKProvider returned nil")
	}
	if p.Name() != "gosdk" {
		t.Errorf("Name() = %q, want %q", p.Name(), "gosdk")
	}
}

func TestNewGoSDKProviderWithToolExecutor(t *testing.T) {
	executor := &mockToolExecutor{}
	p := NewGoSDKProvider("http://localhost:8080", executor)
	if p.toolExecutor == nil {
		t.Error("toolExecutor should be set when passed to constructor")
	}
}

func TestNewNodeSDKProvider(t *testing.T) {
	p := NewNodeSDKProvider("http://localhost:8080")
	if p == nil {
		t.Fatal("NewNodeSDKProvider returned nil")
	}
	if p.Name() != "nodesdk" {
		t.Errorf("Name() = %q, want %q", p.Name(), "nodesdk")
	}
}

// --- GoSDK Session Management ---

func TestGoSDKSessionManagement(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")

	// No sessions initially
	if p.HasActiveSession(1) {
		t.Error("should not have active session initially")
	}

	// Simulate session creation
	p.mu.Lock()
	p.sessions[1] = &goSDKSession{sessionID: "test-session-123"}
	p.mu.Unlock()

	if !p.HasActiveSession(1) {
		t.Error("should have active session after adding")
	}
	if p.HasActiveSession(2) {
		t.Error("should not have session for conversation 2")
	}

	// Close session
	if err := p.CloseSession(1); err != nil {
		t.Errorf("CloseSession error: %v", err)
	}
	if p.HasActiveSession(1) {
		t.Error("should not have session after closing")
	}

	// Close all sessions
	p.mu.Lock()
	p.sessions[10] = &goSDKSession{sessionID: "s1"}
	p.sessions[20] = &goSDKSession{sessionID: "s2"}
	p.mu.Unlock()

	p.CloseAllSessions()
	if p.HasActiveSession(10) || p.HasActiveSession(20) {
		t.Error("CloseAllSessions should remove all sessions")
	}
}

// --- NodeSDK Session Management ---

func TestNodeSDKSessionManagement(t *testing.T) {
	p := NewNodeSDKProvider("http://localhost:8080")

	if p.HasActiveSession(1) {
		t.Error("should not have active session initially")
	}

	p.mu.Lock()
	p.sessions[1] = "node-session-abc"
	p.mu.Unlock()

	if !p.HasActiveSession(1) {
		t.Error("should have active session after adding")
	}
}

// --- GoSDK Budget Control ---

func TestGoSDKBudgetControl(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	if p.budgetUSD != 0 {
		t.Error("budget should be 0 by default")
	}

	p.SetBudgetUSD(5.0)
	if p.budgetUSD != 5.0 {
		t.Errorf("budgetUSD = %f, want 5.0", p.budgetUSD)
	}
}

// --- Tool Registration ---

func TestChatToolsNotEmpty(t *testing.T) {
	tools := ChatTools()
	if len(tools) == 0 {
		t.Error("ChatTools() should return tools")
	}

	// Check that all tools have required fields
	for _, td := range tools {
		if td.Name == "" {
			t.Error("tool has empty name")
		}
		if td.Description == "" {
			t.Errorf("tool %q has empty description", td.Name)
		}
		if td.InputSchema.Type != "object" {
			t.Errorf("tool %q input schema type = %q, want 'object'", td.Name, td.InputSchema.Type)
		}
	}
}

func TestPlanningToolsSubset(t *testing.T) {
	chatTools := ChatTools()
	planningTools := PlanningTools()

	if len(planningTools) >= len(chatTools) {
		t.Error("PlanningTools should be a subset of ChatTools")
	}
	if len(planningTools) == 0 {
		t.Error("PlanningTools should not be empty")
	}
}

func TestConvertInputSchema(t *testing.T) {
	schema := ToolDefinitionInput{
		Type: "object",
		Properties: map[string]ToolPropertySchema{
			"name": {Type: "string", Description: "A name"},
			"id":   {Type: "string", Description: "An ID"},
		},
		Required: []string{"name"},
	}

	result := ConvertInputSchema(schema)
	if result["type"] != "object" {
		t.Errorf("type = %v, want 'object'", result["type"])
	}
	if result["required"] == nil {
		t.Error("required should be set")
	}
	props, ok := result["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	if len(props) != 2 {
		t.Errorf("expected 2 properties, got %d", len(props))
	}
}

// --- GoSDK MCP Server ---

func TestGoSDKBuildMCPServerNilExecutor(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	server := p.buildMCPServer(1)
	if server != nil {
		t.Error("buildMCPServer should return nil when no toolExecutor")
	}
}

func TestGoSDKBuildMCPServerWithExecutor(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	p.SetToolExecutor(&mockToolExecutor{})
	server := p.buildMCPServer(1)
	if server == nil {
		t.Error("buildMCPServer should return server when toolExecutor is set")
	}
}

// --- GoSDK Dual-Mode Routing ---

func TestGoSDKModeRoutingByConversationID(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")

	// Verify no sessions exist initially
	if len(p.sessions) != 0 {
		t.Error("no sessions should exist initially")
	}

	// The routing decision is based on ConversationID:
	// - ConversationID == 0 → one-shot (uses claudecode.Query)
	// - ConversationID > 0 → interactive (uses persistent Client)
	//
	// We can't test the actual calls without Claude CLI, but we verify
	// the option-building logic differs correctly between modes.
	_ = strings.Contains // suppress unused import

	req := &Request{
		System: "test",
		Model:  "claude-sonnet-4-5-20250929",
	}

	oneShotOpts := p.buildOneShotOptions(req)
	interactiveOpts := p.buildInteractiveOptions(req, 1)

	// Interactive should have more options (budget, MCP are possible)
	// One-shot should always be leaner
	if len(oneShotOpts) > len(interactiveOpts) {
		t.Error("one-shot options should not exceed interactive options")
	}
}

func TestGoSDKOneShotOptions(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	p.SetBudgetUSD(5.0)
	p.SetToolExecutor(&mockToolExecutor{})

	req := &Request{
		System: "test system prompt",
		Model:  "claude-sonnet-4-5-20250929",
	}

	opts := p.buildOneShotOptions(req)
	// One-shot options should NOT include budget or MCP tools
	// We can only verify count and that it builds without error
	if len(opts) < 3 {
		t.Errorf("buildOneShotOptions should have at least 3 options (permission, maxTurns, model), got %d", len(opts))
	}
}

func TestGoSDKInteractiveOptions(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	p.SetBudgetUSD(5.0)
	p.SetToolExecutor(&mockToolExecutor{})

	req := &Request{
		System: "test system prompt",
		Model:  "claude-sonnet-4-5-20250929",
	}

	opts := p.buildInteractiveOptions(req, 1)
	// Interactive options should include budget, MCP server, allowedTools
	if len(opts) < 6 {
		t.Errorf("buildInteractiveOptions should have at least 6 options (permission, maxTurns, model, budget, mcpServer, allowedTools), got %d", len(opts))
	}
}

func TestGoSDKInteractiveOptionsNoBudget(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	// No budget set, no tool executor
	req := &Request{Model: "claude-sonnet-4-5-20250929"}
	opts := p.buildInteractiveOptions(req, 1)
	// Should have: permission, maxTurns, model (no budget, no MCP)
	if len(opts) != 3 {
		t.Errorf("buildInteractiveOptions without budget/executor should have 3 options, got %d", len(opts))
	}
}

// --- Streaming Errors ---

func TestGoSDKStreamMessageEmptyPrompt(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	req := &Request{
		Messages: []Message{NewTextMessage("user", "")},
	}
	_, err := p.StreamMessage(context.Background(), req, func(StreamEvent) error { return nil })
	if err == nil {
		t.Error("should error on empty prompt")
	}
}

func TestGoSDKStreamMessageEmptyPromptOneShot(t *testing.T) {
	p := NewGoSDKProvider("http://localhost:8080")
	req := &Request{
		ConversationID: 0, // one-shot
		Messages:       []Message{NewTextMessage("user", "")},
	}
	_, err := p.StreamMessage(context.Background(), req, func(StreamEvent) error { return nil })
	if err == nil {
		t.Error("should error on empty prompt in one-shot mode")
	}
}

func TestNodeSDKStreamMessageEmptyPrompt(t *testing.T) {
	p := NewNodeSDKProvider("http://localhost:8080")
	p.started = true // pretend started to avoid actually starting
	p.sidecarURL = "http://127.0.0.1:99999"
	req := &Request{
		Messages: []Message{NewTextMessage("user", "")},
	}
	_, err := p.StreamMessage(context.Background(), req, func(StreamEvent) error { return nil })
	if err == nil {
		t.Error("should error on empty prompt")
	}
}

// --- Types ---

func TestNewTextMessage(t *testing.T) {
	msg := NewTextMessage("user", "Hello")
	if msg.Role != "user" {
		t.Errorf("Role = %q, want 'user'", msg.Role)
	}
	if msg.TextContent() != "Hello" {
		t.Errorf("TextContent() = %q, want 'Hello'", msg.TextContent())
	}
}

func TestMessageTextContent(t *testing.T) {
	msg := Message{
		Role: "assistant",
		Content: []ContentBlock{
			{Type: "text", Text: "Part 1"},
			{Type: "tool_use", Name: "some_tool"},
			{Type: "text", Text: " Part 2"},
		},
	}
	if got := msg.TextContent(); got != "Part 1 Part 2" {
		t.Errorf("TextContent() = %q, want 'Part 1 Part 2'", got)
	}
}

// --- Pricing ---

func TestCalculateCost(t *testing.T) {
	cost := CalculateCost("claude-sonnet-4-5-20250929", 1000, 500)
	if cost <= 0 {
		t.Error("cost should be positive for known model")
	}

	// Unknown model falls back to default pricing
	cost = CalculateCost("unknown-model", 1000, 500)
	if cost <= 0 {
		t.Error("unknown model should use default pricing")
	}
}

// --- Mock ---

type mockToolExecutor struct{}

func (m *mockToolExecutor) ExecuteTool(_ context.Context, name string, _ map[string]any, _ int64) (string, error) {
	return "mock result for " + name, nil
}
