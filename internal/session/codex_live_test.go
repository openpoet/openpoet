package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCodexLiveAppServerTurn drives the REAL `codex app-server` binary through
// CodexRunner: initialize, thread/start, one turn that writes a file and runs a
// command, then turn completion. It is the only check that the protocol shapes
// this package assumes still match the installed Codex CLI, so it is opt-in:
//
//	OPENPOET_CODEX_LIVE=1 go test ./internal/session -run TestCodexLiveAppServerTurn -v
//
// It needs an authenticated Codex CLI on PATH and makes a real model request.
func TestCodexLiveAppServerTurn(t *testing.T) {
	if os.Getenv("OPENPOET_CODEX_LIVE") == "" {
		t.Skip("set OPENPOET_CODEX_LIVE=1 to run the live codex app-server test")
	}

	workDir := t.TempDir()
	var mu sync.Mutex
	var out strings.Builder
	handler := func(b []byte) {
		mu.Lock()
		out.Write(b)
		mu.Unlock()
	}
	terminal := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}

	cfg := &SessionConfig{
		SessionID:                  "live-codex-test",
		DangerouslySkipPermissions: true, // no hub/db here, so no approval modal can answer
		BackendConfig:              `{"runtime":"app-server"}`,
	}
	r, err := newCodexRunner(workDir, nil, handler, cfg, nil, nil, true)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("start codex app-server: %v\nterminal: %s", err, terminal())
	}
	defer func() { _ = r.Stop() }()

	if r.providerThreadID == "" {
		t.Fatalf("thread/start did not yield a thread id; terminal: %s", terminal())
	}
	t.Logf("thread: %s", r.providerThreadID)

	prompt := "Crie o arquivo hello.txt com o conteudo OK, rode 'cat hello.txt' e responda apenas PRONTO.\r"
	if _, err := r.Write([]byte(prompt)); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	// turn/start is issued from a goroutine, so wait for the turn to exist
	// before waiting for turn/completed to clear it.
	waitFor := func(what string, timeout time.Duration, ready func() bool) {
		deadline := time.Now().Add(timeout)
		for !ready() {
			if time.Now().After(deadline) {
				r.mu.Lock()
				phase := r.agentPhase
				r.mu.Unlock()
				t.Fatalf("timed out waiting for %s; phase=%q terminal:\n%s", what, phase, terminal())
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	transcriptHas := func(kinds ...string) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, event := range r.transcript {
			for _, kind := range kinds {
				if event.Kind == kind {
					return true
				}
			}
		}
		return false
	}
	waitFor("the turn to start", 90*time.Second, func() bool {
		r.mu.Lock()
		active := r.activeTurnID
		r.mu.Unlock()
		return active != "" || transcriptHas("assistant", "error")
	})
	waitFor("the turn to complete", 4*time.Minute, func() bool {
		r.mu.Lock()
		active := r.activeTurnID
		r.mu.Unlock()
		return active == ""
	})

	r.mu.Lock()
	phase, detail := r.agentPhase, r.agentDetail
	transcript := append([]codexTranscriptEvent(nil), r.transcript...)
	r.mu.Unlock()

	if phase == "error" {
		t.Fatalf("turn ended in the error phase: %s\nterminal:\n%s", detail, terminal())
	}

	kinds := map[string]int{}
	for _, event := range transcript {
		kinds[event.Kind]++
		if event.Kind == "error" {
			t.Errorf("transcript carries an error block: %q / %q", event.Title, event.Text)
		}
	}
	t.Logf("transcript kinds: %v", kinds)
	for _, kind := range []string{"user", "assistant", "command"} {
		if kinds[kind] == 0 {
			t.Errorf("transcript has no %q block; kinds=%v terminal:\n%s", kind, kinds, terminal())
		}
	}
	if kinds["file"] == 0 {
		t.Errorf("transcript has no file block, so fileChange items are not being rendered; kinds=%v", kinds)
	}

	if data, err := os.ReadFile(filepath.Join(workDir, "hello.txt")); err != nil {
		t.Errorf("codex did not create hello.txt in the work dir: %v", err)
	} else if !strings.Contains(string(data), "OK") {
		t.Errorf("hello.txt = %q, want it to contain OK", data)
	}

	if got := terminal(); strings.Contains(got, "Unsupported Codex app-server request") {
		t.Errorf("app-server sent a request this runner cannot answer:\n%s", got)
	}
	if unhandled := r.UnhandledNotificationMethods(); len(unhandled) > 0 {
		t.Logf("unhandled notification methods (protocol drift to review): %v", unhandled)
	}
}

// TestCodexLiveAppServerFailedTurn asks the real app-server for a model the
// account cannot use, which is the cheapest way to prove a failed turn reaches
// the terminal: app-server reports it as an `error` notification plus a
// turn/completed with status "failed", and both used to be dropped silently.
func TestCodexLiveAppServerFailedTurn(t *testing.T) {
	if os.Getenv("OPENPOET_CODEX_LIVE") == "" {
		t.Skip("set OPENPOET_CODEX_LIVE=1 to run the live codex app-server test")
	}

	var mu sync.Mutex
	var out strings.Builder
	terminal := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}

	cfg := &SessionConfig{
		SessionID:                  "live-codex-failure-test",
		DangerouslySkipPermissions: true,
		BackendConfig:              `{"runtime":"app-server","model":"gpt-does-not-exist"}`,
	}
	r, err := newCodexRunner(t.TempDir(), nil, func(b []byte) {
		mu.Lock()
		out.Write(b)
		mu.Unlock()
	}, cfg, nil, nil, true)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("start codex app-server: %v\nterminal: %s", err, terminal())
	}
	defer func() { _ = r.Stop() }()

	if _, err := r.Write([]byte("diga ok\r")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		r.mu.Lock()
		phase := r.agentPhase
		r.mu.Unlock()
		if phase == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed turn never reached the error phase; phase=%q terminal:\n%s", phase, terminal())
		}
		time.Sleep(500 * time.Millisecond)
	}

	r.mu.Lock()
	transcript := append([]codexTranscriptEvent(nil), r.transcript...)
	r.mu.Unlock()

	errorBlocks := 0
	for _, event := range transcript {
		if event.Kind == "error" {
			errorBlocks++
			t.Logf("error block: %s / %s", event.Title, event.Text)
			if strings.Contains(event.Text, `"invalid_request_error"`) {
				t.Errorf("error block kept the raw provider envelope: %q", event.Text)
			}
		}
	}
	if errorBlocks != 1 {
		t.Fatalf("transcript has %d error blocks, want exactly 1: %#v", errorBlocks, transcript)
	}
	if !strings.Contains(terminal(), "not supported") {
		t.Errorf("terminal did not explain the failure:\n%s", terminal())
	}
}
