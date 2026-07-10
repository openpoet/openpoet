package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type codexCommandRequest struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
}

func (r *CodexRunner) HandleCodexCommand(ctx context.Context, data json.RawMessage) (interface{}, error) {
	var req codexCommandRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("invalid Codex command request: %w", err)
	}
	req.Action = strings.TrimSpace(req.Action)
	if req.Action == "" {
		return nil, errors.New("missing Codex command action")
	}

	switch req.Action {
	case "ui/status":
		return r.codexStateSnapshot(), nil
	case "ui/transcript":
		return r.codexTranscriptSnapshot(), nil
	case "input/send":
		return r.codexCommandInputSend(ctx, req.Params)
	case "status/read":
		return r.codexCommandStatus(ctx)
	case "model/list":
		return r.codexCommandModelList(ctx, req.Params)
	case "model/set":
		return r.codexCommandModelSet(req.Params)
	case "session/model/set":
		return r.codexCommandSessionModelSet(req.Params)
	case "session/effort/set":
		return r.codexCommandSessionEffortSet(req.Params)
	case "permissions/list":
		return r.codexCommandPermissionsList(ctx)
	case "permissions/set":
		return r.codexCommandPermissionsSet(req.Params)
	case "resume/list":
		return r.codexCommandResumeList(ctx, req.Params)
	case "resume/apply":
		return r.codexCommandResumeApply(ctx, req.Params)
	case "thread/new":
		return r.codexCommandNewThread(ctx)
	case "thread/compact":
		return r.codexCommandCompact(ctx)
	case "turn/stop":
		return r.codexCommandStop()
	default:
		return nil, fmt.Errorf("unsupported Codex command action: %s", req.Action)
	}
}

func (r *CodexRunner) codexCommandInputSend(ctx context.Context, raw json.RawMessage) (interface{}, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid Codex input: %w", err)
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, errors.New("empty Codex input")
	}
	r.addCodexTranscriptBlock("user", in.Text, "", "", "")
	echo := append([]byte("\r\n\x1b[36mYou:\x1b[0m "), codexTerminalTextBytes(in.Text)...)
	echo = append(echo, []byte("\r\n")...)
	r.write(echo)
	method, turnID, err := r.startOrSteerTurn(ctx, in.Text)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"method": method,
		"turnId": turnID,
		"state":  r.codexStateSnapshot(),
	}, nil
}

