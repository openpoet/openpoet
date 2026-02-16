package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"devmanager/internal/database"
	"devmanager/internal/files"
	"devmanager/internal/llm"
	"devmanager/internal/mcp"

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
		defer a.mu.Unlock()
		for i, s := range a.subs {
			if s == ch {
				a.subs = append(a.subs[:i], a.subs[i+1:]...)
				break
			}
		}
		// drain channel
		for range ch {
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
	api      *API
	provider llm.Provider

	mu          sync.RWMutex
	rateLimiter map[string][]time.Time // simple per-minute rate limiting

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
}

// NewAIHandler creates a new AI handler.
func NewAIHandler(api *API, provider llm.Provider) *AIHandler {
	return &AIHandler{
		api:          api,
		provider:     provider,
		rateLimiter:  make(map[string][]time.Time),
		activeStreams: make(map[int64]*activeStreamInfo),
	}
}

func (h *AIHandler) registerStream(convID int64, cancel context.CancelFunc) {
	h.activeStreamsMu.Lock()
	defer h.activeStreamsMu.Unlock()
	// Cancel any existing stream for this conversation
	if existing, ok := h.activeStreams[convID]; ok {
		existing.cancel()
		existing.closeAll()
	}
	h.activeStreams[convID] = &activeStreamInfo{cancel: cancel}
}

func (h *AIHandler) unregisterStream(convID int64) {
	h.activeStreamsMu.Lock()
	defer h.activeStreamsMu.Unlock()
	if info, ok := h.activeStreams[convID]; ok {
		info.closeAll()
		delete(h.activeStreams, convID)
	}
}

