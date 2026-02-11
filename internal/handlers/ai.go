package handlers

import (
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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var docLinkRe = regexp.MustCompile(`/app/doc/([a-zA-Z0-9-]+)`)

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

	// Get model setting
	model, _ := h.api.db.GetSetting(r.Context(), "ai_model")
	if model == "" {
		model = "(provider default)"
	}
	result["model"] = model

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

	// Build system prompt with current state (inject proactive context if AI-initiated)
	var proactiveCtx string
	if conv.Source == "ai" && conv.ProactiveContext != "" && conv.ProactiveContext != "{}" {
		proactiveCtx = conv.ProactiveContext
	}

	// Check if this is a planning conversation — use specialized prompt and tools
	isPlanning := conv.ProactiveType == "planning"
	_, isSessionProvider := p.(llm.SessionProvider)

	var systemPrompt string
	if isPlanning {
		var projectNames []string
		projects, _ := h.api.db.ListProjects(ctx)
		for _, pr := range projects {
			projectNames = append(projectNames, fmt.Sprintf("[%d] %s (%s)", pr.ID, pr.Name, pr.Type))
		}
		systemPrompt = llm.PlanningSystemPrompt(projectNames, proactiveCtx)
	} else {
		systemPrompt = h.buildSystemPromptWithContext(ctx, proactiveCtx)
	}

	// Get model — empty string means "let the provider use its own default"
	model, _ := h.api.db.GetSetting(ctx, "ai_model")

	// Determine if we should use tools:
	// - Session providers (gosdk, nodesdk) handle tools internally via MCP — no tools in request
	// - API key provider uses native Anthropic tool definitions
	var tools []llm.ToolDefinition
	if !isSessionProvider && p.Name() == "apikey" {
		if isPlanning {
			tools = llm.PlanningTools()
		} else {
			tools = llm.ChatTools()
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

	// Stream response — planning mode gets more tokens for detailed responses
	maxTokens := 2048
	if isPlanning {
		maxTokens = 4096
	}
	req := &llm.Request{
		System:         systemPrompt,
		Messages:       messages,
		Tools:          tools,
		MaxTokens:      maxTokens,
		Model:          model,
		ConversationID: conv.ID,
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

	// Create independent context for LLM streaming (not tied to HTTP request lifecycle)
	streamCtx, streamCancel := context.WithCancel(context.Background())
	h.registerStream(conv.ID, streamCancel)

	// Track streaming error for defer
	var streamErr string
	var resp *llm.Response
	var totalUsage llm.Usage

	// Defer: always persist final state of the assistant message
	defer func() {
		h.unregisterStream(conv.ID)
		sf.stop()

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
	maxIter := 5
	if isPlanning {
		maxIter = 15
	}
	if resp != nil && resp.StopReason == "tool_use" && !isSessionProvider {
		loopUsage, loopErr := h.handleToolLoop(streamCtx, safeSendSSE, p, req, resp, conv, &assistantText, &toolCalls, &textMu, maxIter)
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
) (llm.Usage, error) {
	var accumulated llm.Usage

	for i := 0; i < maxIterations && resp.StopReason == "tool_use"; i++ {
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
			result, err := h.executeTool(toolCtx, block.Name, inputMap, conv.ID)
			toolCancel()
			if err != nil {
				result = fmt.Sprintf("Error: %s", err.Error())
			}

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
			return accumulated, err
		}

		// Accumulate token usage from this iteration
		if resp != nil {
			accumulated.InputTokens += resp.Usage.InputTokens
			accumulated.OutputTokens += resp.Usage.OutputTokens
		}
	}

	return accumulated, nil
}

// ExecuteTool implements llm.ToolExecutor. It wraps the private executeTool method
// so that SDK providers can call DevManager tools from their in-process MCP servers.
func (h *AIHandler) ExecuteTool(ctx context.Context, name string, input map[string]any, conversationID int64) (string, error) {
	return h.executeTool(ctx, name, input, conversationID)
}

// executeTool runs a tool and returns the result as a string.
func (h *AIHandler) executeTool(ctx context.Context, name string, input map[string]interface{}, conversationID int64) (string, error) {
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
			ID:      docID,
			Title:   fmt.Sprintf("Memory Doc: %s (v%d)", projectName, doc.Version),
			Content: doc.Content,
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
		if len(tasks) == 0 {
			return fmt.Sprintf("No tasks found for project %d.", projectID), nil
		}
		statusIcons := map[string]string{"todo": "[ ]", "in_progress": "[~]", "done": "[x]", "blocked": "[!]"}
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

		task := &database.ProjectTask{
			ProjectID:   projectID,
			Title:       title,
			Description: description,
			Status:      status,
			Priority:    priority,
		}

		if dueDateStr, ok := input["due_date"].(string); ok && dueDateStr != "" {
			t, err := parseFlexibleTime(dueDateStr)
			if err == nil {
				task.DueDate = sql.NullTime{Time: t, Valid: true}
			}
		}

		if parentIDStr, ok := input["parent_id"].(string); ok && parentIDStr != "" {
			pid, err := strconv.ParseInt(parentIDStr, 10, 64)
			if err == nil {
				task.ParentID = sql.NullInt64{Int64: pid, Valid: true}
			}
		} else if pidF, ok := input["parent_id"].(float64); ok {
			task.ParentID = sql.NullInt64{Int64: int64(pidF), Valid: true}
		}

		if err := h.api.db.CreateTask(ctx, task); err != nil {
			return "", err
		}
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "created", "project_id": projectID, "task": task})
		return fmt.Sprintf("Task '%s' created (ID: %d)", title, task.ID), nil

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

		if t, ok := input["title"].(string); ok && t != "" {
			task.Title = t
		}
		if d, ok := input["description"].(string); ok {
			task.Description = d
		}
		if s, ok := input["status"].(string); ok && s != "" {
			task.Status = s
		}
		if p, ok := input["priority"].(string); ok && p != "" {
			task.Priority = p
		}
		if dueDateStr, ok := input["due_date"].(string); ok {
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

		if err := h.api.db.UpdateTask(ctx, task); err != nil {
			return "", err
		}
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": projectID, "task": task})
		return fmt.Sprintf("Task '%s' updated", task.Title), nil

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
		taskTitle := task.Title

		if err := h.api.db.DeleteTask(ctx, taskID); err != nil {
			return "", err
		}
		h.api.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "deleted", "project_id": projectID, "id": taskID})
		return fmt.Sprintf("Task '%s' deleted", taskTitle), nil

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

		if len(tasks) == 0 {
			return fmt.Sprintf("Project '%s' has no tasks yet.", project.Name), nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## Task Report: %s\n\n", project.Name))
		total := 0
		for _, c := range summary {
			total += c
		}
		sb.WriteString(fmt.Sprintf("**Total:** %d tasks\n", total))
		sb.WriteString(fmt.Sprintf("- Todo: %d\n- In Progress: %d\n- Done: %d\n- Blocked: %d\n\n", summary["todo"], summary["in_progress"], summary["done"], summary["blocked"]))

		// Overdue tasks
		var overdue []database.ProjectTask
		for _, t := range tasks {
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

		// Recommended next task: highest priority non-blocked todo/in_progress, or nearest due
		priorityOrder := map[string]int{"urgent": 4, "high": 3, "medium": 2, "low": 1}
		var best *database.ProjectTask
		for i, t := range tasks {
			if t.Status == "done" || t.Status == "blocked" || t.ParentID.Valid {
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

	case "activate_planning_mode":
		projectID, err := parseIDParam(input, "project_id")
		if err != nil {
			return "", err
		}
		project, err := h.api.db.GetProject(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("project not found")
		}

		// Update conversation to planning mode
		proactiveData, _ := json.Marshal(map[string]interface{}{
			"intent":       "planning",
			"project_id":   projectID,
			"project_name": project.Name,
		})
		err = h.api.db.UpdateAIConversationMode(ctx, conversationID, "planning", string(proactiveData))
		if err != nil {
			return "", fmt.Errorf("failed to activate planning mode: %w", err)
		}

		return fmt.Sprintf("Planning mode activated for project **%s** (ID: %d). "+
			"On the next message, you will have access to file exploration tools (list_directory, read_file, find_files, grep_content) and the planning system prompt. "+
			"Tell the user that planning mode is now active and ask them to describe what they want to implement.", project.Name, projectID), nil

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

	result, err := h.executeTool(ctx, input.Name, input.Args, input.ConversationID)
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
func (h *AIHandler) HandleToolDefinitions(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, llm.ChatTools())
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
func (h *AIHandler) buildSystemPromptWithContext(ctx context.Context, proactiveCtx string) string {
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

	return llm.ChatSystemPromptWithProactiveContext(skillNames, projectNames, mcpNames, proactiveCtx)
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

// HandleInitiatePlanning creates an AI-initiated conversation for planning tasks.
func (h *AIHandler) HandleInitiatePlanning(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID int64 `json:"project_id"`
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

	proactiveCtx, _ := json.Marshal(map[string]interface{}{
		"intent":       "planning",
		"project_id":   input.ProjectID,
		"project_name": project.Name,
	})

	assistantMsg := fmt.Sprintf(
		"Modo de planejamento ativado para o projeto **%s**.\n\n"+
			"Descreva o que você gostaria de implementar ou melhorar, e eu vou:\n"+
			"1. Explorar o código do projeto\n"+
			"2. Entender a arquitetura\n"+
			"3. Quebrar o trabalho em tarefas detalhadas\n\n"+
			"O que você tem em mente?",
		project.Name)

	title := fmt.Sprintf("Planejamento: %s", project.Name)
	conv, err := h.api.db.CreateProactiveConversation(ctx, title, "standard", "planning", string(proactiveCtx), assistantMsg)
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

	// Skip automatic evaluations if disabled in settings (manual "session_request" always allowed)
	if trigger != "session_request" {
		val, _ := h.api.db.GetSetting(ctx, "task_auto_eval_enabled")
		if val != "true" {
			log.Printf("[AI-Session] Auto task evaluation disabled (trigger=%s), skipping", trigger)
			return
		}
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
- Respond with JSON only: {"suggestions": [{"type": "<type>", "title": "<title>", "description": "<why>", "task_id": <id_or_null>, "task_data": {"title": "...", "description": "...", "priority": "..."}}]}
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

	if len(result.Suggestions) == 0 {
		log.Printf("[AI-Session] All suggestions filtered out for session %s (%s)", sessionID[:8], trigger)
		return
	}

	log.Printf("[AI-Session] Got %d suggestions for session %s (after filter)", len(result.Suggestions), sessionID[:8])

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
		st := &database.SessionTask{
			SessionID: suggestion.SessionID,
			TaskID:    *ctx.TaskID,
			Role:      "works_on",
		}
		if err := h.api.db.CreateSessionTask(r.Context(), st); err != nil {
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
			st := &database.SessionTask{
				SessionID: suggestion.SessionID,
				TaskID:    task.ID,
				Role:      "registered_as",
			}
			h.api.db.CreateSessionTask(r.Context(), st)
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

	if err := h.api.db.UpdateAISuggestionStatus(r.Context(), id, "dismissed"); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
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
