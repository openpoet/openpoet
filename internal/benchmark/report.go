package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// PrintReport renders the benchmark results as an ASCII table to stdout.
func PrintReport(report *BenchmarkReport) {
	w := 66

	fmt.Println()
	printLine(w, "top")
	printCenter(w, fmt.Sprintf("OpenPoet AI Benchmark — %s", report.Model))
	printCenter(w, fmt.Sprintf("Judge: %s | %s", report.JudgeModel, report.Timestamp.Format("2006-01-02 15:04")))
	printLine(w, "mid")

	// Header
	fmt.Printf("  %-24s %6s %7s %12s %7s\n", "Scenario", "Score", "Tools", "Validators", "Cost")
	printLine(w, "mid")

	// Scenario rows
	currentCat := ScenarioCategory("")
	for _, r := range report.Scenarios {
		if r.Category != currentCat {
			currentCat = r.Category
			fmt.Printf("  [%s]\n", strings.ToUpper(string(currentCat)))
		}

		toolMark := " -"
		if len(r.ToolCalls) > 0 {
			toolMark = fmt.Sprintf(" %d", len(r.ToolCalls))
		}

		validatorsPassed := 0
		for _, v := range r.Validators {
			if v.Passed {
				validatorsPassed++
			}
		}
		validatorStr := fmt.Sprintf("%d/%d", validatorsPassed, len(r.Validators))

		if r.Error != "" {
			fmt.Printf("  %-24s %6s %7s %12s %7s\n",
				truncateStr(r.ScenarioName, 24), "ERR", "-", "-", "-")
		} else {
			fmt.Printf("  %-24s %6.1f %7s %12s $%.4f\n",
				truncateStr(r.ScenarioName, 24), r.OverallScore, toolMark, validatorStr, r.Cost)
		}
	}

	printLine(w, "mid")

	// Category averages
	fmt.Printf("  Category Averages:\n")
	cats := []ScenarioCategory{CategoryTasks, CategorySkills, CategoryCode, CategoryTools, CategoryMemory, CategoryConversation}
	var parts []string
	for _, cat := range cats {
		if score, ok := report.CategoryScores[cat]; ok {
			parts = append(parts, fmt.Sprintf("%s: %.1f", cat, score))
		}
	}
	// Print in rows of 3
	for i := 0; i < len(parts); i += 3 {
		end := i + 3
		if end > len(parts) {
			end = len(parts)
		}
		fmt.Printf("  %s\n", strings.Join(parts[i:end], "  |  "))
	}

	printLine(w, "mid")

	// Overall
	fmt.Printf("  OVERALL: %.2f/5.00    Total: $%.4f    Duration: %s\n",
		report.OverallScore, report.TotalCost, formatDuration(report.TotalDuration))
	printLine(w, "bot")
	fmt.Println()
}

// SaveJSON writes the report to a JSON file.
func SaveJSON(report *BenchmarkReport, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// ──── Helpers ────

func printLine(w int, pos string) {
	var left, right, fill string
	switch pos {
	case "top":
		left, right, fill = "  +", "+", "-"
	case "mid":
		left, right, fill = "  +", "+", "-"
	case "bot":
		left, right, fill = "  +", "+", "-"
	}
	fmt.Printf("%s%s%s\n", left, strings.Repeat(fill, w-4), right)
}

func printCenter(w int, text string) {
	padding := (w - 4 - len(text)) / 2
	if padding < 0 {
		padding = 0
	}
	fmt.Printf("  |%s%s%s|\n",
		strings.Repeat(" ", padding),
		text,
		strings.Repeat(" ", w-4-padding-len(text)))
}

func truncateStr(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen-2] + ".."
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}