func (h *AIHandler) stopStream(convID int64) {
	h.activeStreamsMu.Lock()
	defer h.activeStreamsMu.Unlock()
	if info, ok := h.activeStreams[convID]; ok {
		info.cancel()
		info.closeAll()
		delete(h.activeStreams, convID)
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
	defer h.activeStreamsMu.Unlock()
	for convID := range h.activeStreams {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"active":          true,
			"conversation_id": convID,
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
	h.stopStream(id)
	respondJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// SetProvider updates the provider (e.g. when settings change).
func (h *AIHandler) SetProvider(p llm.Provider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.provider = p
}

func (h *AIHandler) getProvider() llm.Provider {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.provider
}

// checkRateLimit returns true if the request is within rate limit (20/min).
func (h *AIHandler) checkRateLimit(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)

	// Clean old entries
	times := h.rateLimiter[key]
	var recent []time.Time
	for _, t := range times {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= 20 {
		return false
	}

	h.rateLimiter[key] = append(recent, now)
	return true
}

// HandleStatus returns the AI configuration status.
func (h *AIHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	p := h.getProvider()

	result := map[string]interface{}{
		"configured": p != nil,
	}

	if p != nil {
		result["provider"] = p.Name()
	}

	// Get model setting based on provider
	providerType, _ := h.api.db.GetSetting(r.Context(), "ai_provider")
	var model string
	if providerType == "ollama" {
		model, _ = h.api.db.GetSetting(r.Context(), "ollama_model")
	} else {
		model, _ = h.api.db.GetSetting(r.Context(), "ai_model")
	}
	if model == "" {
		model = "(provider default)"
	}
	result["model"] = model

	// For Ollama, actually test the connection
	if providerType == "ollama" && p != nil {
		ollamaURL, _ := h.api.db.GetSetting(r.Context(), "ollama_base_url")
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}

		// Test if the model exists by calling /v1/models
		resp, err := http.Get(ollamaURL + "/v1/models")
		if err != nil {
			result["error"] = "Cannot connect to Ollama server"
		} else {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				result["error"] = "Ollama server returned error"
			}
		}

		// Optionally test if the specific model is available
		if model != "" && model != "(provider default)" {
			modelResp, err := http.Get(ollamaURL + "/v1/models")
			if err == nil {
				body, _ := io.ReadAll(modelResp.Body)
				modelResp.Body.Close()
				// Check if model name is in the response
				if !bytes.Contains(body, []byte(model)) {
					result["error"] = fmt.Sprintf("Model '%s' not found on Ollama server", model)
				}
			}
		}
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleTestConnection tests an AI provider connection without saving settings.
// It receives the form values directly and tests connectivity.
func (h *AIHandler) HandleTestConnection(w http.ResponseWriter, r *http.Request) {
	var params struct {
		Provider    string `json:"ai_provider"`
		APIKey      string `json:"anthropic_api_key"`
		OllamaURL   string `json:"ollama_base_url"`
		OllamaKey   string `json:"ollama_api_key"`
		OllamaModel string `json:"ollama_model"`
		Model       string `json:"ai_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	result := map[string]interface{}{}

	switch params.Provider {
	case "gosdk":
		available := llm.IsClaudeCLIAvailable()
		result["configured"] = available
		result["provider"] = "gosdk"
		if available {
			result["model"] = "(Claude CLI default)"
		} else {
			result["error"] = "Claude Code CLI not found. Install it with: npm install -g @anthropic-ai/claude-code"
		}

	case "apikey":
		apiKey := params.APIKey
		// If no key provided in form, try to read existing from DB
		if apiKey == "" {
			apiKey, _ = h.api.GetDecryptedSetting(r.Context(), "anthropic_api_key")
		}
		if apiKey == "" {
			result["configured"] = false
			result["error"] = "No API key provided"
		} else {
			// Test with a minimal API call
			result["configured"] = true
			result["provider"] = "apikey"
			model := params.Model
			if model == "" {
				model = "(provider default)"
			}
			result["model"] = model
		}

	case "ollama":
		ollamaURL := params.OllamaURL
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}
		ollamaModel := params.OllamaModel
		if ollamaModel == "" {
			ollamaModel = "qwen3-coder"
		}

		result["configured"] = true
		result["provider"] = "ollama"
		result["model"] = ollamaModel

		// Test connection to Ollama server
		resp, err := http.Get(ollamaURL + "/v1/models")
		if err != nil {
			result["error"] = "Cannot connect to Ollama server at " + ollamaURL
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				result["error"] = "Ollama server returned error"
			} else if !bytes.Contains(body, []byte(ollamaModel)) {
				result["error"] = fmt.Sprintf("Model '%s' not found on Ollama server", ollamaModel)
			}
		}

	case "ollama-sdk":
		ollamaURL := params.OllamaURL
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}
		ollamaModel := params.OllamaModel
		if ollamaModel == "" {
			ollamaModel = "qwen3-coder"
		}

		cliAvailable := llm.IsClaudeCLIAvailable()
		result["configured"] = cliAvailable
		result["provider"] = "ollama-sdk"
		result["model"] = ollamaModel

		if !cliAvailable {
			result["error"] = "Claude Code CLI not found. Install it with: npm install -g @anthropic-ai/claude-code"
		} else {
			// Also test Ollama connectivity
			resp, err := http.Get(ollamaURL + "/v1/models")
			if err != nil {
				result["error"] = "Cannot connect to Ollama server at " + ollamaURL
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					result["error"] = "Ollama server returned error"
				} else if !bytes.Contains(body, []byte(ollamaModel)) {
					result["error"] = fmt.Sprintf("Model '%s' not found on Ollama server", ollamaModel)
				}
			}
		}

	case "nodesdk":
		result["configured"] = true
		result["provider"] = "nodesdk"
		result["model"] = "(Node.js Agent SDK)"

	default:
		// Auto-detect
		if llm.IsClaudeCLIAvailable() {
			result["configured"] = true
			result["provider"] = "gosdk"
			result["model"] = "(Claude CLI auto-detected)"
		} else {
			apiKey := params.APIKey
			if apiKey == "" {
				apiKey, _ = h.api.GetDecryptedSetting(r.Context(), "anthropic_api_key")
			}
			if apiKey != "" {
				result["configured"] = true
				result["provider"] = "apikey"
				result["model"] = "(auto-detected API key)"
			} else {
				result["configured"] = false
				result["error"] = "No provider available. Install Claude CLI or provide an API key."
			}
		}
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleChat handles the main chat endpoint with SSE streaming.
func (h *AIHandler) HandleChat(w http.ResponseWriter, r *http.Request) {
	p := h.getProvider()
	if p == nil {
		respondError(w, http.StatusServiceUnavailable, "AI not configured. Set an API key or install Claude Code CLI.")
		return
	}

	if !h.checkRateLimit("chat") {
		respondError(w, http.StatusTooManyRequests, "Rate limit exceeded. Max 20 requests per minute.")
		return
	}

	var input struct {
		ConversationID int64  `json:"conversation_id"`
		Message        string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	// Truncate input
	if len(input.Message) > 10000 {
		input.Message = input.Message[:10000]
	}

	input.Message = strings.TrimSpace(input.Message)
	if input.Message == "" {
		respondError(w, http.StatusBadRequest, "Message is required")
		return
	}

	ctx := r.Context()

	// Create or get conversation
	var conv *database.AIConversation
	if input.ConversationID > 0 {
		var err error
		conv, err = h.api.db.GetAIConversation(ctx, input.ConversationID)
		if err != nil {
			respondError(w, http.StatusNotFound, "Conversation not found")
			return
		}
	} else {
		// Create new conversation
		title := input.Message
		if len(title) > 100 {
			title = title[:100] + "..."
		}
		conv = &database.AIConversation{Title: title}
		if err := h.api.db.CreateAIConversation(ctx, conv); err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to create conversation")
			return
		}
	}

	// Save user message
	userMsg := &database.AIMessage{
		ConversationID: conv.ID,
		Role:           "user",
		Content:        input.Message,
		ToolCalls:      "[]",
	}
	if err := h.api.db.CreateAIMessage(ctx, userMsg); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to save message")
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

	// Inject proposal feedback (approved/rejected) into the user's message context.
	// This notifies the AI about outcomes of proposals it created earlier.
	feedbackDocs, _ := h.api.db.ListUnacknowledgedFeedback(ctx, conv.ID)
	if len(feedbackDocs) > 0 {
		var fb strings.Builder
		fb.WriteString("[Notificação do sistema — Feedback de propostas]\n")
		for _, doc := range feedbackDocs {
			statusLabel := "APROVADA"
			if doc.Status == "rejected" {
				statusLabel = "REJEITADA"
			}
			fb.WriteString(fmt.Sprintf("- %s: %s — %s\n", doc.Title, doc.Summary, statusLabel))
		}
		fb.WriteString("\n---\n")

		// Prepend feedback to the last user message to avoid consecutive user messages
		if len(messages) > 0 {
			last := &messages[len(messages)-1]
			if last.Role == "user" && len(last.Content) > 0 {
				last.Content[0].Text = fb.String() + last.Content[0].Text
			}
		}

		// Mark as acknowledged
		var docIDs []string
		for _, doc := range feedbackDocs {
			docIDs = append(docIDs, doc.ID)
		}
		if err := h.api.db.AcknowledgeFeedback(ctx, docIDs); err != nil {
			log.Printf("[AIFeedback] Failed to acknowledge feedback: %v", err)
		}
		log.Printf("[AIFeedback] Injected %d proposal feedback(s) for conv %d", len(feedbackDocs), conv.ID)
	}

	// Build system prompt with current state (inject proactive context if AI-initiated)
	var proactiveCtx string
	if conv.Source == "ai" && conv.ProactiveContext != "" && conv.ProactiveContext != "{}" {
		proactiveCtx = conv.ProactiveContext
	}

	_, isSessionProvider := p.(llm.SessionProvider)

	systemPrompt := h.buildSystemPromptWithContext(ctx, proactiveCtx, isSessionProvider)

	// Get model based on provider
	providerType, _ := h.api.db.GetSetting(ctx, "ai_provider")
	var model string
	if providerType == "ollama" || providerType == "ollama-sdk" {
		model, _ = h.api.db.GetSetting(ctx, "ollama_model")
	} else {
		model, _ = h.api.db.GetSetting(ctx, "ai_model")
	}

	// Determine if we should use tools:
	// - Session providers (gosdk, nodesdk, ollama-sdk) handle tools internally via MCP — no tools in request
	// - API key and Ollama direct providers use native tool definitions in the request
	var tools []llm.ToolDefinition
	if !isSessionProvider {
		tools = llm.ChatTools()
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

	// Create assistant message in DB immediately with status='streaming'
	assistantMsg := &database.AIMessage{
		ConversationID: conv.ID,
		Role:           "assistant",
		Content:        "",
		ToolCalls:      "[]",
		Status:         "streaming",
	}
	if err := h.api.db.CreateAIMessage(ctx, assistantMsg); err != nil {
		log.Printf("[AI] Failed to create streaming message: %v", err)
	}

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

	safeSendSSE("message_id", map[string]interface{}{
		"id": assistantMsg.ID,
	})

	// Start debounced flusher to persist streaming content every 2 seconds
	sf := newStreamFlusher(h.api.db, assistantMsg.ID, func() string {
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

		textMu.Lock()
		finalContent := assistantText.String()
		toolCallsJSON, _ := json.Marshal(toolCalls)
		finalToolCalls := string(toolCallsJSON)
		textMu.Unlock()

		saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer saveCancel()

		if streamErr != "" {
			h.api.db.UpdateAIMessageStatus(saveCtx, assistantMsg.ID, "error", finalContent, finalToolCalls, streamErr)
		} else {
			h.api.db.UpdateAIMessageStatus(saveCtx, assistantMsg.ID, "completed", finalContent, finalToolCalls, "")
		}
		h.api.db.TouchAIConversation(saveCtx, conv.ID)
	}()

	var streamingErr error
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

	// Track total token usage across all streaming calls
	if resp != nil {
		totalUsage = resp.Usage
	}

	// Handle tool use loop — session providers handle tools internally, skip for them
	if resp != nil && resp.StopReason == "tool_use" && !isSessionProvider {
		loopUsage, loopErr := h.handleToolLoop(streamCtx, safeSendSSE, p, req, resp, conv, &assistantText, &toolCalls, &textMu, 15, collector, assistantMsg.ID)
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

	// Create a single batch proposal document if task actions were collected.
	// Use a background-derived context because the HTTP request context (ctx) may
	// already be canceled if the user navigated away during streaming.  The LLM
	// streaming itself is resilient (uses streamCtx), but the post-processing code
	// was incorrectly using ctx, causing CreateTempDocument to fail silently.
	if collector.hasActions() {
		proposalCtx, proposalCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer proposalCancel()

		actions := collector.getActions()
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
		projectName := "Projeto"
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
			summaryParts = append(summaryParts, fmt.Sprintf("criar %d tarefa(s)", createCount))
		}
		if updateCount > 0 {
			summaryParts = append(summaryParts, fmt.Sprintf("atualizar %d tarefa(s)", updateCount))
		}
		if deleteCount > 0 {
			summaryParts = append(summaryParts, fmt.Sprintf("remover %d tarefa(s)", deleteCount))
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
		var otherActions []indexedAction           // update, delete actions

		for i, a := range actions {
			switch a.Action {
			case "create":
				if a.ParentRef > 0 {
					// Group by parent_ref (1-based index into the batch)
					parentIdx := a.ParentRef - 1 // convert to 0-based
					childActions[parentIdx] = append(childActions[parentIdx], indexedAction{i, a})
				} else if a.ParentID > 0 {
					// Legacy: group by parent_id (find matching parent in batch)
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
				orderLabel = fmt.Sprintf(" (Ordem: %d)", a.SortOrder)
			}
			contentBuilder.WriteString(fmt.Sprintf("### %d. Criar tarefa: %s%s\n", num, a.Title, orderLabel))
			if a.Description != "" {
				contentBuilder.WriteString(fmt.Sprintf("**Descrição:** %s\n", a.Description))
			}
			contentBuilder.WriteString(fmt.Sprintf("**Prioridade:** %s | **Status:** %s\n", a.Priority, a.Status))
			if a.DueDate != "" {
				contentBuilder.WriteString(fmt.Sprintf("**Data limite:** %s\n", a.DueDate))
			}

			// Render subtasks indented
			if children, ok := childActions[pa.idx]; ok {
				contentBuilder.WriteString("\n**Subtarefas:**\n")
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

		// Render update/delete actions with full task card
		for _, oa := range otherActions {
			a := oa.action
			num++
			switch a.Action {
			case "update":
				contentBuilder.WriteString(fmt.Sprintf("### %d. Atualizar tarefa #%d: %s\n", num, a.TaskID, a.Title))
				if a.Description != "" {
					contentBuilder.WriteString(fmt.Sprintf("**Descrição:** %s\n", a.Description))
				}
				prio := a.Priority
				if prio == "" {
					prio = "-"
				}
				stat := a.Status
				if stat == "" {
					stat = "-"
				}
				contentBuilder.WriteString(fmt.Sprintf("**Prioridade:** %s | **Status:** %s\n", prio, stat))
				if a.DueDate != "" {
					contentBuilder.WriteString(fmt.Sprintf("**Data limite:** %s\n", a.DueDate))
				}
			case "delete":
				contentBuilder.WriteString(fmt.Sprintf("### %d. Excluir tarefa #%d: %s\n", num, a.TaskID, a.Title))
				if a.Description != "" {
					contentBuilder.WriteString(fmt.Sprintf("**Descrição:** %s\n", a.Description))
				}
				prio := a.Priority
				if prio == "" {
					prio = "-"
				}
				stat := a.Status
				if stat == "" {
					stat = "-"
				}
				contentBuilder.WriteString(fmt.Sprintf("**Prioridade:** %s | **Status:** %s\n", prio, stat))
				if a.DueDate != "" {
					contentBuilder.WriteString(fmt.Sprintf("**Data limite:** %s\n", a.DueDate))
				}
			}
			contentBuilder.WriteString("\n")
		}

		docID := uuid.New().String()[:8]

		// "Tarefa:" doc with task proposal approval
		summary := fmt.Sprintf("%s — %s", strings.Join(summaryParts, ", "), projectName)

		// Title prefix based on action types
		docPrefix := "Tarefa"
		if createCount == 0 && deleteCount == 0 && updateCount > 0 {
			docPrefix = "Atualizar Tarefa"
		} else if createCount == 0 && updateCount == 0 && deleteCount > 0 {
			docPrefix = "Excluir Tarefa"
		}

		docTitle := fmt.Sprintf("%s: %s", docPrefix, projectName)
		if len(actions) == 1 {
			docTitle = fmt.Sprintf("%s: %s", docPrefix, actions[0].Title)
		}

		var fullContent strings.Builder
		fullContent.WriteString(fmt.Sprintf("# Proposta de Tarefas — %s\n\n", projectName))
		fullContent.WriteString(fmt.Sprintf("**Resumo:** %s\n\n", summary))
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

			// Emit doc_card only for non-session providers
			if !isSessionProvider {
				safeSendSSE("doc_card", map[string]interface{}{
					"doc_id":     docID,
					"type":       "task_proposal",
					"title":      docTitle,
					"summary":    summary,
					"task_count": len(actions),
				})
			}

			// Broadcast via WebSocket only when the SSE connection is closed
			// (user navigated away). This avoids duplicate cards when both
			// SSE and WebSocket are active simultaneously.
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

	// Session providers handle tools internally — emit doc_card SSE events for
	// any documents created during this stream (the tool loop that normally does
	// this is skipped for session providers).
	if isSessionProvider {
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
			} else if strings.HasPrefix(doc.Title, "Tarefa:") || strings.HasPrefix(doc.Title, "Planejamento:") ||
				strings.HasPrefix(doc.Title, "Atualizar Tarefa:") || strings.HasPrefix(doc.Title, "Excluir Tarefa:") {
				cardType = "task_proposal"
			}
			safeSendSSE("doc_card", map[string]interface{}{
				"doc_id":  doc.ID,
				"type":    cardType,
				"title":   doc.Title,
				"summary": doc.Summary,
				"status":  doc.Status,
			})
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
	db           *database.DB
	msgID        int64
	getText      func() string
	getToolCalls func() string
	lastFlushed  int
	ticker       *time.Ticker
	done         chan struct{}
	mu           sync.Mutex
}

func newStreamFlusher(db *database.DB, msgID int64, getText func() string, getToolCalls func() string) *streamFlusher {
	f := &streamFlusher{
		db:           db,
		msgID:        msgID,
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
	f.db.UpdateAIMessageContent(ctx, f.msgID, content, f.getToolCalls())
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
			result, err := h.executeTool(toolCtx, block.Name, inputMap, conv.ID, messageID, collector)
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
			return accumulated, err
		}

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
// so that SDK providers can call DevManager tools from their in-process MCP servers.
func (h *AIHandler) ExecuteTool(ctx context.Context, name string, input map[string]any, conversationID int64) (string, error) {
	// Look up the current streaming message ID for this conversation
	h.streamMessageIDsMu.Lock()
	msgID := h.streamMessageIDs[conversationID]
	h.streamMessageIDsMu.Unlock()

	// Look up planning collector for this conversation (if in planning mode)
	h.streamCollectorsMu.Lock()
	collector := h.streamCollectors[conversationID]
	h.streamCollectorsMu.Unlock()

	return h.executeTool(ctx, name, input, conversationID, msgID, collector)
}

// executeTool runs a tool and returns the result as a string.
func (h *AIHandler) executeTool(ctx context.Context, name string, input map[string]interface{}, conversationID int64, messageID int64, collector *planningCollector) (string, error) {
	switch name {
	case "create_skill":
		skillName, _ := input["name"].(string)
		content, _ := input["content"].(string)
		category, _ := input["category"].(string)

		if skillName == "" || content == "" {
			return "", fmt.Errorf("name and content are required")
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

	case "list_projects":
		projects, err := h.api.db.ListProjects(ctx)
		if err != nil {
			return "", err
		}
		if len(projects) == 0 {
			return "No projects found.", nil
		}
		var sb strings.Builder
		for _, p := range projects {
			sb.WriteString(fmt.Sprintf("- [%d] %s (%s: %s)\n", p.ID, p.Name, p.Type, p.Path))
		}
		return sb.String(), nil

	case "list_mcp_servers":
		servers, err := h.api.db.ListMCPServers(ctx)
		if err != nil {
			return "", err
		}
		if len(servers) == 0 {
			return "No MCP servers found.", nil
		}
		var sb strings.Builder
		for _, m := range servers {
			status := "enabled"
			if !m.Enabled {
				status = "disabled"
			}
			sb.WriteString(fmt.Sprintf("- [%d] %s: %s (%s)\n", m.ID, m.Name, m.Command, status))
		}
		return sb.String(), nil

	case "create_mcp_server":
		mcpName, _ := input["name"].(string)
		command, _ := input["command"].(string)
		args, _ := input["args"].(string)
		env, _ := input["env"].(string)

		if mcpName == "" || command == "" {
			return "", fmt.Errorf("name and command are required")
		}
		if args == "" {
			args = "[]"
		}
		if env == "" {
			env = "{}"
		}

		mcp := &database.MCPServer{
			Name:    mcpName,
			Command: command,
			Args:    args,
			Env:     env,
			Enabled: true,
		}
		if err := h.api.db.CreateMCPServer(ctx, mcp); err != nil {
			return "", err
		}

		h.api.hub.BroadcastStateUpdate("mcp", map[string]interface{}{"action": "created", "mcp": mcp})
		return fmt.Sprintf("MCP server '%s' created (ID: %d)", mcpName, mcp.ID), nil

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
			"Memory doc do projeto %s carregado (v%d). Um botão 'Ver Documento' foi exibido automaticamente no chat. NÃO gere links.\n\n"+
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
			"IMPORTANTE: O conteúdo NÃO foi salvo ainda. Uma prévia foi criada para o usuário revisar.\n"+
				"Um botão 'Revisar Proposta' foi exibido automaticamente no chat.\n"+
				"NÃO diga que a alteração foi feita — ela AGUARDA aprovação do usuário.\n"+
				"Informe ao usuário que a proposta para o projeto %s está disponível para revisão.\n"+
				"Alterações propostas: %s\n"+
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
		statusIcons := map[string]string{"todo": "[ ]", "in_progress": "[~]", "done": "[x]", "blocked": "[!]", "awaiting_approval": "[?]"}
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
				"IMPORTANTE: Task '%s' NÃO foi criada ainda — será criada após aprovação do usuário. "+
					"NÃO diga que a tarefa foi criada.", title), nil
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
			return fmt.Sprintf("Task '%s' status atualizado para '%s'.", task.Title, task.Status), nil
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
				"IMPORTANTE: Task '%s' NÃO foi atualizada ainda — será aplicada após aprovação do usuário. "+
					"NÃO diga que a tarefa foi atualizada.", task.Title), nil
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
				"IMPORTANTE: Task '%s' NÃO foi excluída ainda — será excluída após aprovação do usuário. "+
					"NÃO diga que a tarefa foi excluída.", task.Title), nil
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
		sb.WriteString(fmt.Sprintf("- Todo: %d\n- In Progress: %d\n- Awaiting Approval: %d\n- Done: %d\n- Blocked: %d\n\n", summary["todo"], summary["in_progress"], summary["awaiting_approval"], summary["done"], summary["blocked"]))

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

		// Recommended next task: highest priority non-blocked todo/in_progress, or nearest due (exclude umbrella parents)
		priorityOrder := map[string]int{"urgent": 4, "high": 3, "medium": 2, "low": 1}
		var best *database.ProjectTask
		for i, t := range tasks {
			if t.Status == "done" || t.Status == "blocked" || t.ParentID.Valid || parentIDs[t.ID] {
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

		return fmt.Sprintf("Documento criado com sucesso. Um botão 'Ver Documento' foi exibido automaticamente no chat. NÃO gere links — o usuário usará o botão nativo. Link interno: /app/doc/%s", doc.ID), nil

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

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// HandleExecuteTool is an HTTP endpoint that Node.js SDK sidecar calls to execute
// DevManager tools. It proxies tool calls to the existing executeTool implementation.
func (h *AIHandler) HandleExecuteTool(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name           string                 `json:"name"`
		Args           map[string]interface{} `json:"args"`
		ConversationID int64                  `json:"conversation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Tool name is required"})
		return
	}
	if input.Args == nil {
		input.Args = make(map[string]interface{})
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := h.executeTool(ctx, input.Name, input.Args, input.ConversationID, 0, nil)
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"result": "",
			"error":  err.Error(),
		})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"result": result,
		"error":  "",
	})
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
			"title":   "Proposta de alteração",
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

	return llm.ChatSystemPrompt(skillNames, projectNames, mcpNames)
}

