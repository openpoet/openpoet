package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/llm"
	"openpoet/internal/mcp"
	"openpoet/internal/sessionmeta"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var docLinkRe = regexp.MustCompile(`/app/doc/([a-zA-Z0-9-]+)`)

// planningCollector accumulates task actions during a chat stream for batch proposal.
type planningCollector struct {
	mu      sync.Mutex
	actions []PlanningTaskAction
}

func (pc *planningCollector) add(action PlanningTaskAction) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.actions = append(pc.actions, action)
}

func (pc *planningCollector) getActions() []PlanningTaskAction {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return append([]PlanningTaskAction{}, pc.actions...)
}

func (pc *planningCollector) hasActions() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return len(pc.actions) > 0
}

func (pc *planningCollector) countCreates() int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	count := 0
	for _, a := range pc.actions {
		if a.Action == "create" {
			count++
		}
	}
	return count
}

// sseEvent represents a Server-Sent Event for broadcasting to reconnected clients.
type sseEvent struct {
	Type string
	Data interface{}
}

// activeStreamInfo holds the cancel function and broadcast subscribers for an active stream.
type activeStreamInfo struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	subs   []chan sseEvent
}

func (a *activeStreamInfo) broadcast(eventType string, data interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	evt := sseEvent{Type: eventType, Data: data}
	for _, ch := range a.subs {
		select {
		case ch <- evt:
		default:
			// subscriber too slow, skip
		}
	}
}

func (a *activeStreamInfo) subscribe() (<-chan sseEvent, func()) {
	ch := make(chan sseEvent, 64)
	a.mu.Lock()
	a.subs = append(a.subs, ch)
	a.mu.Unlock()
	unsub := func() {
		a.mu.Lock()
		for i, s := range a.subs {
			if s == ch {
				a.subs = append(a.subs[:i], a.subs[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
		// Non-blocking drain of remaining buffered events.
		// Do NOT use blocking "for range ch" here — it deadlocks if the
		// channel hasn't been closed yet (e.g. client disconnects before
		// stream ends), because broadcast() also needs a.mu.
		for {
			select {
			case <-ch:
			default:
				return
			}
		}
	}
	return ch, unsub
}

func (a *activeStreamInfo) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ch := range a.subs {
		close(ch)
	}
	a.subs = nil
}

// AIHandler handles all AI-related endpoints.
type AIHandler struct {
	api         *API
	providerMgr *llm.ProviderManager
	mu          sync.RWMutex
	reports     *application.ReportService

	activeStreams   map[int64]*activeStreamInfo
	activeStreamsMu sync.Mutex

	// streamMessageIDs maps conversationID → current assistant messageID during streaming.
	// Used by ExecuteTool (GoSDK path) to associate TempDocuments with the correct message.
	streamMessageIDsMu sync.Mutex
	streamMessageIDs   map[int64]int64

	// streamCollectors maps conversationID → planningCollector during streaming.
	// Used by ExecuteTool (GoSDK path) so planning-mode tool calls can collect task actions.
	streamCollectorsMu sync.Mutex
	streamCollectors   map[int64]*planningCollector

	// streamProactiveTypes maps conversationID → proactiveType during streaming.
	// Used to detect skill_customization context for intercepting skill tools.
	streamProactiveTypesMu sync.Mutex
	streamProactiveTypes   map[int64]string
}

func (h *AIHandler) SetReportService(reports *application.ReportService) {
	h.reports = reports
}

// NewAIHandler creates a new AI handler.
func NewAIHandler(api *API, providerMgr *llm.ProviderManager) *AIHandler {
	return &AIHandler{
		api:           api,
		providerMgr:   providerMgr,
		activeStreams: make(map[int64]*activeStreamInfo),
	}
}

func (h *AIHandler) sharedAIAssistantService(w http.ResponseWriter) (*application.AIAssistantService, bool) {
	var api *API
	if h != nil {
		api = h.api
	}
	services, ok := requirePlatformApplicationServices(api, w)
	if !ok {
		return nil, false
	}
	if services.Collaboration.AI == nil {
		respondError(w, http.StatusServiceUnavailable, "AI application service unavailable")
		return nil, false
	}
	return services.Collaboration.AI, true
}

// SetProviderManager replaces the provider manager (used during reinit).
func (h *AIHandler) SetProviderManager(pm *llm.ProviderManager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.providerMgr = pm
}

// getSlotModel returns the model for a given slot, or empty for auto-detect.
func (h *AIHandler) getSlotModel(slot llm.Slot) string {
	cfg := h.providerMgr.GetSlotConfig(slot)
	if cfg != nil && cfg.Model != "" {
		return cfg.Model
	}
	return ""
}

func (h *AIHandler) registerStream(convID int64, cancel context.CancelFunc) {
	h.activeStreamsMu.Lock()
	existing, hadExisting := h.activeStreams[convID]
	h.activeStreams[convID] = &activeStreamInfo{cancel: cancel}
	h.activeStreamsMu.Unlock()
	// Cancel any existing stream for this conversation (outside lock)
	if hadExisting {
		existing.cancel()
		existing.closeAll()
	}
}

func (h *AIHandler) unregisterStream(convID int64) {
	h.activeStreamsMu.Lock()
	info, ok := h.activeStreams[convID]
	if ok {
		delete(h.activeStreams, convID)
	}
	h.activeStreamsMu.Unlock()
	if ok {
		info.closeAll()
	}
}

func (h *AIHandler) stopStream(convID int64) {
	h.activeStreamsMu.Lock()
	info, ok := h.activeStreams[convID]
	if ok {
		delete(h.activeStreams, convID)
	}
	h.activeStreamsMu.Unlock()
	if ok {
		info.cancel()
		info.closeAll()
	}
}

// broadcastToStream sends an SSE event to all subscribers of a conversation's active stream.
func (h *AIHandler) broadcastToStream(convID int64, eventType string, data interface{}) {
	h.activeStreamsMu.Lock()
	info, ok := h.activeStreams[convID]
	h.activeStreamsMu.Unlock()
	if ok {
		info.broadcast(eventType, data)
	}
}

// HandleActiveStream returns the conversation ID if there's an active LLM stream.
func (h *AIHandler) HandleActiveStream(w http.ResponseWriter, r *http.Request) {
	h.activeStreamsMu.Lock()
	var activeConvID int64
	for convID := range h.activeStreams {
		activeConvID = convID
		break
	}
	h.activeStreamsMu.Unlock()
	if activeConvID > 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"active":          true,
			"conversation_id": activeConvID,
		})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"active": false,
	})
}

// HandleStreamReconnect allows a client to subscribe to an active stream via SSE.
func (h *AIHandler) HandleStreamReconnect(w http.ResponseWriter, r *http.Request) {
	convID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if convID <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}

	h.activeStreamsMu.Lock()
	info, ok := h.activeStreams[convID]
	h.activeStreamsMu.Unlock()
	if !ok {
		respondJSON(w, http.StatusOK, map[string]interface{}{"active": false})
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	ch, unsub := info.subscribe()
	defer unsub()

	// Send initial ping so the client knows the connection is established
	h.sendSSE(w, flusher, "reconnected", map[string]interface{}{
		"conversation_id": convID,
	})

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				// Channel closed — stream ended
				h.sendSSE(w, flusher, "done", map[string]interface{}{
					"conversation_id": convID,
				})
				return
			}
			h.sendSSE(w, flusher, evt.Type, evt.Data)
		case <-r.Context().Done():
			return
		}
	}
}

// HandleStopChat cancels an active LLM stream for a conversation.
func (h *AIHandler) HandleStopChat(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	if err := service.StopConversation(platformUIContext(r), id, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	// Removing the stream registry and closing SSE subscribers remain transport
	// concerns. The shared service owns the stop mutation and provider disconnect.
	h.stopStream(id)
	respondJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// getProviderForSlot returns the provider for the specified AI operation slot.
func (h *AIHandler) getProviderForSlot(slot llm.Slot) llm.Provider {
	return h.providerMgr.GetProvider(slot)
}

// classifyAIError converts raw LLM errors into user-friendly messages.
// The full error is always logged server-side; this returns a clean message for the UI.
func classifyAIError(err error) string {
	errStr := err.Error()
	switch {
	case strings.Contains(errStr, "rate_limit") || strings.Contains(errStr, "rate limit"):
		return "AI is temporarily rate-limited. Please try again in a few seconds."
	case strings.Contains(errStr, "unknown message type"):
		return "AI encountered a temporary communication error. Please try again."
	case strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "context deadline exceeded"):
		return "AI request timed out. Please try again."
	case strings.Contains(errStr, "one-shot failed after"):
		return "AI is temporarily unavailable after multiple attempts. Please try again later."
	case strings.Contains(errStr, "connect error") || strings.Contains(errStr, "CLI not found"):
		return "AI service is not available. Check that Claude CLI is installed."
	default:
		return "AI generation failed. Please try again."
	}
}

// HandleStatus returns the AI configuration status per slot.
func (h *AIHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	slots := []llm.Slot{llm.SlotChat, llm.SlotBackground, llm.SlotSession}
	slotStatuses := make(map[string]interface{})

	for _, slot := range slots {
		p := h.providerMgr.GetProvider(slot)
		cfg := h.providerMgr.GetSlotConfig(slot)
		status := map[string]interface{}{
			"configured": p != nil,
		}
		if p != nil {
			status["provider"] = p.Name()
		}
		if cfg != nil && cfg.Model != "" {
			status["model"] = cfg.Model
		} else {
			status["model"] = "(auto-detect)"
		}
		slotStatuses[string(slot)] = status
	}

	// Legacy top-level fields for backward compatibility
	chatP := h.providerMgr.GetProvider(llm.SlotChat)
	result := map[string]interface{}{
		"configured": chatP != nil,
		"slots":      slotStatuses,
	}
	if chatP != nil {
		result["provider"] = chatP.Name()
	}
	chatModel := h.getSlotModel(llm.SlotChat)
	if chatModel == "" {
		chatModel = "(auto-detect)"
	}
	result["model"] = chatModel

	respondJSON(w, http.StatusOK, result)
}

// filterEnv returns a copy of env with entries matching the given key removed.
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// testAnthropicAPI makes a minimal non-streaming API call to validate an API key and model.
func testAnthropicAPI(apiKey, model string) error {
	if model == "" {
		model = llm.DefaultModel
	}

	client := &http.Client{Timeout: 15 * time.Second}
	testBody, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"max_tokens": 1,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})

	req, err := http.NewRequest("POST", llm.AnthropicAPIURL, bytes.NewReader(testBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", llm.AnthropicAPIVersion)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot connect to Anthropic API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	extractMsg := func() string {
		if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
			return errResp.Error.Message
		}
		return ""
	}

	switch resp.StatusCode {
	case 401:
		return fmt.Errorf("invalid API key (authentication failed)")
	case 403:
		return fmt.Errorf("API key lacks permission (status 403)")
	case 400:
		if msg := extractMsg(); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("bad request (status 400)")
	case 429:
		return fmt.Errorf("rate limited — API key is valid but currently throttled")
	case 529:
		return fmt.Errorf("Anthropic API is overloaded — try again later")
	default:
		if msg := extractMsg(); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}
}

// testClaudeCLI runs a minimal Claude CLI command to validate the model works.
func testClaudeCLI(model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	args := []string{"-p", "hi", "--max-turns", "1", "--output-format", "text"}
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	// Clear CLAUDECODE env var to avoid "nested session" blocking
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			// Extract meaningful part from CLI error output
			for _, line := range strings.Split(msg, "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "╭") && !strings.HasPrefix(line, "│") && !strings.HasPrefix(line, "╰") {
					return fmt.Errorf("%s", line)
				}
			}
			return fmt.Errorf("%s", msg)
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("CLI test timed out after 30s")
		}
		return fmt.Errorf("CLI exited with error: %v", err)
	}
	return nil
}

