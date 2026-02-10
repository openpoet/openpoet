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
	JSONRPC string      `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the MCP server, reading JSON-RPC from stdin and writing to stdout.
// Logs go to stderr to keep stdout clean for the MCP transport.
func Serve(apiURL string) {
	// Redirect log to stderr (stdout is the MCP transport)
	log.SetOutput(os.Stderr)
	log.SetPrefix("[mcp] ")

	sessionID := os.Getenv("DEVMANAGER_SESSION_ID")
	client := NewAPIClient(apiURL)

	reader := bufio.NewReader(os.Stdin)
	writer := os.Stdout

	log.Printf("MCP server started, API URL: %s, Session ID: %s", apiURL, sessionID)

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

		// Skip empty lines
		if len(line) == 0 || (len(line) == 1 && line[0] == '\n') {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid JSON-RPC: %v", err)
			continue
		}

		log.Printf("received: method=%s id=%s", req.Method, string(req.ID))

		// Handle notifications (no id = no response)
		if req.ID == nil || string(req.ID) == "null" {
			log.Printf("notification: %s (no response needed)", req.Method)
			continue
		}

		resp := handleRequest(client, &req, sessionID)
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

func handleRequest(client *APIClient, req *jsonRPCRequest, sessionID string) *jsonRPCResponse {
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
					"name":    "devmanager",
					"version": "1.0.0",
				},
			},
		}

	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"tools": toolsDef(),
			},
		}

	case "tools/call":
		return handleToolCall(client, req, sessionID)

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

func handleToolCall(client *APIClient, req *jsonRPCRequest, sessionID string) *jsonRPCResponse {
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

	log.Printf("tool call: %s args=%s", params.Name, string(params.Arguments))

	result, err := executeTool(client, params.Name, params.Arguments, sessionID)
	if err != nil {
		log.Printf("tool error: %s: %v", params.Name, err)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": fmt.Sprintf("Error: %s", err.Error()),
					},
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
				{
					"type": "text",
					"text": result,
				},
			},
		},
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