// buildSystemPromptWithContext builds system prompt, optionally with proactive conversation context.
// When forMCP is true, adapts the prompt for GoSDK/session providers (MCP tool naming convention).
func (h *AIHandler) buildSystemPromptWithContext(ctx context.Context, proactiveCtx string, forMCP ...bool) string {
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

	return llm.ChatSystemPromptWithProactiveContext(skillNames, projectNames, mcpNames, proactiveCtx, forMCP...)
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

	ctx := r.Context()
	project, err := h.api.db.GetProject(ctx, input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	doc, err := h.api.db.GetMemoryDoc(ctx, input.ProjectID)
	docContent := ""
	if err == nil {
		docContent = doc.Content
	}

	proactiveCtx, _ := json.Marshal(map[string]interface{}{
		"project_id":   input.ProjectID,
		"project_name": project.Name,
		"memory_doc":   docContent,
		"intent":       "edit_memory_doc",
	})

	var assistantMsg string
	if docContent != "" {
		preview := docContent
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		assistantMsg = fmt.Sprintf("Carreguei o memory doc do projeto **%s**.\n\nPrevia do conteudo atual:\n> %s\n\nO que voce gostaria de alterar?", project.Name, preview)
	} else {
		assistantMsg = fmt.Sprintf("O projeto **%s** ainda nao tem um memory doc. Deseja que eu crie um?", project.Name)
	}

	title := fmt.Sprintf("Edit Memory Doc: %s", project.Name)
	conv, err := h.api.db.CreateProactiveConversation(ctx, title, "standard", "memory_doc_edit", string(proactiveCtx), assistantMsg)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create conversation")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation_id": conv.ID,
	})
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

	if input.ProjectID <= 0 {
		respondError(w, http.StatusBadRequest, "project_id is required")
		return
	}

	ctx := r.Context()
	project, err := h.api.db.GetProject(ctx, input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	ctxData := map[string]interface{}{
		"intent":       "task_creation",
		"project_id":   input.ProjectID,
		"project_name": project.Name,
		"title":        input.Title,
		"description":  input.Description,
		"status":       input.Status,
		"priority":     input.Priority,
		"due_date":     input.DueDate,
	}
	if input.ParentID > 0 {
		ctxData["parent_id"] = input.ParentID
	}
	proactiveCtx, _ := json.Marshal(ctxData)

	// Build assistant message showing pre-filled data
	var details string
	if input.Title != "" {
		details += fmt.Sprintf("- **Título:** %s\n", input.Title)
	}
	if input.Description != "" {
		details += fmt.Sprintf("- **Descrição:** %s\n", input.Description)
	}
	if input.Priority != "" {
		details += fmt.Sprintf("- **Prioridade:** %s\n", input.Priority)
	}
	if input.DueDate != "" {
		details += fmt.Sprintf("- **Prazo:** %s\n", input.DueDate)
	}

	assistantMsg := fmt.Sprintf(
		"Criação de tarefa com IA para o projeto **%s**.\n\n", project.Name)
	if details != "" {
		assistantMsg += fmt.Sprintf("Dados já preenchidos:\n%s\n", details)
	}
	assistantMsg += "Descreva melhor o que você precisa e eu vou ajudar a refinar a tarefa, quebrá-la em subtarefas se necessário, e criá-la no projeto."

	title := fmt.Sprintf("Nova Tarefa: %s", project.Name)
	conv, err := h.api.db.CreateProactiveConversation(ctx, title, "standard", "task_creation", string(proactiveCtx), assistantMsg)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create conversation")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation_id": conv.ID,
	})
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

	if input.TaskID <= 0 || input.ProjectID <= 0 {
		respondError(w, http.StatusBadRequest, "task_id and project_id are required")
		return
	}

	ctx := r.Context()
	project, err := h.api.db.GetProject(ctx, input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	task, err := h.api.db.GetTask(ctx, input.TaskID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}

	if task.ProjectID != input.ProjectID {
		respondError(w, http.StatusBadRequest, "Task does not belong to this project")
		return
	}

	// Fetch existing subtasks
	var subtasks []database.ProjectTask
	_ = h.api.db.SelectContext(ctx, &subtasks, "SELECT * FROM project_tasks WHERE parent_id = ? ORDER BY sort_order, created_at", input.TaskID)

	// Build proactive context
	ctxData := map[string]interface{}{
		"intent":           "task_discussion",
		"project_id":       input.ProjectID,
		"project_name":     project.Name,
		"task_id":          task.ID,
		"task_title":       task.Title,
		"task_description": task.Description,
		"task_status":      task.Status,
		"task_priority":    task.Priority,
	}
	if task.DueDate.Valid {
		ctxData["task_due_date"] = task.DueDate.Time.Format("2006-01-02 15:04")
	}
	if len(subtasks) > 0 {
		subtaskList := make([]map[string]interface{}, len(subtasks))
		for i, st := range subtasks {
			subtaskList[i] = map[string]interface{}{
				"id":       st.ID,
				"title":    st.Title,
				"status":   st.Status,
				"priority": st.Priority,
			}
		}
		ctxData["existing_subtasks"] = subtaskList
	}
	proactiveCtx, _ := json.Marshal(ctxData)

	// Build assistant message
	assistantMsg := fmt.Sprintf(
		"Discussão sobre a tarefa **%s** do projeto **%s**.\n\n", task.Title, project.Name)
	assistantMsg += fmt.Sprintf("**Status:** %s | **Prioridade:** %s\n\n",
		task.Status, task.Priority)
	if task.DueDate.Valid {
		assistantMsg += fmt.Sprintf("**Prazo:** %s\n\n",
			task.DueDate.Time.Format("02/01/2006 15:04"))
	}
	if task.Description != "" {
		desc := task.Description
		if len(desc) > 500 {
			desc = desc[:500] + "..."
		}
		assistantMsg += fmt.Sprintf("**Descrição:**\n%s\n\n", desc)
	}
	if len(subtasks) > 0 {
		assistantMsg += fmt.Sprintf("**Subtarefas existentes (%d):**\n", len(subtasks))
		for _, st := range subtasks {
			statusEmoji := map[string]string{
				"todo": "⬜", "in_progress": "🔄", "awaiting_approval": "⏳", "done": "✅", "blocked": "🚫",
			}[st.Status]
			if statusEmoji == "" {
				statusEmoji = "⬜"
			}
			assistantMsg += fmt.Sprintf("- %s %s (%s)\n", statusEmoji, st.Title, st.Status)
		}
		assistantMsg += "\n"
	}
	assistantMsg += "Como posso ajudar? Posso:\n" +
		"- **Refinar** o título ou descrição da tarefa\n" +
		"- **Quebrar** em subtarefas detalhadas\n" +
		"- **Ajustar** status, prioridade ou prazo\n\n" +
		"O que você gostaria de fazer?"

	taskTitle := task.Title
	if len(taskTitle) > 60 {
		taskTitle = taskTitle[:60] + "..."
	}
	title := fmt.Sprintf("Discussão: %s", taskTitle)
	conv, err := h.api.db.CreateProactiveConversation(ctx, title, "standard", "task_discussion", string(proactiveCtx), assistantMsg)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create conversation")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation_id": conv.ID,
	})
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
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}

	if err := h.api.db.DeleteAIConversation(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleDeleteAllConversations deletes all conversations.
func (h *AIHandler) HandleDeleteAllConversations(w http.ResponseWriter, r *http.Request) {
	if err := h.api.db.DeleteAllAIConversations(r.Context()); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleGenerateSkill generates a skill from a description (SSE streaming).
func (h *AIHandler) HandleGenerateSkill(w http.ResponseWriter, r *http.Request) {
	p := h.getProvider()
	if p == nil {
		respondError(w, http.StatusServiceUnavailable, "AI not configured")
		return
	}

	if !h.checkRateLimit("generate") {
		respondError(w, http.StatusTooManyRequests, "Rate limit exceeded")
		return
	}

	var input struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if strings.TrimSpace(input.Description) == "" {
		respondError(w, http.StatusBadRequest, "Description is required")
		return
	}

	model, _ := h.api.db.GetSetting(r.Context(), "ai_model")

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

	req := &llm.Request{
		System: llm.SkillGenerationPrompt,
		Messages: []llm.Message{
			llm.NewTextMessage("user", input.Description),
		},
		MaxTokens: 4096,
		Model:     model,
	}

	var fullText strings.Builder

	resp, err := p.StreamMessage(r.Context(), req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
			h.sendSSE(w, flusher, "text", map[string]interface{}{
				"text": event.Delta.Text,
			})
		}
		return nil
	})

	if err != nil {
		h.sendSSE(w, flusher, "error", map[string]interface{}{
			"message": err.Error(),
		})
		return
	}

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
			Subcategory:         "skill_generate",
			Model:               usageModel,
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CostUSD:             llm.CalculateCost(usageModel, resp.Usage.InputTokens, resp.Usage.OutputTokens),
		})
	}

	h.sendSSE(w, flusher, "done", map[string]interface{}{
		"full_text": fullText.String(),
	})
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
// content for task creation: user prompts and plans.
// Filters out Claude Code's internal processing (tool calls, file reads, etc.)
// and focuses on what the user actually asked and what was planned.
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
			if len(prompt) > 3 {
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
			strings.Contains(lower, "implementation plan") ||
			strings.Contains(lower, "plano de implementação") ||
			strings.Contains(lower, "plano:")) {
			inPlan = true
			planLines = append(planLines, trimmed)
			continue
		}
		if inPlan {
			// End plan on 2+ consecutive blank lines after substantial content
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
	hasStructuredData := len(userPrompts) > 0 || len(planLines) > 0

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

	// 4. Include recent output ONLY as fallback when no structured data was found
	if !hasStructuredData {
		outputStr := string(cleanOutput)
		recentSize := 2000
		if len(outputStr) < recentSize {
			recentSize = len(outputStr)
		}
		rawRecent := outputStr[len(outputStr)-recentSize:]
		filtered := filterClaudeCodeNoise(rawRecent)
		if len(filtered) > 0 {
			sb.WriteString("Recent session output (filtered):\n")
			sb.WriteString(filtered)
		}
	}

	// Debug logging for task suggestion diagnostics
	log.Printf("[SuggestTask/Extract] prompts=%d planLines=%d hasStructuredData=%v contextLen=%d",
		len(userPrompts), len(planLines), hasStructuredData, sb.Len())
	for i, p := range userPrompts {
		preview := p
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		log.Printf("[SuggestTask/Extract] prompt[%d]: %q", i, preview)
	}
	if !hasStructuredData {
		log.Printf("[SuggestTask/Extract] FALLBACK: no user prompts or plan found, using filtered recent output")
	}

	return sb.String()
}

