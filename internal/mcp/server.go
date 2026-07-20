package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
)

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// RequestHandler processes MCP JSON-RPC requests.
// Used by both the stdio transport (Serve) and the HTTP transport (HTTPHandler).
type RequestHandler struct {
	client         *APIClient
	sessionID      string
	context        string // "session", "chat", "http"
	conversationID string
	policy         ToolPolicy
}

// NewRequestHandler creates a RequestHandler with the given parameters.
func NewRequestHandler(client *APIClient, sessionID, context, conversationID string, policy ToolPolicy) *RequestHandler {
	return &RequestHandler{
		client:         client,
		sessionID:      sessionID,
		context:        context,
		conversationID: conversationID,
		policy:         policy,
	}
}

// fetchPolicy fetches the current effective tool policy from the OpenPoet API.
// It resolves the global policy for the handler's context and any project-specific override.
// Returns deny_all as a safe default on any error.
func (h *RequestHandler) fetchPolicy() ToolPolicy {
	if h.client == nil {
		return h.policy
	}

	policy := ToolPolicy{Mode: "deny_all"}

	// Determine which policy key to fetch based on context
	policyKey := "session"
	if h.context == "http" {
		policyKey = "http"
	} else if h.context == "chat" {
		policyKey = "chat"
	}

	if data, err := h.client.Get("/api/config/tool-policies"); err == nil {
		var policies map[string]string
		if json.Unmarshal(data, &policies) == nil {
			if policyStr, ok := policies[policyKey]; ok && policyStr != "" {
				policy = ParsePolicy(policyStr)
			}
		}
	}

	// Per-project policy override (only when session is known)
	if h.sessionID != "" {
		if data, err := h.client.Get("/api/sessions/" + h.sessionID); err == nil {
			var sess struct {
				ProjectID int `json:"project_id"`
			}
			if json.Unmarshal(data, &sess) == nil && sess.ProjectID > 0 {
				if data, err := h.client.Get(fmt.Sprintf("/api/projects/%d/tool-policy", sess.ProjectID)); err == nil {
					var resp struct {
						ToolPolicy string `json:"tool_policy"`
					}
					if json.Unmarshal(data, &resp) == nil && resp.ToolPolicy != "" {
						policy = ParsePolicy(resp.ToolPolicy)
					}
				}
				// Auto-allow share tools if project has shares configured
				if data, err := h.client.Get(fmt.Sprintf("/api/projects/%d/shares", sess.ProjectID)); err == nil {
					var shares []json.RawMessage
					if json.Unmarshal(data, &shares) == nil && len(shares) > 0 {
						policy = policy.AllowTools(ShareToolNames)
					}
				}
			}
		}
	}

	return policy
}

// Handle processes a single JSON-RPC request and returns the response.
func (h *RequestHandler) Handle(req *jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "openpoet",
					"version": "1.0.0",
				},
			},
		}

	case "tools/list":
		h.policy = h.fetchPolicy()
		tools := toolsDef(h.context)
		tools = h.policy.FilterTools(tools)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"tools": tools,
			},
		}

	case "tools/call":
		h.policy = h.fetchPolicy()
		return h.handleToolCall(req)

	default:
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
		}
	}
}

func (h *RequestHandler) handleToolCall(req *jsonRPCRequest) *jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcError{
				Code:    -32602,
				Message: fmt.Sprintf("invalid params: %v", err),
			},
		}
	}

	// Check policy before executing
	if !h.policy.IsAllowed(params.Name) {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": fmt.Sprintf("Error: tool '%s' is not allowed by the current policy", params.Name)},
				},
				"isError": true,
			},
		}
	}

	log.Printf("tool call: %s args=%s", params.Name, string(params.Arguments))

	result, err := executeTool(h.client, params.Name, params.Arguments, h.sessionID, h.conversationID)
	if err != nil {
		log.Printf("tool error: %s: %v", params.Name, err)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": fmt.Sprintf("Error: %s", err.Error())},
				},
				"isError": true,
			},
		}
	}

	log.Printf("tool result: %s: %s", params.Name, truncate(result, 200))

	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": result},
			},
		},
	}
}

// Serve runs the MCP server over stdio (stdin/stdout).
// Logs go to stderr to keep stdout clean for the MCP transport.
func Serve(apiURL string) {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[mcp] ")

	sessionID := os.Getenv("OPENPOET_SESSION_ID")
	context := os.Getenv("OPENPOET_CONTEXT")
	conversationID := os.Getenv("OPENPOET_CONVERSATION_ID")
	sessionToken := os.Getenv("OPENPOET_SESSION_TOKEN")
	client := NewAPIClientWithToken(apiURL, sessionToken)

	// Start with deny_all as safe default. The handler dynamically fetches the
	// current policy from the API on every tools/list and tools/call request,
	// ensuring policy changes are reflected immediately in active sessions.
	handler := NewRequestHandler(client, sessionID, context, conversationID, ToolPolicy{Mode: "deny_all"})

	reader := bufio.NewReader(os.Stdin)
	writer := os.Stdout

	log.Printf("MCP server started, API URL: %s, Session ID: %s, Context: %s, ConversationID: %s", apiURL, sessionID, context, conversationID)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				log.Printf("stdin closed, exiting")
				return
			}
			log.Printf("read error: %v", err)
			return
		}

		if len(line) == 0 || (len(line) == 1 && line[0] == '\n') {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid JSON-RPC: %v", err)
			continue
		}

		log.Printf("received: method=%s id=%s", req.Method, string(req.ID))

		if req.ID == nil || string(req.ID) == "null" {
			log.Printf("notification: %s (no response needed)", req.Method)
			continue
		}

		resp := handler.Handle(&req)
		respBytes, err := json.Marshal(resp)
		if err != nil {
			log.Printf("marshal error: %v", err)
			continue
		}

		respBytes = append(respBytes, '\n')
		if _, err := writer.Write(respBytes); err != nil {
			log.Printf("write error: %v", err)
			return
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