func (h *AIHandler) probeAIConnection(ctx context.Context, request application.AIConnectionTestRequest) (application.AIConnectionTestResult, error) {
	if request.APIKey == "" && request.ConfigID != nil {
		config, err := h.api.db.GetAIConfig(ctx, *request.ConfigID)
		if err != nil {
			return application.AIConnectionTestResult{}, &application.Error{Kind: application.ErrorNotFound, Code: "ai_config_not_found", Message: "AI configuration not found", Cause: err}
		}
		if config.APIKeyEncrypted != "" && config.APIKeyIV != "" {
			request.APIKey, err = h.api.encryptor.Decrypt(config.APIKeyEncrypted, config.APIKeyIV)
			if err != nil {
				return application.AIConnectionTestResult{}, errors.New("AI configuration credential could not be decrypted")
			}
		}
		if request.ProviderType == "" {
			request.ProviderType = config.ProviderType
		}
		if request.Model == "" {
			request.Model = config.Model
		}
		if request.BaseURL == "" {
			request.BaseURL = config.BaseURL
		}
	}
	result := application.AIConnectionTestResult{Provider: request.ProviderType, Model: request.Model}
	fail := func(message string) (application.AIConnectionTestResult, error) {
		result.Configured = false
		result.Message = message
		return result, nil
	}
	switch request.ProviderType {
	case "gosdk":
		if !llm.IsClaudeCLIAvailable() {
			return fail("Claude Code CLI not found. Install it with: npm install -g @anthropic-ai/claude-code")
		}
		if err := testClaudeCLI(request.Model); err != nil {
			return fail(err.Error())
		}
	case "apikey":
		if request.APIKey == "" {
			return fail("No API key provided")
		}
		if result.Model == "" {
			result.Model = llm.DefaultModel
		}
		if err := testAnthropicAPI(request.APIKey, result.Model); err != nil {
			return fail(err.Error())
		}
	case "ollama", "ollama-sdk":
		baseURL := strings.TrimRight(request.BaseURL, "/")
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		if result.Model == "" {
			result.Model = "qwen3-coder"
		}
		if request.ProviderType == "ollama-sdk" && !llm.IsClaudeCLIAvailable() {
			return fail("Claude Code CLI not found. Install it with: npm install -g @anthropic-ai/claude-code")
		}
		testBody, _ := json.Marshal(map[string]any{
			"model": result.Model, "max_tokens": 1,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(testBody))
		if err != nil {
			return fail("Cannot create AI connection test request")
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		if request.APIKey != "" {
			httpRequest.Header.Set("Authorization", "Bearer "+request.APIKey)
		}
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(httpRequest)
		if err != nil {
			return fail("Cannot connect to server at " + baseURL)
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return fail("Authentication failed (status " + strconv.Itoa(response.StatusCode) + ")")
		}
		if response.StatusCode != http.StatusOK {
			var errorResponse struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &errorResponse) == nil && errorResponse.Error.Message != "" {
				return fail(errorResponse.Error.Message)
			}
			return fail("Server returned error (status " + strconv.Itoa(response.StatusCode) + ")")
		}
	default:
		return fail("Unknown provider type: " + request.ProviderType)
	}
	result.Configured = true
	result.Message = "Connection successful"
	return result, nil
}

// HandleTestConnection tests an AI provider connection without saving settings.
// Accepts the new AI config format: provider_type, api_key, model, base_url.
func (h *AIHandler) HandleTestConnection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProviderType string `json:"provider_type"`
		APIKey       string `json:"api_key"`
		Model        string `json:"model"`
		BaseURL      string `json:"base_url"`
		ConfigID     *int64 `json:"config_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	result, err := service.TestConnection(platformUIContext(r), application.AIConnectionTestRequest{
		ProviderType: input.ProviderType, APIKey: input.APIKey, Model: input.Model,
		BaseURL: input.BaseURL, ConfigID: input.ConfigID,
	}, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	response := map[string]any{
		"provider": result.Provider, "model": result.Model, "configured": result.Configured,
	}
	if !result.Configured && result.Message != "" {
		response["error"] = result.Message
	}
	respondJSON(w, http.StatusOK, response)
}

func (h *AIHandler) buildChatRuntime(ctx context.Context, conversation *database.AIConversation, sessionProvider bool) (string, *database.AIAgent) {
	proactiveContext := ""
	if conversation.Source == "ai" && conversation.ProactiveContext != "" && conversation.ProactiveContext != "{}" {
		proactiveContext = conversation.ProactiveContext
		var suggestionStatus string
		_ = h.api.db.GetContext(ctx, &suggestionStatus, `SELECT status FROM ai_suggestions WHERE conversation_id=? LIMIT 1`, conversation.ID)
		if suggestionStatus == "accepted" || suggestionStatus == "dismissed" {
			proactiveContext += fmt.Sprintf("\n\n**IMPORTANT: This suggestion has already been %s by the user. Do NOT repeat or re-offer it. Continue the conversation acknowledging this.**", suggestionStatus)
		}
	}
	var agent *database.AIAgent
	if conversation.AgentID.Valid {
		agent, _ = h.api.db.GetAIAgent(ctx, conversation.AgentID.Int64)
	}
	if agent == nil {
		agent, _ = h.api.db.GetDefaultAIAgent(ctx)
	}
	systemPrompt := h.buildSystemPromptWithContext(ctx, proactiveContext, sessionProvider, agent)
	if agent != nil && !agent.IsDefault {
		systemPrompt += "\n\n## Agent Identity\nYou are the \"" + agent.Name + "\" agent."
		if agent.SystemPrompt != "" {
			systemPrompt += "\n\n### Instructions\n" + agent.SystemPrompt
		}
		if agent.ProjectFilter != "" {
			systemPrompt += "\n\n### Project Access\nYou only have access to the projects listed above. Do not reference or attempt to access any other projects."
		}
		if agent.ToolPolicy != "" {
			systemPrompt += "\n\n### Tool Restrictions\nYou only have access to a subset of tools. Do not attempt to perform actions outside your available tools."
		}
	} else if agent != nil && agent.SystemPrompt != "" {
		systemPrompt += "\n\n## Additional Instructions\n" + agent.SystemPrompt
	}
	return systemPrompt, agent
}

// HandleChat handles the main chat endpoint with SSE streaming.
func (h *AIHandler) HandleChat(w http.ResponseWriter, r *http.Request) {
	p := h.getProviderForSlot(llm.SlotChat)
	if p == nil {
		respondError(w, http.StatusServiceUnavailable, "AI not configured. Set an API key or install Claude Code CLI.")
		return
	}

	var input struct {
		ConversationID int64  `json:"conversation_id"`
		Message        string `json:"message"`
		ProjectID      int64  `json:"project_id"`
		AgentID        int64  `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	var agentID *int64
	if input.AgentID != 0 {
		agentID = &input.AgentID
	}
	ctx := platformUIContext(r)
	preparation, err := service.PrepareChat(ctx, application.AIChatCommand{
		ConversationID: input.ConversationID, ProjectID: input.ProjectID, AgentID: agentID,
		Prompt: input.Message, Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	input.Message = preparation.Prompt
	conv, err := h.api.db.GetAIConversation(ctx, preparation.Conversation.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to load conversation")
		return
	}

	// Load message history
	dbMessages, err := h.api.db.ListAIMessages(ctx, conv.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to load messages")
		return
	}

	// Convert to LLM messages — session providers only need the latest message
	var messages []llm.Message
	if sp, ok := p.(llm.SessionProvider); ok && sp.HasActiveSession(conv.ID) {
		// Session provider with active session: only send latest user message
		messages = []llm.Message{llm.NewTextMessage("user", input.Message)}
	} else {
		// Stateless provider or first message: send full history
		for _, m := range dbMessages {
			if m.Role == "assistant" && m.Status != "completed" {
				continue
			}
			messages = append(messages, llm.NewTextMessage(m.Role, m.Content))
		}
	}

	if preparation.Feedback != "" && len(messages) > 0 {
		last := &messages[len(messages)-1]
		if last.Role == "user" && len(last.Content) > 0 {
			last.Content[0].Text = preparation.Feedback + last.Content[0].Text
		}
	}

	_, isSessionProvider := p.(llm.SessionProvider)
	systemPrompt, agent := h.buildChatRuntime(ctx, conv, isSessionProvider)

	model := h.getSlotModel(llm.SlotChat)

	// Determine if we should use tools:
	// - Session providers (gosdk, ollama-sdk) handle tools internally via MCP — no tools in request
	// - API key and Ollama direct providers use native tool definitions in the request
	var tools []llm.ToolDefinition
	if !isSessionProvider {
		tools = llm.ChatTools()
		// Merge project custom tools — uses same fallback as GoSDK path:
		// project-specific tools if conversation has context, otherwise all project tools
		customToolDefs := h.GetCustomToolsForConversation(conv.ID)
		tools = append(tools, customToolDefs...)
		// Apply agent tool policy filtering
		if agent != nil && agent.ToolPolicy != "" {
			tools = filterToolsByPolicy(tools, agent.ToolPolicy)
		}
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	// SSE tolerant to disconnection — streaming continues even if browser disconnects
	var sseDisconnected bool
	var sseMu sync.Mutex
	safeSendSSE := func(eventType string, data interface{}) {
		// Always broadcast to reconnected subscribers
		h.broadcastToStream(conv.ID, eventType, data)
		// Send to original HTTP writer if still connected
		sseMu.Lock()
		defer sseMu.Unlock()
		if sseDisconnected {
			return
		}
		h.sendSSE(w, flusher, eventType, data)
	}

	// Detect HTTP disconnection (page refresh, tab close)
	go func() {
		<-r.Context().Done()
		sseMu.Lock()
		sseDisconnected = true
		sseMu.Unlock()
	}()

	// Send conversation ID
	safeSendSSE("conversation", map[string]interface{}{
		"id":    conv.ID,
		"title": conv.Title,
	})

	// Log tool names being sent to the model
	if len(tools) > 0 {
		var tnames []string
		for _, t := range tools {
			tnames = append(tnames, t.Name)
		}
		log.Printf("[AI] Sending %d tools to model: %v", len(tools), tnames)
	}

	req := &llm.Request{
		System:         systemPrompt,
		Messages:       messages,
		Tools:          tools,
		MaxTokens:      4096,
		Model:          model,
		ConversationID: conv.ID,
		SessionID:      conv.SessionID,
	}

	var assistantText strings.Builder
	var toolCalls []map[string]interface{}
	var textMu sync.Mutex // protects assistantText and toolCalls from concurrent access

	messageID, err := service.BeginChatResponse(ctx, conv.ID, platformUIAuthorization(r))
	if err != nil {
		safeSendSSE("error", map[string]any{"message": err.Error()})
		return
	}
	assistantMsg := &database.AIMessage{ID: messageID, ConversationID: conv.ID, Role: "assistant", ToolCalls: "[]", Status: "streaming", CreatedAt: time.Now()}

	// Register the current message ID for GoSDK tool execution
	h.streamMessageIDsMu.Lock()
	if h.streamMessageIDs == nil {
		h.streamMessageIDs = make(map[int64]int64)
	}
	h.streamMessageIDs[conv.ID] = assistantMsg.ID
	h.streamMessageIDsMu.Unlock()

	// Create task collector and make it accessible to GoSDK tool execution.
	// Must be stored BEFORE streaming starts, since GoSDK calls tools during streaming.
	collector := &planningCollector{}
	h.streamCollectorsMu.Lock()
	if h.streamCollectors == nil {
		h.streamCollectors = make(map[int64]*planningCollector)
	}
	h.streamCollectors[conv.ID] = collector
	h.streamCollectorsMu.Unlock()

	// Store proactive type for this conversation (used to detect skill_customization context)
	h.streamProactiveTypesMu.Lock()
	if h.streamProactiveTypes == nil {
		h.streamProactiveTypes = make(map[int64]string)
	}
	h.streamProactiveTypes[conv.ID] = conv.ProactiveType
	h.streamProactiveTypesMu.Unlock()

	safeSendSSE("message_id", map[string]interface{}{
		"id": assistantMsg.ID,
	})

	// Start debounced flusher to persist streaming content every 2 seconds.
	// The timer remains a transport concern; persistence crosses the same
	// Application Service boundary used by Automation chat.
	chatMetadata := application.EventMetadataFromContext(ctx)
	sf := newStreamFlusher(func(flushCtx context.Context, content, toolCallsJSON string) error {
		flushCtx = application.WithEventMetadata(flushCtx, chatMetadata)
		return service.UpdateChatResponse(flushCtx, application.AIChatProgress{
			ConversationID: conv.ID, MessageID: assistantMsg.ID,
			Content: content, ToolCallsJSON: toolCallsJSON,
		}, platformUIAuthorization(r))
	}, func() string {
		textMu.Lock()
		defer textMu.Unlock()
		return assistantText.String()
	}, func() string {
		textMu.Lock()
		defer textMu.Unlock()
		j, _ := json.Marshal(toolCalls)
		return string(j)
	})

	// Create independent context for LLM streaming (not tied to HTTP request lifecycle).
	// Add a 10-minute maximum timeout to prevent indefinite hangs when the Claude Code
	// subprocess stalls. This is a safety net — normal responses complete much faster.
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	h.registerStream(conv.ID, streamCancel)

	// Track streaming error for defer
	var streamErr string
	var resp *llm.Response
	var totalUsage llm.Usage

	// Defer: always persist final state of the assistant message
	defer func() {
		h.unregisterStream(conv.ID)
		sf.stop()

		// Clear the stream message ID mapping
		h.streamMessageIDsMu.Lock()
		delete(h.streamMessageIDs, conv.ID)
		h.streamMessageIDsMu.Unlock()

		// Clear the stream collector mapping
		h.streamCollectorsMu.Lock()
		delete(h.streamCollectors, conv.ID)
		h.streamCollectorsMu.Unlock()

		// Clear the proactive type mapping
		h.streamProactiveTypesMu.Lock()
		delete(h.streamProactiveTypes, conv.ID)
		h.streamProactiveTypesMu.Unlock()

		textMu.Lock()
		finalContent := assistantText.String()
		toolCallsJSON, _ := json.Marshal(toolCalls)
		finalToolCalls := string(toolCallsJSON)
		textMu.Unlock()

		saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer saveCancel()
		saveCtx = application.WithEventMetadata(saveCtx, application.EventMetadataFromContext(ctx))
		status := "completed"
		if streamErr != "" {
			status = "error"
		}
		if err := service.CompleteChatResponse(saveCtx, application.AIChatCompletion{
			ConversationID: conv.ID, MessageID: assistantMsg.ID, Content: finalContent,
			ToolCallsJSON: finalToolCalls, Status: status, Error: streamErr,
		}, platformUIAuthorization(r)); err != nil {
			log.Printf("[AI] Failed to finalize assistant message %d: %v", assistantMsg.ID, err)
		}
	}()

	var streamingErr error
	log.Printf("[AI-AUDIT] CALL_START subcategory=chat conversation=%d model=%s", conv.ID, req.Model)
	resp, streamingErr = p.StreamMessage(streamCtx, req, func(event llm.StreamEvent) error {
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil {
				if event.ContentBlock.Type == "tool_use" {
					safeSendSSE("tool_start", map[string]interface{}{
						"id":   event.ContentBlock.ID,
						"name": event.ContentBlock.Name,
					})
				}
			}
		case "content_block_delta":
			if event.Delta != nil && event.Delta.Type == "text_delta" {
				safeSendSSE("text", map[string]interface{}{
					"text": event.Delta.Text,
				})
				textMu.Lock()
				assistantText.WriteString(event.Delta.Text)
				textMu.Unlock()
			}
		}
		return nil
	})

	if streamingErr != nil {
		log.Printf("[AI] Stream error: %v", streamingErr)
		log.Printf("[AI-AUDIT] CALL_FAIL subcategory=chat conversation=%d error=%v", conv.ID, streamingErr)
		if streamCtx.Err() == context.Canceled {
			streamErr = "aborted"
		} else {
			streamErr = streamingErr.Error()
		}
		safeSendSSE("error", map[string]interface{}{
			"message": streamingErr.Error(),
		})
		return
	}
	log.Printf("[AI-AUDIT] CALL_OK subcategory=chat conversation=%d", conv.ID)

	// Track total token usage across all streaming calls
	if resp != nil {
		totalUsage = resp.Usage
	}

	// Handle tool use loop — session providers handle tools internally, skip for them
	if resp != nil && resp.StopReason == "tool_use" && !isSessionProvider {
		loopUsage, loopErr := h.handleToolLoop(streamCtx, safeSendSSE, p, req, resp, conv, &assistantText, &toolCalls, &textMu, 15, collector, assistantMsg.ID, platformUIAuthorization(r))
		if loopErr != nil {
			log.Printf("[AI] Tool loop error: %v", loopErr)
			if streamCtx.Err() == context.Canceled {
				streamErr = "aborted"
			} else {
				streamErr = loopErr.Error()
			}
			safeSendSSE("error", map[string]interface{}{
				"message": loopErr.Error(),
			})
			return
		}
		totalUsage.InputTokens += loopUsage.InputTokens
		totalUsage.OutputTokens += loopUsage.OutputTokens
		totalUsage.CacheReadTokens += loopUsage.CacheReadTokens
		totalUsage.CacheCreationTokens += loopUsage.CacheCreationTokens
	}

	// Process collected actions (tasks and skills) from the planning collector.
	// Use a background-derived context because the HTTP request context (ctx) may
	// already be canceled if the user navigated away during streaming.  The LLM
	// streaming itself is resilient (uses streamCtx), but the post-processing code
	// was incorrectly using ctx, causing CreateTempDocument to fail silently.
	if collector.hasActions() {
		proposalCtx, proposalCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer proposalCancel()

		allActions := collector.getActions()

		// Separate task, skill, and tool execution actions
		var taskActions []PlanningTaskAction
		var skillActions []PlanningTaskAction
		var toolExecActions []PlanningTaskAction
		for _, a := range allActions {
			switch a.Action {
			case "create_project_skill", "update_project_skill":
				skillActions = append(skillActions, a)
			case "execute_custom_tool":
				toolExecActions = append(toolExecActions, a)
			default:
				taskActions = append(taskActions, a)
			}
		}

		// Process task actions (existing logic)
		if len(taskActions) > 0 {
			actions := taskActions
			projectID := actions[0].ProjectID

			// Auto-assign sequential sort_order for create actions that have sort_order=0
			createIdx := 0
			for i := range actions {
				if actions[i].Action == "create" && actions[i].SortOrder == 0 {
					createIdx++
					actions[i].SortOrder = createIdx
				}
			}

			project, err := h.api.db.GetProject(proposalCtx, projectID)
			projectName := "Project"
			if err == nil {
				projectName = project.Name
			}

			// Build summary
			var summaryParts []string
			createCount, updateCount, deleteCount := 0, 0, 0
			for _, a := range actions {
				switch a.Action {
				case "create":
					createCount++
				case "update":
					updateCount++
				case "delete":
					deleteCount++
				}
			}
			if createCount > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("create %d task(s)", createCount))
			}
			if updateCount > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("update %d task(s)", updateCount))
			}
			if deleteCount > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("delete %d task(s)", deleteCount))
			}

			// Build markdown content for preview — group subtasks under parents
			var contentBuilder strings.Builder

			// Identify parent (no parent_id) vs subtask actions for create
			type indexedAction struct {
				idx    int
				action PlanningTaskAction
			}
			var parentActions []indexedAction
			childActions := map[int][]indexedAction{} // parent sort_order -> children
			var otherActions []indexedAction          // update, delete actions

			for i, a := range actions {
				switch a.Action {
				case "create":
					if a.ParentRef > 0 {
						parentIdx := a.ParentRef - 1
						childActions[parentIdx] = append(childActions[parentIdx], indexedAction{i, a})
					} else if a.ParentID > 0 {
						parentIdx := -1
						for j, p := range actions {
							if p.Action == "create" && j < i {
								parentIdx = j
							}
						}
						if parentIdx >= 0 {
							childActions[parentIdx] = append(childActions[parentIdx], indexedAction{i, a})
						} else {
							parentActions = append(parentActions, indexedAction{i, a})
						}
					} else {
						parentActions = append(parentActions, indexedAction{i, a})
					}
				default:
					otherActions = append(otherActions, indexedAction{i, a})
				}
			}

			num := 0
			for _, pa := range parentActions {
				a := pa.action
				num++
				orderLabel := ""
				if a.SortOrder > 0 {
					orderLabel = fmt.Sprintf(" (Order: %d)", a.SortOrder)
				}
				contentBuilder.WriteString(fmt.Sprintf("### %d. Create task: %s%s\n", num, a.Title, orderLabel))
				if a.Description != "" {
					contentBuilder.WriteString(fmt.Sprintf("**Description:** %s\n", a.Description))
				}
				contentBuilder.WriteString(fmt.Sprintf("**Priority:** %s | **Status:** %s\n", a.Priority, a.Status))
				if a.DueDate != "" {
					contentBuilder.WriteString(fmt.Sprintf("**Due date:** %s\n", a.DueDate))
				}

				if children, ok := childActions[pa.idx]; ok {
					contentBuilder.WriteString("\n**Subtasks:**\n")
					for subNum, ca := range children {
						contentBuilder.WriteString(fmt.Sprintf("- **%d.%d** %s", num, subNum+1, ca.action.Title))
						if ca.action.Description != "" {
							contentBuilder.WriteString(fmt.Sprintf(" — %s", ca.action.Description))
						}
						contentBuilder.WriteString("\n")
					}
				}
				contentBuilder.WriteString("\n")
			}

			for _, oa := range otherActions {
				a := oa.action
				num++
				switch a.Action {
				case "update":
					contentBuilder.WriteString(fmt.Sprintf("### %d. Update task #%d: %s\n", num, a.TaskID, a.Title))
					if a.Description != "" {
						contentBuilder.WriteString(fmt.Sprintf("**Description:** %s\n", a.Description))
					}
					prio := a.Priority
					if prio == "" {
						prio = "-"
					}
					stat := a.Status
					if stat == "" {
						stat = "-"
					}
					contentBuilder.WriteString(fmt.Sprintf("**Priority:** %s | **Status:** %s\n", prio, stat))
					if a.DueDate != "" {
						contentBuilder.WriteString(fmt.Sprintf("**Due date:** %s\n", a.DueDate))
					}
				case "delete":
					contentBuilder.WriteString(fmt.Sprintf("### %d. Delete task #%d: %s\n", num, a.TaskID, a.Title))
					if a.Description != "" {
						contentBuilder.WriteString(fmt.Sprintf("**Description:** %s\n", a.Description))
					}
					prio := a.Priority
					if prio == "" {
						prio = "-"
					}
					stat := a.Status
					if stat == "" {
						stat = "-"
					}
					contentBuilder.WriteString(fmt.Sprintf("**Priority:** %s | **Status:** %s\n", prio, stat))
					if a.DueDate != "" {
						contentBuilder.WriteString(fmt.Sprintf("**Due date:** %s\n", a.DueDate))
					}
				}
				contentBuilder.WriteString("\n")
			}

			docID := uuid.New().String()[:8]
			summary := fmt.Sprintf("%s — %s", strings.Join(summaryParts, ", "), projectName)

			docPrefix := "Task"
			if createCount == 0 && deleteCount == 0 && updateCount > 0 {
				docPrefix = "Update Task"
			} else if createCount == 0 && updateCount == 0 && deleteCount > 0 {
				docPrefix = "Delete Task"
			}

			docTitle := fmt.Sprintf("%s: %s", docPrefix, projectName)
			if len(actions) == 1 {
				docTitle = fmt.Sprintf("%s: %s", docPrefix, actions[0].Title)
			}

			var fullContent strings.Builder
			fullContent.WriteString(fmt.Sprintf("# Task Proposal — %s\n\n", projectName))
			fullContent.WriteString(fmt.Sprintf("**Summary:** %s\n\n", summary))
			fullContent.WriteString(contentBuilder.String())

			tempDoc := &database.TempDocument{
				ID:             docID,
				Title:          docTitle,
				Content:        fullContent.String(),
				ConversationID: sql.NullInt64{Int64: conv.ID, Valid: conv.ID > 0},
				Summary:        summary,
				MessageID:      assistantMsg.ID,
			}
			if err := h.api.db.CreateTempDocument(proposalCtx, tempDoc); err != nil {
				log.Printf("[TaskProposal] Failed to create temp document: %v", err)
			} else {
				h.api.storePendingTaskProposal(docID, actions, summary)

				if !isSessionProvider {
					safeSendSSE("doc_card", map[string]interface{}{
						"doc_id":     docID,
						"type":       "task_proposal",
						"title":      docTitle,
						"summary":    summary,
						"task_count": len(actions),
					})
				}

				sseMu.Lock()
				disconnected := sseDisconnected
				sseMu.Unlock()
				if disconnected {
					h.api.hub.BroadcastChatDocCard(map[string]interface{}{
						"doc_id":          docID,
						"type":            "task_proposal",
						"title":           docTitle,
						"summary":         summary,
						"task_count":      len(actions),
						"conversation_id": conv.ID,
					})
				}
			}
		}

		// Process skill actions (create/update project skills via approval)
		for _, sa := range skillActions {
			projectID := sa.ProjectID
			project, err := h.api.db.GetProject(proposalCtx, projectID)
			projectName := "Project"
			if err == nil {
				projectName = project.Name
			}

			skillName := sa.Title
			skillContent, _ := sa.Extra["content"].(string)
			skillCategory, _ := sa.Extra["category"].(string)

			actionLabel := "Create"
			if sa.Action == "update_project_skill" {
				actionLabel = "Update"
			}

			// Build preview content
			var contentBuilder strings.Builder
			contentBuilder.WriteString(fmt.Sprintf("# Skill Proposal — %s\n\n", projectName))
			contentBuilder.WriteString(fmt.Sprintf("**Action:** %s project skill\n", actionLabel))
			contentBuilder.WriteString(fmt.Sprintf("**Name:** %s\n", skillName))
			if skillCategory != "" {
				contentBuilder.WriteString(fmt.Sprintf("**Category:** %s\n", skillCategory))
			}
			contentBuilder.WriteString(fmt.Sprintf("\n---\n\n### Skill Content\n\n%s\n", skillContent))

			docID := uuid.New().String()[:8]
			summary := fmt.Sprintf("%s skill '%s' — %s", actionLabel, skillName, projectName)
			docTitle := fmt.Sprintf("Skill: %s", skillName)

			tempDoc := &database.TempDocument{
				ID:             docID,
				Title:          docTitle,
				Content:        contentBuilder.String(),
				ConversationID: sql.NullInt64{Int64: conv.ID, Valid: conv.ID > 0},
				Summary:        summary,
				MessageID:      assistantMsg.ID,
			}
			if err := h.api.db.CreateTempDocument(proposalCtx, tempDoc); err != nil {
				log.Printf("[SkillProposal] Failed to create temp document: %v", err)
				continue
			}

			proposal := &pendingSkillProposal{
				ProjectID: projectID,
				SkillName: skillName,
				Content:   skillContent,
				Category:  skillCategory,
			}
			if sa.Action == "create_project_skill" {
				proposal.Action = "create"
			} else {
				proposal.Action = "update"
				proposal.SkillID = sa.TaskID // TaskID reused for skill ID
			}
			h.api.storePendingSkillProposal(docID, proposal)

			if !isSessionProvider {
				safeSendSSE("doc_card", map[string]interface{}{
					"doc_id":  docID,
					"type":    "skill_proposal",
					"title":   docTitle,
					"summary": summary,
				})
			}

			sseMu.Lock()
			disconnected := sseDisconnected
			sseMu.Unlock()
			if disconnected {
				h.api.hub.BroadcastChatDocCard(map[string]interface{}{
					"doc_id":          docID,
					"type":            "skill_proposal",
					"title":           docTitle,
					"summary":         summary,
					"conversation_id": conv.ID,
				})
			}
		}

		// Process custom tool execution proposals (confirm-required tools)
		for _, ta := range toolExecActions {
			toolName, _ := ta.Extra["tool_name"].(string)
			description, _ := ta.Extra["description"].(string)
			inputParams, _ := ta.Extra["input"].(map[string]interface{})

			var contentBuilder strings.Builder
			contentBuilder.WriteString(fmt.Sprintf("# Tool Execution — %s\n\n", toolName))
			if description != "" {
				contentBuilder.WriteString(fmt.Sprintf("**Description:** %s\n\n", description))
			}
			if len(inputParams) > 0 {
				contentBuilder.WriteString("**Parameters:**\n")
				for k, v := range inputParams {
					contentBuilder.WriteString(fmt.Sprintf("- **%s:** %v\n", k, v))
				}
			}

			docID := uuid.New().String()[:8]
			summary := fmt.Sprintf("Run '%s'", toolName)
			docTitle := fmt.Sprintf("Tool: %s", toolName)

			tempDoc := &database.TempDocument{
				ID:             docID,
				Title:          docTitle,
				Content:        contentBuilder.String(),
				ConversationID: sql.NullInt64{Int64: conv.ID, Valid: conv.ID > 0},
				Summary:        summary,
				MessageID:      assistantMsg.ID,
			}
			if err := h.api.db.CreateTempDocument(proposalCtx, tempDoc); err != nil {
				log.Printf("[ToolProposal] Failed to create temp document: %v", err)
				continue
			}

			h.api.storePendingToolProposal(docID, &pendingToolProposal{
				Action:         ta,
				ConversationID: conv.ID,
			})

			if !isSessionProvider {
				safeSendSSE("doc_card", map[string]interface{}{
					"doc_id":  docID,
					"type":    "tool_proposal",
					"title":   docTitle,
					"summary": summary,
				})
			}

			sseMu.Lock()
			disconnected := sseDisconnected
			sseMu.Unlock()
			if disconnected {
				h.api.hub.BroadcastChatDocCard(map[string]interface{}{
					"doc_id":          docID,
					"type":            "tool_proposal",
					"title":           docTitle,
					"summary":         summary,
					"conversation_id": conv.ID,
				})
			}
		}
	}

	// Emit doc_card SSE events for TempDocuments created during this stream.
	// - Session providers: emit ALL cards (tool loop is skipped, this is the only source)
	// - Non-session providers: only emit "file" cards (other types are already emitted
	//   inline by buildDocCard and the collector code above)
	{
		docCtx, docCancel := context.WithTimeout(context.Background(), 5*time.Second)
		docs, _ := h.api.db.ListTempDocumentsByConversation(docCtx, conv.ID)
		docCancel()
		for _, doc := range docs {
			// Only emit cards for documents created after the assistant message started
			if assistantMsg != nil && doc.CreatedAt.Before(assistantMsg.CreatedAt) {
				continue
			}
			cardType := "document"
			if strings.HasPrefix(doc.Title, "Memory Doc:") {
				cardType = "proposal"
			} else if strings.HasPrefix(doc.Title, "Task:") || strings.HasPrefix(doc.Title, "Planning:") ||
				strings.HasPrefix(doc.Title, "Update Task:") || strings.HasPrefix(doc.Title, "Delete Task:") {
				cardType = "task_proposal"
			} else if strings.HasPrefix(doc.Title, "Tool:") {
				cardType = "tool_proposal"
			} else if strings.HasPrefix(doc.Title, "Skill:") {
				cardType = "skill_proposal"
			} else if strings.HasPrefix(doc.Title, "File:") {
				cardType = "file"
			}

			// Non-session providers already emit non-file cards inline; skip to avoid duplicates
			if !isSessionProvider && cardType != "file" {
				continue
			}

			card := map[string]interface{}{
				"doc_id":  doc.ID,
				"type":    cardType,
				"title":   doc.Title,
				"summary": doc.Summary,
				"status":  doc.Status,
			}
			if cardType == "file" {
				card["title"] = strings.TrimPrefix(doc.Title, "File:")
				var meta map[string]interface{}
				if err := json.Unmarshal([]byte(doc.Content), &meta); err == nil {
					card["project_id"] = meta["project_id"]
					card["path"] = meta["path"]
				}
			}
			safeSendSSE("doc_card", card)
		}
	}

	// Record token usage — prefer model from response (actual model used)
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dbCancel()
	var costUSD float64
	if totalUsage.InputTokens > 0 || totalUsage.OutputTokens > 0 {
		usageModel := model
		if resp != nil && resp.Model != "" {
			usageModel = resp.Model
		}
		if usageModel == "" {
			usageModel = "unknown"
		}
		costUSD = llm.CalculateCost(usageModel, totalUsage.InputTokens, totalUsage.OutputTokens)
		h.api.db.CreateTokenUsage(dbCtx, &database.TokenUsage{
			Source:              "ai_assistant",
			Subcategory:         "chat",
			ConversationID:      sql.NullInt64{Int64: conv.ID, Valid: true},
			Model:               usageModel,
			InputTokens:         totalUsage.InputTokens,
			OutputTokens:        totalUsage.OutputTokens,
			CacheReadTokens:     totalUsage.CacheReadTokens,
			CacheCreationTokens: totalUsage.CacheCreationTokens,
			CostUSD:             costUSD,
		})
	}

	// Persist Claude Code session ID for resume across server restarts
	if resp != nil && resp.SessionID != "" && resp.SessionID != conv.SessionID {
		if err := h.api.db.UpdateAIConversationSessionID(dbCtx, conv.ID, resp.SessionID); err != nil {
			log.Printf("[AI] Failed to persist session ID: %v", err)
		} else {
			log.Printf("[AI] Persisted session ID %s for conv %d", resp.SessionID, conv.ID)
		}
	}

	safeSendSSE("done", map[string]interface{}{
		"conversation_id": conv.ID,
		"usage": map[string]interface{}{
			"input_tokens":  totalUsage.InputTokens,
			"output_tokens": totalUsage.OutputTokens,
			"total_tokens":  totalUsage.InputTokens + totalUsage.OutputTokens,
			"cost_usd":      costUSD,
		},
	})
}