// HandleSuggestTaskData analyzes session output and suggests task title/description/priority.
func (h *AIHandler) HandleSuggestTaskData(w http.ResponseWriter, r *http.Request) {
	p := h.getProvider()
	if p == nil {
		respondError(w, http.StatusServiceUnavailable, "AI not configured")
		return
	}

	if !h.checkRateLimit("suggest_task") {
		respondError(w, http.StatusTooManyRequests, "Rate limit exceeded")
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

	// Strip ANSI escape codes and control characters from terminal output
	// These break the Claude CLI when passed as --print argument
	ansiRe := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\|\x1b[^[\]].?`)
	cleanOutput := ansiRe.ReplaceAll(output, nil)
	// Also remove other control characters except newline, tab, carriage return
	controlRe := regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	cleanOutput = controlRe.ReplaceAll(cleanOutput, nil)

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

	model, _ := h.api.db.GetSetting(r.Context(), "ai_model")

	// Extract structured context from raw terminal output
	sessionContext := extractSessionContext(cleanOutput)

	log.Printf("[SuggestTask] session=%s name=%q project=%s contextLen=%d", sessionID, sess.Name, project.Name, len(sessionContext))

	prompt := fmt.Sprintf(`You are a project manager creating a task ticket based on a Claude Code session.

Project: %s
Session name: "%s"

The session context below is ordered by reliability:
- "User prompts" = what the user typed into Claude Code. This is the MOST RELIABLE source of intent.
- "Plan" = Claude Code's implementation plan. Use to add detail.
- "Recent session output" = fallback terminal output (only present if no prompts/plan were found). Treat with EXTREME CAUTION — may contain unrelated file contents or tool output.

Session context:
%s

CRITICAL RULES:
1. Base the task PRIMARILY on user prompts (if present). These are what the user actually asked for.
2. If no user prompts are available, use the plan or session name. If only "Recent session output" is available, be VERY conservative — only use it if it clearly indicates a user goal.
3. NEVER create tasks about: "reading files", "investigating code", "exploring codebase", "analyzing structure", "running commands", "reviewing output". These are Claude Code's internal investigation steps, NOT user goals.
4. The task must describe WHAT the user wants to accomplish, not what Claude Code is doing internally.
5. If the context seems incoherent or unrelated to any clear goal, create a generic task based on the session name and project name. A vague but correct task is better than a specific but wrong one.

Respond with ONLY valid JSON, no markdown:
{"title": "...", "description": "...", "priority": "..."}

Rules:
- Title: imperative verb + objective, max 80 chars. Examples: "Implementar validação de formulário de login", "Corrigir erro 500 na API de pagamentos", "Refatorar módulo de autenticação"
- Description: 2-3 sentences describing the GOAL and expected outcome. Include which area of the codebase is affected if clear from context. Write as a task assignment: what should be done and why.
- Priority: "urgent" for hotfixes/production issues, "high" for important features/bugs, "medium" for regular work, "low" for nice-to-haves
- Use Portuguese (pt-BR)`, project.Name, sess.Name, sessionContext)

	req := &llm.Request{
		System:    "You are a task management assistant. Respond ONLY with valid JSON, no markdown.",
		Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
		MaxTokens: 256,
		Model:     model,
	}

	var fullText strings.Builder
	resp, err := p.StreamMessage(r.Context(), req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		respondError(w, http.StatusInternalServerError, "AI generation failed: "+err.Error())
		return
	}

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
	// Strip markdown code blocks if present
	if strings.HasPrefix(responseText, "```") {
		lines := strings.Split(responseText, "\n")
		if len(lines) > 2 {
			responseText = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var result struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    string `json:"priority"`
	}

	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
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
	p := h.getProvider()
	if p == nil {
		respondError(w, http.StatusServiceUnavailable, "AI not configured")
		return
	}

	if !h.checkRateLimit("validate") {
		respondError(w, http.StatusTooManyRequests, "Rate limit exceeded")
		return
	}

	var input struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if strings.TrimSpace(input.Content) == "" {
		respondError(w, http.StatusBadRequest, "Content is required")
		return
	}

	model, _ := h.api.db.GetSetting(r.Context(), "ai_model")

	req := &llm.Request{
		System: llm.SkillValidationPrompt,
		Messages: []llm.Message{
			llm.NewTextMessage("user", input.Content),
		},
		MaxTokens: 1024,
		Model:     model,
	}

	var fullText strings.Builder

	resp, err := p.StreamMessage(r.Context(), req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

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
			Subcategory:         "skill_validate",
			Model:               usageModel,
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CostUSD:             llm.CalculateCost(usageModel, resp.Usage.InputTokens, resp.Usage.OutputTokens),
		})
	}

	// Try to parse the response as JSON
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(fullText.String()), &result); err != nil {
		// Return raw text if not valid JSON
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"valid":       true,
			"issues":      []string{},
			"suggestions": []string{"Could not parse validation response"},
			"raw":         fullText.String(),
		})
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

