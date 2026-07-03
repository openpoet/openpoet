package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func requireCodexTranscriptStatus(t *testing.T, r *CodexRunner, title, text string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range r.transcript {
		if event.Kind == "status" && event.Title == title && strings.Contains(event.Text, text) {
			return
		}
	}
	t.Fatalf("missing transcript status title=%q text containing %q in %#v", title, text, r.transcript)
}

func TestCodexApprovalDecisionFromHookResponse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "allow",
			raw:  `{"hookSpecificOutput":{"decision":{"behavior":"allow"}}}`,
			want: "accept",
		},
		{
			name: "allow always preserves original behavior",
			raw:  `{"hookSpecificOutput":{"decision":{"behavior":"allow","originalBehavior":"allowAlways"}}}`,
			want: "acceptForSession",
		},
		{
			name: "deny",
			raw:  `{"hookSpecificOutput":{"decision":{"behavior":"deny"}}}`,
			want: "decline",
		},
		{
			name: "unknown",
			raw:  `{"hookSpecificOutput":{"decision":{"behavior":"ask"}}}`,
			want: "cancel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexApprovalDecisionFromHookResponse(json.RawMessage(tt.raw))
			if got != tt.want {
				t.Fatalf("decision = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexApprovalCacheKey(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		payload  map[string]interface{}
		wantKey  string
		wantOK   bool
	}{
		{
			name:     "bash command",
			toolName: "Bash",
			payload:  map[string]interface{}{"command": "go test ./..."},
			wantKey:  "Bash:go test ./...",
			wantOK:   true,
		},
		{
			name:     "bash nested command action",
			toolName: "Bash",
			payload:  map[string]interface{}{"action": map[string]interface{}{"command": "rg Codex"}},
			wantKey:  "Bash:rg Codex",
			wantOK:   true,
		},
		{
			name:     "file change grant root",
			toolName: "FileChange",
			payload:  map[string]interface{}{"grantRoot": "/tmp/project"},
			wantKey:  "FileChange:/tmp/project",
			wantOK:   true,
		},
		{
			name:     "file change without grant root is not cacheable",
			toolName: "FileChange",
			payload:  map[string]interface{}{"itemId": "item-1"},
			wantOK:   false,
		},
		{
			name:     "unknown tool is not cacheable",
			toolName: "AskUserQuestion",
			payload:  map[string]interface{}{"question": "Continue?"},
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotOK := codexApprovalCacheKey(tt.toolName, tt.payload)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotKey != tt.wantKey {
				t.Fatalf("key = %q, want %q", gotKey, tt.wantKey)
			}
		})
	}
}

