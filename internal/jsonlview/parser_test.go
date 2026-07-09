package jsonlview

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const (
	userLine = `{"type":"user","uuid":"u1","timestamp":"2026-07-05T12:00:00Z","sessionId":"s1","message":{"role":"user","content":"hello"}}` + "\n"
)

func assistantLine(text string) string {
	return `{"type":"assistant","uuid":"a1","timestamp":"2026-07-05T12:00:01Z","sessionId":"s1","message":{"role":"assistant","id":"m1","content":[{"type":"text","text":"` + text + `"}],"stop_reason":"end_turn"}}` + "\n"
}

func TestParseFileFromOffsetKeepsPartialLineForNextRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	partial := assistantLine("complete")
	split := len(partial) - 8

	if err := os.WriteFile(path, []byte(userLine+partial[:split]), 0o600); err != nil {
		t.Fatal(err)
	}

	events, offset, err := ParseFileFromOffset(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("first read events = %d, want 1", len(events))
	}
	if offset != int64(len(userLine)) {
		t.Fatalf("first read offset = %d, want %d", offset, len(userLine))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(partial[split:]); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	events, offset, err = ParseFileFromOffset(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("second read events = %d, want 1", len(events))
	}
	if events[0].Type != "assistant" || events[0].Message.ContentBlocks[0].Text != "complete" {
		t.Fatalf("second read event = %#v", events[0])
	}
	if offset != int64(len(userLine)+len(partial)) {
		t.Fatalf("second read offset = %d, want %d", offset, len(userLine)+len(partial))
	}
}

func TestParseReaderFromOffsetKeepsPartialLineForNextRead(t *testing.T) {
	partial := assistantLine("remote")
	split := len(partial) - 8
	data := []byte(userLine + partial[:split])

	events, offset, err := ParseReaderFromOffset(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("first read events = %d, want 1", len(events))
	}
	if offset != int64(len(userLine)) {
		t.Fatalf("first read offset = %d, want %d", offset, len(userLine))
	}

	data = []byte(userLine + partial)
	events, offset, err = ParseReaderFromOffset(bytes.NewReader(data), int64(len(data)), offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("second read events = %d, want 1", len(events))
	}
	if events[0].Message.ContentBlocks[0].Text != "remote" {
		t.Fatalf("second read text = %q, want remote", events[0].Message.ContentBlocks[0].Text)
	}
}

func TestParseReaderMergesAssistantBlocksByIdentity(t *testing.T) {
	data := stringsJoinLines(
		`{"type":"assistant","uuid":"a1","timestamp":"2026-07-05T12:00:00Z","sessionId":"s1","message":{"role":"assistant","id":"m1","content":[{"type":"text","text":"hel"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"a.go"}}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-07-05T12:00:01Z","sessionId":"s1","message":{"role":"assistant","id":"m1","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"a.go"}},{"type":"tool_use","id":"tool-2","name":"Write","input":{"file_path":"b.go"}}],"stop_reason":"tool_use"}}`,
	)

	events, err := ParseReader(bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}

	blocks := events[0].Message.ContentBlocks
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3: %#v", len(blocks), blocks)
	}
	if blocks[0].Text != "hello" {
		t.Fatalf("text block = %q, want hello", blocks[0].Text)
	}
	if blocks[1].ToolID != "tool-1" || blocks[2].ToolID != "tool-2" {
		t.Fatalf("tool ids = %q, %q; want tool-1, tool-2", blocks[1].ToolID, blocks[2].ToolID)
	}
}

func TestWatcherDoesNotAdvanceOffsetWhenEventsChannelIsFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(userLine), 0o600); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(path, 0)
	for i := 0; i < cap(w.events); i++ {
		w.events <- []*SessionEvent{{Type: "progress"}}
	}

	w.poll()
	if w.offset != 0 {
		t.Fatalf("offset with full channel = %d, want 0", w.offset)
	}

	<-w.events
	w.poll()
	if w.offset != int64(len(userLine)) {
		t.Fatalf("offset after successful enqueue = %d, want %d", w.offset, len(userLine))
	}
}

func stringsJoinLines(lines ...string) string {
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return buf.String()
}