// EvaluateSession uses AI to evaluate a session and proactively suggest task actions.
// Triggered on session_start, session_end, user_prompt, plan_accepted, or session_request.
func (h *AIHandler) EvaluateSession(ctx context.Context, sessionID string, trigger string, outputSnapshot []byte) {
	log.Printf("[AI-Session] EvaluateSession called: session=%s trigger=%s outputLen=%d", sessionID[:8], trigger, len(outputSnapshot))

	p := h.getProvider()
	if p == nil {
		log.Printf("[AI-Session] ABORTED: provider is nil")
		return
	}
	log.Printf("[AI-Session] Provider OK: %T", p)

	// Skip all evaluations if disabled in settings
	val, _ := h.api.db.GetSetting(ctx, "task_auto_eval_enabled")
	if val != "true" {
		log.Printf("[AI-Session] Task evaluation disabled (trigger=%s), skipping", trigger)
		return
	}

	sess, err := h.api.db.GetSession(ctx, sessionID)
	if err != nil {
		log.Printf("[AI-Session] Session %s not found: %v", sessionID, err)
		return
	}

	project, err := h.api.db.GetProject(ctx, sess.ProjectID)
	if err != nil {
		log.Printf("[AI-Session] Project %d not found: %v", sess.ProjectID, err)
		return
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
		return
	}
	if (trigger == "user_prompt" || trigger == "plan_accepted") && hasLinkedTask {
		log.Printf("[AI-Session] Skipping %s: task already linked [%d] %s", trigger, linkedTask.ID, linkedTask.Title)
		return
	}

	// Format tasks list
	var tasksList string
	if len(tasks) == 0 {
		tasksList = "No tasks exist for this project."
	} else {
		for _, t := range tasks {
			tasksList += fmt.Sprintf("- [%d] %s (status: %s, priority: %s)\n", t.ID, t.Title, t.Status, t.Priority)
			if t.Description != "" {
				desc := t.Description
				if len(desc) > 100 {
					desc = desc[:100] + "..."
				}
				tasksList += fmt.Sprintf("  Description: %s\n", desc)
			}
		}
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
		triggerDesc = "started"
		instructions = `- Suggest linking to an existing task if the session's work seems related.
- Valid types for this trigger: "link_task" (needs task_id)
- Return empty suggestions array if no task seems related.`
		allowedTypes["link_task"] = true

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
		instructions = `- This session has NO linked task yet. Based on the session output, suggest creating a new task or linking to an existing one.
- Valid types for this trigger: "create_task" (needs task_data), "link_task" (needs task_id)
- Return empty suggestions array if it's too early to determine the session's purpose.`
		allowedTypes["create_task"] = true
		allowedTypes["link_task"] = true

	case "plan_accepted":
		triggerDesc = "is currently running (a plan was just approved)"
		instructions = `- This session has NO linked task yet. A plan was approved, which gives clear indication of what will be done.
- Suggest creating a task that describes the planned work.
- Valid types for this trigger: "create_task" (needs task_data), "link_task" (needs task_id)
- Return empty suggestions array only if the plan is trivial.`
		allowedTypes["create_task"] = true
		allowedTypes["link_task"] = true

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

	prompt := fmt.Sprintf(`You are the DevManager AI Assistant evaluating a session that just %s.

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
- Use Portuguese (pt-BR) for title and description fields.`, tasksList, instructions)

	model, _ := h.api.db.GetSetting(ctx, "ai_model")

	log.Printf("[AI-Session] Calling LLM: model=%s promptLen=%d", model, len(prompt))

	req := &llm.Request{
		System:    "You are a task management assistant. Respond ONLY with valid JSON, no markdown code blocks.",
		Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
		MaxTokens: 2048,
		Model:     model,
	}

	var fullText strings.Builder
	evalResp, err := p.StreamMessage(ctx, req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		log.Printf("[AI-Session] LLM call FAILED for session %s: %v", sessionID, err)
		return
	}

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

	if strings.HasPrefix(responseText, "```") {
		lines := strings.Split(responseText, "\n")
		if len(lines) > 2 {
			responseText = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		log.Printf("[AI-Session] JSON parse FAILED for session %s: %v\nResponse: %s", sessionID, err, responseText)
		return
	}

	if len(result.Suggestions) == 0 {
		log.Printf("[AI-Session] No suggestions for session %s (%s)", sessionID[:8], trigger)
		return
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
		if allowedTypes[s.Type] {
			filtered = append(filtered, s)
		} else {
			log.Printf("[AI-Session] Filtered out invalid type '%s' for trigger '%s' (session %s)", s.Type, trigger, sessionID[:8])
		}
	}
	result.Suggestions = filtered

	// Record AI summary in task history (for session_end with linked task)
	if trigger == "session_end" && hasLinkedTask && result.Summary != "" {
		h.api.recordTaskHistory(ctx, linkedTask.ID, linkedTask.ProjectID, "session_ended", map[string]interface{}{
			"session_id": sessionID, "summary": result.Summary,
		}, "ai", sessionID)
	}

	if len(result.Suggestions) == 0 {
		log.Printf("[AI-Session] All suggestions filtered out for session %s (%s)", sessionID[:8], trigger)
		return
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
		return
	}

	// Create and broadcast each suggestion with proactive conversation
	typeLabels := map[string]string{
		"link_task":     "Vincular Tarefa",
		"create_task":   "Nova Tarefa",
		"update_task":   "Atualizar Tarefa",
		"complete_task": "Completar Tarefa",
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
		assistantMsg := fmt.Sprintf("Ao analisar a sessao do projeto **%s**, identifiquei uma oportunidade:\n\n"+
			"**Tipo:** %s\n"+
			"**Titulo:** %s\n", project.Name, typeLabel, s.Title)
		if s.Description != "" {
			assistantMsg += fmt.Sprintf("**Descricao:** %s\n", s.Description)
		}
		assistantMsg += "\nPosso ajudar a refinar esta sugestao. O que voce gostaria de fazer?"

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
			{Label: "Aceitar", Action: "accept", Style: "primary"},
			{Label: "Discutir", Action: "discuss", Style: "outline"},
			{Label: "Ignorar", Action: "dismiss", Style: "secondary"},
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
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid suggestion ID")
		return
	}

	suggestion, err := h.api.db.GetAISuggestion(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Suggestion not found")
		return
	}

	if suggestion.Status != "pending" {
		respondError(w, http.StatusConflict, "Suggestion already processed")
		return
	}

	// Parse context
	var ctx struct {
		TaskID   *int64                 `json:"task_id"`
		TaskData map[string]interface{} `json:"task_data"`
	}
	json.Unmarshal([]byte(suggestion.ContextJSON), &ctx)

	var resultMsg string

	switch suggestion.Type {
	case "link_task":
		if ctx.TaskID == nil || suggestion.SessionID == "" {
			respondError(w, http.StatusBadRequest, "Missing task_id or session_id for link_task")
			return
		}
		if err := h.api.db.LinkSessionToTask(r.Context(), suggestion.SessionID, *ctx.TaskID); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Rename session to task title
		if linkedTask, err := h.api.db.GetTask(r.Context(), *ctx.TaskID); err == nil && linkedTask != nil {
			newName := "Task: " + linkedTask.Title
			h.api.db.ExecContext(r.Context(), "UPDATE sessions SET name = ? WHERE id = ?", newName, suggestion.SessionID)
			h.api.hub.BroadcastStateUpdate("session", map[string]interface{}{
				"action":     "renamed",
				"session_id": suggestion.SessionID,
				"name":       newName,
			})
		}
		resultMsg = fmt.Sprintf("Session linked to task %d", *ctx.TaskID)

	case "create_task":
		title, _ := ctx.TaskData["title"].(string)
		if title == "" {
			title = suggestion.Title
		}
		desc, _ := ctx.TaskData["description"].(string)
		priority, _ := ctx.TaskData["priority"].(string)
		if priority == "" {
			priority = "medium"
		}
		task := &database.ProjectTask{
			ProjectID:   suggestion.ProjectID,
			Title:       title,
			Description: desc,
			Status:      "todo",
			Priority:    priority,
		}
		if err := h.api.db.CreateTask(r.Context(), task); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Link session to new task if session exists
		if suggestion.SessionID != "" {
			h.api.db.LinkSessionToTask(r.Context(), suggestion.SessionID, task.ID)
			// Rename session to match new task title
			newName := "Task: " + task.Title
			h.api.db.ExecContext(r.Context(), "UPDATE sessions SET name = ? WHERE id = ?", newName, suggestion.SessionID)
			h.api.hub.BroadcastStateUpdate("session", map[string]interface{}{
				"action":     "renamed",
				"session_id": suggestion.SessionID,
				"name":       newName,
			})
		}
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{
			"action":     "created",
			"project_id": suggestion.ProjectID,
			"task":       task,
		})
		resultMsg = fmt.Sprintf("Task created: [%d] %s", task.ID, title)

	case "update_task":
		if ctx.TaskID == nil {
			respondError(w, http.StatusBadRequest, "Missing task_id for update_task")
			return
		}
		task, err := h.api.db.GetTask(r.Context(), *ctx.TaskID)
		if err != nil {
			respondError(w, http.StatusNotFound, "Task not found")
			return
		}
		if status, ok := ctx.TaskData["status"].(string); ok && status != "" {
			task.Status = status
		}
		if desc, ok := ctx.TaskData["description"].(string); ok {
			task.Description = desc
		}
		if err := h.api.db.UpdateTask(r.Context(), task); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{
			"action":     "updated",
			"project_id": task.ProjectID,
			"task":       task,
		})
		resultMsg = fmt.Sprintf("Task [%d] updated", *ctx.TaskID)

	case "complete_task":
		if ctx.TaskID == nil {
			respondError(w, http.StatusBadRequest, "Missing task_id for complete_task")
			return
		}
		if err := h.api.db.UpdateTaskStatus(r.Context(), *ctx.TaskID, "done"); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		task, _ := h.api.db.GetTask(r.Context(), *ctx.TaskID)
		if task != nil {
			h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{
				"action":     "updated",
				"project_id": task.ProjectID,
				"task":       task,
			})
		}
		resultMsg = fmt.Sprintf("Task [%d] marked as done", *ctx.TaskID)

	default:
		respondError(w, http.StatusBadRequest, "Unknown suggestion type: "+suggestion.Type)
		return
	}

	// Mark suggestion as accepted
	h.api.db.UpdateAISuggestionStatus(r.Context(), id, "accepted")

	// Record suggestion_accepted in task history
	var historyTaskID int64
	if ctx.TaskID != nil {
		historyTaskID = *ctx.TaskID
	}
	if historyTaskID > 0 {
		h.api.recordTaskHistory(r.Context(), historyTaskID, suggestion.ProjectID, "suggestion_accepted", map[string]interface{}{
			"suggestion_type": suggestion.Type, "title": suggestion.Title,
		}, "user", suggestion.SessionID)
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "accepted",
		"message": resultMsg,
	})
}