// streamFlusher periodically persists streaming content to the database.
type streamFlusher struct {
	persist      func(context.Context, string, string) error
	getText      func() string
	getToolCalls func() string
	lastFlushed  int
	ticker       *time.Ticker
	done         chan struct{}
	mu           sync.Mutex
}

func newStreamFlusher(persist func(context.Context, string, string) error, getText func() string, getToolCalls func() string) *streamFlusher {
	f := &streamFlusher{
		persist:      persist,
		getText:      getText,
		getToolCalls: getToolCalls,
		ticker:       time.NewTicker(2 * time.Second),
		done:         make(chan struct{}),
	}
	go f.run()
	return f
}

func (f *streamFlusher) run() {
	for {
		select {
		case <-f.ticker.C:
			f.flush()
		case <-f.done:
			return
		}
	}
}

func (f *streamFlusher) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	content := f.getText()
	if len(content) == f.lastFlushed {
		return
	}
	f.lastFlushed = len(content)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if f.persist != nil {
		if err := f.persist(ctx, content, f.getToolCalls()); err != nil {
			log.Printf("[AI] Failed to persist streaming message progress: %v", err)
		}
	}
}

func (f *streamFlusher) stop() {
	f.ticker.Stop()
	close(f.done)
	f.flush()
}

// handleToolLoop executes tool use requests and continues the conversation.
// Returns accumulated token usage from all iterations.
func (h *AIHandler) handleToolLoop(
	ctx context.Context,
	sendSSE func(string, interface{}),
	p llm.Provider,
	req *llm.Request,
	resp *llm.Response,
	conv *database.AIConversation,
	assistantText *strings.Builder,
	toolCalls *[]map[string]interface{},
	textMu *sync.Mutex,
	maxIterations int,
	collector *planningCollector,
	messageID int64,
	authorization application.ActionAuthorization,
) (llm.Usage, error) {
	var accumulated llm.Usage

	for i := 0; i < maxIterations && resp.StopReason == "tool_use"; i++ {
		// Log iteration info
		var toolNames []string
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				toolNames = append(toolNames, fmt.Sprintf("%s(id=%s)", block.Name, block.ID))
			}
		}
		log.Printf("[ToolLoop] iteration=%d stopReason=%s tools=%v totalMsgs=%d", i, resp.StopReason, toolNames, len(req.Messages))

		// Process tool calls from the response
		var toolResults []llm.ContentBlock

		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}

			inputMap, ok := block.Input.(map[string]interface{})
			if !ok {
				inputMap = make(map[string]interface{})
			}

			textMu.Lock()
			*toolCalls = append(*toolCalls, map[string]interface{}{
				"id":    block.ID,
				"name":  block.Name,
				"input": inputMap,
			})
			textMu.Unlock()

			sendSSE("tool_executing", map[string]interface{}{
				"id":   block.ID,
				"name": block.Name,
			})

			toolCtx, toolCancel := context.WithTimeout(context.Background(), 30*time.Second)
			result, err := h.executeTool(toolCtx, block.Name, inputMap, conv.ID, messageID, collector, conv.ProactiveType, authorization)
			toolCancel()
			if err != nil {
				result = fmt.Sprintf("Error: %s", err.Error())
			}

			resultSnippet := result
			if len(resultSnippet) > 200 {
				resultSnippet = resultSnippet[:200] + "..."
			}
			inputJSON, _ := json.Marshal(inputMap)
			log.Printf("[ToolLoop] executed %s(id=%s) input=%s resultLen=%d snippet=%q", block.Name, block.ID, string(inputJSON), len(result), resultSnippet)

			sendSSE("tool_result", map[string]interface{}{
				"id":     block.ID,
				"name":   block.Name,
				"result": result,
			})

			// Emit native doc_card for document-related tools
			if card := h.buildDocCard(block.Name, result, inputMap); card != nil {
				sendSSE("doc_card", card)
			}

			// File cards for open_file are emitted via WebSocket broadcast
			// from executeTool() and persisted as TempDocuments.

			toolResults = append(toolResults, llm.ContentBlock{
				Type:      "tool_result",
				ToolUseID: block.ID,
				Content:   result,
			})
		}

		// Add the assistant's response and tool results to messages
		req.Messages = append(req.Messages, llm.Message{
			Role:    "assistant",
			Content: resp.Content,
		})
		req.Messages = append(req.Messages, llm.Message{
			Role:    "user",
			Content: toolResults,
		})

		log.Printf("[ToolLoop] sending %d tool results back to model, total messages now: %d", len(toolResults), len(req.Messages))

		// Continue streaming
		var err error
		log.Printf("[AI-AUDIT] CALL_START subcategory=chat_tool_loop iteration=%d", i)
		resp, err = p.StreamMessage(ctx, req, func(event llm.StreamEvent) error {
			if event.Type == "content_block_start" && event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				sendSSE("tool_start", map[string]interface{}{
					"id":   event.ContentBlock.ID,
					"name": event.ContentBlock.Name,
				})
			}
			if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
				sendSSE("text", map[string]interface{}{
					"text": event.Delta.Text,
				})
				textMu.Lock()
				assistantText.WriteString(event.Delta.Text)
				textMu.Unlock()
			}
			return nil
		})

		if err != nil {
			log.Printf("[ToolLoop] StreamMessage error at iteration %d: %v", i, err)
			log.Printf("[AI-AUDIT] CALL_FAIL subcategory=chat_tool_loop iteration=%d error=%v", i, err)
			return accumulated, err
		}
		log.Printf("[AI-AUDIT] CALL_OK subcategory=chat_tool_loop iteration=%d", i)

		// Log model's response after receiving tool results
		if resp != nil {
			var nextTools []string
			for _, b := range resp.Content {
				if b.Type == "tool_use" {
					nextTools = append(nextTools, b.Name)
				}
			}
			log.Printf("[ToolLoop] model response after iteration %d: stopReason=%s nextTools=%v contentBlocks=%d", i, resp.StopReason, nextTools, len(resp.Content))
		}

		// Accumulate token usage from this iteration
		if resp != nil {
			accumulated.InputTokens += resp.Usage.InputTokens
			accumulated.OutputTokens += resp.Usage.OutputTokens
		}
	}

	log.Printf("[ToolLoop] finished after iterations, final stopReason=%s", resp.StopReason)
	return accumulated, nil
}

// ExecuteTool implements llm.ToolExecutor. It wraps the private executeTool method
// so that SDK providers can call OpenPoet tools from their in-process MCP servers.
func (h *AIHandler) ExecuteTool(ctx context.Context, name string, input map[string]any, conversationID int64) (string, error) {
	// This interface is called only by the configured in-process SDK provider.
	// It has no payload-controlled actor or approver; the local UI boundary is
	// therefore fixed here, while Automation uses ExecuteToolAuthorized below.
	return h.ExecuteToolAuthorized(ctx, name, input, conversationID, platformUIAuthorization(nil))
}

func (h *AIHandler) ExecuteToolAuthorized(ctx context.Context, name string, input map[string]any, conversationID int64, authorization application.ActionAuthorization) (string, error) {
	// Look up the current streaming message ID for this conversation
	h.streamMessageIDsMu.Lock()
	msgID := h.streamMessageIDs[conversationID]
	h.streamMessageIDsMu.Unlock()

	// Look up planning collector for this conversation (if in planning mode)
	h.streamCollectorsMu.Lock()
	collector := h.streamCollectors[conversationID]
	h.streamCollectorsMu.Unlock()

	// Look up proactive type for this conversation
	h.streamProactiveTypesMu.Lock()
	proactiveType := h.streamProactiveTypes[conversationID]
	h.streamProactiveTypesMu.Unlock()

	return h.executeTool(ctx, name, input, conversationID, msgID, collector, proactiveType, authorization)
}

