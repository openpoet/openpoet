package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
)

func TestManagerReopenUsesSessionBackendAndConversation(t *testing.T) {
	for _, tc := range []struct {
		backend, projectBackend, config, model string
		wantArgs                               []string
	}{
		{"codex", "claude_code", `{"runtime":"tui"}`, "gpt-6-astra", []string{
			"resume", "--no-alt-screen", "--ask-for-approval", "on-request", "--sandbox", "workspace-write",
			"--model", "gpt-6-astra", "-c", `model_reasoning_effort="high"`, "provider-conversation",
		}},
		{"claude_code", "codex", `{}`, "opus", []string{"--resume", "provider-conversation", "--model", "opus", "--effort", "high"}},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			m, db := newExitTestManager(t)
			ctx := context.Background()
			project := &database.Project{Name: "example", Path: "/home/dev/project", Type: "remote", Backend: tc.projectBackend, BackendConfig: tc.config}
			if err := db.CreateProject(ctx, project); err != nil {
				t.Fatal(err)
			}
			sess := &database.Session{ID: "reopened-session", ProjectID: project.ID, Status: "error", Backend: tc.backend,
				ProviderSessionID: "provider-conversation", Model: tc.model, RequestedModel: tc.model, Effort: "high", StartTime: time.Now()}
			if err := db.CreateSession(ctx, sess); err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateSessionProviderSessionID(ctx, sess.ID, sess.ProviderSessionID); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{done: make(chan struct{})}
			originalFactory := remoteRunnerFactory
			t.Cleanup(func() { remoteRunnerFactory = originalFactory })
			remoteRunnerFactory = func(p *database.Project, env map[string]string, output func([]byte), decrypt func(string, string) (string, error), args []string) (Runner, error) {
				remote, err := NewRemoteRunner(p, env, output, decrypt, args)
				if err != nil {
					t.Fatal(err)
				}
				if remote.backend.Type() != BackendType(tc.backend) {
					t.Fatalf("selected executable %q for session backend %q", remote.backendCommand(), tc.backend)
				}
				if !reflect.DeepEqual(args, tc.wantArgs) {
					t.Fatalf("resume args = %#v, want %#v", args, tc.wantArgs)
				}
				return runner, nil
			}
			if err := m.ReopenSession(ctx, sess, project, nil, nil); err != nil {
				t.Fatal(err)
			}
			stored, err := db.GetSession(ctx, sess.ID)
			if err != nil || stored.Status != "running" || stored.ProviderSessionID != sess.ProviderSessionID || stored.Backend != tc.backend {
				t.Fatalf("reopened session = %+v, err = %v", stored, err)
			}
			if project.Backend != tc.projectBackend {
				t.Fatal("reopen mutated the project default")
			}
			if len(runner.writes) != 0 {
				t.Fatal("reopen sent a prompt")
			}
			close(runner.done)
			deadline := time.Now().Add(time.Second)
			for m.IsSessionRunning(sess.ID) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		})
	}
}

type reopenRPCWriter struct {
	runner  *CodexRunner
	fail    bool
	methods []string
	params  map[string]interface{}
}

func (w *reopenRPCWriter) Close() error { return nil }
func (w *reopenRPCWriter) Write(data []byte) (int, error) {
	var req struct {
		ID     json.RawMessage        `json:"id"`
		Method string                 `json:"method"`
		Params map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	w.methods = append(w.methods, req.Method)
	w.params = req.Params
	response := &codexWireMessage{ID: req.ID, Result: json.RawMessage(`{"thread":{"id":"original-conversation"}}`)}
	if w.fail {
		response.Error = &codexRPCError{Code: -32000, Message: "thread unavailable"}
	}
	w.runner.handleMessage(response)
	return len(data), nil
}

func TestCodexReopenNeverStartsANewConversation(t *testing.T) {
	for _, tc := range []struct {
		name, providerID string
		fail             bool
		wantError        string
	}{
		{"resume", "original-conversation", false, ""},
		{"resume rejected", "original-conversation", true, "thread unavailable"},
		{"missing provider id", "", false, "provider session id is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newExitTestCodexRunner(t)
			r.cfg = &SessionConfig{IsReopen: true, ProviderSessionID: tc.providerID,
				BackendConfig: `{"model":"gpt-6-astra","reasoning_effort":"high"}`}
			wire := &reopenRPCWriter{runner: r, fail: tc.fail}
			r.stdin = wire
			err := r.openThread(context.Background())
			if tc.wantError == "" {
				if err != nil || r.providerThreadID != tc.providerID {
					t.Fatalf("thread=%q err=%v", r.providerThreadID, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}
			if tc.providerID == "" {
				if len(wire.methods) != 0 {
					t.Fatalf("missing ID sent requests: %v", wire.methods)
				}
				return
			}
			if !reflect.DeepEqual(wire.methods, []string{"thread/resume"}) {
				t.Fatalf("reopen must only resume, without new threads or prompts: %v", wire.methods)
			}
			if wire.params["threadId"] != tc.providerID || wire.params["model"] != "gpt-6-astra" || wire.params["excludeTurns"] != true {
				t.Fatalf("resume params lost persisted settings: %v", wire.params)
			}
		})
	}
}

func TestManagerReopenCodexOverrideSelectsRemoteAppServer(t *testing.T) {
	m, db := newExitTestManager(t)
	ctx := context.Background()
	project := &database.Project{Name: "example", Path: "/home/dev/project", Type: "remote", Backend: "claude_code", BackendConfig: `{}`,
		SSHAuthType: sql.NullString{String: "password", Valid: true}}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess := &database.Session{ID: "codex-override", ProjectID: project.ID, Status: "error", Backend: "codex", Harness: "codex/app-server",
		ProviderSessionID: "provider-conversation", Model: "gpt-6-astra", RequestedModel: "gpt-6-astra", Effort: "high", StartTime: time.Now()}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	originalFactory := remoteRunnerFactory
	t.Cleanup(func() { remoteRunnerFactory = originalFactory })
	remoteRunnerFactory = func(*database.Project, map[string]string, func([]byte), func(string, string) (string, error), []string) (Runner, error) {
		t.Error("Codex app-server reopen selected the generic CLI runner")
		return nil, fmt.Errorf("wrong runner")
	}
	// Stop at credential validation, before any network or real CLI access.
	err := m.ReopenSession(ctx, sess, project, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "SSH password not configured") {
		t.Fatalf("expected app-server SSH credential validation, got %v", err)
	}
	if project.Backend != "claude_code" {
		t.Fatal("reopen mutated the project default")
	}
}