func (r *CodexRunner) codexCommandStatus(ctx context.Context) (interface{}, error) {
	cc := r.displayCodexConfig()
	r.mu.Lock()
	threadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	modelOverrideSet := r.modelOverrideSet
	reasoningOverrideSet := r.reasoningSet
	serviceTierSet := r.serviceTierSet
	approvalOverride := r.approvalOverride
	sandboxOverride := r.sandboxOverride
	sessionID := ""
	skipPermissions := false
	if r.cfg != nil {
		sessionID = r.cfg.SessionID
		skipPermissions = r.cfg.DangerouslySkipPermissions
	}
	r.mu.Unlock()

	approval := cc.ApprovalPolicy
	sandbox := cc.SandboxMode
	if skipPermissions {
		approval = "never"
		sandbox = "danger-full-access"
	}

	status := map[string]interface{}{
		"sessionId":            sessionID,
		"threadId":             threadID,
		"activeTurnId":         activeTurnID,
		"activeTurn":           activeTurnID != "",
		"model":                cc.Model,
		"reasoningEffort":      cc.ReasoningEffort,
		"serviceTier":          cc.ServiceTier,
		"approvalPolicy":       approval,
		"sandboxMode":          sandbox,
		"modelOverride":        modelOverrideSet,
		"reasoningOverride":    reasoningOverrideSet,
		"serviceTierOverride":  serviceTierSet,
		"approvalOverride":     approvalOverride != "",
		"sandboxOverride":      sandboxOverride != "",
		"skipPermissions":      skipPermissions,
		"configReadAvailable":  false,
		"permissionProfileAPI": "list-only",
	}

	config, err := r.codexCommandRequest(ctx, "config/read", map[string]interface{}{
		"cwd":           r.workDir,
		"includeLayers": false,
	})
	if err == nil {
		status["configReadAvailable"] = true
		status["config"] = config
	} else {
		status["configReadError"] = err.Error()
	}

	r.write([]byte("\x1b[1mCodex status\x1b[0m\r\n"))
	r.write([]byte(fmt.Sprintf("Session: %s\r\n", displayCodexSlashValue(sessionID))))
	r.write([]byte(fmt.Sprintf("Thread: %s\r\n", displayCodexSlashValue(threadID))))
	active := "no"
	if activeTurnID != "" {
		active = "yes (" + activeTurnID + ")"
	}
	statusLines := []string{
		fmt.Sprintf("Session: %s", displayCodexSlashValue(sessionID)),
		fmt.Sprintf("Thread: %s", displayCodexSlashValue(threadID)),
		fmt.Sprintf("Active turn: %s", active),
		fmt.Sprintf("Model: %s%s", displayCodexSlashValue(cc.Model), codexSlashOverrideSuffix(modelOverrideSet)),
		fmt.Sprintf("Reasoning effort: %s%s", displayCodexSlashValue(cc.ReasoningEffort), codexSlashOverrideSuffix(reasoningOverrideSet)),
		fmt.Sprintf("Service tier: %s%s", displayCodexSlashValue(cc.ServiceTier), codexSlashOverrideSuffix(serviceTierSet)),
		fmt.Sprintf("Approval policy: %s%s", approval, codexSlashOverrideSuffix(approvalOverride != "")),
		fmt.Sprintf("Sandbox: %s%s", sandbox, codexSlashOverrideSuffix(sandboxOverride != "")),
	}
	r.addCodexCommandFeedback("Codex status", strings.Join(statusLines, "\n"))
	r.write([]byte(fmt.Sprintf("Active turn: %s\r\n", active)))
	r.write([]byte(fmt.Sprintf("Model: %s%s\r\n", displayCodexSlashValue(cc.Model), codexSlashOverrideSuffix(modelOverrideSet))))
	r.write([]byte(fmt.Sprintf("Reasoning effort: %s%s\r\n", displayCodexSlashValue(cc.ReasoningEffort), codexSlashOverrideSuffix(reasoningOverrideSet))))
	r.write([]byte(fmt.Sprintf("Service tier: %s%s\r\n", displayCodexSlashValue(cc.ServiceTier), codexSlashOverrideSuffix(serviceTierSet))))
	r.write([]byte(fmt.Sprintf("Approval policy: %s%s\r\n", approval, codexSlashOverrideSuffix(approvalOverride != ""))))
	r.write([]byte(fmt.Sprintf("Sandbox: %s%s\r\n", sandbox, codexSlashOverrideSuffix(sandboxOverride != ""))))
	r.writePrompt()
	return status, nil
}

func (r *CodexRunner) codexCommandModelList(ctx context.Context, raw json.RawMessage) (interface{}, error) {
	var in struct {
		Cursor        string `json:"cursor"`
		IncludeHidden bool   `json:"includeHidden"`
		Limit         int    `json:"limit"`
	}
	_ = json.Unmarshal(raw, &in)
	if in.Limit <= 0 || in.Limit > 100 {
		in.Limit = 100
	}

	params := map[string]interface{}{
		"limit":         in.Limit,
		"includeHidden": in.IncludeHidden,
	}
	if in.Cursor != "" {
		params["cursor"] = in.Cursor
	}
	return r.codexCommandRequest(ctx, "model/list", params)
}