// executeTool runs a tool and returns the result as a string.
func (h *AIHandler) executeTool(ctx context.Context, name string, input map[string]interface{}, conversationID int64, messageID int64, collector *planningCollector, proactiveType string, authorization application.ActionAuthorization) (string, error) {
	// Agent project filter enforcement: block access to projects not in the agent's filter
	if pidRaw, ok := input["project_id"]; ok && conversationID > 0 {
		if allowed := h.getAgentAllowedProjectIDs(ctx, conversationID); allowed != nil {
			pid, _ := parseIDParam(input, "project_id")
			if pid > 0 && !allowed[pid] {
				log.Printf("[AI-Agent] BLOCKED tool %q: project %d not in allowed set for conv %d", name, pid, conversationID)
				return "", fmt.Errorf("this agent does not have access to project %d", pid)
			}
			// If project_id is present but couldn't be parsed, block by default
			if pid == 0 && pidRaw != nil {
				log.Printf("[AI-Agent] BLOCKED tool %q: unparseable project_id %v for conv %d", name, pidRaw, conversationID)
				return "", fmt.Errorf("this agent does not have access to the requested project")
			}
		}
	}

	switch name {
	case "create_skill":
		skillName, _ := input["name"].(string)
		content, _ := input["content"].(string)
		category, _ := input["category"].(string)

		if skillName == "" || content == "" {
			return "", fmt.Errorf("name and content are required")
		}

		// In skill_customization context, intercept and collect for approval
		if proactiveType == "skill_customization" && collector != nil {
			projectID := h.getProjectIDFromConversation(ctx, conversationID)
			if projectID > 0 {
				collector.add(PlanningTaskAction{
					Action:    "create_project_skill",
					ProjectID: projectID,
					Title:     skillName,
					Extra: map[string]interface{}{
						"content":  content,
						"category": category,
					},
				})
				return fmt.Sprintf(
					"IMPORTANT: Skill '%s' has NOT been created yet — it will be applied after user approval. "+
						"Do NOT say the skill was created. Inform the user that the proposal has been registered and they can review it in the card below.", skillName), nil
			}
		}

		skill := &database.Skill{
			Name:     skillName,
			Content:  content,
			Enabled:  true,
			Category: category,
		}
		if err := h.api.db.CreateSkill(ctx, skill); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "created", "skill": skill})
		return fmt.Sprintf("Skill '%s' created (ID: %d)", skillName, skill.ID), nil

	case "update_skill":
		idStr, _ := input["id"].(string)
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			// Try float (JSON numbers)
			if idF, ok := input["id"].(float64); ok {
				id = int64(idF)
			} else {
				return "", fmt.Errorf("invalid skill ID")
			}
		}

		// In skill_customization context, intercept and collect for approval
		if proactiveType == "skill_customization" && collector != nil {
			skillName, _ := input["name"].(string)
			content, _ := input["content"].(string)
			category, _ := input["category"].(string)
			projectID := h.getProjectIDFromConversation(ctx, conversationID)
			if projectID > 0 {
				collector.add(PlanningTaskAction{
					Action:    "update_project_skill",
					ProjectID: projectID,
					TaskID:    id, // reuse TaskID field for skill ID
					Title:     skillName,
					Extra: map[string]interface{}{
						"content":  content,
						"category": category,
					},
				})
				displayName := skillName
				if displayName == "" {
					displayName = fmt.Sprintf("#%d", id)
				}
				return fmt.Sprintf(
					"IMPORTANT: Skill '%s' has NOT been updated yet — it will be applied after user approval. "+
						"Do NOT say the skill was updated. Inform the user that the proposal has been registered and they can review it in the card below.", displayName), nil
			}
		}

		skill, err := h.api.db.GetSkill(ctx, id)
		if err != nil {
			return "", fmt.Errorf("skill not found")
		}

		if n, ok := input["name"].(string); ok && n != "" {
			skill.Name = n
		}
		if c, ok := input["content"].(string); ok && c != "" {
			skill.Content = c
		}
		if cat, ok := input["category"].(string); ok {
			skill.Category = cat
		}

		if err := h.api.db.UpdateSkill(ctx, skill); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "updated", "skill": skill})
		return fmt.Sprintf("Skill '%s' updated", skill.Name), nil

	case "delete_skill":
		idStr, _ := input["id"].(string)
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			if idF, ok := input["id"].(float64); ok {
				id = int64(idF)
			} else {
				return "", fmt.Errorf("invalid skill ID")
			}
		}

		skill, err := h.api.db.GetSkill(ctx, id)
		if err != nil {
			return "", fmt.Errorf("skill not found")
		}
		name := skill.Name

		if err := h.api.db.DeleteSkill(ctx, id); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "deleted", "id": id})
		return fmt.Sprintf("Skill '%s' deleted", name), nil

	case "get_skill":
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", err
		}
		skill, err := h.api.db.GetSkill(ctx, id)
		if err != nil {
			return "", fmt.Errorf("skill not found")
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Name: %s\n", skill.Name))
		sb.WriteString(fmt.Sprintf("Category: %s\n", skill.Category))
		sb.WriteString(fmt.Sprintf("Enabled: %v\n", skill.Enabled))
		sb.WriteString(fmt.Sprintf("---\n%s", skill.Content))
		return sb.String(), nil

	case "list_skills":
		skills, err := h.api.db.ListSkills(ctx)
		if err != nil {
			return "", err
		}
		if len(skills) == 0 {
			return "No skills found.", nil
		}
		var sb strings.Builder
		for _, s := range skills {
			status := "enabled"
			if !s.Enabled {
				status = "disabled"
			}
			sb.WriteString(fmt.Sprintf("- [%d] %s (%s, %s)\n", s.ID, s.Name, s.Category, status))
		}
		return sb.String(), nil

	case "create_project_skill":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		skillName, _ := input["name"].(string)
		content, _ := input["content"].(string)
		category, _ := input["category"].(string)

		if skillName == "" || content == "" {
			return "", fmt.Errorf("name and content are required")
		}

		ps := &database.ProjectSkill{
			ProjectID: projectID,
			Name:      skillName,
			Content:   content,
			Enabled:   true,
			Category:  category,
		}
		if err := h.api.db.CreateProjectSkill(ctx, ps); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{
			"action": "created", "project_id": projectID, "skill": ps,
		})
		return fmt.Sprintf("Project skill '%s' created (ID: %d) for project %d", skillName, ps.ID, projectID), nil

	case "update_project_skill":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", fmt.Errorf("invalid project skill ID")
		}

		ps, err := h.api.db.GetProjectSkill(ctx, id)
		if err != nil {
			return "", fmt.Errorf("project skill not found")
		}
		if ps.ProjectID != projectID {
			return "", fmt.Errorf("project skill not found in this project")
		}

		if n, ok := input["name"].(string); ok && n != "" {
			ps.Name = n
		}
		if c, ok := input["content"].(string); ok && c != "" {
			ps.Content = c
		}
		if cat, ok := input["category"].(string); ok {
			ps.Category = cat
		}
		if enabled, ok := input["enabled"].(bool); ok {
			ps.Enabled = enabled
		}

		if err := h.api.db.UpdateProjectSkill(ctx, ps); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{
			"action": "updated", "project_id": projectID, "skill": ps,
		})
		return fmt.Sprintf("Project skill '%s' updated", ps.Name), nil

	case "delete_project_skill":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", fmt.Errorf("invalid project skill ID")
		}

		ps, err := h.api.db.GetProjectSkill(ctx, id)
		if err != nil {
			return "", fmt.Errorf("project skill not found")
		}
		if ps.ProjectID != projectID {
			return "", fmt.Errorf("project skill not found in this project")
		}
		name := ps.Name

		if err := h.api.db.DeleteProjectSkill(ctx, id); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{
			"action": "deleted", "project_id": projectID, "id": id,
		})
		return fmt.Sprintf("Project skill '%s' deleted", name), nil

	case "get_project_skill":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", fmt.Errorf("invalid project skill ID")
		}
		ps, err := h.api.db.GetProjectSkill(ctx, id)
		if err != nil {
			return "", fmt.Errorf("project skill not found")
		}
		if ps.ProjectID != projectID {
			return "", fmt.Errorf("project skill not found in this project")
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Name: %s\n", ps.Name))
		sb.WriteString(fmt.Sprintf("Category: %s\n", ps.Category))
		sb.WriteString(fmt.Sprintf("Enabled: %v\n", ps.Enabled))
		sb.WriteString(fmt.Sprintf("---\n%s", ps.Content))
		return sb.String(), nil

	case "list_project_skills":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}

		globalSkills, err := h.api.db.ListSkills(ctx)
		if err != nil {
			return "", err
		}

		projectSkills, err := h.api.db.ListProjectSkills(ctx, projectID)
		if err != nil {
			return "", err
		}

		var sb strings.Builder
		if len(globalSkills) > 0 {
			sb.WriteString("Global skills:\n")
			for _, s := range globalSkills {
				status := "enabled"
				if !s.Enabled {
					status = "disabled"
				}
				sb.WriteString(fmt.Sprintf("- [%d] %s (%s, %s)\n", s.ID, s.Name, s.Category, status))
			}
		}
		if len(projectSkills) > 0 {
			sb.WriteString("\nProject-specific skills:\n")
			for _, ps := range projectSkills {
				status := "enabled"
				if !ps.Enabled {
					status = "disabled"
				}
				sb.WriteString(fmt.Sprintf("- [%d] %s (%s, %s)\n", ps.ID, ps.Name, ps.Category, status))
			}
		}
		if sb.Len() == 0 {
			return "No skills found for this project.", nil
		}
		return sb.String(), nil

	case "list_projects":
		projects, err := h.api.db.ListProjects(ctx)
		if err != nil {
			return "", err
		}
		// Apply agent project filter
		if allowed := h.getAgentAllowedProjectIDs(ctx, conversationID); allowed != nil {
			var filtered []database.Project
			for _, p := range projects {
				if allowed[p.ID] {
					filtered = append(filtered, p)
				}
			}
			projects = filtered
		}
		if len(projects) == 0 {
			return "No projects found.", nil
		}
		var sb strings.Builder
		for _, p := range projects {
			sb.WriteString(fmt.Sprintf("- [%d] %s (%s: %s)\n", p.ID, p.Name, p.Type, p.Path))
		}
		return sb.String(), nil

	case "get_mcp_server":
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		servers, err := services.Configuration.MCP.ListGlobal(ctx)
		if err != nil {
			return "", err
		}
		for _, server := range servers {
			if server.ID == id {
				return fmt.Sprintf("Name: %s\nEnabled: %v\nHas command: %v\nHas args: %v\nHas env: %v", server.Name, server.Enabled, server.HasCommand, server.HasArgs, server.HasEnv), nil
			}
		}
		return "", fmt.Errorf("MCP server not found")

	case "list_mcp_servers":
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		servers, err := services.Configuration.MCP.ListGlobal(ctx)
		if err != nil {
			return "", err
		}
		if len(servers) == 0 {
			return "No MCP servers found.", nil
		}
		var output strings.Builder
		for _, server := range servers {
			status := "enabled"
			if !server.Enabled {
				status = "disabled"
			}
			output.WriteString(fmt.Sprintf("- [%d] %s (%s)\n", server.ID, server.Name, status))
		}
		return output.String(), nil

	case "create_mcp_server":
		name, _ := input["name"].(string)
		command, _ := input["command"].(string)
		args, _ := input["args"].(string)
		env, _ := input["env"].(string)
		if args == "" {
			args = "[]"
		}
		if env == "" {
			env = "{}"
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		server, err := services.Configuration.MCP.CreateGlobal(ctx, authorization, application.MCPServerInput{
			Name: name, Command: command, Args: args, Env: env, Enabled: true,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server '%s' created (ID: %d)", server.Name, server.ID), nil

	case "create_project_mcp_server":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		name, _ := input["name"].(string)
		command, _ := input["command"].(string)
		args, _ := input["args"].(string)
		env, _ := input["env"].(string)
		if args == "" {
			args = "[]"
		}
		if env == "" {
			env = "{}"
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		server, err := services.Configuration.MCP.CreateProject(ctx, authorization, projectID, application.MCPServerInput{
			Name: name, Command: command, Args: args, Env: env, Enabled: true,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project MCP server '%s' created (ID: %d) for project %d", server.Name, server.ID, projectID), nil

	case "update_project_mcp_server":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", fmt.Errorf("invalid project MCP server ID")
		}
		command := application.UpdateMCPServerCommand{ID: id}
		if value, exists := input["name"].(string); exists && value != "" {
			command.Name = &value
		}
		if value, exists := input["command"].(string); exists && value != "" {
			command.Command = &value
		}
		if value, exists := input["args"].(string); exists {
			command.Args = &value
		}
		if value, exists := input["env"].(string); exists {
			command.Env = &value
		}
		if value, exists := input["enabled"].(bool); exists {
			command.Enabled = &value
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		server, err := services.Configuration.MCP.UpdateProject(ctx, authorization, projectID, command)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project MCP server '%s' updated", server.Name), nil

	case "delete_project_mcp_server":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", fmt.Errorf("invalid project MCP server ID")
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		if err := services.Configuration.MCP.DeleteProject(ctx, authorization, projectID, id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Project MCP server %d deleted", id), nil

	case "get_project_mcp_server":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		id, err := parseIDParam(input, "id")
		if err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		servers, err := services.Configuration.MCP.ListProject(ctx, projectID)
		if err != nil {
			return "", err
		}
		for _, server := range servers {
			if server.ID == id {
				return fmt.Sprintf("Name: %s\nEnabled: %v\nHas command: %v\nHas args: %v\nHas env: %v", server.Name, server.Enabled, server.HasCommand, server.HasArgs, server.HasEnv), nil
			}
		}
		return "", fmt.Errorf("project MCP server not found")

	case "list_project_mcp_servers":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.MCP == nil {
			return "", errors.New("MCP application service unavailable")
		}
		globalServers, err := services.Configuration.MCP.ListGlobal(ctx)
		if err != nil {
			return "", err
		}
		projectServers, err := services.Configuration.MCP.ListProject(ctx, projectID)
		if err != nil {
			return "", err
		}
		var output strings.Builder
		if len(globalServers) > 0 {
			output.WriteString("Global MCP servers:\n")
			for _, server := range globalServers {
				output.WriteString(fmt.Sprintf("- [%d] %s (enabled=%v)\n", server.ID, server.Name, server.Enabled))
			}
		}
		if len(projectServers) > 0 {
			output.WriteString("\nProject-specific MCP servers:\n")
			for _, server := range projectServers {
				output.WriteString(fmt.Sprintf("- [%d] %s (enabled=%v)\n", server.ID, server.Name, server.Enabled))
			}
		}
		if output.Len() == 0 {
			return "No MCP servers found for this project.", nil
		}
		return output.String(), nil

	case "update_setting":
		key, _ := input["key"].(string)
		value, _ := input["value"].(string)

		if key == "" {
			return "", fmt.Errorf("key is required")
		}

		// Block sensitive settings
		if key == "vapid_private_key" {
			return "", fmt.Errorf("cannot modify this setting")
		}

		if err := h.api.db.SetSetting(ctx, key, value); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("settings", map[string]interface{}{"action": "updated"})
		return fmt.Sprintf("Setting '%s' updated", key), nil

	case "sync_config":
		if h.api.configSync == nil {
			return "", fmt.Errorf("config sync not available")
		}
		if err := h.api.configSync.SyncAllProjects(ctx); err != nil {
			return "", err
		}
		return "Configuration synced to all projects.", nil

	case "get_memory_doc":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		doc, err := h.api.db.GetMemoryDoc(ctx, projectID)
		if err != nil {
			return fmt.Sprintf("No memory doc exists for project %d yet. Sync the project to load its CLAUDE.md.", projectID), nil
		}

		// Create temp document so user can view in the doc viewer
		docID := uuid.New().String()[:8]
		project, _ := h.api.db.GetProject(ctx, projectID)
		projectName := fmt.Sprintf("Project %d", projectID)
		if project != nil {
			projectName = project.Name
		}
		tempDoc := &database.TempDocument{
			ID:        docID,
			Title:     fmt.Sprintf("Memory Doc: %s (v%d)", projectName, doc.Version),
			Content:   doc.Content,
			MessageID: messageID,
		}
		if err := h.api.db.CreateTempDocument(ctx, tempDoc); err != nil {
			// Fallback: return link to project page if temp doc creation fails
			return fmt.Sprintf("Memory doc loaded (v%d). [Ver Memory Doc: %s](/app/project/%d)", doc.Version, projectName, projectID), nil
		}

		// Return viewer link + internal-only content for AI editing reference
		// The doc_card SSE event is sent automatically by handleToolLoop
		return fmt.Sprintf(
			"Project %s memory doc loaded (v%d). A 'View Document' button was automatically displayed in chat. Do NOT generate links.\n\n"+
				"<internal_reference>\n%s\n</internal_reference>\n\nLink interno: /app/doc/%s",
			projectName, doc.Version, doc.Content, docID,
		), nil

	case "update_memory_doc":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		content, _ := input["content"].(string)
		if content == "" {
			return "", fmt.Errorf("content is required")
		}
		summary, _ := input["summary"].(string)

		// Verify project exists
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}

		// Create a temp document for user review instead of saving directly
		docID := uuid.New().String()[:8]
		tempDoc := &database.TempDocument{
			ID:             docID,
			Title:          fmt.Sprintf("Memory Doc: %s", project.Name),
			Content:        content,
			ConversationID: sql.NullInt64{Int64: conversationID, Valid: conversationID > 0},
			Summary:        summary,
			MessageID:      messageID,
		}
		if err := h.api.db.CreateTempDocument(ctx, tempDoc); err != nil {
			return "", err
		}

		// Store pending approval metadata
		h.api.storePendingMemoryDoc(docID, projectID, content, summary)

		return fmt.Sprintf(
			"IMPORTANT: The content has NOT been saved yet. A preview was created for the user to review.\n"+
				"A 'Review Proposal' button was automatically displayed in chat.\n"+
				"Do NOT say the change was made — it AWAITS user approval.\n"+
				"Inform the user that the proposal for project %s is available for review.\n"+
				"Proposed changes: %s\n"+
				"Link interno: /app/doc/%s",
			project.Name, summary, docID), nil

	case "list_tasks":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		tasks, err := h.api.db.ListTasksByProject(ctx, projectID)
		if err != nil {
			return "", err
		}
		database.ApplyUmbrellaStatus(tasks)
		if len(tasks) == 0 {
			return fmt.Sprintf("No tasks found for project %d.", projectID), nil
		}
		statusIcons := map[string]string{"todo": "[ ]", "in_progress": "[~]", "done": "[x]", "awaiting_approval": "[?]"}
		priorityLabels := map[string]string{"low": "LOW", "medium": "MED", "high": "HIGH", "urgent": "URG"}
		var sb strings.Builder
		for _, t := range tasks {
			icon := statusIcons[t.Status]
			prio := priorityLabels[t.Priority]
			indent := ""
			if t.ParentID.Valid {
				indent = "  "
			}
			due := ""
			if t.DueDate.Valid {
				due = " | due: " + t.DueDate.Time.Format("2006-01-02 15:04")
				if t.DueDate.Time.Before(time.Now()) && t.Status != "done" {
					due += " OVERDUE"
				}
			}
			sb.WriteString(fmt.Sprintf("%s%s [%d] %s (%s%s)\n", indent, icon, t.ID, t.Title, prio, due))
		}
		return sb.String(), nil

	case "get_task":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		taskID, err := parseIDParam(input, "task_id")
		if err != nil {
			return "", err
		}

		task, err := h.api.db.GetTask(ctx, taskID)
		if err != nil || task.ProjectID != projectID {
			return "", fmt.Errorf("task not found")
		}

		// Fetch all tasks to compute umbrella status and list subtasks
		allTasks, err := h.api.db.ListTasksByProject(ctx, projectID)
		if err == nil {
			database.ApplyUmbrellaStatus(allTasks)
			for _, t := range allTasks {
				if t.ID == task.ID {
					task.Status = t.Status
					break
				}
			}
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Task #%d: %s\n", task.ID, task.Title))
		sb.WriteString(fmt.Sprintf("Status: %s\n", task.Status))
		sb.WriteString(fmt.Sprintf("Priority: %s\n", task.Priority))
		if task.Description != "" {
			sb.WriteString(fmt.Sprintf("Description: %s\n", task.Description))
		}
		if task.DueDate.Valid {
			due := task.DueDate.Time.Format("2006-01-02 15:04")
			if task.DueDate.Time.Before(time.Now()) && task.Status != "done" {
				due += " OVERDUE"
			}
			sb.WriteString(fmt.Sprintf("Due: %s\n", due))
		}
		if task.ParentID.Valid {
			sb.WriteString(fmt.Sprintf("Parent task: #%d\n", task.ParentID.Int64))
		}

		// List subtasks if any
		if allTasks != nil {
			var subtasks []string
			for _, t := range allTasks {
				if t.ParentID.Valid && t.ParentID.Int64 == task.ID {
					subtasks = append(subtasks, fmt.Sprintf("#%d %s (%s)", t.ID, t.Title, t.Status))
				}
			}
			if len(subtasks) > 0 {
				sb.WriteString("Subtasks:\n")
				for _, s := range subtasks {
					sb.WriteString(fmt.Sprintf("  - %s\n", s))
				}
			}
		}

		sb.WriteString(fmt.Sprintf("Created: %s\n", task.CreatedAt.Format("2006-01-02 15:04")))
		sb.WriteString(fmt.Sprintf("Updated: %s\n", task.UpdatedAt.Format("2006-01-02 15:04")))
		return sb.String(), nil

	case "create_task":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		title, _ := input["title"].(string)
		if title == "" {
			return "", fmt.Errorf("title is required")
		}
		description, _ := input["description"].(string)
		status, _ := input["status"].(string)
		if status == "" {
			status = "todo"
		}
		priority, _ := input["priority"].(string)
		if priority == "" {
			priority = "medium"
		}
		dueDateStr, _ := input["due_date"].(string)

		var parentID int64
		if parentIDStr, ok := input["parent_id"].(string); ok && parentIDStr != "" {
			pid, err := strconv.ParseInt(parentIDStr, 10, 64)
			if err == nil {
				parentID = pid
			}
		} else if pidF, ok := input["parent_id"].(float64); ok {
			parentID = int64(pidF)
		}

		var parentRef int
		if prStr, ok := input["parent_ref"].(string); ok && prStr != "" {
			pr, err := strconv.Atoi(prStr)
			if err == nil {
				parentRef = pr
			}
		} else if prF, ok := input["parent_ref"].(float64); ok {
			parentRef = int(prF)
		}

		var sortOrder int
		if soStr, ok := input["sort_order"].(string); ok && soStr != "" {
			so, err := strconv.Atoi(soStr)
			if err == nil {
				sortOrder = so
			}
		} else if soF, ok := input["sort_order"].(float64); ok {
			sortOrder = int(soF)
		}

		// Collect task actions into the batch collector (if available).
		// A single proposal card with all tasks is created after streaming completes.
		if collector != nil {
			collector.add(PlanningTaskAction{
				Action:      "create",
				ProjectID:   projectID,
				Title:       title,
				Description: description,
				Status:      status,
				Priority:    priority,
				DueDate:     dueDateStr,
				ParentID:    parentID,
				ParentRef:   parentRef,
				SortOrder:   sortOrder,
			})
			return fmt.Sprintf(
				"IMPORTANT: Task '%s' has NOT been created yet — it will be created after user approval. "+
					"Do NOT say the task was created.", title), nil
		}
		// Fallback: no collector available (e.g. called from HandleExecuteTool HTTP endpoint)
		return "", fmt.Errorf("task creation requires a streaming context")

	case "update_task":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		taskID, err := parseIDParam(input, "task_id")
		if err != nil {
			return "", err
		}

		task, err := h.api.db.GetTask(ctx, taskID)
		if err != nil || task.ProjectID != projectID {
			return "", fmt.Errorf("task not found")
		}

		// Detect which fields are being changed
		hasTitle := false
		hasDescription := false
		hasStatus := false
		hasPriority := false
		hasDueDate := false

		if t, ok := input["title"].(string); ok && t != "" {
			hasTitle = true
			task.Title = t
		}
		if d, ok := input["description"].(string); ok && d != task.Description {
			hasDescription = true
			task.Description = d
		}
		if s, ok := input["status"].(string); ok && s != "" {
			hasStatus = true
			task.Status = s
		}
		if p, ok := input["priority"].(string); ok && p != "" {
			hasPriority = true
			task.Priority = p
		}
		if dueDateStr, ok := input["due_date"].(string); ok {
			hasDueDate = true
			if dueDateStr == "" {
				task.DueDate = sql.NullTime{}
			} else {
				t, err := parseFlexibleTime(dueDateStr)
				if err == nil {
					task.DueDate = sql.NullTime{Time: t, Valid: true}
					task.DueNotified = false
				}
			}
		}

		// Status-only change: execute immediately, no confirmation needed
		isStatusOnly := hasStatus && !hasTitle && !hasDescription && !hasPriority && !hasDueDate
		if isStatusOnly {
			if err := h.api.db.UpdateTaskStatus(ctx, taskID, task.Status); err != nil {
				return "", err
			}
			updatedTask, _ := h.api.db.GetTask(ctx, taskID)
			if updatedTask != nil {
				h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{
					"action": "updated", "project_id": projectID, "task": updatedTask,
				})
			}
			return fmt.Sprintf("Task '%s' status updated to '%s'.", task.Title, task.Status), nil
		}

		// Content change: collect with ALL fields (merged current + new) for full card display
		if collector != nil {
			dueDate := ""
			if task.DueDate.Valid {
				dueDate = task.DueDate.Time.Format("2006-01-02 15:04")
			}
			collector.add(PlanningTaskAction{
				Action:      "update",
				ProjectID:   projectID,
				TaskID:      taskID,
				Title:       task.Title,
				Description: task.Description,
				Status:      task.Status,
				Priority:    task.Priority,
				DueDate:     dueDate,
			})
			return fmt.Sprintf(
				"IMPORTANT: Task '%s' has NOT been updated yet — it will be applied after user approval. "+
					"Do NOT say the task was updated.", task.Title), nil
		}
		// Fallback: no collector available
		return "", fmt.Errorf("task update requires a streaming context")

	case "delete_task":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		taskID, err := parseIDParam(input, "task_id")
		if err != nil {
			return "", err
		}

		task, err := h.api.db.GetTask(ctx, taskID)
		if err != nil || task.ProjectID != projectID {
			return "", fmt.Errorf("task not found")
		}

		if collector != nil {
			dueDate := ""
			if task.DueDate.Valid {
				dueDate = task.DueDate.Time.Format("2006-01-02 15:04")
			}
			collector.add(PlanningTaskAction{
				Action:      "delete",
				ProjectID:   projectID,
				TaskID:      taskID,
				Title:       task.Title,
				Description: task.Description,
				Status:      task.Status,
				Priority:    task.Priority,
				DueDate:     dueDate,
			})
			return fmt.Sprintf(
				"IMPORTANT: Task '%s' has NOT been deleted yet — it will be deleted after user approval. "+
					"Do NOT say the task was deleted.", task.Title), nil
		}
		return "", fmt.Errorf("task deletion requires a streaming context")

	case "get_task_report":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}

		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}

		summary, err := h.api.db.GetTaskSummaryByProject(ctx, projectID)
		if err != nil {
			return "", err
		}

		tasks, err := h.api.db.ListTasksByProject(ctx, projectID)
		if err != nil {
			return "", err
		}
		database.ApplyUmbrellaStatus(tasks)

		if len(tasks) == 0 {
			return fmt.Sprintf("Project '%s' has no tasks yet.", project.Name), nil
		}

		// Identify umbrella tasks and build children map
		parentIDs := make(map[int64]bool)
		childrenByParent := make(map[int64][]database.ProjectTask)
		for _, t := range tasks {
			if t.ParentID.Valid {
				parentIDs[t.ParentID.Int64] = true
				childrenByParent[t.ParentID.Int64] = append(childrenByParent[t.ParentID.Int64], t)
			}
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## Task Report: %s\n\n", project.Name))
		total := 0
		for _, c := range summary {
			total += c
		}
		sb.WriteString(fmt.Sprintf("**Total:** %d tasks (excluding umbrella parents)\n", total))
		sb.WriteString(fmt.Sprintf("- Todo: %d\n- In Progress: %d\n- Awaiting Approval: %d\n- Done: %d\n\n", summary["todo"], summary["in_progress"], summary["awaiting_approval"], summary["done"]))

		// Umbrella tasks with progress
		var umbrellas []database.ProjectTask
		for _, t := range tasks {
			if parentIDs[t.ID] {
				umbrellas = append(umbrellas, t)
			}
		}
		if len(umbrellas) > 0 {
			sb.WriteString("**Umbrella Tasks:**\n")
			for _, u := range umbrellas {
				kids := childrenByParent[u.ID]
				doneCount := 0
				for _, k := range kids {
					if k.Status == "done" {
						doneCount++
					}
				}
				sb.WriteString(fmt.Sprintf("- [%d] %s (%d/%d done)\n", u.ID, u.Title, doneCount, len(kids)))
			}
			sb.WriteString("\n")
		}

		// Overdue tasks (exclude umbrella parents)
		var overdue []database.ProjectTask
		for _, t := range tasks {
			if parentIDs[t.ID] {
				continue
			}
			if t.DueDate.Valid && t.DueDate.Time.Before(time.Now()) && t.Status != "done" {
				overdue = append(overdue, t)
			}
		}
		if len(overdue) > 0 {
			sb.WriteString("**Overdue:**\n")
			for _, t := range overdue {
				sb.WriteString(fmt.Sprintf("- [%d] %s (due: %s, %s)\n", t.ID, t.Title, t.DueDate.Time.Format("2006-01-02"), t.Priority))
			}
			sb.WriteString("\n")
		}

		// Recommended next task: highest priority todo/in_progress, or nearest due (exclude umbrella parents)
		priorityOrder := map[string]int{"urgent": 4, "high": 3, "medium": 2, "low": 1}
		var best *database.ProjectTask
		for i, t := range tasks {
			if t.Status == "done" || t.ParentID.Valid || parentIDs[t.ID] {
				continue
			}
			if best == nil {
				best = &tasks[i]
				continue
			}
			// Prefer higher priority
			if priorityOrder[t.Priority] > priorityOrder[best.Priority] {
				best = &tasks[i]
			} else if priorityOrder[t.Priority] == priorityOrder[best.Priority] {
				// Tie-break by due date (sooner is better)
				if t.DueDate.Valid && (!best.DueDate.Valid || t.DueDate.Time.Before(best.DueDate.Time)) {
					best = &tasks[i]
				}
			}
		}

		if best != nil {
			due := ""
			if best.DueDate.Valid {
				due = fmt.Sprintf(", due: %s", best.DueDate.Time.Format("2006-01-02"))
			}
			sb.WriteString(fmt.Sprintf("**Recommended next:** [%d] %s (%s%s)\n", best.ID, best.Title, best.Priority, due))
		}

		return sb.String(), nil

	case "dashboard":
		return h.executeChatDashboard(ctx, conversationID)

	case "batch":
		callsRaw, ok := input["calls"].([]interface{})
		if !ok {
			return "", fmt.Errorf("calls is required")
		}
		if len(callsRaw) > 10 {
			return "", fmt.Errorf("batch limited to 10 calls, got %d", len(callsRaw))
		}
		results := make([]map[string]interface{}, 0, len(callsRaw))
		for _, raw := range callsRaw {
			call, ok := raw.(map[string]interface{})
			if !ok {
				results = append(results, map[string]interface{}{"error": "invalid call"})
				continue
			}
			toolName, _ := call["tool"].(string)
			if toolName == "" {
				results = append(results, map[string]interface{}{"error": "tool is required"})
				continue
			}
			if toolName == "batch" || toolName == "openpoet_batch" {
				results = append(results, map[string]interface{}{"tool": toolName, "error": "cannot nest batch calls"})
				continue
			}
			args, _ := call["args"].(map[string]interface{})
			if args == nil {
				args = map[string]interface{}{}
			}
			result, err := h.executeTool(ctx, strings.TrimPrefix(toolName, "openpoet_"), args, conversationID, messageID, collector, proactiveType, authorization)
			item := map[string]interface{}{"tool": toolName}
			if err != nil {
				item["error"] = err.Error()
			} else {
				item["result"] = result
			}
			results = append(results, item)
		}
		out, _ := json.MarshalIndent(results, "", "  ")
		return string(out), nil

	case "start_session":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		var taskID *int64
		if raw, ok := input["task_id"]; ok && fmt.Sprintf("%v", raw) != "" {
			id, err := parseIDParam(input, "task_id")
			if err != nil {
				return "", err
			}
			taskID = &id
		}
		autoStartTaskPrompt := taskID != nil
		if _, ok := input["auto_start_task_prompt"]; ok {
			autoStartTaskPrompt = boolInput(input, "auto_start_task_prompt")
		}
		sess, err := h.api.startManagedSession(ctx, startSessionInput{
			ProjectID:                  projectID,
			TaskID:                     taskID,
			DangerouslySkipPermissions: boolInput(input, "dangerously_skip_permissions"),
			AutoStartTaskPrompt:        autoStartTaskPrompt,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session started: %s (project: %d, status: %s, name: %s)", sess.ID, sess.ProjectID, sess.Status, sess.Name), nil

	case "stop_session":
		sessionID, _ := input["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		if _, err := h.getAllowedSession(ctx, conversationID, sessionID); err != nil {
			return "", err
		}
		if err := h.api.stopManagedSession(ctx, sessionID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Session %s stopped.", sessionID), nil

	case "list_sessions":
		sessions, err := h.api.db.ListSessions(ctx)
		if err != nil {
			return "", err
		}
		statusFilter, _ := input["status"].(string)
		projectFilter := int64(0)
		if _, ok := input["project_id"]; ok {
			projectFilter, _ = parseIDParam(input, "project_id")
		}
		allowed := h.getAgentAllowedProjectIDs(ctx, conversationID)
		var sb strings.Builder
		for _, s := range sessions {
			if allowed != nil && !allowed[s.ProjectID] {
				continue
			}
			if statusFilter != "" && s.Status != statusFilter {
				continue
			}
			if projectFilter > 0 && s.ProjectID != projectFilter {
				continue
			}
			task := "none"
			if s.TaskID.Valid {
				task = fmt.Sprintf("%d", s.TaskID.Int64)
			}
			meta := h.sessionMetadataForTool(ctx, &s)
			sb.WriteString(fmt.Sprintf("- %s | %s | project: %d | status: %s | task: %s | model: %s | effort: %s | harness: %s\n",
				s.ID, s.Name, s.ProjectID, s.Status, task, meta.Model, meta.Effort, meta.Harness))
		}
		if sb.Len() == 0 {
			return "No sessions matching filter.", nil
		}
		return sb.String(), nil

	case "get_session":
		sessionID, _ := input["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		sess, err := h.getAllowedSession(ctx, conversationID, sessionID)
		if err != nil {
			return "", err
		}
		return h.formatSessionForTool(ctx, sess), nil

	case "set_session_model":
		sessionID, _ := input["session_id"].(string)
		model, _ := input["model"].(string)
		if sessionID == "" || strings.TrimSpace(model) == "" {
			return "", fmt.Errorf("session_id and model are required")
		}
		if _, err := h.getAllowedSession(ctx, conversationID, sessionID); err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok {
			return "", errors.New("platform application services unavailable")
		}
		updated, err := services.Execution.Sessions.SetModel(ctx, application.SetSessionModelCommand{
			SessionID: sessionID, Model: model, Authorization: authorization,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session %s model changed to %s (effort: %s, harness: %s)", sessionID, updated.Model, updated.Effort, updated.Harness), nil

	case "set_session_effort":
		sessionID, _ := input["session_id"].(string)
		effort, _ := input["effort"].(string)
		if sessionID == "" || strings.TrimSpace(effort) == "" {
			return "", fmt.Errorf("session_id and effort are required")
		}
		if _, err := h.getAllowedSession(ctx, conversationID, sessionID); err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok {
			return "", errors.New("platform application services unavailable")
		}
		updated, err := services.Execution.Sessions.SetEffort(ctx, application.SetSessionEffortCommand{
			SessionID: sessionID, Effort: effort, Authorization: authorization,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session %s effort changed to %s (model: %s, harness: %s)", sessionID, updated.Effort, updated.Model, updated.Harness), nil

	case "read_session_history":
		sessionID, _ := input["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		if _, err := h.getAllowedSession(ctx, conversationID, sessionID); err != nil {
			return "", err
		}
		result, err := h.api.readSessionHistory(ctx, sessionID, sessionHistoryRequest{
			Mode:          stringInput(input, "mode"),
			Query:         stringInput(input, "query"),
			Regex:         boolInput(input, "regex"),
			CaseSensitive: boolInput(input, "case_sensitive"),
			Lines:         intInput(input, "lines", 80),
			Offset:        intInput(input, "offset", 1),
			Limit:         intInput(input, "limit", 0),
			ContextLines:  intInput(input, "context", 2),
			MaxChars:      intInput(input, "max_chars", 12000),
		})
		if err != nil {
			return "", err
		}
		truncated := ""
		if result.Truncated {
			truncated = " | truncated"
		}
		return fmt.Sprintf("Session history: %s | source: %s | mode: %s | lines: %d/%d | offset: %d%s\n\n%s",
			result.SessionID, result.Source, result.Mode, result.ReturnedLines, result.TotalLines, result.Offset, truncated, result.Content), nil

	case "send_to_session":
		sessionID, _ := input["session_id"].(string)
		text, _ := input["text"].(string)
		if text == "" {
			text, _ = input["prompt"].(string)
		}
		if sessionID == "" || text == "" {
			return "", fmt.Errorf("session_id and text are required")
		}
		if _, err := h.getAllowedSession(ctx, conversationID, sessionID); err != nil {
			return "", err
		}
		if err := h.api.submitSessionLine(sessionID, text); err != nil {
			return "", err
		}
		return fmt.Sprintf("Sent to session %s: %s", sessionID, truncateForTool(text, 100)), nil

	case "link_session_task":
		sessionID, _ := input["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		sess, err := h.getAllowedSession(ctx, conversationID, sessionID)
		if err != nil {
			return "", err
		}
		task, err := h.linkSessionTaskForTool(ctx, sess, input)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session %s linked to task #%d: %s", sessionID, task.ID, task.Title), nil

	case "unlink_session_task":
		sessionID, _ := input["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		sess, err := h.getAllowedSession(ctx, conversationID, sessionID)
		if err != nil {
			return "", err
		}
		task, err := h.api.db.GetTaskForSession(ctx, sessionID)
		if err != nil || task == nil {
			return "", fmt.Errorf("session has no linked task")
		}
		if _, err := h.api.db.UnlinkSessionFromTask(ctx, sessionID); err != nil {
			return "", err
		}
		newName := sess.Name
		if strings.HasPrefix(newName, "Task: ") {
			newName = "Session " + sessionID[:8]
		}
		h.api.db.ExecContext(ctx, "UPDATE sessions SET name = ? WHERE id = ?", newName, sessionID)
		h.api.hub.BroadcastStateUpdate("session", map[string]interface{}{"action": "renamed", "session_id": sessionID, "name": newName})
		h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "session_unlinked", map[string]interface{}{"session_id": sessionID, "session_name": sess.Name}, "user", sessionID)
		return fmt.Sprintf("Session %s unlinked from task #%d.", sessionID, task.ID), nil

	case "stop_session_and_update_task":
		sessionID, _ := input["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		if _, err := h.getAllowedSession(ctx, conversationID, sessionID); err != nil {
			return "", err
		}
		task, err := h.api.db.GetTaskForSession(ctx, sessionID)
		if err != nil || task == nil {
			return "", fmt.Errorf("session has no linked task")
		}
		if err := h.api.stopManagedSession(ctx, sessionID); err != nil {
			return "", err
		}
		changed, err := h.updateTaskFromSessionTool(ctx, task, input, sessionID)
		if err != nil {
			return "", fmt.Errorf("session stopped, but failed to update linked task: %w", err)
		}
		if !changed {
			return fmt.Sprintf("Session %s stopped. Linked task #%d was not changed.", sessionID, task.ID), nil
		}
		return fmt.Sprintf("Session %s stopped and linked task #%d updated.", sessionID, task.ID), nil

	case "create_document":
		title, _ := input["title"].(string)
		content, _ := input["content"].(string)
		if content == "" {
			return "", fmt.Errorf("content is required")
		}
		if title == "" {
			title = "Documento"
		}

		doc := &database.TempDocument{
			ID:             uuid.New().String()[:8],
			Title:          title,
			Content:        content,
			ConversationID: sql.NullInt64{Int64: conversationID, Valid: conversationID > 0},
			MessageID:      messageID,
		}
		if err := h.api.db.CreateTempDocument(ctx, doc); err != nil {
			return "", err
		}

		return fmt.Sprintf("Document created successfully. A 'View Document' button was automatically displayed in chat. Do NOT generate links — the user will use the native button. Internal link: /app/doc/%s", doc.ID), nil

	case "list_directory":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}
		path, _ := input["path"].(string)

		var fileList []files.FileInfo
		if project.Type == "local" {
			fm := files.NewLocalFileManager(project.Path)
			fileList, err = fm.List(path)
		} else {
			fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
			fileList, err = fm.List(path)
		}
		if err != nil {
			return "", err
		}

		var sb strings.Builder
		if path == "" {
			sb.WriteString(fmt.Sprintf("Directory listing for project %s (root):\n", project.Name))
		} else {
			sb.WriteString(fmt.Sprintf("Directory listing for %s:\n", path))
		}
		for _, f := range fileList {
			if f.IsDir {
				sb.WriteString(fmt.Sprintf("  [DIR]  %s/\n", f.Name))
			} else {
				sb.WriteString(fmt.Sprintf("  %6d  %s\n", f.Size, f.Name))
			}
		}
		if len(fileList) == 0 {
			sb.WriteString("  (empty directory)\n")
		}
		return sb.String(), nil

	case "read_file":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}
		filePath, _ := input["path"].(string)
		if filePath == "" {
			return "", fmt.Errorf("path is required")
		}

		const maxSize int64 = 2 * 1024 * 1024
		var content []byte

		if project.Type == "local" {
			fm := files.NewLocalFileManager(project.Path)
			reader, fileInfo, err := fm.ReadStream(filePath)
			if err != nil {
				return "", err
			}
			defer reader.Close()
			if fileInfo.Size > maxSize {
				return "", fmt.Errorf("file too large (max 2MB, file is %d bytes)", fileInfo.Size)
			}
			content, err = io.ReadAll(io.LimitReader(reader, maxSize+1))
			if err != nil {
				return "", err
			}
		} else {
			fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
			file, fileInfo, sshClient, sftpClient, err := fm.ReadStream(filePath)
			if err != nil {
				return "", err
			}
			defer file.Close()
			defer sftpClient.Close()
			defer sshClient.Close()
			if fileInfo.Size > maxSize {
				return "", fmt.Errorf("file too large (max 2MB, file is %d bytes)", fileInfo.Size)
			}
			content, err = io.ReadAll(io.LimitReader(file, maxSize+1))
			if err != nil {
				return "", err
			}
		}

		// Apply offset/limit if provided
		offset := 0
		limit := 0
		if v, _ := input["offset"].(string); v != "" {
			offset, _ = strconv.Atoi(v)
		} else if v, ok := input["offset"].(float64); ok {
			offset = int(v)
		}
		if v, _ := input["limit"].(string); v != "" {
			limit, _ = strconv.Atoi(v)
		} else if v, ok := input["limit"].(float64); ok {
			limit = int(v)
		}

		lines := strings.Split(string(content), "\n")
		totalLines := len(lines)

		if offset > 0 {
			offset-- // convert from 1-based to 0-based
			if offset >= totalLines {
				return fmt.Sprintf("--- %s (%d lines total) ---\nOffset %d is beyond end of file.", filePath, totalLines, offset+1), nil
			}
			lines = lines[offset:]
		}
		if limit > 0 && limit < len(lines) {
			lines = lines[:limit]
		}

		var sb strings.Builder
		startLine := offset + 1
		if offset <= 0 {
			startLine = 1
		}
		sb.WriteString(fmt.Sprintf("--- %s (%d lines total) ---\n", filePath, totalLines))
		for i, line := range lines {
			sb.WriteString(fmt.Sprintf("%4d | %s\n", startLine+i, line))
		}
		return sb.String(), nil

	case "find_files":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}
		pattern, _ := input["pattern"].(string)
		if pattern == "" {
			return "", fmt.Errorf("pattern is required")
		}

		var results []string
		if project.Type == "local" {
			fm := files.NewLocalFileManager(project.Path)
			results, err = fm.Glob(pattern)
		} else {
			fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
			results, err = fm.Glob(pattern)
		}
		if err != nil {
			return "", err
		}

		if len(results) == 0 {
			return fmt.Sprintf("No files matching '%s' found in project %s.", pattern, project.Name), nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d file(s) matching '%s':\n", len(results), pattern))
		for _, r := range results {
			sb.WriteString(fmt.Sprintf("  %s\n", r))
		}
		return sb.String(), nil

	case "grep_content":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}
		pattern, _ := input["pattern"].(string)
		if pattern == "" {
			return "", fmt.Errorf("pattern is required")
		}
		searchPath, _ := input["path"].(string)
		fileGlob, _ := input["glob"].(string)

		var results []files.GrepResult
		if project.Type == "local" {
			fm := files.NewLocalFileManager(project.Path)
			results, err = fm.Grep(pattern, searchPath, fileGlob)
		} else {
			fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
			results, err = fm.Grep(pattern, searchPath, fileGlob)
		}
		if err != nil {
			return "", err
		}

		if len(results) == 0 {
			return fmt.Sprintf("No matches for '%s' found in project %s.", pattern, project.Name), nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d match(es) for '%s':\n", len(results), pattern))
		for _, r := range results {
			// Truncate long lines
			line := r.Content
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %s:%d: %s\n", r.File, r.Line, line))
		}
		return sb.String(), nil

	case "open_file":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}
		pathsRaw, _ := input["paths"]
		paths := toStringSlice(pathsRaw)
		if len(paths) == 0 {
			return "", fmt.Errorf("paths is required and must contain at least one file path")
		}
		// Persist file cards as TempDocuments (rendered on stream end + conversation reload)
		for _, p := range paths {
			fileName := p
			if idx := strings.LastIndex(p, "/"); idx >= 0 {
				fileName = p[idx+1:]
			}
			docID := uuid.New().String()[:8]
			contentJSON, _ := json.Marshal(map[string]interface{}{
				"project_id": fmt.Sprintf("%d", projectID),
				"path":       p,
			})
			tempDoc := &database.TempDocument{
				ID:      docID,
				Title:   fmt.Sprintf("File:%s", fileName),
				Content: string(contentJSON),
				Summary: p,
				ConversationID: sql.NullInt64{
					Int64: conversationID,
					Valid: conversationID > 0,
				},
				MessageID: messageID,
				Status:    "file",
			}
			_ = h.api.db.CreateTempDocument(ctx, tempDoc)
		}
		var fileList strings.Builder
		for _, p := range paths {
			fileList.WriteString("  - " + p + "\n")
		}
		return fmt.Sprintf("Done. %d file card(s) displayed to user:\n%sProject: %s. Do not call open_file again for these files.", len(paths), fileList.String(), project.Name), nil

	// ---- Project custom tools CRUD ----

	case "list_project_custom_tools":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.CustomTools == nil {
			return "", errors.New("custom tool application service unavailable")
		}
		tools, err := services.Configuration.CustomTools.List(ctx, projectID)
		if err != nil {
			return "", err
		}
		if len(tools) == 0 {
			return "No custom tools found for this project.", nil
		}
		var output strings.Builder
		for _, tool := range tools {
			status := "enabled"
			if !tool.Enabled {
				status = "disabled"
			}
			confirmation := ""
			if tool.Confirm {
				confirmation = ", requires confirmation"
			}
			output.WriteString(fmt.Sprintf("- [%d] %s (%s%s): %s\n", tool.ID, tool.Name, status, confirmation, tool.Description))
		}
		return output.String(), nil

	case "create_project_custom_tool":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		parameters, _ := input["parameters"].(string)
		if parameters == "" {
			parameters = "{}"
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.CustomTools == nil {
			return "", errors.New("custom tool application service unavailable")
		}
		tool, err := services.Configuration.CustomTools.Create(ctx, authorization, projectID, application.CustomToolInput{
			Name: stringInput(input, "name"), Description: stringInput(input, "description"),
			Command: stringInput(input, "command"), Parameters: parameters,
			Confirm: boolInput(input, "confirm"), WorkingDir: stringInput(input, "working_dir"), Enabled: true,
		})
		if err != nil {
			return "", err
		}
		h.invalidateProjectSessions(ctx, projectID)
		return fmt.Sprintf("Custom tool '%s' created (ID: %d)", tool.Name, tool.ID), nil

	case "update_project_custom_tool":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		toolID, err := parseIDParam(input, "id")
		if err != nil {
			return "", err
		}
		command := application.UpdateCustomToolCommand{ID: toolID}
		if value, exists := input["name"].(string); exists && value != "" {
			command.Name = &value
		}
		if value, exists := input["description"].(string); exists {
			command.Description = &value
		}
		if value, exists := input["command"].(string); exists && value != "" {
			command.Command = &value
		}
		if value, exists := input["parameters"].(string); exists {
			command.Parameters = &value
		}
		if value, exists := input["working_dir"].(string); exists {
			command.WorkingDir = &value
		}
		if value, exists := input["confirm"].(bool); exists {
			command.Confirm = &value
		}
		if value, exists := input["enabled"].(bool); exists {
			command.Enabled = &value
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.CustomTools == nil {
			return "", errors.New("custom tool application service unavailable")
		}
		tool, err := services.Configuration.CustomTools.Update(ctx, authorization, projectID, command)
		if err != nil {
			return "", err
		}
		h.invalidateProjectSessions(ctx, projectID)
		return fmt.Sprintf("Custom tool '%s' updated", tool.Name), nil

	case "delete_project_custom_tool":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		toolID, err := parseIDParam(input, "id")
		if err != nil {
			return "", err
		}
		services, ok := h.api.platformApplicationServices()
		if !ok || services.Configuration.CustomTools == nil {
			return "", errors.New("custom tool application service unavailable")
		}
		if err := services.Configuration.CustomTools.Delete(ctx, authorization, projectID, toolID); err != nil {
			return "", err
		}
		h.invalidateProjectSessions(ctx, projectID)
		return fmt.Sprintf("Custom tool %d deleted", toolID), nil

	default:
		// Check if this is a custom project tool (prefixed with "custom_")
		if strings.HasPrefix(name, "custom_") {
			return h.executeCustomProjectTool(ctx, name, input, conversationID, collector)
		}
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// projectToolToDefinition converts a ProjectTool DB model to an LLM ToolDefinition.
// Tool names are prefixed with "custom_" to avoid collisions with built-in tools.
func projectToolToDefinition(t database.ProjectTool) llm.ToolDefinition {
	props := make(map[string]llm.ToolPropertySchema)
	var required []string

	// Parse parameters JSON: {"param_name": {"type": "string", "description": "...", "required": true}}
	var params map[string]struct {
		Type        string `json:"type"`
		Description string `json:"description"`
		Required    bool   `json:"required"`
	}
	if json.Unmarshal([]byte(t.Parameters), &params) == nil {
		for pname, p := range params {
			ptype := p.Type
			if ptype == "" {
				ptype = "string"
			}
			props[pname] = llm.ToolPropertySchema{
				Type:        ptype,
				Description: p.Description,
			}
			if p.Required {
				required = append(required, pname)
			}
		}
	}

	desc := t.Description
	if desc == "" {
		desc = fmt.Sprintf("Custom project tool: %s", t.Name)
	}

	if t.Confirm {
		desc += " [requires user confirmation before execution]"
	}

	// Always include project_id so the tool can be called from any conversation context
	props["project_id"] = llm.ToolPropertySchema{
		Type:        "integer",
		Description: fmt.Sprintf("The project ID this tool belongs to (default: %d)", t.ProjectID),
	}
	required = append(required, "project_id")

	return llm.ToolDefinition{
		Name:        "custom_" + t.Name,
		Description: desc,
		InputSchema: llm.ToolDefinitionInput{
			Type:       "object",
			Properties: props,
			Required:   required,
		},
	}
}

// executeCustomProjectTool runs a custom project tool by executing its shell command.
// Parameters are passed as environment variables with TOOL_ prefix.
// When collector is non-nil and tool.Confirm is true, the execution is deferred to a
// proposal card for user approval instead of running immediately.
func (h *AIHandler) executeCustomProjectTool(ctx context.Context, name string, input map[string]interface{}, conversationID int64, collector *planningCollector) (string, error) {
	toolName := strings.TrimPrefix(name, "custom_")

	// Try conversation project context first, then fall back to project_id from input
	projectID := h.getProjectIDFromConversation(ctx, conversationID)
	if projectID == 0 {
		// Accept project_id from tool input (passed by AI when calling from any context)
		if pidF, ok := input["project_id"].(float64); ok && pidF > 0 {
			projectID = int64(pidF)
		}
	}
	if projectID == 0 {
		return "", fmt.Errorf("no project context for custom tool execution — provide project_id")
	}

	// Strip numeric project prefix from tool name if present (e.g. "1_run_tests" -> "run_tests")
	if idx := strings.Index(toolName, "_"); idx > 0 {
		if _, err := strconv.ParseInt(toolName[:idx], 10, 64); err == nil {
			toolName = toolName[idx+1:]
		}
	}

	project, err := h.api.db.GetProject(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("project not found")
	}

	tool, err := h.api.db.GetProjectToolByName(ctx, projectID, toolName)
	if err != nil {
		return "", fmt.Errorf("custom tool %q not found for project", toolName)
	}

	if !tool.Enabled {
		return "", fmt.Errorf("custom tool %q is disabled", toolName)
	}

	// Build working directory
	workDir := project.Path
	if tool.WorkingDir != "" {
		workDir = project.Path + "/" + tool.WorkingDir
	}

	// Defer to proposal card if tool requires confirmation
	if tool.Confirm && collector != nil {
		collector.add(PlanningTaskAction{
			Action:    "execute_custom_tool",
			ProjectID: projectID,
			Title:     tool.Name,
			Extra: map[string]interface{}{
				"tool_id":     tool.ID,
				"project_id":  projectID,
				"tool_name":   tool.Name,
				"description": tool.Description,
				"input":       input,
			},
		})
		return fmt.Sprintf(
			"IMPORTANT: Tool '%s' requires user confirmation and has NOT been executed yet. "+
				"A confirmation card will appear for user approval. Do NOT say the tool was executed.",
			tool.Name), nil
	}

	return h.executeStoredCustomTool(ctx, project, tool, input, workDir)
}

func (h *AIHandler) executeCustomProjectToolByID(ctx context.Context, projectID, toolID int64, input map[string]any) (string, error) {
	project, err := h.api.db.GetProject(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("project not found")
	}
	tool, err := h.api.db.GetProjectTool(ctx, toolID)
	if err != nil || tool.ProjectID != projectID {
		return "", fmt.Errorf("custom tool not found")
	}
	if !tool.Enabled {
		return "", fmt.Errorf("custom tool %q is disabled", tool.Name)
	}
	workDir := project.Path
	if tool.WorkingDir != "" {
		workDir = project.Path + "/" + tool.WorkingDir
	}
	return h.executeStoredCustomTool(ctx, project, tool, input, workDir)
}

func (h *AIHandler) executeStoredCustomTool(ctx context.Context, project *database.Project, tool *database.ProjectTool, input map[string]any, workDir string) (string, error) {
	if project.Type != "local" {
		return h.executeCustomToolSSH(ctx, project, tool, input, workDir)
	}
	env := os.Environ()
	for key, value := range input {
		envKey := "TOOL_" + strings.ToUpper(key)
		env = append(env, fmt.Sprintf("%s=%v", envKey, value))
	}
	commandContext, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resolvedTool, err := h.resolveCustomToolForExecution(tool)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(commandContext, "sh", "-c", resolvedTool.Command)
	command.Dir = workDir
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	var result strings.Builder
	if stdout.Len() > 0 {
		result.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		if result.Len() > 0 {
			result.WriteString("\n--- stderr ---\n")
		}
		result.WriteString(stderr.String())
	}
	if err != nil {
		if result.Len() == 0 {
			return "", fmt.Errorf("command failed: %w", err)
		}
		result.WriteString(fmt.Sprintf("\n[exit code: %s]", err.Error()))
	}
	output := result.String()
	const maxOutput = 100_000
	if len(output) > maxOutput {
		output = output[:maxOutput] + "\n... (output truncated)"
	}
	return output, nil
}

// executeCustomToolSSH runs a custom project tool on a remote project via SSH.
func (h *AIHandler) executeCustomToolSSH(ctx context.Context, project *database.Project, tool *database.ProjectTool, input map[string]interface{}, workDir string) (string, error) {
	fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())

	// Build env exports + command
	var cmdBuilder strings.Builder
	cmdBuilder.WriteString(fmt.Sprintf("cd %s && ", shellQuote(workDir)))
	for k, v := range input {
		envKey := "TOOL_" + strings.ToUpper(k)
		cmdBuilder.WriteString(fmt.Sprintf("export %s=%s && ", envKey, shellQuote(fmt.Sprintf("%v", v))))
	}
	resolvedTool, err := h.resolveCustomToolForExecution(tool)
	if err != nil {
		return "", err
	}
	cmdBuilder.WriteString(resolvedTool.Command)

	output, err := fm.RunCommand(cmdBuilder.String())
	if err != nil {
		return "", err
	}

	const maxOutput = 100_000
	if len(output) > maxOutput {
		output = output[:maxOutput] + "\n... (output truncated)"
	}

	return output, nil
}

// shellQuote wraps a string in single quotes for safe shell usage.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// HandleExecuteTool is an HTTP endpoint that Node.js SDK sidecar calls to execute
// OpenPoet tools. It proxies tool calls to the existing executeTool implementation.
func (h *AIHandler) HandleExecuteTool(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name           string         `json:"name"`
		Args           map[string]any `json:"args"`
		ConversationID int64          `json:"conversation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.Args == nil {
		input.Args = make(map[string]any)
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(platformUIContext(r), 30*time.Second)
	defer cancel()
	result, err := service.ExecuteTool(ctx, application.AIToolExecutionRequest{
		Name: input.Name, Arguments: input.Args, ConversationID: input.ConversationID,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]any{"result": "", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"result": result.Output, "error": ""})
}

// HandleToolDefinitions returns the chat tool definitions as JSON.
// Used by the Node.js SDK sidecar to register tools at startup.
// Applies the chat tool policy to filter available tools.
func (h *AIHandler) HandleToolDefinitions(w http.ResponseWriter, r *http.Request) {
	tools := llm.ChatTools()

	// Apply chat tool policy
	policyStr, _ := h.api.db.GetSetting(r.Context(), "mcp_tool_policy_chat")
	if policyStr != "" {
		policy := mcp.ParsePolicy(policyStr)
		var names []string
		for _, t := range tools {
			names = append(names, t.Name)
		}
		allowed := mcp.FilterByPolicy(policy, names)
		allowedSet := make(map[string]bool, len(allowed))
		for _, n := range allowed {
			allowedSet[n] = true
		}
		var filtered []llm.ToolDefinition
		for _, t := range tools {
			if allowedSet[t.Name] {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}

	respondJSON(w, http.StatusOK, tools)
}

// buildDocCard checks if a tool execution produced a document link and returns
// metadata for a native doc_card SSE event. Returns nil if not applicable.
func (h *AIHandler) buildDocCard(toolName, result string, input map[string]interface{}) map[string]interface{} {
	matches := docLinkRe.FindStringSubmatch(result)
	if len(matches) < 2 {
		return nil
	}
	docID := matches[1]

	switch toolName {
	case "update_memory_doc":
		summary, _ := input["summary"].(string)
		return map[string]interface{}{
			"doc_id":  docID,
			"type":    "proposal",
			"title":   "Change proposal",
			"summary": summary,
		}
	case "get_memory_doc":
		return map[string]interface{}{
			"doc_id": docID,
			"type":   "view",
			"title":  "Memory Doc",
		}
	case "create_document":
		title, _ := input["title"].(string)
		if title == "" {
			title = "Documento"
		}
		return map[string]interface{}{
			"doc_id": docID,
			"type":   "document",
			"title":  title,
		}
	default:
		return nil
	}
}

// toStringSlice converts an interface{} (typically []interface{} from JSON) to []string.
func toStringSlice(v interface{}) []string {
	switch val := v.(type) {
	case []interface{}:
		var result []string
		for _, item := range val {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return val
	default:
		return nil
	}
}

// buildSystemPrompt constructs the system prompt with current state.
func (h *AIHandler) buildSystemPrompt(ctx context.Context) string {
	var skillNames []string
	skills, _ := h.api.db.ListSkills(ctx)
	for _, s := range skills {
		status := "enabled"
		if !s.Enabled {
			status = "disabled"
		}
		skillNames = append(skillNames, fmt.Sprintf("[%d] %s (%s, %s)", s.ID, s.Name, s.Category, status))
	}

	var projectNames []string
	projects, _ := h.api.db.ListProjects(ctx)
	for _, p := range projects {
		projectNames = append(projectNames, fmt.Sprintf("[%d] %s (%s)", p.ID, p.Name, p.Type))
	}

	var mcpNames []string
	mcps, _ := h.api.db.ListMCPServers(ctx)
	for _, m := range mcps {
		mcpNames = append(mcpNames, fmt.Sprintf("[%d] %s", m.ID, m.Name))
	}

	return llm.ChatSystemPrompt(skillNames, projectNames, mcpNames, nil)
}

// buildSystemPromptWithContext builds system prompt, optionally with proactive conversation context.
// When forMCP is true, adapts the prompt for GoSDK/session providers (MCP tool naming convention).
// The optional agent parameter filters projects based on the agent's project_filter.
func (h *AIHandler) buildSystemPromptWithContext(ctx context.Context, proactiveCtx string, forMCP bool, agent ...*database.AIAgent) string {
	var skillNames []string
	skills, _ := h.api.db.ListSkills(ctx)
	for _, s := range skills {
		status := "enabled"
		if !s.Enabled {
			status = "disabled"
		}
		skillNames = append(skillNames, fmt.Sprintf("[%d] %s (%s, %s)", s.ID, s.Name, s.Category, status))
	}

	projects, _ := h.api.db.ListProjects(ctx)

	// Apply agent project filter if present
	if len(agent) > 0 && agent[0] != nil && agent[0].ProjectFilter != "" {
		projects = h.filterProjectsByAgent(ctx, projects, agent[0].ProjectFilter)
	}

	var projectNames []string
	for _, p := range projects {
		projectNames = append(projectNames, fmt.Sprintf("[%d] %s (%s)", p.ID, p.Name, p.Type))
	}

	var mcpNames []string
	mcps, _ := h.api.db.ListMCPServers(ctx)
	for _, m := range mcps {
		mcpNames = append(mcpNames, fmt.Sprintf("[%d] %s", m.ID, m.Name))
	}

	// Collect custom tool names for the system prompt.
	// If conversation has proactive context with project_id, show that project's tools.
	// Otherwise, show tools from all projects so the model knows they exist.
	var customToolNames []string
	var customToolProjectID int64
	if proactiveCtx != "" {
		var ctxData map[string]interface{}
		if json.Unmarshal([]byte(proactiveCtx), &ctxData) == nil {
			if pidF, ok := ctxData["project_id"].(float64); ok {
				customToolProjectID = int64(pidF)
			}
		}
	}

	buildToolDesc := func(t database.ProjectTool) string {
		desc := fmt.Sprintf("custom_%s: %s", t.Name, t.Description)
		if t.Confirm {
			desc += " [requires confirmation]"
		}
		var params map[string]struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(t.Parameters), &params) == nil && len(params) > 0 {
			var pnames []string
			for pname := range params {
				pnames = append(pnames, pname)
			}
			desc += fmt.Sprintf(" (params: %s)", strings.Join(pnames, ", "))
		}
		return desc
	}

	if customToolProjectID > 0 {
		if tools, err := h.api.db.ListEnabledProjectTools(ctx, customToolProjectID); err == nil {
			for _, t := range tools {
				customToolNames = append(customToolNames, buildToolDesc(t))
			}
		}
	} else {
		// No project context — include tools from all projects
		for _, p := range projects {
			if tools, err := h.api.db.ListEnabledProjectTools(ctx, p.ID); err == nil {
				for _, t := range tools {
					desc := buildToolDesc(t) + fmt.Sprintf(" [project: %s, id: %d]", p.Name, p.ID)
					customToolNames = append(customToolNames, desc)
				}
			}
		}
	}

	return llm.ChatSystemPromptWithProactiveContext(skillNames, projectNames, mcpNames, customToolNames, proactiveCtx, forMCP)
}

// HandleInitiateMemoryDocEdit creates an AI-initiated conversation for editing a project's memory doc.
func (h *AIHandler) HandleInitiateMemoryDocEdit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID int64 `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	conversation, err := service.InitiateMemoryDocEdit(platformUIContext(r), input.ProjectID, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation.ID})
}

// HandleInitiateTaskCreation creates a proactive AI conversation to assist with task creation.
func (h *AIHandler) HandleInitiateTaskCreation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID   int64  `json:"project_id"`
		ParentID    int64  `json:"parent_id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Priority    string `json:"priority"`
		DueDate     string `json:"due_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	conversation, err := service.InitiateTaskCreation(platformUIContext(r), application.AITaskCreationCommand{
		ProjectID: input.ProjectID, ParentID: input.ParentID, Title: input.Title,
		Description: input.Description, Status: input.Status, Priority: input.Priority,
		DueDate: input.DueDate, Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation.ID})
}

// HandleInitiateTaskDiscussion creates a proactive AI conversation to discuss an existing task.
func (h *AIHandler) HandleInitiateTaskDiscussion(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TaskID    int64 `json:"task_id"`
		ProjectID int64 `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	conversation, err := service.InitiateTaskDiscussion(
		platformUIContext(r), input.ProjectID, input.TaskID, platformUIAuthorization(r),
	)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation.ID})
}

// invalidateProjectSessions disconnects all chat sessions linked to a project,
// forcing the MCP server to rebuild with updated custom tools on the next query.
func (h *AIHandler) invalidateProjectSessions(ctx context.Context, projectID int64) {
	pattern := fmt.Sprintf(`%%"project_id":%d%%`, projectID)
	var convIDs []int64
	if err := h.api.db.SelectContext(ctx, &convIDs,
		`SELECT id FROM ai_conversations WHERE proactive_context LIKE ?`, pattern); err != nil {
		return
	}
	for _, cid := range convIDs {
		h.providerMgr.DisconnectSession(cid)
	}
}

// getProjectIDFromConversation extracts the project_id from a conversation's proactive context JSON.
func (h *AIHandler) getProjectIDFromConversation(ctx context.Context, conversationID int64) int64 {
	conv, err := h.api.db.GetAIConversation(ctx, conversationID)
	if err != nil {
		return 0
	}
	if conv.ProactiveContext == "" {
		return 0
	}
	var ctxData map[string]interface{}
	if err := json.Unmarshal([]byte(conv.ProactiveContext), &ctxData); err != nil {
		return 0
	}
	if pidF, ok := ctxData["project_id"].(float64); ok {
		return int64(pidF)
	}
	return 0
}

// GetCustomToolsForConversation implements llm.CustomToolsProvider.
// Returns custom project tool definitions. If the conversation has a project context,
// returns tools for that project. Otherwise returns tools from all projects so the
// AI can call any tool when given a project_id.
func (h *AIHandler) GetCustomToolsForConversation(conversationID int64) []llm.ToolDefinition {
	ctx := context.Background()

	// Try conversation-specific project first
	if projectID := h.getProjectIDFromConversation(ctx, conversationID); projectID > 0 {
		customTools, err := h.api.db.ListEnabledProjectTools(ctx, projectID)
		if err == nil && len(customTools) > 0 {
			var defs []llm.ToolDefinition
			for _, ct := range customTools {
				defs = append(defs, projectToolToDefinition(ct))
			}
			return defs
		}
	}

	// No project context — load tools from all projects
	projects, err := h.api.db.ListProjects(ctx)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool) // avoid duplicate tool names
	var defs []llm.ToolDefinition
	for _, p := range projects {
		tools, err := h.api.db.ListEnabledProjectTools(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, ct := range tools {
			key := "custom_" + ct.Name
			if seen[key] {
				// If multiple projects have the same tool name, make it unique
				key = fmt.Sprintf("custom_%d_%s", ct.ProjectID, ct.Name)
				ct.Name = fmt.Sprintf("%d_%s", ct.ProjectID, ct.Name)
			}
			seen[key] = true
			defs = append(defs, projectToolToDefinition(ct))
		}
	}
	return defs
}

// HandleInitiateSkillCustomization creates a proactive AI conversation to help customize a global skill for a project.
func (h *AIHandler) HandleInitiateSkillCustomization(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID int64 `json:"project_id"`
		SkillID   int64 `json:"skill_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	conversation, err := service.InitiateSkillCustomization(
		platformUIContext(r), input.ProjectID, input.SkillID, platformUIAuthorization(r),
	)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation.ID})
}

// HandleListConversations lists all conversations.
func (h *AIHandler) HandleListConversations(w http.ResponseWriter, r *http.Request) {
	// Auto-prune: keep only the last 15 conversations
	_ = h.api.db.PruneOldAIConversations(r.Context(), 15)

	convs, err := h.api.db.ListAIConversations(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if convs == nil {
		convs = []database.AIConversation{}
	}
	respondJSON(w, http.StatusOK, convs)
}

// HandleGetConversation returns a conversation with its messages.
func (h *AIHandler) HandleGetConversation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}

	conv, err := h.api.db.GetAIConversation(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Conversation not found")
		return
	}

	// Auto-mark AI-initiated conversations as read when opened
	if conv.Source == "ai" && !conv.IsRead {
		h.api.db.MarkConversationRead(r.Context(), id)
		conv.IsRead = true
	}

	messages, err := h.api.db.ListAIMessages(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if messages == nil {
		messages = []database.AIMessage{}
	}

	docCards, _ := h.api.db.ListTempDocumentsByConversation(r.Context(), id)
	if docCards == nil {
		docCards = []database.TempDocument{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": conv,
		"messages":     messages,
		"doc_cards":    docCards,
	})
}

// HandleDeleteConversation deletes a conversation.
func (h *AIHandler) HandleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}

	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	if err := service.DeleteConversation(platformUIContext(r), id, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleDeleteAllConversations deletes all conversations.
func (h *AIHandler) HandleDeleteAllConversations(w http.ResponseWriter, r *http.Request) {
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	if _, err := service.DeleteAllConversations(platformUIContext(r), platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleGenerateSkill generates a skill from a description (SSE streaming).
func (h *AIHandler) HandleGenerateSkill(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}
	result, err := service.GenerateSkillStream(platformUIContext(r), input.Description, platformUIAuthorization(r), func(delta string) error {
		h.sendSSE(w, flusher, "text", map[string]any{"text": delta})
		return nil
	})
	if err != nil {
		h.sendSSE(w, flusher, "error", map[string]any{"message": err.Error()})
		return
	}
	h.sendSSE(w, flusher, "done", map[string]any{"full_text": result.Content})
}

// filterClaudeCodeNoise removes Claude Code's internal processing lines from raw
// terminal output, keeping only lines that might indicate user intent or results.
func filterClaudeCodeNoise(raw string) string {
	lines := strings.Split(raw, "\n")
	var filtered []string
	prevBlank := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip empty lines, but keep one blank between content
		if trimmed == "" {
			if !prevBlank && len(filtered) > 0 {
				filtered = append(filtered, "")
				prevBlank = true
			}
			continue
		}
		prevBlank = false

		// Skip if line starts with tool call indicator ● (Read, Edit, Bash, etc.)
		if strings.HasPrefix(trimmed, "●") {
			continue
		}

		// Skip spinner characters
		if len(trimmed) > 0 {
			r := []rune(trimmed)
			switch r[0] {
			case '⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏':
				continue
			}
		}

		// Skip box-drawing borders (tool output frames)
		if strings.HasPrefix(trimmed, "╭") || strings.HasPrefix(trimmed, "╰") || strings.HasPrefix(trimmed, "│") {
			continue
		}

		// Skip Claude Code stats lines
		if strings.HasPrefix(trimmed, "Tokens:") || strings.HasPrefix(trimmed, "Cost:") {
			continue
		}

		// Skip bare file paths (common in tool output, e.g. "/Users/foo/bar.go")
		if strings.HasPrefix(trimmed, "/") && !strings.Contains(trimmed, " ") {
			continue
		}

		filtered = append(filtered, trimmed)
	}

	// Trim trailing blank lines
	for len(filtered) > 0 && filtered[len(filtered)-1] == "" {
		filtered = filtered[:len(filtered)-1]
	}

	return strings.Join(filtered, "\n")
}

// extractSessionContext parses cleaned terminal output to extract the most relevant
// content for task creation: user prompts, plans, and raw terminal output for cross-reference.
// Uses a hybrid approach: heuristic extraction for structured data + raw output as LLM fallback.
func extractSessionContext(cleanOutput []byte) string {
	var sb strings.Builder
	lines := strings.Split(string(cleanOutput), "\n")

	// 1. Extract user prompts (lines starting with ❯, the Claude Code prompt marker)
	var userPrompts []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") {
			prompt := strings.TrimSpace(strings.TrimPrefix(trimmed, "❯"))
			// Multi-line prompt: grab continuation lines
			for j := i + 1; j < len(lines) && j < i+5; j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" || strings.HasPrefix(next, "❯") ||
					strings.HasPrefix(next, "⠋") || strings.HasPrefix(next, "⠙") ||
					strings.HasPrefix(next, "●") || strings.HasPrefix(next, "╭") {
					break
				}
				prompt += " " + next
			}
			// Skip ghost text hints (Claude Code shows "Try ..." as placeholder)
			lowerPrompt := strings.ToLower(prompt)
			isGhostText := strings.HasPrefix(lowerPrompt, "try ") || strings.HasPrefix(lowerPrompt, "try\"") || strings.HasPrefix(lowerPrompt, "try\u00a0")
			if len(prompt) > 3 && !isGhostText {
				userPrompts = append(userPrompts, prompt)
			}
		}
	}

	// 2. Extract plan sections (look for plan-related headers)
	inPlan := false
	var planLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !inPlan && (strings.Contains(lower, "plan:") ||
			strings.Contains(lower, "## plan") ||
			strings.Contains(lower, "implementation plan")) {
			inPlan = true
			planLines = append(planLines, trimmed)
			continue
		}
		if inPlan {
			if trimmed == "" && len(planLines) > 3 {
				emptyCount := 0
				for k := len(planLines) - 1; k >= 0; k-- {
					if planLines[k] == "" {
						emptyCount++
					} else {
						break
					}
				}
				if emptyCount >= 2 {
					inPlan = false
					continue
				}
			}
			planLines = append(planLines, trimmed)
			if len(planLines) > 50 {
				inPlan = false
			}
		}
	}

	// 3. Build structured context
	if len(userPrompts) > 0 {
		sb.WriteString("User prompts (what the user asked Claude Code to do):\n")
		for i, p := range userPrompts {
			sb.WriteString(fmt.Sprintf("%d. \"%s\"\n", i+1, p))
		}
		sb.WriteString("\n")
	}

	if len(planLines) > 0 {
		sb.WriteString("Plan generated by Claude Code:\n")
		sb.WriteString(strings.Join(planLines, "\n"))
		sb.WriteString("\n\n")
	}

	// 4. Always include raw terminal output as fallback context for the LLM
	// This allows the LLM to cross-reference and correct heuristic extraction errors
	outputStr := string(cleanOutput)
	rawSize := 3000
	if len(outputStr) < rawSize {
		rawSize = len(outputStr)
	}
	rawRecent := outputStr[len(outputStr)-rawSize:]
	filtered := filterClaudeCodeNoise(rawRecent)
	if len(filtered) > 0 {
		sb.WriteString("Raw terminal output (for cross-reference):\n")
		sb.WriteString(filtered)
	}

	return sb.String()
}

// extractJSONObject extracts a JSON object from text that may contain markdown
// code blocks, leading/trailing prose, or other wrapping. It finds the first '{'
// and last '}' to isolate the JSON object.
func extractJSONObject(text string) string {
	text = strings.TrimSpace(text)
	// Remove markdown code block if present
	if strings.Contains(text, "```") {
		lines := strings.Split(text, "\n")
		var jsonLines []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				jsonLines = append(jsonLines, line)
			}
		}
		if len(jsonLines) > 0 {
			text = strings.Join(jsonLines, "\n")
		}
	}
	// Find first { and last } to extract the JSON object
	start := strings.IndexRune(text, '{')
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

