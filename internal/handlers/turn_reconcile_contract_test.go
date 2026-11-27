package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/configsync"
	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
	"openpoet/internal/llm"
	"openpoet/internal/notifications"
	"openpoet/internal/security"
	"openpoet/internal/session"
	"openpoet/internal/updater"
	"openpoet/internal/voice"
	"openpoet/internal/websocket"
)

// fakeClaudeScript stands in for the claude CLI: it records its environment
// (hook URL, session id, hook token) and every line typed into its terminal.
const fakeClaudeScript = `#!/bin/bash
env > "__FAKE_DIR__/env-$OPENPOET_SESSION_ID"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "__FAKE_DIR__/typed-$OPENPOET_SESSION_ID"
done
`

type contractServer struct {
	t        *testing.T
	db       *database.DB
	url      string
	token    string
	fakeDir  string
	home     string
	project  *database.Project
	sequence int
}

// newContractServer composes the server like main.go (platform services,
// hook routes, the Automation API) on a real loopback listener, so the
// session manager hands that URL to the agent and the bridge reaches it.
func newContractServer(t *testing.T) *contractServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs bash and a PTY")
	}
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	base := t.TempDir()
	c := &contractServer{t: t, fakeDir: filepath.Join(base, "fake"), home: filepath.Join(base, "home")}
	for _, dir := range []string{c.fakeDir, c.home, filepath.Join(base, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "bin", "claude"), []byte(strings.ReplaceAll(fakeClaudeScript, "__FAKE_DIR__", c.fakeDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", c.home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", filepath.Join(base, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()

	db, err := database.New(filepath.Join(base, "contract.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c.db = db
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Stop)
	encryptor, err := security.NewEncryptor("turn-contract-test-key")
	if err != nil {
		t.Fatal(err)
	}
	manager := session.NewManager(db, hub, address)
	t.Cleanup(manager.StopAll)
	syncer := configsync.NewConfigSyncer(db, encryptor.Decrypt, address)
	push, err := notifications.NewWebPushService(db, "test@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	notificationService := notifications.NewService(db, hub, push)
	hook := NewHookHandler(hub, notificationService, manager)
	api := NewAPI(db, hub, manager, syncer, encryptor, notificationService, hook)
	structured := NewStructuredViewHandler(db, hub, api.DecryptFunc())
	aiHandler := NewAIHandler(api, llm.NewProviderManager("http://localhost:0"))
	api.SetAIHandler(aiHandler)
	appUpdater := updater.New("test")
	api.SetUpdater(appUpdater)
	api.SetStructuredView(structured)
	if err := api.ConfigurePlatformServices(PlatformServices{
		DB: db, Hub: hub, SessionManager: manager, ConfigSync: syncer, Encryptor: encryptor,
		HookHandler: hook, FileHandler: NewFileHandler(api), GitHandler: NewGitHandler(api),
		VoiceHandler:   NewVoiceHandler(api, func() (voice.ProviderType, string, string) { return voice.ProviderOpenAI, "", "" }),
		StructuredView: structured, Updater: appUpdater, AIHandler: aiHandler,
		Notifications: notificationService, WebPush: push, ReinitializeAI: func() {},
	}); err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {
		r.Post("/hooks/permission", hook.HandlePermission)
		r.Post("/hooks/event", hook.HandleEvent)
		r.Post("/hooks/pretooluse", hook.HandlePreToolUseGate)
	})
	router.Mount("/api/automation/v1", automation.NewHandler(db, automation.Dependencies{
		Capabilities: api.CapabilityRegistry(), PlatformCapabilities: api.PlatformCapabilityRegistry(),
		Snapshot: db, Waiter: db, Sessions: db, ProjectScope: db, SessionIDs: db,
	}))
	server := httptest.NewUnstartedServer(router)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	c.url = server.URL

	client, err := automation.ProvisionClient(context.Background(), db, "operator-fixture",
		[]automation.Scope{automation.ScopeSessionsRead, automation.ScopeSessionsWrite, automation.ScopeEventsRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.token = client.Token

	projectDir := filepath.Join(base, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c.project = &database.Project{Name: "turn-contract", Path: projectDir, Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(context.Background(), c.project); err != nil {
		t.Fatal(err)
	}
	return c
}

type contractReply struct {
	Status int
	Body   map[string]any
}

func (r contractReply) errorCode() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func (r contractReply) result() map[string]any {
	result, _ := r.Body["result"].(map[string]any)
	return result
}

// command posts an envelope to the Automation API as the operator would.
func (c *contractServer) command(capability string, target, payload map[string]any, reason, correlation string) contractReply {
	c.t.Helper()
	c.sequence++
	envelope := map[string]any{
		"command_id": fmt.Sprintf("cmd-%d", c.sequence), "idempotency_key": fmt.Sprintf("idem-%d", c.sequence),
		"capability": capability, "target": target, "payload": payload,
	}
	if reason != "" {
		envelope["reason"] = reason
	}
	if correlation != "" {
		envelope["correlation_id"] = correlation
	}
	body, _ := json.Marshal(envelope)
	req, _ := http.NewRequest(http.MethodPost, c.url+"/api/automation/v1/commands", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return contractReply{Status: resp.StatusCode, Body: out}
}

func (c *contractServer) waitFile(path string) []byte {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s never appeared", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// hook runs the bridge OpenPoet wrote into the project, with the agent's own
// environment, exactly as Claude Code would.
func (c *contractServer) hook(sessionID string, env []string, event map[string]any) {
	c.t.Helper()
	body, _ := json.Marshal(event)
	cmd := exec.Command("bash", filepath.Join(c.project.Path, ".claude", "hooks", "bridge.sh"))
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		c.t.Fatalf("bridge: %v %s", err, out)
	}
}

func (c *contractServer) writeTranscript(sessionID string, lines ...map[string]any) {
	c.t.Helper()
	path := jsonlview.ResolveJSONLPath(c.project.Path, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatal(err)
	}
	var buf bytes.Buffer
	for _, line := range lines {
		data, _ := json.Marshal(line)
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func transcriptLine(kind, uuid string, at time.Time, message map[string]any) map[string]any {
	return map[string]any{"type": kind, "uuid": uuid, "timestamp": at.UTC().Format(time.RFC3339Nano), "message": message}
}

// The operator's contract end to end: a real PTY session, hook events sent
// by the real bridge, the stuck turn refused by send_input, diagnosed by
// sessions.turn_preflight, refused while unsafe, reconciled with audit, and
// guarded input accepted again.
func TestOperatorTurnReconcileContract(t *testing.T) {
	c := newContractServer(t)
	created := c.command("sessions.create", map[string]any{"project_id": c.project.ID}, map[string]any{"model": "haiku", "effort": "low"}, "contract test session", "ain:example-7")
	if created.Status != http.StatusOK {
		t.Fatalf("sessions.create = %d %v", created.Status, created.Body)
	}
	sessionID, _ := created.result()["id"].(string)
	if sessionID == "" {
		t.Fatalf("sessions.create result = %v", created.Body)
	}
	target := map[string]any{"id": sessionID}
	envData := c.waitFile(filepath.Join(c.fakeDir, "env-"+sessionID))
	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	if !strings.Contains(string(envData), "OPENPOET_HOOK_TOKEN=") {
		t.Fatal("agent environment carries no hook token")
	}
	bridge, err := os.ReadFile(filepath.Join(c.project.Path, ".claude", "hooks", "bridge.sh"))
	if err != nil || !strings.Contains(string(bridge), "X-Hook-Event-ID") {
		t.Fatalf("project bridge is not the new one: %v", err)
	}

	prompt := "Run the checksum script: python3 work.py and tell me the checksum."
	c.hook(sessionID, env, map[string]any{"hook_event_name": "SessionStart", "session_id": sessionID, "source": "startup"})
	c.hook(sessionID, env, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sessionID, "prompt": prompt})
	c.hook(sessionID, env, map[string]any{"hook_event_name": "PreToolUse", "session_id": sessionID, "tool_name": "Bash", "tool_use_id": "toolu_B", "tool_input": map[string]any{"command": "python3 work.py"}})
	// Esc: Claude Code sends nothing more. The PostToolUse below never happens.

	deadline := time.Now().Add(10 * time.Second)
	for {
		view := c.command("sessions.get", target, map[string]any{}, "", "")
		turn, _ := view.result()["turn"].(map[string]any)
		if turn != nil && turn["open"] == true && turn["last_event"] == "PreToolUse" {
			if tools, _ := turn["pending_tools"].([]any); len(tools) != 1 {
				t.Fatalf("pending_tools = %v", turn["pending_tools"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn never opened: %v", view.Body)
		}
		time.Sleep(50 * time.Millisecond)
	}

	sent := c.command("sessions.send_input", target, map[string]any{"text": "next step", "await_ack": false}, "", "ain:example-7")
	if sent.errorCode() != "session_busy" {
		t.Fatalf("guarded send on the stuck turn = %d %v", sent.Status, sent.Body)
	}

	// The transcript still shows the tool running: refused.
	start := time.Now().Add(-time.Hour)
	promptLine := transcriptLine("user", "u1", start, map[string]any{"role": "user", "content": prompt})
	toolLine := transcriptLine("assistant", "a1", start.Add(time.Second), map[string]any{"role": "assistant", "id": "msg_1", "model": "claude-haiku-5-5", "stop_reason": "tool_use",
		"content": []any{map[string]any{"type": "tool_use", "id": "toolu_B", "name": "Bash", "input": map[string]any{"command": "python3 work.py"}}}})
	c.writeTranscript(sessionID, promptLine, toolLine)
	preflight := c.command("sessions.turn_preflight", target, map[string]any{}, "", "")
	if preflight.Status != http.StatusOK {
		t.Fatalf("preflight = %d %v", preflight.Status, preflight.Body)
	}
	result := preflight.result()
	blockers := fmt.Sprint(result["blockers"])
	if result["verdict"] != application.TurnVerdictBlocked || !strings.Contains(blockers, "tool_pending") || !strings.Contains(blockers, "hooks_recent") {
		t.Fatalf("preflight while the tool runs = %v", result)
	}
	refused := c.command("sessions.reconcile_turn", target, map[string]any{"turn_token": result["turn_token"]}, "turn left open after an interrupt", "ain:example-7")
	if refused.errorCode() != "turn_reconcile_refused" {
		t.Fatalf("reconcile while unsafe = %d %v", refused.Status, refused.Body)
	}
	missingRef := c.command("sessions.reconcile_turn", target, map[string]any{"turn_token": result["turn_token"]}, "no authorization ref", "")
	if missingRef.errorCode() != "correlation_id_required" {
		t.Fatalf("reconcile without correlation_id = %d %v", missingRef.Status, missingRef.Body)
	}

	// The interruption lands in the transcript, and the hooks go quiet.
	c.writeTranscript(sessionID, promptLine, toolLine,
		transcriptLine("user", "u2", start.Add(2*time.Second), map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_B", "is_error": true, "content": "The user doesn't want to proceed with this tool use. The tool use was rejected"}}}),
		transcriptLine("user", "u3", start.Add(2*time.Second), map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user for tool use]"}}}),
	)
	quietUntil := time.Now().Add(application.TurnReconcileQuietPeriod + time.Second)
	for time.Now().Before(quietUntil) {
		time.Sleep(250 * time.Millisecond)
	}
	preflight = c.command("sessions.turn_preflight", target, map[string]any{}, "", "")
	result = preflight.result()
	if result["verdict"] != application.TurnVerdictReconcilable {
		t.Fatalf("preflight after the interrupt = %v", result)
	}
	evidence, _ := result["evidence"].(map[string]any)
	if evidence["kind"] != application.TurnEvidenceInterrupted {
		t.Fatalf("evidence = %v", evidence)
	}
	token := result["turn_token"]

	reconciled := c.command("sessions.reconcile_turn", target, map[string]any{"turn_token": token}, "turn left open after an interrupt", "ain:example-7")
	if reconciled.Status != http.StatusOK || reconciled.result()["outcome"] != "reconciled" || reconciled.result()["changed"] != true {
		t.Fatalf("reconcile = %d %v", reconciled.Status, reconciled.Body)
	}
	again := c.command("sessions.reconcile_turn", target, map[string]any{"turn_token": token}, "turn left open after an interrupt", "ain:example-7")
	if again.Status != http.StatusOK || again.result()["outcome"] != "already_closed" {
		t.Fatalf("repeated reconcile = %d %v", again.Status, again.Body)
	}
	if typed, _ := os.ReadFile(filepath.Join(c.fakeDir, "typed-"+sessionID)); len(typed) != 0 {
		t.Fatalf("reconciliation typed into the session: %q", typed)
	}

	sent = c.command("sessions.send_input", target, map[string]any{"text": "next step", "await_ack": false}, "", "ain:example-7")
	if sent.Status != http.StatusOK || sent.result()["sent"] != true {
		t.Fatalf("guarded send after reconcile = %d %v", sent.Status, sent.Body)
	}
	if typed := c.waitFile(filepath.Join(c.fakeDir, "typed-"+sessionID)); !strings.Contains(string(typed), "next step") {
		t.Fatalf("typed = %q", typed)
	}

	// The audit trail: outbox events for the refused, reconciled and no-op
	// attempts, with the operator's authorization reference.
	var events []struct {
		Payload       string `db:"payload_json"`
		CorrelationID string `db:"correlation_id"`
	}
	if err := c.db.SelectContext(context.Background(), &events,
		`SELECT payload_json, correlation_id FROM event_outbox WHERE event_type = 'platform.session.turn_reconcile_recorded' AND aggregate_id = ? ORDER BY sequence`, sessionID); err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, event := range events {
		var payload map[string]any
		_ = json.Unmarshal([]byte(event.Payload), &payload)
		outcomes = append(outcomes, fmt.Sprint(payload["outcome"]))
		if event.CorrelationID != "ain:example-7" || payload["authorization_ref"] != "ain:example-7" || payload["reason"] == "" {
			t.Fatalf("audit event = %+v", event)
		}
	}
	if strings.Join(outcomes, ",") != "refused,reconciled,already_closed" {
		t.Fatalf("audited outcomes = %v", outcomes)
	}
}