// DismissSuggestion dismisses an AI suggestion.
func (h *AIHandler) DismissSuggestion(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid suggestion ID")
		return
	}

	// Get suggestion details before dismissing for history
	suggestion, _ := h.api.db.GetAISuggestion(r.Context(), id)

	if err := h.api.db.UpdateAISuggestionStatus(r.Context(), id, "dismissed"); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Record suggestion_dismissed in task history if linked to a task
	if suggestion != nil {
		var ctx struct {
			TaskID *int64 `json:"task_id"`
		}
		json.Unmarshal([]byte(suggestion.ContextJSON), &ctx)
		if ctx.TaskID != nil && *ctx.TaskID > 0 {
			h.api.recordTaskHistory(r.Context(), *ctx.TaskID, suggestion.ProjectID, "suggestion_dismissed", map[string]interface{}{
				"suggestion_type": suggestion.Type, "title": suggestion.Title,
			}, "user", suggestion.SessionID)
		}
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
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid suggestion ID")
		return
	}

	suggestion, err := h.api.db.GetAISuggestion(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Suggestion not found")
		return
	}

	// If suggestion already has a conversation, return it
	if suggestion.ConversationID.Valid {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"conversation_id": suggestion.ConversationID.Int64,
		})
		return
	}

	// Create a new proactive conversation for this suggestion
	typeLabels := map[string]string{
		"link_task":     "Vincular Tarefa",
		"create_task":   "Nova Tarefa",
		"update_task":   "Atualizar Tarefa",
		"complete_task": "Completar Tarefa",
	}
	typeLabel := typeLabels[suggestion.Type]
	if typeLabel == "" {
		typeLabel = suggestion.Type
	}

	assistantMsg := fmt.Sprintf("Identifiquei uma oportunidade e gostaria de sugerir uma acao:\n\n"+
		"**Tipo:** %s\n"+
		"**Titulo:** %s\n", typeLabel, suggestion.Title)
	if suggestion.Description != "" {
		assistantMsg += fmt.Sprintf("**Descricao:** %s\n", suggestion.Description)
	}
	assistantMsg += "\nPosso ajudar a refinar esta sugestao. O que voce gostaria de fazer?"

	extra := map[string]interface{}{
		"suggestion_id": suggestion.ID,
		"session_id":    suggestion.SessionID,
		"project_id":    suggestion.ProjectID,
	}

	conv, err := h.CreateProactiveNotification(
		r.Context(),
		suggestion.Level,
		"task_suggestion",
		suggestion.Title,
		assistantMsg,
		nil, // No broadcast actions needed - this is a direct discuss
		extra,
	)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create discussion")
		return
	}

	// Link suggestion to conversation
	h.api.db.UpdateAISuggestionConversation(r.Context(), suggestion.ID, conv.ID)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation_id": conv.ID,
	})
}

