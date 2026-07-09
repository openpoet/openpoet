package handlers

import (
	"strings"
	"testing"
)

func TestSliceSessionHistoryModes(t *testing.T) {
	content := strings.Join([]string{
		"first line",
		"second prompt",
		"third line",
		"fourth result",
		"fifth line",
	}, "\n")

	tail := sliceSessionHistory("sess-1", "terminal_buffer", content, sessionHistoryRequest{Mode: "tail", Lines: 2, MaxChars: 1000})
	if tail.Offset != 4 || tail.Content != "fourth result\nfifth line" {
		t.Fatalf("tail = offset %d content %q", tail.Offset, tail.Content)
	}

	head := sliceSessionHistory("sess-1", "terminal_buffer", content, sessionHistoryRequest{Mode: "head", Lines: 2, MaxChars: 1000})
	if head.Offset != 1 || head.Content != "first line\nsecond prompt" {
		t.Fatalf("head = offset %d content %q", head.Offset, head.Content)
	}

	window := sliceSessionHistory("sess-1", "terminal_buffer", content, sessionHistoryRequest{Mode: "window", Offset: 2, Limit: 2, MaxChars: 1000})
	if window.Offset != 2 || window.Content != "second prompt\nthird line" {
		t.Fatalf("window = offset %d content %q", window.Offset, window.Content)
	}
}

func TestSliceSessionHistorySearch(t *testing.T) {
	content := strings.Join([]string{
		"alpha",
		"prompt: build it",
		"beta",
		"PROMPT: test it",
		"gamma",
	}, "\n")

	result := sliceSessionHistory("sess-1", "terminal_buffer", content, sessionHistoryRequest{
		Query:        "prompt",
		Limit:        2,
		ContextLines: 1,
		MaxChars:     1000,
	})

	if result.Mode != "search" {
		t.Fatalf("mode = %q, want search", result.Mode)
	}
	if !strings.Contains(result.Content, "> 2 | prompt: build it") {
		t.Fatalf("missing first match: %q", result.Content)
	}
	if !strings.Contains(result.Content, "> 4 | PROMPT: test it") {
		t.Fatalf("missing second match: %q", result.Content)
	}
}

func TestCleanSessionHistoryOutput(t *testing.T) {
	got := cleanSessionHistoryOutput("\x1b[31mred\x1b[0m\r\nnext\x00line")
	if got != "red\nnextline" {
		t.Fatalf("clean output = %q", got)
	}
}
