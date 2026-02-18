package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"devmanager/internal/llm"
)

// ValidatorFunc is a deterministic check on a model response.
type ValidatorFunc func(response string, toolCalls []ToolCallRecord) ValidatorResult

// ValidatorResult holds the outcome of a programmatic validator.
type ValidatorResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// MockToolExecutor implements llm.ToolExecutor with canned responses.
type MockToolExecutor struct {
	project *TestProject
}

// NewMockToolExecutor creates an executor backed by a test project.
func NewMockToolExecutor(project *TestProject) *MockToolExecutor {
	return &MockToolExecutor{project: project}
}

// ExecuteTool returns canned responses for known tools.
func (m *MockToolExecutor) ExecuteTool(_ context.Context, name string, input map[string]any, _ int64) (string, error) {
	switch name {
	case "list_tasks":
		return m.project.TasksJSON(), nil
	case "get_task":
		return `{"id": 1, "title": "Implement JWT authentication", "status": "todo", "priority": "high", "description": "Add JWT auth to API endpoints."}`, nil
	case "create_task":
		return `{"status": "pending_approval", "task_id": 99, "message": "Task proposal created. User must approve."}`, nil
	case "update_task":
		return `{"status": "updated", "task_id": 1}`, nil
	case "delete_task":
		return `{"status": "pending_approval", "message": "Deletion proposal created."}`, nil
	case "list_projects":
		return m.project.ProjectsJSON(), nil
	case "list_directory":
		return m.project.DirectoryJSON(input["path"]), nil
	case "read_file":
		return m.project.FileContent(input["path"]), nil
	case "find_files":
		return m.project.GrepContentResponse(input["pattern"]), nil
	case "grep_content":
		return m.project.GrepContentResponse(input["pattern"]), nil
	case "get_memory_doc":
		return m.project.MemoryDocResponse(), nil
	case "update_memory_doc":
		return `{"status": "pending_approval", "message": "Proposal created. User must approve via viewer."}`, nil
	case "create_document":
		return `{"document_id": 1, "status": "created", "viewer_url": "/documents/view/1"}`, nil
	case "create_skill":
		return `{"skill_id": 1, "status": "created", "message": "Skill created successfully."}`, nil
	case "update_skill":
		return `{"status": "updated", "skill_id": 1}`, nil
	case "delete_skill":
		return `{"status": "deleted", "skill_id": 1}`, nil
	case "list_skills":
		return `[{"id": 1, "name": "go-best-practices", "category": "coding", "enabled": true}, {"id": 2, "name": "api-testing", "category": "testing", "enabled": true}]`, nil
	case "list_mcp_servers":
		return `[]`, nil
	case "create_mcp_server":
		return `{"id": 1, "status": "created"}`, nil
	case "sync_config":
		return `{"status": "synced", "projects": 1}`, nil
	case "update_setting":
		return `{"status": "updated"}`, nil
	case "get_task_report":
		return `{"total": 5, "todo": 3, "in_progress": 1, "done": 1, "urgent": 1, "overdue": 0, "next_recommended": {"id": 4, "title": "Fix email validation"}}`, nil
	default:
		return fmt.Sprintf(`{"status": "ok", "tool": "%s"}`, name), nil
	}
}

// ──── Programmatic Validators ────

// ValidateToolCalled checks that a specific tool was called at least once.
func ValidateToolCalled(toolName string) ValidatorFunc {
	return func(_ string, toolCalls []ToolCallRecord) ValidatorResult {
		for _, tc := range toolCalls {
			if tc.Name == toolName {
				return ValidatorResult{
					Name:   fmt.Sprintf("tool_called:%s", toolName),
					Passed: true,
					Detail: fmt.Sprintf("Tool %s was called", toolName),
				}
			}
		}
		called := make([]string, len(toolCalls))
		for i, tc := range toolCalls {
			called[i] = tc.Name
		}
		return ValidatorResult{
			Name:   fmt.Sprintf("tool_called:%s", toolName),
			Passed: false,
			Detail: fmt.Sprintf("Tool %s was NOT called. Called: %v", toolName, called),
		}
	}
}

// ValidateToolCalledAtLeast checks that a tool was called at least N times.
func ValidateToolCalledAtLeast(toolName string, minCalls int) ValidatorFunc {
	return func(_ string, toolCalls []ToolCallRecord) ValidatorResult {
		count := 0
		for _, tc := range toolCalls {
			if tc.Name == toolName {
				count++
			}
		}
		passed := count >= minCalls
		return ValidatorResult{
			Name:   fmt.Sprintf("tool_called_min:%s>=%d", toolName, minCalls),
			Passed: passed,
			Detail: fmt.Sprintf("Tool %s called %d times (min: %d)", toolName, count, minCalls),
		}
	}
}