// processCR splits carriage-return-separated segments into separate lines.
// Terminal output often uses \r to overwrite lines (e.g., ghost text replaced by real input).
// This ensures the real user input appears as its own line for extraction.
func processCR(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	var result [][]byte
	for _, line := range lines {
		if !bytes.Contains(line, []byte("\r")) {
			result = append(result, line)
			continue
		}
		segments := bytes.Split(line, []byte("\r"))
		for _, seg := range segments {
			trimmed := bytes.TrimSpace(seg)
			if len(trimmed) > 0 {
				result = append(result, trimmed)
			}
		}
	}
	return bytes.Join(result, []byte("\n"))
}

// HandleSuggestTaskData analyzes session output and suggests task title/description/priority.
func (h *AIHandler) HandleSuggestTaskData(w http.ResponseWriter, r *http.Request) {
	p := h.getProviderForSlot(llm.SlotBackground)
	if p == nil {
		respondError(w, http.StatusServiceUnavailable, "AI not configured")
		return
	}

	sessionID := chi.URLParam(r, "id")

	output, err := h.api.sessionMgr.GetSessionOutput(sessionID)
	if err != nil || len(output) == 0 {
		respondError(w, http.StatusNotFound, "Session not running or no output")
		return
	}

	// Keep first 5KB (user's initial prompt) + last 25KB (recent activity)
	if len(output) > 30000 {
		head := output[:5000]
		tail := output[len(output)-25000:]
		output = append(head, append([]byte("\n...[truncated]...\n"), tail...)...)
	}

	// Replace cursor-forward sequences with spaces before stripping other ANSI codes.
	// \x1b[<N>C means "move cursor forward N columns" and represents visual spacing.
	// Without this, words get concatenated (e.g. "fix type check errors" → "fixtypecheckerrors").
	cursorFwdRe := regexp.MustCompile(`\x1b\[(\d*)C`)
	output = cursorFwdRe.ReplaceAllFunc(output, func(match []byte) []byte {
		// Extract N from \x1b[NC, default to 1
		sub := cursorFwdRe.FindSubmatch(match)
		n := 1
		if len(sub) > 1 && len(sub[1]) > 0 {
			if v, err := strconv.Atoi(string(sub[1])); err == nil && v > 0 && v <= 20 {
				n = v
			}
		}
		return bytes.Repeat([]byte(" "), n)
	})

	// Strip remaining ANSI escape codes and control characters from terminal output
	ansiRe := regexp.MustCompile(
		`\x1b\[\?[0-9;]*[a-zA-Z]|` + // DEC private mode (e.g. ?2004h)
			`\x1b\[[0-9;]*[a-zA-Z]|` + // Standard CSI sequences
			`\x1b\][^\x07]*\x07|` + // OSC with BEL terminator
			`\x1b\][^\x1b]*\x1b\\|` + // OSC with ST terminator
			`\x1b[^[\]].?`) // Other escape sequences
	cleanOutput := ansiRe.ReplaceAll(output, nil)
	// Remove other control characters except newline, tab, carriage return
	controlRe := regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	cleanOutput = controlRe.ReplaceAll(cleanOutput, nil)

	// Process carriage returns: split CR-separated segments into separate lines
	// so that ghost text overwrites are separated from real user input
	cleanOutput = processCR(cleanOutput)

	if len(cleanOutput) == 0 {
		respondError(w, http.StatusNotFound, "Session has no readable output")
		return
	}

	sess, err := h.api.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	model := h.getSlotModel(llm.SlotBackground)

	// Extract structured context from terminal output (heuristics + raw fallback)
	sessionContext := extractSessionContext(cleanOutput)

	prompt := fmt.Sprintf(`You are a project manager creating a task ticket based on a Claude Code session.

Project: %s
Session name: "%s"

The session context below is ordered by reliability:
- "User prompts" = what the user typed into Claude Code. This is the MOST RELIABLE source of intent.
- "Plan" = Claude Code's implementation plan. Use to add detail.
- "Raw terminal output" = cleaned terminal output for cross-reference. Use this to verify or correct the extracted prompts above. If user prompts seem wrong or unrelated, look here for the actual user input.

Session context:
%s

CRITICAL RULES:
1. Base the task PRIMARILY on user prompts (if present). These are what the user actually asked for.
2. CROSS-REFERENCE user prompts with the raw terminal output. If extracted prompts look like ghost text or placeholder suggestions (e.g. "Try edit app.js to..."), ignore them and use the raw output to find the real user intent.
3. If no user prompts are available, use the plan or session name.
4. NEVER create tasks about: "reading files", "investigating code", "exploring codebase", "analyzing structure", "running commands", "reviewing output". These are Claude Code's internal investigation steps, NOT user goals.
5. The task must describe WHAT the user wants to accomplish, not what Claude Code is doing internally.
6. If the context seems incoherent or unrelated to any clear goal, create a generic task based on the session name and project name. A vague but correct task is better than a specific but wrong one.

Respond with ONLY valid JSON, no markdown:
{"title": "...", "description": "...", "priority": "..."}

Rules:
- Title: imperative verb + objective, max 80 chars. Examples: "Implement login form validation", "Fix 500 error in payments API", "Refactor authentication module"
- Description: 2-3 sentences describing the GOAL and expected outcome. Include which area of the codebase is affected if clear from context. Write as a task assignment: what should be done and why.
- Priority: "urgent" for hotfixes/production issues, "high" for important features/bugs, "medium" for regular work, "low" for nice-to-haves
- Use English`, project.Name, sess.Name, sessionContext)

	req := &llm.Request{
		System:    "You are a task management assistant. Respond ONLY with valid JSON, no markdown.",
		Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
		MaxTokens: 256,
		Model:     model,
	}

	var fullText strings.Builder
	log.Printf("[AI-AUDIT] CALL_START subcategory=suggest_task session=%s model=%s", sessionID, model)
	resp, err := p.StreamMessage(r.Context(), req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		log.Printf("[AI-AUDIT] CALL_FAIL subcategory=suggest_task session=%s error=%v", sessionID, err)
		respondError(w, http.StatusInternalServerError, classifyAIError(err))
		return
	}
	log.Printf("[AI-AUDIT] CALL_OK subcategory=suggest_task session=%s text_len=%d", sessionID, fullText.Len())

	// Record token usage
	if resp != nil && (resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0) {
		usageModel := model
		if resp.Model != "" {
			usageModel = resp.Model
		}
		if usageModel == "" {
			usageModel = "unknown"
		}
		h.api.db.CreateTokenUsage(r.Context(), &database.TokenUsage{
			Source:              "ai_assistant",
			Subcategory:         "suggest_task",
			Model:               usageModel,
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CostUSD:             llm.CalculateCost(usageModel, resp.Usage.InputTokens, resp.Usage.OutputTokens),
		})
	}

	// Parse JSON response
	responseText := strings.TrimSpace(fullText.String())

	// Extract JSON object from response (handles markdown code blocks, leading text, etc.)
	responseText = extractJSONObject(responseText)

	var result struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    string `json:"priority"`
	}

	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		log.Printf("[AI-AUDIT] JSON parse FAILED subcategory=suggest_task session=%s error=%v response=%s", sessionID, err, fullText.String())
		respondError(w, http.StatusInternalServerError, "Failed to parse AI response")
		return
	}

	// Validate priority
	validPriorities := map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}
	if !validPriorities[result.Priority] {
		result.Priority = "medium"
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleValidateSkill validates a skill's format and content.
func (h *AIHandler) HandleValidateSkill(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	result, err := service.ValidateSkill(platformUIContext(r), input.Content, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// parseIDParam extracts an int64 from input map, trying both string and float64 types.
func parseIDParam(input map[string]interface{}, key string) (int64, error) {
	v, ok := input[key]
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	switch id := v.(type) {
	case string:
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return n, nil
	case float64:
		return int64(id), nil
	}
	return 0, fmt.Errorf("invalid %s type", key)
}

func stringInput(input map[string]interface{}, key string) string {
	v, _ := input[key].(string)
	return v
}

func boolInput(input map[string]interface{}, key string) bool {
	switch v := input[key].(type) {
	case bool:
		return v
	case string:
		return parseBoolQuery(v)
	}
	return false
}

func intInput(input map[string]interface{}, key string, fallback int) int {
	v, ok := input[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		return parseIntQuery(n, fallback)
	}
	return fallback
}

func truncateForTool(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (h *AIHandler) getAllowedSession(ctx context.Context, conversationID int64, sessionID string) (*database.Session, error) {
	sess, err := h.api.db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found")
	}
	if allowed := h.getAgentAllowedProjectIDs(ctx, conversationID); allowed != nil && !allowed[sess.ProjectID] {
		return nil, fmt.Errorf("this agent does not have access to project %d", sess.ProjectID)
	}
	return sess, nil
}

func (h *AIHandler) formatSessionForTool(ctx context.Context, sess *database.Session) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Session: %s\nName: %s\nProject ID: %d\nStatus: %s\nBackend: %s\n", sess.ID, sess.Name, sess.ProjectID, sess.Status, sess.Backend))
	meta := h.sessionMetadataForTool(ctx, sess)
	sb.WriteString(fmt.Sprintf("Model: %s\nEffort: %s\nHarness: %s\n", meta.Model, meta.Effort, meta.Harness))
	if meta.HarnessDetails != "" {
		sb.WriteString(fmt.Sprintf("Harness details: %s\n", meta.HarnessDetails))
	}
	if task, err := h.api.db.GetTaskForSession(ctx, sess.ID); err == nil && task != nil {
		sb.WriteString(fmt.Sprintf("Linked Task: #%d %s (%s, %s)\n", task.ID, task.Title, task.Status, task.Priority))
	} else {
		sb.WriteString("Linked Task: none\n")
	}
	return sb.String()
}

func (h *AIHandler) sessionMetadataForTool(ctx context.Context, sess *database.Session) sessionmeta.Metadata {
	if sess == nil {
		return sessionmeta.FromProjectConfig("", "")
	}
	project, err := h.api.db.GetProject(ctx, sess.ProjectID)
	if err != nil || project == nil {
		return sessionmeta.WithSessionValues(sessionmeta.FromProjectConfig(sess.Backend, ""), sess.Model, sess.Effort, sess.Harness)
	}
	backend := sess.Backend
	if strings.TrimSpace(backend) == "" {
		backend = project.Backend
	}
	return sessionmeta.WithSessionValues(sessionmeta.FromProjectConfig(backend, project.BackendConfig), sess.Model, sess.Effort, sess.Harness)
}

func (h *AIHandler) linkSessionTaskForTool(ctx context.Context, sess *database.Session, input map[string]interface{}) (*database.ProjectTask, error) {
	if existingTask, _ := h.api.db.GetTaskForSession(ctx, sess.ID); existingTask != nil {
		if _, err := h.api.db.UnlinkSessionFromTask(ctx, sess.ID); err != nil {
			return nil, fmt.Errorf("failed to unlink existing task")
		}
		h.api.recordTaskHistory(ctx, existingTask.ID, existingTask.ProjectID, "session_unlinked", map[string]interface{}{
			"session_id": sess.ID, "session_name": sess.Name,
		}, "user", sess.ID)
	}

	var task *database.ProjectTask
	if raw, ok := input["task_id"]; ok && fmt.Sprintf("%v", raw) != "" {
		taskID, err := parseIDParam(input, "task_id")
		if err != nil {
			return nil, err
		}
		found, err := h.api.db.GetTask(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("task not found")
		}
		if found.ProjectID != sess.ProjectID {
			return nil, fmt.Errorf("task belongs to a different project")
		}
		task = found
	} else {
		title, _ := input["title"].(string)
		if strings.TrimSpace(title) == "" {
			return nil, fmt.Errorf("task_id or title is required")
		}
		priority := stringInput(input, "priority")
		if priority == "" {
			priority = "medium"
		}
		task = &database.ProjectTask{
			ProjectID:   sess.ProjectID,
			Title:       strings.TrimSpace(title),
			Description: stringInput(input, "description"),
			Status:      "in_progress",
			Priority:    priority,
		}
		if err := h.api.db.CreateTask(ctx, task); err != nil {
			return nil, err
		}
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "created", "project_id": sess.ProjectID, "task": task})
	}

	newName := "Task: " + task.Title
	if err := h.api.db.LinkSessionToTask(ctx, sess.ID, task.ID); err != nil {
		return nil, err
	}
	h.api.db.ExecContext(ctx, "UPDATE sessions SET name = ? WHERE id = ?", newName, sess.ID)
	h.api.hub.BroadcastStateUpdate("session", map[string]interface{}{"action": "renamed", "session_id": sess.ID, "name": newName})
	h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "session_linked", map[string]interface{}{"session_id": sess.ID, "session_name": newName}, "user", sess.ID)
	h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "task_assigned", map[string]interface{}{"session_id": sess.ID, "session_name": newName}, "system", sess.ID)

	if task.Status == "todo" {
		oldStatus := task.Status
		if err := h.api.db.UpdateTaskStatus(ctx, task.ID, "in_progress"); err == nil {
			task.Status = "in_progress"
			h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": task.ProjectID, "task": task})
			h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "status_change", map[string]interface{}{"old": oldStatus, "new": "in_progress", "reason": "auto_on_link"}, "system", sess.ID)
		}
	}

	return task, nil
}

