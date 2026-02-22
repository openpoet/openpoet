package benchmark

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"openpoet/internal/llm"
)

// ScenarioCategory groups scenarios by type.
type ScenarioCategory string

const (
	CategoryTasks        ScenarioCategory = "tasks"
	CategorySkills       ScenarioCategory = "skills"
	CategoryCode         ScenarioCategory = "code"
	CategoryTools        ScenarioCategory = "tools"
	CategoryMemory       ScenarioCategory = "memory"
	CategoryConversation ScenarioCategory = "conversation"
)

// Scenario defines a benchmark test case.
type Scenario struct {
	ID            string
	Name          string
	Category      ScenarioCategory
	Description   string
	Messages      []llm.Message
	Validators    []ValidatorFunc
	JudgeRubric   string
	ExpectedTools []string
}

// ToolCallRecord captures a tool call made during a scenario.
type ToolCallRecord struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ScenarioResult holds the outcome of running one scenario.
type ScenarioResult struct {
	ScenarioID   string            `json:"scenario_id"`
	ScenarioName string            `json:"scenario_name"`
	Category     ScenarioCategory  `json:"category"`
	ToolCalls    []ToolCallRecord  `json:"tool_calls"`
	TextResponse string            `json:"text_response"`
	Validators   []ValidatorResult `json:"validators"`
	JudgeScores  JudgeScores       `json:"judge_scores"`
	OverallScore float64           `json:"overall_score"`
	Cost         float64           `json:"cost_usd"`
	Latency      time.Duration     `json:"latency_ms"`
	Error        string            `json:"error,omitempty"`
}

// JudgeScores holds the LLM-as-Judge evaluation scores.
type JudgeScores struct {
	Correctness  int    `json:"correctness"`
	ToolUsage    int    `json:"tool_usage"`
	Conciseness  int    `json:"conciseness"`
	Completeness int    `json:"completeness"`
	Relevance    int    `json:"relevance"`
	Reasoning    string `json:"reasoning"`
}

// Average returns the mean of all 5 dimensions.
func (j JudgeScores) Average() float64 {
	return float64(j.Correctness+j.ToolUsage+j.Conciseness+j.Completeness+j.Relevance) / 5.0
}

// BenchmarkReport is the complete output of a benchmark run.
type BenchmarkReport struct {
	Model          string                       `json:"model"`
	JudgeModel     string                       `json:"judge_model"`
	Timestamp      time.Time                    `json:"timestamp"`
	Scenarios      []ScenarioResult             `json:"scenarios"`
	CategoryScores map[ScenarioCategory]float64 `json:"category_scores"`
	OverallScore   float64                      `json:"overall_score"`
	TotalCost      float64                      `json:"total_cost_usd"`
	TotalDuration  time.Duration                `json:"total_duration_ms"`
}

// RunConfig holds CLI configuration for a benchmark run.
type RunConfig struct {
	APIKey     string
	Model      string
	JudgeModel string
	Scenarios  string
	Runs       int
	Output     string
	Verbose    bool
	List       bool
}

// BenchmarkRunner orchestrates benchmark execution.
type BenchmarkRunner struct {
	subject  *llm.AnthropicProvider
	judge    *llm.AnthropicProvider
	project  *TestProject
	executor *MockToolExecutor
	config   RunConfig
	verbose  bool
}

// NewBenchmarkRunner creates a runner with configured providers.
func NewBenchmarkRunner(cfg RunConfig) *BenchmarkRunner {
	subject := llm.NewAnthropicProvider(cfg.APIKey)
	judge := llm.NewAnthropicProvider(cfg.APIKey)
	project := NewTestProject()
	executor := NewMockToolExecutor(project)

	return &BenchmarkRunner{
		subject:  subject,
		judge:    judge,
		project:  project,
		executor: executor,
		config:   cfg,
		verbose:  cfg.Verbose,
	}
}

