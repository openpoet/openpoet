package session

import (
	"regexp"
	"strings"
)

// NotificationTrigger represents a pattern that triggers a notification
type NotificationTrigger struct {
	Pattern     *regexp.Regexp
	Type        string // 'info', 'warning', 'error', 'question'
	Title       string
	ExtractBody func(match []string) string
}

// DefaultTriggers contains the default notification patterns
var DefaultTriggers = []NotificationTrigger{
	{
		// Custom notification tag
		Pattern:     regexp.MustCompile(`\[NOTIFY(?::(\w+))?\]\s*(.+)`),
		Type:        "info",
		Title:       "Notification",
		ExtractBody: func(match []string) string { return match[2] },
	},
	{
		// Question/waiting for input
		Pattern:     regexp.MustCompile(`(?i)(?:waiting for|please (?:enter|provide|confirm)|do you want to|would you like to|are you sure)\s*[?:]?`),
		Type:        "question",
		Title:       "Input Required",
		ExtractBody: func(match []string) string { return match[0] },
	},
	{
		// Error messages
		Pattern:     regexp.MustCompile(`(?i)(?:error|failed|failure|exception|fatal):\s*(.+)`),
		Type:        "error",
		Title:       "Error",
		ExtractBody: func(match []string) string { return match[1] },
	},
	{
		// Warning messages
		Pattern:     regexp.MustCompile(`(?i)(?:warning|warn):\s*(.+)`),
		Type:        "warning",
		Title:       "Warning",
		ExtractBody: func(match []string) string { return match[1] },
	},
	{
		// Task completion
		Pattern:     regexp.MustCompile(`(?i)(?:completed|finished|done|success(?:fully)?)[.!]?\s*$`),
		Type:        "info",
		Title:       "Task Completed",
		ExtractBody: func(match []string) string { return match[0] },
	},
}

// NotificationResult represents a detected notification
type NotificationResult struct {
	Type  string
	Title string
	Body  string
}

// ParseOutputForNotifications checks output for notification triggers
func ParseOutputForNotifications(output string) []NotificationResult {
	var results []NotificationResult
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		for _, trigger := range DefaultTriggers {
			match := trigger.Pattern.FindStringSubmatch(line)
			if match != nil {
				notifType := trigger.Type

				// Handle custom notification type in [NOTIFY:type] format
				if trigger.Title == "Notification" && len(match) > 1 && match[1] != "" {
					notifType = strings.ToLower(match[1])
					if notifType != "info" && notifType != "warning" && notifType != "error" && notifType != "question" {
						notifType = "info"
					}
				}

				results = append(results, NotificationResult{
					Type:  notifType,
					Title: trigger.Title,
					Body:  trigger.ExtractBody(match),
				})
				break // Only match first trigger per line
			}
		}
	}

	return results
}

// NotificationBuffer accumulates output and detects notifications
type NotificationBuffer struct {
	buffer    strings.Builder
	lineCount int
	maxLines  int
	handler   func(NotificationResult)
}

func NewNotificationBuffer(handler func(NotificationResult)) *NotificationBuffer {
	return &NotificationBuffer{
		maxLines: 100, // Keep last 100 lines for context
		handler:  handler,
	}
}

func (nb *NotificationBuffer) Write(data []byte) {
	text := string(data)
	nb.buffer.WriteString(text)

	// Check for notifications in new data
	notifications := ParseOutputForNotifications(text)
	for _, n := range notifications {
		if nb.handler != nil {
			nb.handler(n)
		}
	}

	// Trim buffer if too large
	content := nb.buffer.String()
	lines := strings.Split(content, "\n")
	if len(lines) > nb.maxLines {
		nb.buffer.Reset()
		nb.buffer.WriteString(strings.Join(lines[len(lines)-nb.maxLines:], "\n"))
	}
}

func (nb *NotificationBuffer) GetBuffer() string {
	return nb.buffer.String()
}