func (h *AIHandler) updateTaskFromSessionTool(ctx context.Context, task *database.ProjectTask, input map[string]interface{}, sessionID string) (bool, error) {
	oldStatus, oldPriority, oldTitle, oldDescription := task.Status, task.Priority, task.Title, task.Description
	changed := false
	if v, ok := input["title"].(string); ok && strings.TrimSpace(v) != "" {
		task.Title = strings.TrimSpace(v)
		changed = true
	}
	if v, ok := input["description"].(string); ok {
		task.Description = v
		changed = true
	}
	if v, ok := input["status"].(string); ok && v != "" {
		valid := map[string]bool{"todo": true, "in_progress": true, "awaiting_approval": true, "done": true}
		if !valid[v] {
			return false, fmt.Errorf("invalid status")
		}
		task.Status = v
		changed = true
	}
	if v, ok := input["priority"].(string); ok && v != "" {
		valid := map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}
		if !valid[v] {
			return false, fmt.Errorf("invalid priority")
		}
		task.Priority = v
		changed = true
	}
	if v, ok := input["due_date"].(string); ok {
		if v == "" {
			task.DueDate = sql.NullTime{}
		} else {
			t, err := parseFlexibleTime(v)
			if err != nil {
				return false, fmt.Errorf("invalid due_date")
			}
			task.DueDate = sql.NullTime{Time: t, Valid: true}
			task.DueNotified = false
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err := h.api.db.UpdateTask(ctx, task); err != nil {
		return false, err
	}
	h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": task.ProjectID, "task": task})
	if task.Status != oldStatus {
		h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "status_change", map[string]interface{}{"old": oldStatus, "new": task.Status}, "user", sessionID)
	}
	if task.Priority != oldPriority {
		h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "priority_change", map[string]interface{}{"old": oldPriority, "new": task.Priority}, "user", sessionID)
	}
	if task.Title != oldTitle {
		h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "title_updated", map[string]interface{}{"old": oldTitle, "new": task.Title}, "user", sessionID)
	}
	if task.Description != oldDescription {
		h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "description_updated", map[string]interface{}{}, "user", sessionID)
	}
	return true, nil
}