// Run executes all filtered scenarios and produces a report.
func (r *BenchmarkRunner) Run(ctx context.Context) (*BenchmarkReport, error) {
	scenarios := filterScenarios(AllScenarios(), r.config.Scenarios)
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("no scenarios match filter %q", r.config.Scenarios)
	}

	report := &BenchmarkReport{
		Model:      r.config.Model,
		JudgeModel: r.config.JudgeModel,
		Timestamp:  time.Now(),
	}

	fmt.Printf("\n  OpenPoet AI Benchmark\n")
	fmt.Printf("  Model: %s\n", r.config.Model)
	fmt.Printf("  Judge: %s\n", r.config.JudgeModel)
	fmt.Printf("  Scenarios: %d | Runs per scenario: %d\n\n", len(scenarios), r.config.Runs)

	start := time.Now()

	for i, scenario := range scenarios {
		fmt.Printf("  [%d/%d] %s ... ", i+1, len(scenarios), scenario.Name)

		var bestResult *ScenarioResult
		for run := 0; run < r.config.Runs; run++ {
			result := r.runScenario(ctx, scenario)
			if bestResult == nil || result.OverallScore > bestResult.OverallScore {
				bestResult = result
			}
		}

		if bestResult.Error != "" {
			fmt.Printf("ERROR: %s\n", bestResult.Error)
		} else {
			validatorsPassed := 0
			for _, v := range bestResult.Validators {
				if v.Passed {
					validatorsPassed++
				}
			}
			fmt.Printf("%.1f/5  validators: %d/%d  $%.4f\n",
				bestResult.OverallScore, validatorsPassed, len(bestResult.Validators), bestResult.Cost)
		}

		if r.verbose && bestResult.Error == "" {
			fmt.Printf("         Response: %s\n", truncate(bestResult.TextResponse, 120))
			for _, tc := range bestResult.ToolCalls {
				fmt.Printf("         Tool: %s\n", tc.Name)
			}
			fmt.Printf("         Judge: %s\n", truncate(bestResult.JudgeScores.Reasoning, 120))
			fmt.Println()
		}

		report.Scenarios = append(report.Scenarios, *bestResult)
		report.TotalCost += bestResult.Cost
	}

	report.TotalDuration = time.Since(start)
	report.CategoryScores = computeCategoryScores(report.Scenarios)
	report.OverallScore = computeOverallScore(report.Scenarios)

	return report, nil
}

// runScenario executes a single scenario with a tool loop.
func (r *BenchmarkRunner) runScenario(ctx context.Context, s Scenario) *ScenarioResult {
	result := &ScenarioResult{
		ScenarioID:   s.ID,
		ScenarioName: s.Name,
		Category:     s.Category,
	}

	start := time.Now()

	// Build system prompt using the real production prompt
	systemPrompt := llm.ChatSystemPrompt(
		r.project.Skills(),
		r.project.Projects(),
		[]string{},
	)

	// Start with the scenario's messages
	messages := make([]llm.Message, len(s.Messages))
	copy(messages, s.Messages)

	tools := llm.ChatTools()
	var totalCost float64

	// Tool loop: max 5 iterations
	const maxIterations = 5
	for iter := 0; iter < maxIterations; iter++ {
		req := &llm.Request{
			System:    systemPrompt,
			Messages:  messages,
			Tools:     tools,
			MaxTokens: 4096,
			Model:     r.config.Model,
		}

		resp, err := r.subject.StreamMessage(ctx, req, nil)
		if err != nil {
			result.Error = fmt.Sprintf("API error: %v", err)
			result.Latency = time.Since(start)
			return result
		}

		totalCost += llm.CalculateCost(resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)

		// Collect text and tool calls from response
		var assistantBlocks []llm.ContentBlock
		for _, block := range resp.Content {
			assistantBlocks = append(assistantBlocks, block)
			switch block.Type {
			case "text":
				result.TextResponse += block.Text
			case "tool_use":
				inputMap, _ := toMap(block.Input)
				result.ToolCalls = append(result.ToolCalls, ToolCallRecord{
					Name:  block.Name,
					Input: inputMap,
				})
			}
		}

		// Add assistant response to messages
		messages = append(messages, llm.Message{
			Role:    "assistant",
			Content: assistantBlocks,
		})

		// If no tool use, we're done
		if resp.StopReason != "tool_use" {
			break
		}

		// Execute tool calls and add results
		var toolResultBlocks []llm.ContentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			inputMap, _ := toMap(block.Input)
			toolResult, err := r.executor.ExecuteTool(ctx, block.Name, inputMap, 0)
			if err != nil {
				toolResult = fmt.Sprintf(`{"error": "%s"}`, err.Error())
			}
			toolResultBlocks = append(toolResultBlocks, llm.ContentBlock{
				Type:      "tool_result",
				ToolUseID: block.ID,
				Content:   toolResult,
			})
		}

		messages = append(messages, llm.Message{
			Role:    "user",
			Content: toolResultBlocks,
		})
	}

	result.Cost = totalCost
	result.Latency = time.Since(start)

	// Run programmatic validators
	for _, vf := range s.Validators {
		result.Validators = append(result.Validators, vf(result.TextResponse, result.ToolCalls))
	}

	// Run LLM-as-Judge evaluation
	judgeScores, judgeCost, err := evaluateWithJudge(ctx, r.judge, r.config.JudgeModel, s, result)
	if err != nil {
		result.Error = fmt.Sprintf("judge error: %v", err)
	} else {
		result.JudgeScores = judgeScores
		result.Cost += judgeCost
	}

	// Compute overall score: judge average with validator penalty
	result.OverallScore = result.JudgeScores.Average()
	for _, v := range result.Validators {
		if !v.Passed {
			result.OverallScore -= 0.5
		}
	}
	if result.OverallScore < 0 {
		result.OverallScore = 0
	}

	return result
}