func (r *CodexRunner) codexCommandModelSet(raw json.RawMessage) (interface{}, error) {
	var in struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
		ServiceTier     string `json:"serviceTier"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid model selection: %w", err)
	}

	model := strings.TrimSpace(in.Model)
	reasoning := strings.TrimSpace(in.ReasoningEffort)
	serviceTier := strings.TrimSpace(in.ServiceTier)
	if model == "" || strings.EqualFold(model, "default") || strings.EqualFold(model, "reset") {
		r.mu.Lock()
		r.modelOverride = ""
		r.modelOverrideSet = false
		r.reasoningOverride = ""
		r.reasoningSet = false
		r.serviceTierOverride = ""
		r.serviceTierSet = false
		r.mu.Unlock()
		msg := "Model overrides cleared. The configured default will be used on future turns."
		r.addCodexCommandFeedback("Model", msg)
		r.write([]byte(msg + "\r\n"))
		r.writePrompt()
		r.publishCodexState()
		return r.codexCommandCurrentModel(), nil
	}
	if strings.ContainsAny(model, "\r\n\t ") {
		return nil, errors.New("model id cannot contain whitespace")
	}

	r.mu.Lock()
	r.modelOverride = model
	r.modelOverrideSet = true
	r.reasoningOverride = reasoning
	r.reasoningSet = reasoning != ""
	r.serviceTierOverride = serviceTier
	r.serviceTierSet = serviceTier != ""
	r.mu.Unlock()

	msg := fmt.Sprintf("Model override set to %s for future turns", model)
	if reasoning != "" {
		msg += fmt.Sprintf(" / effort %s", reasoning)
	}
	if serviceTier != "" {
		msg += fmt.Sprintf(" / tier %s", serviceTier)
	}
	r.addCodexCommandFeedback("Model", msg+".")
	r.write([]byte(msg + ".\r\n"))
	r.writePrompt()
	r.publishCodexState()
	return r.codexCommandCurrentModel(), nil
}

// codexCommandSessionModelSet changes only the model override. Unlike the UI's
// model/set action, it intentionally preserves reasoning effort and service tier.
func (r *CodexRunner) codexCommandSessionModelSet(raw json.RawMessage) (interface{}, error) {
	var in struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid model selection: %w", err)
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, errors.New("model is required")
	}

	r.mu.Lock()
	if strings.EqualFold(model, "default") || strings.EqualFold(model, "reset") {
		r.modelOverride = ""
		r.modelOverrideSet = false
		model = "default"
	} else {
		if strings.ContainsAny(model, "\r\n\t ") {
			r.mu.Unlock()
			return nil, errors.New("model id cannot contain whitespace")
		}
		r.modelOverride = model
		r.modelOverrideSet = true
	}
	r.mu.Unlock()

	msg := fmt.Sprintf("Session model set to %s for future turns.", model)
	r.addCodexCommandFeedback("Model", msg)
	r.write([]byte(msg + "\r\n"))
	r.writePrompt()
	r.publishCodexState()
	return r.codexCommandCurrentModel(), nil
}

// codexCommandSessionEffortSet changes only the reasoning effort override.
func (r *CodexRunner) codexCommandSessionEffortSet(raw json.RawMessage) (interface{}, error) {
	var in struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid effort selection: %w", err)
	}
	effort := strings.ToLower(strings.TrimSpace(in.Effort))
	if effort == "" {
		return nil, errors.New("effort is required")
	}

	r.mu.Lock()
	if effort == "default" || effort == "reset" {
		r.reasoningOverride = ""
		r.reasoningSet = false
		effort = "default"
	} else {
		r.reasoningOverride = effort
		r.reasoningSet = true
	}
	r.mu.Unlock()

	msg := fmt.Sprintf("Session reasoning effort set to %s for future turns.", effort)
	r.addCodexCommandFeedback("Reasoning effort", msg)
	r.write([]byte(msg + "\r\n"))
	r.writePrompt()
	r.publishCodexState()
	return r.codexCommandCurrentModel(), nil
}

func (r *CodexRunner) codexCommandPermissionsList(ctx context.Context) (interface{}, error) {
	profiles, err := r.codexCommandRequest(ctx, "permissionProfile/list", map[string]interface{}{
		"cwd":   r.workDir,
		"limit": 100,
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"current":               r.codexCommandCurrentPermissions(),
		"presets":               codexCommandPermissionPresets(),
		"profiles":              profiles,
		"profileApplySupported": false,
	}, nil
}

func (r *CodexRunner) codexCommandPermissionsSet(raw json.RawMessage) (interface{}, error) {
	var in struct {
		Preset  string `json:"preset"`
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid permissions selection: %w", err)
	}
	if strings.TrimSpace(in.Profile) != "" {
		return nil, errors.New("Codex app-server currently exposes permission profiles as list-only; use a preset for future turns")
	}
	preset := strings.TrimSpace(strings.ToLower(in.Preset))
	if preset == "" {
		return nil, errors.New("missing permissions preset")
	}
	if preset == "default" || preset == "reset" {
		r.mu.Lock()
		r.approvalOverride = ""
		r.sandboxOverride = ""
		r.mu.Unlock()
		cc := r.effectiveCodexConfig()
		msg := fmt.Sprintf("Permission overrides cleared. Using approval=%s sandbox=%s", cc.ApprovalPolicy, cc.SandboxMode)
		r.addCodexCommandFeedback("Permissions", msg)
		r.write([]byte(msg + "\r\n"))
		r.writePrompt()
		r.publishCodexState()
		return r.codexCommandCurrentPermissions(), nil
	}

	approval, sandbox, ok := codexSlashPermissionPreset(preset)
	if !ok {
		return nil, fmt.Errorf("unsupported permissions preset: %s", preset)
	}
	r.mu.Lock()
	if approval != "" {
		r.approvalOverride = approval
	}
	if sandbox != "" {
		r.sandboxOverride = sandbox
	}
	r.mu.Unlock()

	current := r.codexCommandCurrentPermissions()
	msg := fmt.Sprintf("Permissions updated for future turns: approval=%s sandbox=%s", current["approvalPolicy"], current["sandboxMode"])
	r.addCodexCommandFeedback("Permissions", msg)
	r.write([]byte(msg + "\r\n"))
	r.writePrompt()
	r.publishCodexState()
	return current, nil
}

func (r *CodexRunner) codexCommandResumeList(ctx context.Context, raw json.RawMessage) (interface{}, error) {
	var in struct {
		Cursor     string `json:"cursor"`
		SearchTerm string `json:"searchTerm"`
		Limit      int    `json:"limit"`
	}
	_ = json.Unmarshal(raw, &in)
	if in.Limit <= 0 || in.Limit > 50 {
		in.Limit = 20
	}
	params := map[string]interface{}{
		"archived":      false,
		"cwd":           r.workDir,
		"limit":         in.Limit,
		"sortKey":       "updated_at",
		"sortDirection": "desc",
	}
	if strings.TrimSpace(in.SearchTerm) != "" {
		params["searchTerm"] = strings.TrimSpace(in.SearchTerm)
	}
	if strings.TrimSpace(in.Cursor) != "" {
		params["cursor"] = strings.TrimSpace(in.Cursor)
	}
	return r.codexCommandRequest(ctx, "thread/list", params)
}

func (r *CodexRunner) codexCommandResumeApply(ctx context.Context, raw json.RawMessage) (interface{}, error) {
	var in struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid resume selection: %w", err)
	}
	threadID := strings.TrimSpace(in.ThreadID)
	if threadID == "" {
		return nil, errors.New("missing thread id")
	}

	r.mu.Lock()
	activeTurnID := r.activeTurnID
	r.mu.Unlock()
	if activeTurnID != "" {
		return nil, errors.New("cannot resume another thread while a Codex turn is active")
	}

	params := r.threadParams()
	params["threadId"] = threadID
	delete(params, "sessionStartSource")

	result, err := r.codexCommandRequest(ctx, "thread/resume", params)
	if err != nil {
		return nil, err
	}
	rawResult, _ := json.Marshal(result)
	resumedThreadID := codexThreadIDFromResult(rawResult)
	if resumedThreadID == "" {
		resumedThreadID = threadID
	}
	r.switchCodexThread(resumedThreadID)
	r.replaceCodexTranscriptFromThreadResponse(rawResult)
	msg := fmt.Sprintf("Resumed Codex thread: %s", resumedThreadID)
	r.addCodexCommandFeedback("Resume", msg)
	r.write([]byte(msg + "\r\n"))
	r.writePrompt()
	return map[string]interface{}{
		"threadId":   resumedThreadID,
		"thread":     result,
		"transcript": r.codexTranscriptSnapshot(),
	}, nil
}

func (r *CodexRunner) codexCommandNewThread(ctx context.Context) (interface{}, error) {
	r.mu.Lock()
	currentThreadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	r.mu.Unlock()
	if currentThreadID == "" {
		return nil, errors.New("Codex thread is not ready yet")
	}
	if activeTurnID != "" {
		return nil, errors.New("cannot start a new Codex thread while a turn is active")
	}

	params := r.threadParams()
	params["sessionStartSource"] = "clear"

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := r.codexCommandRequest(ctx, "thread/start", params)
	if err != nil {
		return nil, err
	}
	rawResult, _ := json.Marshal(result)
	threadID := codexThreadIDFromResult(rawResult)
	if threadID == "" {
		return nil, errors.New("Codex thread/start response did not include a thread id")
	}
	r.switchCodexThread(threadID)
	r.addCodexCommandFeedback("Thread", fmt.Sprintf("Started new Codex thread: %s", threadID))
	r.write([]byte("\x1b[2J\x1b[H"))
	r.write([]byte(fmt.Sprintf("\x1b[90mStarted new Codex thread: %s\x1b[0m\r\n", threadID)))
	r.writePrompt()
	return map[string]interface{}{
		"threadId":       threadID,
		"previousThread": currentThreadID,
		"thread":         result,
	}, nil
}

func (r *CodexRunner) codexCommandCompact(ctx context.Context) (interface{}, error) {
	r.mu.Lock()
	threadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	r.mu.Unlock()
	if threadID == "" {
		return nil, errors.New("Codex thread is not ready yet")
	}
	if activeTurnID != "" {
		return nil, errors.New("cannot compact while a Codex turn is active")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := r.request(ctx, "thread/compact/start", map[string]interface{}{"threadId": threadID})
	if err != nil {
		return nil, err
	}
	var result interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		result = string(raw)
	}
	msg := "Codex compaction requested."
	r.addCodexCommandFeedback("Compact", msg)
	r.write([]byte(msg + "\r\n"))
	r.writePrompt()
	return map[string]interface{}{
		"threadId": threadID,
		"result":   result,
	}, nil
}

func (r *CodexRunner) codexCommandStop() (interface{}, error) {
	r.mu.Lock()
	threadID := r.providerThreadID
	activeTurnID := r.activeTurnID
	r.mu.Unlock()
	if threadID == "" {
		return nil, errors.New("Codex thread is not ready yet")
	}
	if activeTurnID == "" {
		msg := "No active Codex turn to stop."
		r.addCodexCommandFeedback("Stop", msg)
		r.write([]byte(msg + "\r\n"))
		r.writePrompt()
		return map[string]interface{}{
			"threadId":   threadID,
			"stopped":    false,
			"activeTurn": false,
		}, nil
	}

	go r.interruptTurn()
	msg := fmt.Sprintf("Stop requested for Codex turn: %s", activeTurnID)
	r.addCodexCommandFeedback("Stop", msg)
	r.write([]byte(msg + "\r\n"))
	r.writePrompt()
	return map[string]interface{}{
		"threadId": threadID,
		"turnId":   activeTurnID,
		"stopped":  true,
	}, nil
}

func (r *CodexRunner) codexCommandRequest(ctx context.Context, method string, params interface{}) (interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	raw, err := r.request(ctx, method, params)
	if err != nil {
		return nil, err
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return raw, nil
	}
	return out, nil
}

func (r *CodexRunner) addCodexCommandFeedback(title, text string) {
	r.addCodexTranscriptBlock("status", text, title, "", "complete")
}

func (r *CodexRunner) codexCommandCurrentModel() map[string]interface{} {
	cc := r.displayCodexConfig()
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]interface{}{
		"model":               cc.Model,
		"reasoningEffort":     cc.ReasoningEffort,
		"serviceTier":         cc.ServiceTier,
		"modelOverride":       r.modelOverrideSet,
		"reasoningOverride":   r.reasoningSet,
		"serviceTierOverride": r.serviceTierSet,
	}
}

func (r *CodexRunner) codexCommandCurrentPermissions() map[string]string {
	cc := r.effectiveCodexConfig()
	return map[string]string{
		"approvalPolicy": cc.ApprovalPolicy,
		"sandboxMode":    cc.SandboxMode,
	}
}

func codexCommandPermissionPresets() []map[string]string {
	return []map[string]string{
		{"id": "default", "label": "Default", "description": "Use the project or Codex config defaults."},
		{"id": "read-only", "label": "Read Only", "description": "Chat and inspect without file writes."},
		{"id": "workspace-write", "label": "Workspace Write", "description": "Allow normal edits in the workspace, asking for escalations."},
		{"id": "danger-full-access", "label": "Danger Full Access", "description": "Allow full local access without sandbox prompts."},
	}
}