func TestCodexTerminalTextBytesNormalizesLineFeeds(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text",
			in:   "hello",
			want: "hello",
		},
		{
			name: "lone line feeds",
			in:   "one\ntwo\nthree",
			want: "one\r\ntwo\r\nthree",
		},
		{
			name: "existing carriage returns",
			in:   "one\r\ntwo\r\n",
			want: "one\r\ntwo\r\n",
		},
		{
			name: "mixed endings",
			in:   "one\r\ntwo\nthree",
			want: "one\r\ntwo\r\nthree",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(codexTerminalTextBytes(tt.in))
			if got != tt.want {
				t.Fatalf("normalized = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexOutputPreservesVisibleInputLine(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	r.writePrompt()
	if _, err := r.Write([]byte("faca deploy")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	r.writeContentDelta(json.RawMessage(`{"delta":"pensando alto"}`), "reasoning")

	text := out.String()
	wantSuffix := codexClearInputLine + "\r\n\x1b[90mReasoning:\x1b[0m pensando alto\r\n" + codexPromptANSI + "faca deploy"
	if !strings.HasSuffix(text, wantSuffix) {
		t.Fatalf("output did not redraw input line correctly:\n got: %q\nwant suffix: %q", text, wantSuffix)
	}
}

func TestCodexTypingWhileTurnActiveStartsOwnInputLine(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
		activeTurnID:  "turn-1",
	}

	if _, err := r.Write([]byte("a")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	got := out.String()
	want := "\r\n" + codexPromptANSI + "a"
	if got != want {
		t.Fatalf("active-turn input line = %q, want %q", got, want)
	}
}

func TestCodexTurnStartedDoesNotEmitWorkingMessage(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	r.handleNotification("turn/started", json.RawMessage(`{"turn":{"id":"turn-1"}}`))

	if r.activeTurnID != "turn-1" {
		t.Fatalf("activeTurnID = %q, want turn-1", r.activeTurnID)
	}
	if r.agentPhase != "thinking" || r.agentDetail != "Starting turn" {
		t.Fatalf("phase/detail = %q/%q, want thinking/Starting turn", r.agentPhase, r.agentDetail)
	}
	if strings.Contains(out.String(), "Codex is working") {
		t.Fatalf("terminal output included working message: %q", out.String())
	}
	for _, event := range r.transcript {
		if event.Title == "Turn started" || strings.Contains(event.Text, "Codex is working") {
			t.Fatalf("transcript included working message: %#v", r.transcript)
		}
	}
}

func TestCodexInputPreservesUTF8Characters(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	input := "faça ação çãé"
	if _, err := r.Write([]byte(input)); err != nil {
		t.Fatalf("write input: %v", err)
	}

	if got := string(r.inputBuffer); got != input {
		t.Fatalf("input buffer = %q, want %q", got, input)
	}
	if got := out.String(); !strings.HasSuffix(got, codexPromptANSI+input) {
		t.Fatalf("output suffix = %q, want suffix %q", got, codexPromptANSI+input)
	}
}

func TestCodexMCPElicitationResponseFromHookResponse(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantAction string
	}{
		{
			name:       "allow maps to accept",
			raw:        `{"hookSpecificOutput":{"decision":{"behavior":"allow"}}}`,
			wantAction: "accept",
		},
		{
			name:       "allow always maps to accept",
			raw:        `{"hookSpecificOutput":{"decision":{"behavior":"allow","originalBehavior":"allowAlways"}}}`,
			wantAction: "accept",
		},
		{
			name:       "deny maps to decline",
			raw:        `{"hookSpecificOutput":{"decision":{"behavior":"deny"}}}`,
			wantAction: "decline",
		},
		{
			name:       "unknown maps to cancel",
			raw:        `{"hookSpecificOutput":{"decision":{"behavior":"passthrough"}}}`,
			wantAction: "cancel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexMCPElicitationResponseFromHookResponse(json.RawMessage(tt.raw))
			if got["action"] != tt.wantAction {
				t.Fatalf("action = %q, want %q", got["action"], tt.wantAction)
			}
		})
	}
}

func TestCodexUserInputAnswersForQuestions(t *testing.T) {
	questions := []interface{}{
		map[string]interface{}{"id": "mode", "question": "Mode?"},
		map[string]interface{}{"id": "confirm", "question": "Continue?"},
		map[string]interface{}{"id": "ignored", "question": "Ignored?"},
	}
	answers := map[string]interface{}{
		"mode":      "default",
		"Continue?": []interface{}{"yes", "always"},
	}

	got := codexUserInputAnswersForQuestions(questions, answers)

	if got["mode"].(map[string]interface{})["answers"].([]string)[0] != "default" {
		t.Fatalf("mode answer = %#v", got["mode"])
	}
	confirmAnswers := got["confirm"].(map[string]interface{})["answers"].([]string)
	if len(confirmAnswers) != 2 || confirmAnswers[0] != "yes" || confirmAnswers[1] != "always" {
		t.Fatalf("confirm answers = %#v", confirmAnswers)
	}
	if _, ok := got["ignored"]; ok {
		t.Fatalf("unexpected ignored answer: %#v", got["ignored"])
	}
}

func TestCodexSlashCommandDetection(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{text: "/", want: true},
		{text: "/status", want: true},
		{text: " /clear ", want: true},
		{text: "/review", want: true},
		{text: "/tmp/openpoet", want: false},
		{text: "please run /status", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := isCodexSlashCommand(tt.text); got != tt.want {
				t.Fatalf("isCodexSlashCommand(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestCodexSlashHelpDoesNotStartTurn(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	r.handleSlashCommand("/")

	text := out.String()
	if !strings.Contains(text, "Codex slash commands in OpenPoet") {
		t.Fatalf("help output did not include heading: %q", text)
	}
	if !strings.Contains(text, "OpenPoet Codex>") {
		t.Fatalf("help output did not restore prompt: %q", text)
	}
	if r.activeTurnID != "" {
		t.Fatalf("help should not start a turn, activeTurnID=%q", r.activeTurnID)
	}
}

func TestCodexSlashModelOverrideAffectsTurnParams(t *testing.T) {
	r := &CodexRunner{
		cfg: &SessionConfig{
			BackendConfig: `{"model":"gpt-default"}`,
		},
	}

	r.handleSlashCommand("/model gpt-test")
	params := r.turnStartParams("thread-1", "hello")
	if params["model"] != "gpt-test" {
		t.Fatalf("model override = %#v, want gpt-test", params["model"])
	}

	r.handleSlashCommand("/model default")
	params = r.turnStartParams("thread-1", "hello")
	if params["model"] != "gpt-default" {
		t.Fatalf("model after reset = %#v, want gpt-default", params["model"])
	}
}

func TestCodexSlashFastOverrideAffectsTurnParams(t *testing.T) {
	r := &CodexRunner{
		cfg: &SessionConfig{
			BackendConfig: `{"service_tier":"standard"}`,
		},
	}

	r.handleSlashCommand("/fast on")
	params := r.turnStartParams("thread-1", "hello")
	if params["serviceTier"] != "fast" {
		t.Fatalf("serviceTier override = %#v, want fast", params["serviceTier"])
	}

	r.handleSlashCommand("/fast off")
	params = r.turnStartParams("thread-1", "hello")
	if _, ok := params["serviceTier"]; ok {
		t.Fatalf("serviceTier should be omitted when fast is off: %#v", params["serviceTier"])
	}

	r.handleSlashCommand("/fast default")
	params = r.turnStartParams("thread-1", "hello")
	if params["serviceTier"] != "standard" {
		t.Fatalf("serviceTier after reset = %#v, want standard", params["serviceTier"])
	}
}

func TestCodexSlashPermissionsOverrideAffectsTurnParams(t *testing.T) {
	r := &CodexRunner{
		cfg: &SessionConfig{
			BackendConfig: `{"approval_policy":"on-request","sandbox_mode":"workspace-write"}`,
		},
	}

	r.handleSlashCommand("/permissions danger-full-access")
	params := r.turnStartParams("thread-1", "hello")
	if params["approvalPolicy"] != "never" {
		t.Fatalf("approvalPolicy = %#v, want never", params["approvalPolicy"])
	}
	sandbox, ok := params["sandboxPolicy"].(map[string]interface{})
	if !ok || sandbox["type"] != "dangerFullAccess" {
		t.Fatalf("sandboxPolicy = %#v, want dangerFullAccess", params["sandboxPolicy"])
	}

	r.handleSlashCommand("/permissions read-only")
	params = r.turnStartParams("thread-1", "hello")
	if params["approvalPolicy"] != "untrusted" {
		t.Fatalf("approvalPolicy = %#v, want untrusted", params["approvalPolicy"])
	}
	sandbox, ok = params["sandboxPolicy"].(map[string]interface{})
	if !ok || sandbox["type"] != "readOnly" {
		t.Fatalf("sandboxPolicy = %#v, want readOnly", params["sandboxPolicy"])
	}
}

func TestCodexSlashResumeWithoutThreadIDShowsUsage(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	r.handleSlashCommand("/resume")

	text := out.String()
	if !strings.Contains(text, "Usage: /resume <codex-thread-id>") {
		t.Fatalf("resume output did not include usage: %q", text)
	}
	if strings.Contains(text, "not implemented") {
		t.Fatalf("resume should be implemented, got: %q", text)
	}
}

func TestCodexCommandModelSetUpdatesFutureTurnOverrides(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	result, err := r.codexCommandModelSet(json.RawMessage(`{
		"model":"gpt-test",
		"reasoningEffort":"high",
		"serviceTier":"fast"
	}`))
	if err != nil {
		t.Fatalf("codexCommandModelSet returned error: %v", err)
	}

	current, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result = %#v, want map", result)
	}
	if current["model"] != "gpt-test" {
		t.Fatalf("model = %#v, want gpt-test", current["model"])
	}
	if current["reasoningEffort"] != "high" {
		t.Fatalf("reasoningEffort = %#v, want high", current["reasoningEffort"])
	}
	if current["serviceTier"] != "fast" {
		t.Fatalf("serviceTier = %#v, want fast", current["serviceTier"])
	}
	if !strings.Contains(out.String(), "Model override set to gpt-test") {
		t.Fatalf("terminal output did not confirm model update: %q", out.String())
	}
	requireCodexTranscriptStatus(t, r, "Model", "Model override set to gpt-test")

	params := r.turnStartParams("thread-1", "hello")
	if params["model"] != "gpt-test" {
		t.Fatalf("turn model = %#v, want gpt-test", params["model"])
	}
	if params["effort"] != "high" {
		t.Fatalf("turn effort = %#v, want high", params["effort"])
	}
	if params["serviceTier"] != "fast" {
		t.Fatalf("turn serviceTier = %#v, want fast", params["serviceTier"])
	}
}

func TestCodexCommandPermissionsSetUpdatesFutureTurnOverrides(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler: func(b []byte) { out.Write(b) },
	}

	result, err := r.codexCommandPermissionsSet(json.RawMessage(`{"preset":"read-only"}`))
	if err != nil {
		t.Fatalf("codexCommandPermissionsSet returned error: %v", err)
	}

	current, ok := result.(map[string]string)
	if !ok {
		t.Fatalf("result = %#v, want map", result)
	}
	if current["approvalPolicy"] != "untrusted" {
		t.Fatalf("approvalPolicy = %#v, want untrusted", current["approvalPolicy"])
	}
	if current["sandboxMode"] != "read-only" {
		t.Fatalf("sandboxMode = %#v, want read-only", current["sandboxMode"])
	}
	if !strings.Contains(out.String(), "Permissions updated for future turns") {
		t.Fatalf("terminal output did not confirm permission update: %q", out.String())
	}
	requireCodexTranscriptStatus(t, r, "Permissions", "Permissions updated for future turns")

	params := r.turnStartParams("thread-1", "hello")
	if params["approvalPolicy"] != "untrusted" {
		t.Fatalf("turn approvalPolicy = %#v, want untrusted", params["approvalPolicy"])
	}
	sandbox, ok := params["sandboxPolicy"].(map[string]interface{})
	if !ok || sandbox["type"] != "readOnly" {
		t.Fatalf("turn sandboxPolicy = %#v, want readOnly", params["sandboxPolicy"])
	}
}

func TestCodexCommandPermissionsSetRejectsListOnlyProfile(t *testing.T) {
	r := &CodexRunner{}

	_, err := r.codexCommandPermissionsSet(json.RawMessage(`{"profile":"custom-profile"}`))
	if err == nil {
		t.Fatal("expected profile apply to be rejected")
	}
	if !strings.Contains(err.Error(), "list-only") {
		t.Fatalf("error = %q, want list-only explanation", err.Error())
	}
}

func TestCodexCommandStopWithoutActiveTurnWritesPrompt(t *testing.T) {
	var out strings.Builder
	r := &CodexRunner{
		outputHandler:    func(b []byte) { out.Write(b) },
		providerThreadID: "thread-1",
	}

	result, err := r.codexCommandStop()
	if err != nil {
		t.Fatalf("codexCommandStop returned error: %v", err)
	}
	payload, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result = %#v, want map", result)
	}
	if payload["stopped"] != false {
		t.Fatalf("stopped = %#v, want false", payload["stopped"])
	}
	if !strings.Contains(out.String(), "No active Codex turn to stop.") {
		t.Fatalf("output = %q, want no-active-turn message", out.String())
	}
	if !strings.Contains(out.String(), "OpenPoet Codex>") {
		t.Fatalf("output = %q, want prompt redraw", out.String())
	}
	requireCodexTranscriptStatus(t, r, "Stop", "No active Codex turn to stop.")
}

func TestCodexInterruptedTurnSuppressesLateOutput(t *testing.T) {
	r := &CodexRunner{
		interruptedTurns: map[string]bool{"turn-1": true},
	}

	if !r.shouldSuppressTurnOutput(json.RawMessage(`{"turnId":"turn-1","delta":"late"}`)) {
		t.Fatal("expected interrupted turn output to be suppressed")
	}
	if r.shouldSuppressTurnOutput(json.RawMessage(`{"turnId":"turn-2","delta":"current"}`)) {
		t.Fatal("did not expect current turn output to be suppressed")
	}
	if r.shouldSuppressTurnOutput(json.RawMessage(`{"delta":"no turn"}`)) {
		t.Fatal("did not expect output without a turn id to be suppressed")
	}
}

func TestCodexCommandProcessTrackingUsesCamelCaseItemType(t *testing.T) {
	r := &CodexRunner{
		commandProcesses: make(map[string]map[string]codexCommandProcess),
	}

	r.writeItemStarted(json.RawMessage(`{
		"turnId":"turn-1",
		"item":{
			"type":"commandExecution",
			"command":"sleep 1",
			"processId":"proc-1"
		}
	}`))

	got := r.commandProcessIDsLocked("turn-1")
	if len(got) != 1 || got[0] != "proc-1" {
		t.Fatalf("tracked process IDs = %#v, want [proc-1]", got)
	}

	r.writeItemCompleted(json.RawMessage(`{
		"turnId":"turn-1",
		"item":{
			"type":"commandExecution",
			"status":"completed",
			"processId":"proc-1"
		}
	}`))
	if got := r.commandProcessIDsLocked("turn-1"); len(got) != 0 {
		t.Fatalf("tracked process IDs after completion = %#v, want empty", got)
	}
}

func TestCodexStateSnapshotIncludesAgentMetadata(t *testing.T) {
	started := time.Unix(100, 0)
	r := &CodexRunner{
		cfg:              &SessionConfig{SessionID: "session-1", BackendConfig: `{"model":"gpt-5","reasoning_effort":"high","approval_policy":"on-request","sandbox_mode":"workspace-write"}`},
		providerThreadID: "thread-1",
		activeTurnID:     "turn-1",
		agentPhase:       "running_command",
		agentDetail:      "go test ./...",
		lastInputTokens:  1200,
		lastOutputTokens: 340,
		commandProcesses: map[string]map[string]codexCommandProcess{
			"turn-1": {
				"proc-1": {ID: "proc-1", Command: "go test ./...", StartedAt: started},
			},
		},
	}

	state := r.codexStateSnapshot()
	if state["phase"] != "running_command" || state["detail"] != "go test ./..." {
		t.Fatalf("unexpected phase/detail: %#v", state)
	}
	if state["model"] != "gpt-5" || state["reasoningEffort"] != "high" {
		t.Fatalf("unexpected model fields: %#v", state)
	}
	if state["activeProcessCount"] != 1 {
		t.Fatalf("activeProcessCount = %#v, want 1", state["activeProcessCount"])
	}
	processes, ok := state["activeCommandProcesses"].([]map[string]interface{})
	if !ok || len(processes) != 1 || processes[0]["command"] != "go test ./..." {
		t.Fatalf("unexpected activeCommandProcesses: %#v", state["activeCommandProcesses"])
	}
}

func TestDisplayCodexConfigResolvesCatalogDefault(t *testing.T) {
	r := &CodexRunner{
		cfg: &SessionConfig{SessionID: "session-1", BackendConfig: `{}`},
		modelDefaults: codexModelDefaults{
			Model:           "gpt-5-codex",
			ReasoningEffort: "medium",
			ServiceTier:     "standard",
		},
		modelDefaultsByID: map[string]codexModelDefaults{
			"gpt-5-codex": {
				Model:           "gpt-5-codex",
				ReasoningEffort: "medium",
				ServiceTier:     "standard",
			},
		},
	}

	cc := r.displayCodexConfig()
	if cc.Model != "gpt-5-codex" {
		t.Fatalf("Model = %q, want resolved default", cc.Model)
	}
	if cc.ReasoningEffort != "medium" {
		t.Fatalf("ReasoningEffort = %q, want catalog default", cc.ReasoningEffort)
	}
	if cc.ServiceTier != "standard" {
		t.Fatalf("ServiceTier = %q, want catalog default", cc.ServiceTier)
	}

	state := r.codexStateSnapshot()
	if state["model"] != "gpt-5-codex" || state["reasoningEffort"] != "medium" || state["serviceTier"] != "standard" {
		t.Fatalf("snapshot did not use resolved defaults: %#v", state)
	}
}

func TestDisplayCodexConfigUsesConfiguredModelDefaults(t *testing.T) {
	r := &CodexRunner{
		cfg: &SessionConfig{SessionID: "session-1", BackendConfig: `{"model":"gpt-5-mini"}`},
		modelDefaults: codexModelDefaults{
			Model:           "gpt-5-codex",
			ReasoningEffort: "medium",
			ServiceTier:     "standard",
		},
		modelDefaultsByID: map[string]codexModelDefaults{
			"gpt-5-mini": {
				Model:           "gpt-5-mini",
				ReasoningEffort: "high",
				ServiceTier:     "flex",
			},
		},
	}

	cc := r.displayCodexConfig()
	if cc.Model != "gpt-5-mini" {
		t.Fatalf("Model = %q, want configured model", cc.Model)
	}
	if cc.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want configured model default", cc.ReasoningEffort)
	}
	if cc.ServiceTier != "flex" {
		t.Fatalf("ServiceTier = %q, want configured model default", cc.ServiceTier)
	}
}

func TestSignalCodexCommandProcessRejectsInvalidProcessID(t *testing.T) {
	if err := signalCodexCommandProcess("not-a-pid"); err == nil {
		t.Fatal("expected invalid process id error")
	}
	if err := signalCodexCommandProcess("0"); err == nil {
		t.Fatal("expected non-positive process id error")
	}
}