// RunCLI is the entry point for the benchmark subcommand.
func RunCLI(args []string) {
	fs := flag.NewFlagSet("benchmark", flag.ExitOnError)

	var cfg RunConfig
	fs.StringVar(&cfg.APIKey, "api-key", os.Getenv("ANTHROPIC_API_KEY"), "Anthropic API key")
	fs.StringVar(&cfg.Model, "model", llm.DefaultModel, "Model to benchmark")
	fs.StringVar(&cfg.JudgeModel, "judge-model", "claude-haiku-4-5-20251001", "Judge model for evaluation")
	fs.StringVar(&cfg.Scenarios, "scenarios", "all", "Scenario filter: all, tasks, skills, code, tools, memory, conversation")
	fs.IntVar(&cfg.Runs, "runs", 1, "Runs per scenario (best score kept)")
	fs.StringVar(&cfg.Output, "output", "", "Save results to JSON file")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "Show detailed output per scenario")
	fs.BoolVar(&cfg.List, "list", false, "List available scenarios and exit")

	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}

	// List mode
	if cfg.List {
		scenarios := AllScenarios()
		fmt.Printf("\nAvailable Benchmark Scenarios (%d total):\n\n", len(scenarios))
		currentCat := ScenarioCategory("")
		for _, s := range scenarios {
			if s.Category != currentCat {
				currentCat = s.Category
				fmt.Printf("  [%s]\n", currentCat)
			}
			fmt.Printf("    %-25s %s\n", s.ID, s.Description)
		}
		fmt.Println()
		return
	}

	if cfg.APIKey == "" {
		log.Fatal("API key required: use --api-key or set ANTHROPIC_API_KEY")
	}

	runner := NewBenchmarkRunner(cfg)
	ctx := context.Background()

	report, err := runner.Run(ctx)
	if err != nil {
		log.Fatalf("benchmark failed: %v", err)
	}

	// Print summary report
	PrintReport(report)

	// Save JSON if requested
	if cfg.Output != "" {
		if err := SaveJSON(report, cfg.Output); err != nil {
			log.Fatalf("save results: %v", err)
		}
		fmt.Printf("\n  Results saved to %s\n", cfg.Output)
	}
}

// filterScenarios returns scenarios matching the given category filter.
func filterScenarios(scenarios []Scenario, filter string) []Scenario {
	if filter == "all" || filter == "" {
		return scenarios
	}
	cat := ScenarioCategory(filter)
	var result []Scenario
	for _, s := range scenarios {
		if s.Category == cat {
			result = append(result, s)
		}
	}
	return result
}

// computeCategoryScores calculates the average score per category.
func computeCategoryScores(results []ScenarioResult) map[ScenarioCategory]float64 {
	sums := make(map[ScenarioCategory]float64)
	counts := make(map[ScenarioCategory]int)
	for _, r := range results {
		if r.Error == "" {
			sums[r.Category] += r.OverallScore
			counts[r.Category]++
		}
	}
	scores := make(map[ScenarioCategory]float64)
	for cat, sum := range sums {
		if counts[cat] > 0 {
			scores[cat] = sum / float64(counts[cat])
		}
	}
	return scores
}

// computeOverallScore returns the mean of all scenario scores.
func computeOverallScore(results []ScenarioResult) float64 {
	var sum float64
	var count int
	for _, r := range results {
		if r.Error == "" {
			sum += r.OverallScore
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// toMap converts an interface{} to map[string]any (for tool inputs).
func toMap(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(b, &m)
	return m, err
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