// HandleMarkRead marks an AI conversation as read.
func (h *AIHandler) HandleMarkRead(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid conversation ID")
		return
	}

	if err := h.api.db.MarkConversationRead(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
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

	var title, body, pType string
	var actions []ProactiveAction

	switch input.Level {
	case "critical":
		pType = "alert"
		title = "Erro critico detectado"
		body = "O sistema detectou um erro critico que requer atencao imediata. O ultimo processo falhou com erro de permissao e os dados podem estar em estado inconsistente."
		actions = []ProactiveAction{
			{Label: "Ver detalhes", Action: "discuss", Style: "primary"},
			{Label: "Ignorar", Action: "dismiss", Style: "secondary"},
		}
	case "subtle":
		pType = "memory_doc_update"
		title = "Memory Doc atualizado"
		body = "O memory doc do projeto foi atualizado com o conteudo do CLAUDE.md."
		actions = []ProactiveAction{
			{Label: "Ver", Action: "open", Style: "outline"},
		}
	default:
		input.Level = "standard"
		pType = "task_suggestion"
		title = "Nova tarefa sugerida"
		body = "Ao analisar a sessao recente, identifiquei que seria util criar uma tarefa para revisar as alteracoes feitas no pipeline de deploy."
		actions = []ProactiveAction{
			{Label: "Aceitar", Action: "accept", Style: "primary"},
			{Label: "Discutir", Action: "discuss", Style: "outline"},
			{Label: "Ignorar", Action: "dismiss", Style: "secondary"},
		}
	}

	conv, err := h.CreateProactiveNotification(r.Context(), input.Level, pType, title, body, actions, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "sent",
		"level":           input.Level,
		"conversation_id": conv.ID,
	})
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
	h.mu.RLock()
	p := h.provider
	h.mu.RUnlock()

	var content string
	if p != nil {
		prompt := llm.VerificationDocPrompt(task.Title, task.Description, projectName, sessionSummaries, historyEntries)
		model, _ := h.api.db.GetSetting(ctx, "ai_model")

		req := &llm.Request{
			System:    "You are a technical documentation assistant. Generate clear, actionable verification documents in Portuguese (pt-BR).",
			Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
			MaxTokens: 4096,
			Model:     model,
		}

		var fullText strings.Builder
		resp, err := p.StreamMessage(ctx, req, func(event llm.StreamEvent) error {
			if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
				fullText.WriteString(event.Delta.Text)
			}
			return nil
		})

		if err != nil {
			log.Printf("[AI-Verification] LLM call failed for task %d: %v, using fallback", task.ID, err)
			content = h.createFallbackVerificationDoc(task, projectName, sessionSummaries)
		} else {
			content = strings.TrimSpace(fullText.String())
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
		}
	} else {
		log.Printf("[AI-Verification] No LLM provider, using fallback for task %d", task.ID)
		content = h.createFallbackVerificationDoc(task, projectName, sessionSummaries)
	}

	h.persistVerificationDoc(ctx, task, content)
}