// ValidateAnyToolCalled checks that at least one of the specified tools was called.
func ValidateAnyToolCalled(toolNames ...string) ValidatorFunc {
	return func(_ string, toolCalls []ToolCallRecord) ValidatorResult {
		nameSet := make(map[string]bool)
		for _, n := range toolNames {
			nameSet[n] = true
		}
		for _, tc := range toolCalls {
			if nameSet[tc.Name] {
				return ValidatorResult{
					Name:   fmt.Sprintf("any_tool_called:%s", strings.Join(toolNames, "|")),
					Passed: true,
					Detail: fmt.Sprintf("Tool %s was called", tc.Name),
				}
			}
		}
		return ValidatorResult{
			Name:   fmt.Sprintf("any_tool_called:%s", strings.Join(toolNames, "|")),
			Passed: false,
			Detail: fmt.Sprintf("None of %v were called", toolNames),
		}
	}
}

// ValidateNoToolCalled checks that no tools were called.
func ValidateNoToolCalled() ValidatorFunc {
	return func(_ string, toolCalls []ToolCallRecord) ValidatorResult {
		if len(toolCalls) == 0 {
			return ValidatorResult{
				Name:   "no_tool_called",
				Passed: true,
				Detail: "No tools were called",
			}
		}
		called := make([]string, len(toolCalls))
		for i, tc := range toolCalls {
			called[i] = tc.Name
		}
		return ValidatorResult{
			Name:   "no_tool_called",
			Passed: false,
			Detail: fmt.Sprintf("Tools were called: %v", called),
		}
	}
}

// ValidateMaxSentences checks that the response has at most N sentences.
func ValidateMaxSentences(n int) ValidatorFunc {
	return func(response string, _ []ToolCallRecord) ValidatorResult {
		sentences := countSentences(response)
		passed := sentences <= n
		return ValidatorResult{
			Name:   fmt.Sprintf("max_sentences:%d", n),
			Passed: passed,
			Detail: fmt.Sprintf("Response has %d sentences (max: %d)", sentences, n),
		}
	}
}

// ValidateContains checks that the response contains a substring.
func ValidateContains(substring string) ValidatorFunc {
	return func(response string, _ []ToolCallRecord) ValidatorResult {
		passed := strings.Contains(strings.ToLower(response), strings.ToLower(substring))
		return ValidatorResult{
			Name:   fmt.Sprintf("contains:%s", substring),
			Passed: passed,
			Detail: fmt.Sprintf("Response %s '%s'", boolWord(passed, "contains", "does NOT contain"), substring),
		}
	}
}

// ValidateNotContains checks that the response does NOT contain a substring.
func ValidateNotContains(substring string) ValidatorFunc {
	return func(response string, _ []ToolCallRecord) ValidatorResult {
		passed := !strings.Contains(response, substring)
		return ValidatorResult{
			Name:   fmt.Sprintf("not_contains:%s", substring),
			Passed: passed,
			Detail: fmt.Sprintf("Response %s '%s'", boolWord(passed, "does not contain", "CONTAINS"), substring),
		}
	}
}

// ValidateResponseLanguage checks if the response is in the expected language.
// Uses a simple heuristic based on common language-specific words.
func ValidateResponseLanguage(lang string) ValidatorFunc {
	return func(response string, _ []ToolCallRecord) ValidatorResult {
		lower := strings.ToLower(response)
		isPt := strings.ContainsAny(lower, "ãõçê") ||
			strings.Contains(lower, " temos ") ||
			strings.Contains(lower, " projeto ") ||
			strings.Contains(lower, " configurado")
		isEn := strings.Contains(lower, " have ") ||
			strings.Contains(lower, " project") ||
			strings.Contains(lower, " there ") ||
			strings.Contains(lower, "configured") ||
			strings.Contains(lower, " we ")

		var passed bool
		switch lang {
		case "en":
			passed = isEn || !isPt
		default:
			passed = true
		}

		return ValidatorResult{
			Name:   fmt.Sprintf("language:%s", lang),
			Passed: passed,
			Detail: fmt.Sprintf("Expected %s (pt_signals=%v, en_signals=%v)", lang, isPt, isEn),
		}
	}
}

// ──── LLM-as-Judge ────

