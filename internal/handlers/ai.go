package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"devmanager/internal/database"
	"devmanager/internal/llm"

	"github.com/go-chi/chi/v5"
)

// AIHandler handles all AI-related endpoints.
type AIHandler struct {
	api      *API
	provider llm.Provider

	mu          sync.RWMutex
	rateLimiter map[string][]time.Time // simple per-minute rate limiting
}

// NewAIHandler creates a new AI handler.
func NewAIHandler(api *API, provider llm.Provider) *AIHandler {
	return &AIHandler{
		api:         api,
		provider:    provider,
		rateLimiter: make(map[string][]time.Time),
	}
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
		model = "claude-sonnet-4-5-20250929"
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

	// Convert to LLM messages
	var messages []llm.Message
	for _, m := range dbMessages {
		messages = append(messages, llm.NewTextMessage(m.Role, m.Content))
	}

	// Build system prompt with current state
	systemPrompt := h.buildSystemPrompt(ctx)

	// Get model
	model, _ := h.api.db.GetSetting(ctx, "ai_model")
	if model == "" {
		model = "claude-sonnet-4-5-20250929"
	}

	// Determine if we should use tools (only with apikey provider — claudecli doesn't support native tools)
	var tools []llm.ToolDefinition
	if p.Name() == "apikey" {
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

	// Send conversation ID
	h.sendSSE(w, flusher, "conversation", map[string]interface{}{
		"id":    conv.ID,
		"title": conv.Title,
	})

	// Stream response
	req := &llm.Request{
		System:    systemPrompt,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: 4096,
		Model:     model,
	}

	var assistantText strings.Builder
	var toolCalls []map[string]interface{}

	resp, err := p.StreamMessage(ctx, req, func(event llm.StreamEvent) error {
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil {
				if event.ContentBlock.Type == "tool_use" {
					h.sendSSE(w, flusher, "tool_start", map[string]interface{}{
						"id":   event.ContentBlock.ID,
						"name": event.ContentBlock.Name,
					})
				}
			}
		case "content_block_delta":
			if event.Delta != nil && event.Delta.Type == "text_delta" {
				h.sendSSE(w, flusher, "text", map[string]interface{}{
					"text": event.Delta.Text,
				})
				assistantText.WriteString(event.Delta.Text)
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("[AI] Stream error: %v", err)
		h.sendSSE(w, flusher, "error", map[string]interface{}{
			"message": err.Error(),
		})
		return
	}

	// Handle tool use loop
	if resp != nil && resp.StopReason == "tool_use" {
		err = h.handleToolLoop(ctx, w, flusher, p, req, resp, conv, &assistantText, &toolCalls)
		if err != nil {
			log.Printf("[AI] Tool loop error: %v", err)
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"message": err.Error(),
			})
			return
		}
	}

	// Save assistant message
	toolCallsJSON, _ := json.Marshal(toolCalls)
	assistantMsg := &database.AIMessage{
		ConversationID: conv.ID,
		Role:           "assistant",
		Content:        assistantText.String(),
		ToolCalls:      string(toolCallsJSON),
	}
	h.api.db.CreateAIMessage(ctx, assistantMsg)
	h.api.db.TouchAIConversation(ctx, conv.ID)

	h.sendSSE(w, flusher, "done", map[string]interface{}{
		"conversation_id": conv.ID,
	})
}

// handleToolLoop executes tool use requests and continues the conversation.
func (h *AIHandler) handleToolLoop(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	p llm.Provider,
	req *llm.Request,
	resp *llm.Response,
	conv *database.AIConversation,
	assistantText *strings.Builder,
	toolCalls *[]map[string]interface{},
) error {
	maxIterations := 5

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

			*toolCalls = append(*toolCalls, map[string]interface{}{
				"id":    block.ID,
				"name":  block.Name,
				"input": inputMap,
			})

			h.sendSSE(w, flusher, "tool_executing", map[string]interface{}{
				"id":   block.ID,
				"name": block.Name,
			})

			result, err := h.executeTool(ctx, block.Name, inputMap)
			if err != nil {
				result = fmt.Sprintf("Error: %s", err.Error())
			}

			h.sendSSE(w, flusher, "tool_result", map[string]interface{}{
				"id":     block.ID,
				"name":   block.Name,
				"result": result,
			})

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
				h.sendSSE(w, flusher, "tool_start", map[string]interface{}{
					"id":   event.ContentBlock.ID,
					"name": event.ContentBlock.Name,
				})
			}
			if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
				h.sendSSE(w, flusher, "text", map[string]interface{}{
					"text": event.Delta.Text,
				})
				assistantText.WriteString(event.Delta.Text)
			}
			return nil
		})

		if err != nil {
			return err
		}
	}

	return nil
}

// executeTool runs a tool and returns the result as a string.
func (h *AIHandler) executeTool(ctx context.Context, name string, input map[string]interface{}) (string, error) {
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

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
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

// HandleListConversations lists all conversations.
func (h *AIHandler) HandleListConversations(w http.ResponseWriter, r *http.Request) {
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

	messages, err := h.api.db.ListAIMessages(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if messages == nil {
		messages = []database.AIMessage{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": conv,
		"messages":     messages,
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
	if model == "" {
		model = "claude-sonnet-4-5-20250929"
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

	req := &llm.Request{
		System: llm.SkillGenerationPrompt,
		Messages: []llm.Message{
			llm.NewTextMessage("user", input.Description),
		},
		MaxTokens: 4096,
		Model:     model,
	}

	var fullText strings.Builder

	_, err := p.StreamMessage(r.Context(), req, func(event llm.StreamEvent) error {
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
	if model == "" {
		model = "claude-sonnet-4-5-20250929"
	}

	req := &llm.Request{
		System: llm.SkillValidationPrompt,
		Messages: []llm.Message{
			llm.NewTextMessage("user", input.Content),
		},
		MaxTokens: 1024,
		Model:     model,
	}

	var fullText strings.Builder

	_, err := p.StreamMessage(r.Context(), req, func(event llm.StreamEvent) error {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			fullText.WriteString(event.Delta.Text)
		}
		return nil
	})

	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
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