// createFallbackVerificationDoc creates a simple verification document without AI.
func (h *AIHandler) createFallbackVerificationDoc(task *database.ProjectTask, projectName string, sessions []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Resumo\n\nA tarefa **%s** do projeto **%s** foi marcada como aguardando aprovação.\n\n", task.Title, projectName))
	if task.Description != "" {
		sb.WriteString(fmt.Sprintf("**Descrição:** %s\n\n", task.Description))
	}
	sb.WriteString("## Como Verificar\n\n")
	sb.WriteString("1. Verifique se as alterações foram aplicadas corretamente\n")
	sb.WriteString("2. Teste as funcionalidades mencionadas na descrição da tarefa\n")
	sb.WriteString("3. Confirme que o build compila sem erros\n\n")
	if len(sessions) > 0 {
		sb.WriteString("## Sessões Vinculadas\n\n")
		for _, s := range sessions {
			sb.WriteString(fmt.Sprintf("- %s\n", s))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## Observações\n\nDocumento gerado automaticamente (sem IA). Verifique manualmente o trabalho realizado.\n")
	return sb.String()
}

// persistVerificationDoc saves the verification document and links it to the task.
func (h *AIHandler) persistVerificationDoc(ctx context.Context, task *database.ProjectTask, content string) {
	docID := uuid.New().String()[:8]
	doc := &database.TempDocument{
		ID:      docID,
		Title:   fmt.Sprintf("Verificação: %s", task.Title),
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