const judgeSystemPrompt = `You are an expert evaluator for an AI assistant called "DevManager AI".
Your job is to score the assistant's response on 5 dimensions, each from 1 to 5.

Respond with ONLY a valid JSON object (no markdown, no code blocks):
{
  "correctness": N,
  "tool_usage": N,
  "conciseness": N,
  "completeness": N,
  "relevance": N,
  "reasoning": "Brief explanation of your scores"
}

Scoring guide:
- 5: Excellent — meets all criteria perfectly
- 4: Good — minor issues only
- 3: Acceptable — meets basic requirements but has notable gaps
- 2: Poor — significant issues, partially meets requirements
- 1: Failed — does not meet requirements at all

IMPORTANT context about DevManager AI:
- It must be VERY brief (1-2 sentences max for most responses)
- It must use the correct tools for each request
- It must NOT announce plans before acting
- It must NOT paste document content in chat (uses create_document or viewer cards)
- It must match the user's language (Portuguese or English)`

// evaluateWithJudge sends the scenario + response to the judge model for scoring.
func evaluateWithJudge(ctx context.Context, judge *llm.AnthropicProvider, judgeModel string, scenario Scenario, result *ScenarioResult) (JudgeScores, float64, error) {
	// Build the evaluation prompt
	var sb strings.Builder
	sb.WriteString("## Original User Message\n")
	for _, m := range scenario.Messages {
		sb.WriteString(m.TextContent())
		sb.WriteString("\n")
	}

	sb.WriteString("\n## Assistant's Text Response\n")
	if result.TextResponse != "" {
		sb.WriteString(result.TextResponse)
	} else {
		sb.WriteString("(no text response)")
	}
	sb.WriteString("\n")

	if len(result.ToolCalls) > 0 {
		sb.WriteString("\n## Tool Calls Made\n")
		for _, tc := range result.ToolCalls {
			inputJSON, _ := json.Marshal(tc.Input)
			sb.WriteString(fmt.Sprintf("- %s(%s)\n", tc.Name, string(inputJSON)))
		}
	} else {
		sb.WriteString("\n## Tool Calls Made\nNone\n")
	}

	sb.WriteString("\n## Evaluation Rubric\n")
	sb.WriteString(scenario.JudgeRubric)
	sb.WriteString("\n")

	if len(scenario.ExpectedTools) > 0 {
		sb.WriteString("\n## Expected Tools\n")
		sb.WriteString(strings.Join(scenario.ExpectedTools, ", "))
		sb.WriteString("\n")
	}

	req := &llm.Request{
		System: judgeSystemPrompt,
		Messages: []llm.Message{
			llm.NewTextMessage("user", sb.String()),
		},
		MaxTokens: 1024,
		Model:     judgeModel,
	}

	resp, err := judge.StreamMessage(ctx, req, nil)
	if err != nil {
		return JudgeScores{}, 0, fmt.Errorf("judge API call: %w", err)
	}

	cost := llm.CalculateCost(resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)

	// Parse judge response
	var text string
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}

	// Extract JSON from response (handle potential markdown wrapping)
	text = extractJSON(text)

	var scores JudgeScores
	if err := json.Unmarshal([]byte(text), &scores); err != nil {
		return JudgeScores{
			Correctness:  3,
			ToolUsage:    3,
			Conciseness:  3,
			Completeness: 3,
			Relevance:    3,
			Reasoning:    fmt.Sprintf("Failed to parse judge response: %s", text),
		}, cost, nil
	}

	// Clamp scores to 1-5
	scores.Correctness = clamp(scores.Correctness, 1, 5)
	scores.ToolUsage = clamp(scores.ToolUsage, 1, 5)
	scores.Conciseness = clamp(scores.Conciseness, 1, 5)
	scores.Completeness = clamp(scores.Completeness, 1, 5)
	scores.Relevance = clamp(scores.Relevance, 1, 5)

	return scores, cost, nil
}

// ──── Helpers ────

func countSentences(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	count := 0
	for _, r := range text {
		if r == '.' || r == '!' || r == '?' {
			count++
		}
	}
	if count == 0 && len(text) > 0 {
		count = 1 // text without punctuation counts as 1 sentence
	}
	return count
}

func boolWord(b bool, trueWord, falseWord string) string {
	if b {
		return trueWord
	}
	return falseWord
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// extractJSON finds the first JSON object in the text.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	// Remove markdown code block if present
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		var jsonLines []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				jsonLines = append(jsonLines, line)
			}
		}
		text = strings.Join(jsonLines, "\n")
	}
	// Find first { and last }
	start := strings.IndexRune(text, '{')
	end := strings.LastIndexFunc(text, func(r rune) bool { return r == '}' })
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