func (h *AIHandler) executeChatDashboard(ctx context.Context, conversationID int64) (string, error) {
	projects, err := h.api.db.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	if allowed := h.getAgentAllowedProjectIDs(ctx, conversationID); allowed != nil {
		filtered := make([]database.Project, 0, len(projects))
		for _, p := range projects {
			if allowed[p.ID] {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}
	sessions, _ := h.api.db.ListSessions(ctx)
	activeByProject := map[int64]int{}
	for _, s := range sessions {
		if s.Status == "running" || s.Status == "starting" {
			activeByProject[s.ProjectID]++
		}
	}
	var sb strings.Builder
	sb.WriteString("OpenPoet dashboard:\n")
	for _, p := range projects {
		summary, _ := h.api.db.GetTaskSummaryByProject(ctx, p.ID)
		sb.WriteString(fmt.Sprintf("- [%d] %s | sessions active: %d | tasks todo:%d in_progress:%d awaiting:%d done:%d\n",
			p.ID, p.Name, activeByProject[p.ID], summary["todo"], summary["in_progress"], summary["awaiting_approval"], summary["done"]))
	}
	if len(projects) == 0 {
		sb.WriteString("No projects available.\n")
	}
	return sb.String(), nil
}

// EvaluateSession uses AI to evaluate a session and proactively suggest task actions.
// Triggered on session_start, session_end, user_prompt, plan_accepted, or session_request.
func (h *AIHandler) EvaluateSession(ctx context.Context, sessionID string, trigger string, outputSnapshot []byte) bool {
	log.Printf("[AI-Session] EvaluateSession called: session=%s trigger=%s outputLen=%d", sessionID[:8], trigger, len(outputSnapshot))

	p := h.getProviderForSlot(llm.SlotBackground)
	if p == nil {
		log.Printf("[AI-Session] ABORTED: provider is nil")
		return false
	}
	log.Printf("[AI-Session] Provider OK: %T", p)

	// Skip all evaluations if disabled in settings
	val, _ := h.api.db.GetSetting(ctx, "task_auto_eval_enabled")
	if val != "true" {
		log.Printf("[AI-Session] Task evaluation disabled (trigger=%s), skipping", trigger)
		return false
	}

	sess, err := h.api.db.GetSession(ctx, sessionID)
	if err != nil {
		log.Printf("[AI-Session] Session %s not found: %v", sessionID, err)
		return false
	}

	project, err := h.api.db.GetProject(ctx, sess.ProjectID)
	if err != nil {
		log.Printf("[AI-Session] Project %d not found: %v", sess.ProjectID, err)
		return false
	}
	log.Printf("[AI-Session] Context: project=%s session_status=%s tasks_query_start", project.Name, sess.Status)

	tasks, _ := h.api.db.ListTasksByProject(ctx, project.ID)

	// Check for linked task
	var linkedTask *database.ProjectTask
	if lt, err := h.api.db.GetTaskForSession(ctx, sessionID); err == nil {
		linkedTask = lt
	}
	hasLinkedTask := linkedTask != nil

	// Early exits based on trigger + linked task state
	if trigger == "session_end" && !hasLinkedTask {
		log.Printf("[AI-Session] Skipping session_end: no linked task (no point creating task for finished work)")
		return false
	}

	// Format tasks list (exclude done tasks to avoid suggesting completed work)
	var tasksList string
	var activeTasks int
	for _, t := range tasks {
		if t.Status == "done" {
			continue
		}
		activeTasks++
		tasksList += fmt.Sprintf("- [%d] %s (status: %s, priority: %s)\n", t.ID, t.Title, t.Status, t.Priority)
		if t.Description != "" {
			desc := t.Description
			if len(desc) > 100 {
				desc = desc[:100] + "..."
			}
			tasksList += fmt.Sprintf("  Description: %s\n", desc)
		}
	}
	if activeTasks == 0 {
		tasksList = "No active tasks exist for this project."
	}

	linkedTaskInfo := ""
	if hasLinkedTask {
		linkedTaskInfo = fmt.Sprintf("\nThis session is linked to task: [%d] %s (status: %s)", linkedTask.ID, linkedTask.Title, linkedTask.Status)
	}

	// Truncate output to ~30KB
	outputStr := ""
	if len(outputSnapshot) > 0 {
		if len(outputSnapshot) > 30000 {
			outputSnapshot = outputSnapshot[len(outputSnapshot)-30000:]
		}
		outputStr = string(outputSnapshot)
	}

	// Build trigger-specific prompt
	var triggerDesc string
	var instructions string

	// Allowed suggestion types per trigger (used for post-LLM filtering)
	allowedTypes := map[string]bool{}

	switch trigger {
	case "session_start":
		// Skip: no terminal output yet, so no context to evaluate.
		// Task linking is handled by user_prompt trigger (with actual context)
		// and by the UI modal shown before session start.
		log.Printf("[AI-Session] Skipping session_start: no context to evaluate (task suggestions deferred to user_prompt)")
		return false

	case "session_end":
		triggerDesc = "ended"
		instructions = `- This session is linked to a task. Evaluate whether the task status should change based on what was accomplished.
- If the work appears complete, suggest "complete_task".
- If progress was made but work is not finished, suggest "update_task" with a status change or description update.
- Do NOT suggest "create_task" or "link_task" — the session already has a linked task.
- Valid types for this trigger: "complete_task" (needs task_id), "update_task" (needs task_id)
- Return empty suggestions array if no status change is warranted.`
		allowedTypes["complete_task"] = true
		allowedTypes["update_task"] = true

	case "user_prompt":
		triggerDesc = "is currently running (user submitted a prompt)"
		allowedTypes["create_task"] = true
		if hasLinkedTask {
			instructions = fmt.Sprintf(`- This session is linked to task [%d] "%s". Based on the session output, suggest creating a NEW separate task if the work reveals something that should be tracked independently.
- Do NOT suggest "link_task" — the session already has a linked task.
- Valid types for this trigger: "create_task" (needs task_data)
- Return empty suggestions array if no new task is warranted.`, linkedTask.ID, linkedTask.Title)
		} else {
			instructions = `- This session has NO linked task yet. Based on the session output, suggest creating a new task or linking to an existing one.
- Valid types for this trigger: "create_task" (needs task_data), "link_task" (needs task_id)
- Return empty suggestions array if it's too early to determine the session's purpose.`
			allowedTypes["link_task"] = true
		}

	case "plan_accepted":
		if hasLinkedTask {
			// Drift detection: session already has a task, but a new plan was approved
			triggerDesc = "is currently running (a NEW plan was just approved, but the session is ALREADY linked to a task)"
			// Fetch plan content for drift comparison
			planContent, _, _ := h.api.db.GetSessionPlan(ctx, sessionID)
			if len(planContent) > 5000 {
				planContent = planContent[:5000] + "\n... (truncated)"
			}
			taskDesc := linkedTask.Description
			if len(taskDesc) > 2000 {
				taskDesc = taskDesc[:2000] + "\n... (truncated)"
			}
			instructions = fmt.Sprintf(`- This session is ALREADY linked to task [%d] "%s".
- Task description: %s
- A NEW plan was just approved in this session. Here is the plan content:

%s

- Compare the plan content against the linked task's title and description.
- If the plan describes work that is DIFFERENT from the linked task (scope drift), suggest "unlink_task" with task_data describing the NEW work from the plan.
- If the plan is a continuation, refinement, or sub-task of the linked task, return empty suggestions (no drift).
- If the task description should be updated to reflect what the plan describes, suggest "update_task".
- Valid types for this trigger: "unlink_task" (needs task_data with title+description+priority for the new task), "update_task" (needs task_id and task_data)
- Be conservative: only suggest "unlink_task" when the plan is clearly about different work, not just a different approach to the same task.`, linkedTask.ID, linkedTask.Title, taskDesc, planContent)
			allowedTypes["unlink_task"] = true
			allowedTypes["update_task"] = true
		} else {
			triggerDesc = "is currently running (a plan was just approved)"
			instructions = `- This session has NO linked task yet. A plan was approved, which gives clear indication of what will be done.
- Suggest creating a task that describes the planned work.
- Valid types for this trigger: "create_task" (needs task_data), "link_task" (needs task_id)
- Return empty suggestions array only if the plan is trivial.`
			allowedTypes["create_task"] = true
			allowedTypes["link_task"] = true
		}

	default: // session_request
		triggerDesc = "is currently running (evaluation requested by the session)"
		instructions = `- Analyze the context and suggest relevant task actions.
- Valid types: "link_task" (needs task_id), "create_task" (needs task_data), "update_task" (needs task_id), "complete_task" (needs task_id)
- Return empty suggestions array if no action is warranted.`
		allowedTypes["link_task"] = true
		allowedTypes["create_task"] = true
		allowedTypes["update_task"] = true
		allowedTypes["complete_task"] = true
	}

	prompt := fmt.Sprintf(`You are the OpenPoet AI Assistant evaluating a session that just %s.

Project: %s (%s, %s)
Session: %s (name: %s, status: %s)%s`, triggerDesc, project.Name, project.Type, project.Path, sess.ID[:8], sess.Name, sess.Status, linkedTaskInfo)

	if outputStr != "" {
		prompt += fmt.Sprintf(`

Recent session output (last ~30KB):
%s`, outputStr)
	}

	prompt += fmt.Sprintf(`

Current tasks for this project:
%s
Instructions:
%s
- Respond with JSON only: {"suggestions": [{"type": "<type>", "title": "<title>", "description": "<why>", "task_id": <id_or_null>, "task_data": {"title": "...", "description": "...", "priority": "..."}}], "summary": "<1-2 sentence summary of what was accomplished in this session, or empty string>"}
- The "summary" field should describe what was accomplished. Only include it for session_end triggers. Use empty string for other triggers.
- Be judicious - only suggest when it truly makes sense.
- Use English for title and description fields.`, tasksList, instructions)

	model := h.getSlotModel(llm.SlotBackground)

	log.Printf("[AI-Session] Calling LLM: model=%s promptLen=%d", model, len(prompt))

	req := &llm.Request{
		System:    "You are a task management assistant. Respond ONLY with valid JSON, no markdown code blocks.",
		Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
		MaxTokens: 2048,
		Model:     model,
	}

	var fullText strings.Builder
	log.Printf("[AI-AUDIT] CALL_START subcategory=session_eval session=%s trigger=%s model=%s", sessionID[:8], trigger, model)
	evalResp, err := p.StreamMessage(ctx, req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		log.Printf("[AI-Session] LLM call FAILED for session %s: %v", sessionID, err)
		log.Printf("[AI-AUDIT] CALL_FAIL subcategory=session_eval session=%s error=%v", sessionID[:8], err)
		return false
	}
	log.Printf("[AI-AUDIT] CALL_OK subcategory=session_eval session=%s text_len=%d", sessionID[:8], fullText.Len())

	// Record token usage
	if evalResp != nil && (evalResp.Usage.InputTokens > 0 || evalResp.Usage.OutputTokens > 0) {
		usageModel := model
		if evalResp.Model != "" {
			usageModel = evalResp.Model
		}
		if usageModel == "" {
			usageModel = "unknown"
		}
		h.api.db.CreateTokenUsage(ctx, &database.TokenUsage{
			Source:              "ai_assistant",
			Subcategory:         "session_eval",
			Model:               usageModel,
			InputTokens:         evalResp.Usage.InputTokens,
			OutputTokens:        evalResp.Usage.OutputTokens,
			CacheReadTokens:     evalResp.Usage.CacheReadTokens,
			CacheCreationTokens: evalResp.Usage.CacheCreationTokens,
			CostUSD:             llm.CalculateCost(usageModel, evalResp.Usage.InputTokens, evalResp.Usage.OutputTokens),
		})
	}

	// Parse response
	var result struct {
		Suggestions []struct {
			Type        string                 `json:"type"`
			Title       string                 `json:"title"`
			Description string                 `json:"description"`
			TaskID      *int64                 `json:"task_id"`
			TaskData    map[string]interface{} `json:"task_data"`
		} `json:"suggestions"`
		Summary string `json:"summary"`
	}

	responseText := strings.TrimSpace(fullText.String())
	log.Printf("[AI-Session] LLM response for session %s: %s", sessionID[:8], responseText)

	responseText = extractJSONObject(responseText)

	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		log.Printf("[AI-Session] JSON parse FAILED for session %s: %v\nResponse: %s", sessionID, err, responseText)
		return false
	}

	// Persist the structured summary before suggestion early exits. Reports are
	// lifecycle records, not a side effect of whether a task suggestion exists.
	if trigger == "session_end" && strings.TrimSpace(result.Summary) != "" {
		if hasLinkedTask {
			h.api.recordTaskHistory(ctx, linkedTask.ID, linkedTask.ProjectID, "session_ended", map[string]interface{}{
				"session_id": sessionID, "summary": result.Summary,
			}, "ai", sessionID)
		}
		if h.reports != nil {
			if _, err := h.reports.EnrichSessionSummary(ctx, sessionID, result.Summary); err != nil {
				log.Printf("[AI-Session] Failed to enrich structured report for session %s: %v", sessionID, err)
			}
		}
	}

	if len(result.Suggestions) == 0 {
		log.Printf("[AI-Session] No suggestions for session %s (%s)", sessionID[:8], trigger)
		return false
	}

	// Post-LLM filter: remove suggestion types not allowed for this trigger
	var filtered []struct {
		Type        string                 `json:"type"`
		Title       string                 `json:"title"`
		Description string                 `json:"description"`
		TaskID      *int64                 `json:"task_id"`
		TaskData    map[string]interface{} `json:"task_data"`
	}
	for _, s := range result.Suggestions {
		if !allowedTypes[s.Type] {
			log.Printf("[AI-Session] Filtered out invalid type '%s' for trigger '%s' (session %s)", s.Type, trigger, sessionID[:8])
			continue
		}
		// Discard link_task if it suggests linking to the already-linked task
		if s.Type == "link_task" && hasLinkedTask && s.TaskID != nil && *s.TaskID == linkedTask.ID {
			log.Printf("[AI-Session] Filtered out link_task: session already linked to task [%d] %s", linkedTask.ID, linkedTask.Title)
			continue
		}
		filtered = append(filtered, s)
	}
	result.Suggestions = filtered

	if len(result.Suggestions) == 0 {
		log.Printf("[AI-Session] All suggestions filtered out for session %s (%s)", sessionID[:8], trigger)
		return false
	}

	log.Printf("[AI-Session] Got %d suggestions for session %s (after filter)", len(result.Suggestions), sessionID[:8])

	// Auto-update: if enabled and trigger is session_end with linked task, apply changes directly
	autoUpdateVal, _ := h.api.db.GetSetting(ctx, "task_auto_update_enabled")
	if autoUpdateVal == "true" && trigger == "session_end" && hasLinkedTask {
		for _, s := range result.Suggestions {
			switch s.Type {
			case "complete_task":
				taskID := linkedTask.ID
				if s.TaskID != nil {
					taskID = *s.TaskID
				}
				oldStatus := linkedTask.Status
				// Transition to awaiting_approval instead of done, triggering verification doc
				if err := h.api.db.UpdateTaskStatus(ctx, taskID, "awaiting_approval"); err == nil {
					t, _ := h.api.db.GetTask(ctx, taskID)
					if t != nil {
						h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": t.ProjectID, "task": t})
						go h.GenerateVerificationDoc(context.Background(), t)
					}
					h.api.recordTaskHistory(ctx, taskID, linkedTask.ProjectID, "status_change", map[string]interface{}{
						"old": oldStatus, "new": "awaiting_approval", "reason": "auto_update",
					}, "ai", sessionID)
					log.Printf("[AI-Session] Auto-moved task %d to awaiting_approval for session %s", taskID, sessionID[:8])
				}
			case "update_task":
				if s.TaskID != nil {
					if t, err := h.api.db.GetTask(ctx, *s.TaskID); err == nil && t != nil {
						oldStatus := t.Status
						changed := false
						if status, ok := s.TaskData["status"].(string); ok && status != "" && status != t.Status {
							t.Status = status
							changed = true
						}
						if desc, ok := s.TaskData["description"].(string); ok && desc != "" {
							t.Description = desc
							changed = true
						}
						if changed {
							h.api.db.UpdateTask(ctx, t)
							h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": t.ProjectID, "task": t})
							if t.Status != oldStatus {
								h.api.recordTaskHistory(ctx, t.ID, t.ProjectID, "status_change", map[string]interface{}{
									"old": oldStatus, "new": t.Status, "reason": "auto_update",
								}, "ai", sessionID)
							}
							log.Printf("[AI-Session] Auto-updated task %d for session %s", t.ID, sessionID[:8])
						}
					}
				}
			}
		}
		log.Printf("[AI-Session] Auto-update applied for session %s, skipping suggestion creation", sessionID[:8])
		return true
	}

	// Create and broadcast each suggestion with proactive conversation
	typeLabels := map[string]string{
		"link_task":     "Link Task",
		"create_task":   "New Task",
		"update_task":   "Update Task",
		"complete_task": "Complete Task",
	}

	for _, s := range result.Suggestions {
		contextData := map[string]interface{}{
			"task_id":   s.TaskID,
			"task_data": s.TaskData,
		}
		contextJSON, _ := json.Marshal(contextData)

		// Build assistant message for the proactive conversation
		typeLabel := typeLabels[s.Type]
		if typeLabel == "" {
			typeLabel = s.Type
		}
		assistantMsg := fmt.Sprintf("While analyzing the session for project **%s**, I identified an opportunity:\n\n"+
			"**Type:** %s\n"+
			"**Title:** %s\n", project.Name, typeLabel, s.Title)
		if s.Description != "" {
			assistantMsg += fmt.Sprintf("**Description:** %s\n", s.Description)
		}
		assistantMsg += "\nI can help refine this suggestion. What would you like to do?"

		// Create proactive conversation
		proactiveExtra := map[string]interface{}{
			"session_id": sessionID,
			"project_id": project.ID,
			"task_id":    s.TaskID,
			"task_data":  s.TaskData,
		}
		proactiveContextJSON, _ := json.Marshal(proactiveExtra)
		conv, err := h.api.db.CreateProactiveConversation(ctx, s.Title, "standard", "task_suggestion", string(proactiveContextJSON), assistantMsg)
		if err != nil {
			log.Printf("[AI-Session] Failed to create proactive conversation: %v", err)
		}

		suggestion := &database.AISuggestion{
			SessionID:   sessionID,
			ProjectID:   project.ID,
			Type:        s.Type,
			Title:       s.Title,
			Description: s.Description,
			ContextJSON: string(contextJSON),
			Status:      "pending",
			Level:       "standard",
		}
		if conv != nil {
			suggestion.ConversationID = sql.NullInt64{Int64: conv.ID, Valid: true}
		}

		if err := h.api.db.CreateAISuggestion(ctx, suggestion); err != nil {
			log.Printf("[AI-Session] Failed to save suggestion: %v", err)
			continue
		}

		log.Printf("[AI-Session] Suggestion created: [%d] %s - %s (conv=%v)", suggestion.ID, s.Type, s.Title, suggestion.ConversationID)

		// Broadcast as proactive notification
		actions := []ProactiveAction{
			{Label: "Accept", Action: "accept", Style: "primary"},
			{Label: "Discuss", Action: "discuss", Style: "outline"},
			{Label: "Ignore", Action: "dismiss", Style: "secondary"},
		}
		payload := map[string]interface{}{
			"level":          "standard",
			"proactive_type": "task_suggestion",
			"title":          s.Title,
			"body":           s.Description,
			"suggestion_id":  suggestion.ID,
			"actions":        actions,
		}
		if conv != nil {
			payload["conversation_id"] = conv.ID
		}
		h.api.hub.BroadcastAIProactive(payload)
	}
	return true
}

// ListSuggestions returns pending AI suggestions.
func (h *AIHandler) ListSuggestions(w http.ResponseWriter, r *http.Request) {
	suggestions, err := h.api.db.ListPendingAISuggestions(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if suggestions == nil {
		suggestions = []database.AISuggestion{}
	}
	respondJSON(w, http.StatusOK, suggestions)
}

// AcceptSuggestion accepts an AI suggestion and executes the corresponding action.
func (h *AIHandler) AcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid suggestion ID")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	result, err := service.AcceptSuggestion(platformUIContext(r), id, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": result.Status, "message": result.Message})
}

// DismissSuggestion dismisses an AI suggestion.
func (h *AIHandler) DismissSuggestion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid suggestion ID")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	if err := service.DismissSuggestion(platformUIContext(r), id, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// ProactiveAction represents an action button for a proactive AI notification.
type ProactiveAction struct {
	Label  string `json:"label"`
	Action string `json:"action"` // 'accept', 'discuss', 'dismiss', 'open', etc.
	Style  string `json:"style"`  // 'primary', 'outline', 'secondary'
}

// CreateProactiveNotification creates an AI-initiated conversation with a notification broadcast.
// This is the central framework method for AI proactive interactions.
func (h *AIHandler) CreateProactiveNotification(ctx context.Context, level, pType, title, assistantMsg string, actions []ProactiveAction, extra map[string]interface{}) (*database.AIConversation, error) {
	contextJSON := "{}"
	if extra != nil {
		if b, err := json.Marshal(extra); err == nil {
			contextJSON = string(b)
		}
	}

	conv, err := h.api.db.CreateProactiveConversation(ctx, title, level, pType, contextJSON, assistantMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to create proactive conversation: %w", err)
	}

	// Build broadcast payload
	payload := map[string]interface{}{
		"level":           level,
		"proactive_type":  pType,
		"title":           title,
		"body":            assistantMsg,
		"conversation_id": conv.ID,
		"actions":         actions,
	}
	// Merge extra fields into payload
	for k, v := range extra {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}

	h.api.hub.BroadcastAIProactive(payload)

	return conv, nil
}

// HandleDiscussSuggestion returns or creates a conversation for discussing a suggestion.
func (h *AIHandler) HandleDiscussSuggestion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid suggestion ID")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	conversation, err := service.DiscussSuggestion(platformUIContext(r), id, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation.ID})
}

// HandleMarkRead marks an AI conversation as read.
func (h *AIHandler) HandleMarkRead(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}

	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	if err := service.MarkConversationRead(platformUIContext(r), id, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleUnreadCount returns the count of unread AI-initiated conversations.
func (h *AIHandler) HandleUnreadCount(w http.ResponseWriter, r *http.Request) {
	count, err := h.api.db.CountUnreadProactive(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]int{"count": count})
}

// HandleTestProactive sends a test proactive notification at a given level.
// POST /ai/test-proactive with JSON body: {"level": "critical|standard|subtle"}
func (h *AIHandler) HandleTestProactive(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	service, ok := h.sharedAIAssistantService(w)
	if !ok {
		return
	}
	conversation, err := service.TestProactive(platformUIContext(r), input.Level, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	if input.Level != "critical" && input.Level != "subtle" {
		input.Level = "standard"
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "sent", "level": input.Level, "conversation_id": conversation.ID})
}

// GenerateVerificationDoc generates a verification document for a task transitioning to awaiting_approval.
// It gathers context (project, sessions, history) and calls the LLM to generate the document.
// If no LLM provider is available, it creates a fallback document.
func (h *AIHandler) GenerateVerificationDoc(ctx context.Context, task *database.ProjectTask) {
	log.Printf("[AI-Verification] Generating verification doc for task %d: %s", task.ID, task.Title)

	// Get project info
	project, err := h.api.db.GetProject(ctx, task.ProjectID)
	projectName := "Unknown"
	if err == nil && project != nil {
		projectName = project.Name
	}

	// Get linked sessions
	sessions, _ := h.api.db.GetSessionsForTask(ctx, task.ID)
	var sessionSummaries []string
	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = s.ID[:8]
		}
		sessionSummaries = append(sessionSummaries, fmt.Sprintf("%s (%s)", name, s.Status))
	}

	// Get task history
	history, _ := h.api.db.ListTaskHistory(ctx, task.ID, 50)
	var historyEntries []string
	for _, entry := range history {
		historyEntries = append(historyEntries, fmt.Sprintf("[%s] %s — %s", entry.EventType, entry.Details, entry.Actor))
	}

	// Try to get LLM provider
	p := h.getProviderForSlot(llm.SlotBackground)

	if p == nil {
		log.Printf("[AI-Verification] No LLM provider configured for task %d", task.ID)
		h.broadcastVerificationError(task, "No AI provider configured for the background slot. Configure one in Settings > AI Providers.")
		return
	}

	prompt := llm.VerificationDocPrompt(task.Title, task.Description, projectName, sessionSummaries, historyEntries)
	model := h.getSlotModel(llm.SlotBackground)

	req := &llm.Request{
		System:    "You are a technical documentation assistant. Generate clear, actionable verification documents in English.",
		Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
		MaxTokens: 4096,
		Model:     model,
	}

	var fullText strings.Builder
	log.Printf("[AI-AUDIT] CALL_START subcategory=verification_doc task=%d model=%s", task.ID, model)
	resp, err := p.StreamMessage(ctx, req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		log.Printf("[AI-Verification] LLM call failed for task %d: %v", task.ID, err)
		log.Printf("[AI-AUDIT] CALL_FAIL subcategory=verification_doc task=%d error=%v", task.ID, err)
		h.broadcastVerificationError(task, fmt.Sprintf("AI failed to generate verification document: %v", err))
		return
	}

	log.Printf("[AI-AUDIT] CALL_OK subcategory=verification_doc task=%d text_len=%d", task.ID, fullText.Len())
	content := strings.TrimSpace(fullText.String())
	// Record token usage
	if resp != nil && (resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0) {
		usageModel := model
		if resp.Model != "" {
			usageModel = resp.Model
		}
		if usageModel == "" {
			usageModel = "unknown"
		}
		h.api.db.CreateTokenUsage(ctx, &database.TokenUsage{
			Source:              "ai_assistant",
			Subcategory:         "verification_doc",
			Model:               usageModel,
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CostUSD:             llm.CalculateCost(usageModel, resp.Usage.InputTokens, resp.Usage.OutputTokens),
		})
	}

	h.persistVerificationDoc(ctx, task, content)
}

// broadcastVerificationError sends a notification toast, records a task history event,
// and broadcasts a state update so the frontend can replace the spinner with an error message.
func (h *AIHandler) broadcastVerificationError(task *database.ProjectTask, reason string) {
	h.api.hub.BroadcastNotification(map[string]interface{}{
		"title": "Verification doc failed",
		"body":  reason,
		"type":  "error",
	})
	// Record a history event so the frontend knows the error even after page refresh
	h.api.recordTaskHistory(context.Background(), task.ID, task.ProjectID, "verification_doc_failed", map[string]interface{}{
		"error": reason,
	}, "ai", "")
	// Broadcast a task-specific event so the frontend can update the spinner in real-time
	h.api.hub.BroadcastStateUpdate("verification_error", map[string]interface{}{
		"task_id":    task.ID,
		"project_id": task.ProjectID,
		"error":      reason,
	})
}

// persistVerificationDoc saves the verification document and links it to the task.
func (h *AIHandler) persistVerificationDoc(ctx context.Context, task *database.ProjectTask, content string) {
	docID := uuid.New().String()[:8]
	doc := &database.TempDocument{
		ID:      docID,
		Title:   fmt.Sprintf("Verification: %s", task.Title),
		Content: content,
		Status:  "pending",
		TaskID:  sql.NullInt64{Int64: task.ID, Valid: true},
	}

	if err := h.api.db.CreateTempDocument(ctx, doc); err != nil {
		log.Printf("[AI-Verification] Failed to create document for task %d: %v", task.ID, err)
		return
	}

	if err := h.api.db.SetTaskVerificationDoc(ctx, task.ID, docID); err != nil {
		log.Printf("[AI-Verification] Failed to link document to task %d: %v", task.ID, err)
		return
	}

	// Re-fetch task and broadcast update so UI shows the document link
	updatedTask, err := h.api.db.GetTask(ctx, task.ID)
	if err == nil {
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": task.ProjectID, "task": updatedTask})
	}

	h.api.recordTaskHistory(ctx, task.ID, task.ProjectID, "verification_doc_created", map[string]interface{}{
		"doc_id": docID,
	}, "ai", "")

	log.Printf("[AI-Verification] Created verification doc %s for task %d", docID, task.ID)
}

// getAgentAllowedProjectIDs returns the set of allowed project IDs for a conversation's agent.
// Returns nil if no filter is set (all projects allowed).
func (h *AIHandler) getAgentAllowedProjectIDs(ctx context.Context, conversationID int64) map[int64]bool {
	if conversationID <= 0 {
		return nil
	}
	conv, err := h.api.db.GetAIConversation(ctx, conversationID)
	if err != nil || !conv.AgentID.Valid {
		return nil
	}
	agent, err := h.api.db.GetAIAgent(ctx, conv.AgentID.Int64)
	if err != nil || agent.ProjectFilter == "" {
		return nil
	}
	projects, _ := h.api.db.ListProjects(ctx)
	filtered := h.filterProjectsByAgent(ctx, projects, agent.ProjectFilter)
	if len(filtered) == len(projects) {
		return nil // no restriction
	}
	allowed := make(map[int64]bool, len(filtered))
	for _, p := range filtered {
		allowed[p.ID] = true
	}
	return allowed
}

// filterToolsByPolicy filters tools based on an agent's tool policy JSON.
// Policy format: {"allowed":["tool1","tool2"]} or {"denied":["tool3"]}. Empty means all tools.
func filterToolsByPolicy(tools []llm.ToolDefinition, policyJSON string) []llm.ToolDefinition {
	var policy struct {
		Allowed []string `json:"allowed"`
		Denied  []string `json:"denied"`
	}
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		return tools
	}
	if len(policy.Allowed) > 0 {
		allowSet := make(map[string]bool, len(policy.Allowed))
		for _, name := range policy.Allowed {
			allowSet[name] = true
		}
		var filtered []llm.ToolDefinition
		for _, t := range tools {
			if allowSet[t.Name] {
				filtered = append(filtered, t)
			}
		}
		return filtered
	}
	if len(policy.Denied) > 0 {
		denySet := make(map[string]bool, len(policy.Denied))
		for _, name := range policy.Denied {
			denySet[name] = true
		}
		var filtered []llm.ToolDefinition
		for _, t := range tools {
			if !denySet[t.Name] {
				filtered = append(filtered, t)
			}
		}
		return filtered
	}
	return tools
}

// filterProjectsByAgent filters projects based on an agent's project_filter JSON.
// Format: {"project_ids":[1,2],"tag_ids":[3,4]}. Projects matching either criterion are included.
func (h *AIHandler) filterProjectsByAgent(ctx context.Context, projects []database.Project, filterJSON string) []database.Project {
	var filter struct {
		ProjectIDs []int64 `json:"project_ids"`
		TagIDs     []int64 `json:"tag_ids"`
	}
	if err := json.Unmarshal([]byte(filterJSON), &filter); err != nil {
		return projects
	}
	if len(filter.ProjectIDs) == 0 && len(filter.TagIDs) == 0 {
		return projects
	}

	// Build set of allowed project IDs
	allowed := make(map[int64]bool)
	for _, pid := range filter.ProjectIDs {
		allowed[pid] = true
	}

	// If tag IDs specified, find projects with those tags
	if len(filter.TagIDs) > 0 {
		tagSet := make(map[int64]bool, len(filter.TagIDs))
		for _, tid := range filter.TagIDs {
			tagSet[tid] = true
		}
		allPT, _ := h.api.db.ListAllProjectTagDetails(ctx)
		for _, pt := range allPT {
			if tagSet[pt.TagID] {
				allowed[pt.ProjectID] = true
			}
		}
	}

	var filtered []database.Project
	for _, p := range projects {
		if allowed[p.ID] {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// sendSSE writes an SSE event to the response.
func (h *AIHandler) sendSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	jsonData, err := json.Marshal(map[string]interface{}{
		"type": eventType,
		"data": data,
	})
	if err != nil {
		return
	}

	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()
}
