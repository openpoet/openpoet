package mcp

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"
)

// RunCLI handles the "openpoet cli" subcommand.
// It exposes the same MCP tools as the stdio/HTTP transports, but via bash commands.
// This allows backends like GitHub Copilot (which may not support MCP) to interact
// with OpenPoet through shell commands instead.
//
// Environment variables (set automatically by the session backend):
//
//	OPENPOET_HOOK_URL    - API base URL (fallback: OPENPOET_API_URL, then http://localhost:8080)
//	OPENPOET_SESSION_ID  - Session ID for context-aware tools
//
// Usage:
//
//	openpoet cli tools                          - list available tools
//	openpoet cli call <tool_name> [json_args]   - call a tool
func RunCLI(args []string) {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[cli] ")

	if len(args) == 0 {
		printCLIUsage()
		os.Exit(1)
	}

	apiURL := os.Getenv("OPENPOET_HOOK_URL")
	if apiURL == "" {
		apiURL = os.Getenv("OPENPOET_API_URL")
	}
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	sessionID := os.Getenv("OPENPOET_SESSION_ID")

	client := NewAPIClient(apiURL)
	handler := NewRequestHandler(client, sessionID, "session", "", ToolPolicy{Mode: "deny_all"})

	switch args[0] {
	case "tools":
		cliListTools(handler)
	case "call":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: openpoet cli call <tool_name> [json_args]")
			os.Exit(1)
		}
		argsJSON := "{}"
		if len(args) >= 3 {
			argsJSON = strings.Join(args[2:], " ")
		}
		cliCallTool(handler, args[1], argsJSON)
	default:
		fmt.Fprintf(os.Stderr, "Unknown cli subcommand: %s\n", args[0])
		printCLIUsage()
		os.Exit(1)
	}
}

func printCLIUsage() {
	fmt.Fprintf(os.Stderr, `Usage: openpoet cli <command>

Commands:
  tools                         List available tools with descriptions
  call <tool_name> [json_args]  Call a tool by name with JSON arguments

Environment variables:
  OPENPOET_HOOK_URL    API base URL (default: http://localhost:8080)
  OPENPOET_SESSION_ID  Session ID for context-aware operations

Examples:
  openpoet cli tools
  openpoet cli call openpoet_list_tasks '{"project_id":"1"}'
  openpoet cli call openpoet_create_task '{"project_id":"1","title":"Fix bug"}'
`)
}

func cliListTools(handler *RequestHandler) {
	handler.policy = handler.fetchPolicy()
	tools := toolsDef("session")
	tools = handler.policy.FilterTools(tools)

	if len(tools) == 0 {
		fmt.Println("No tools available (check tool policy)")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, t := range tools {
		desc := t.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\n", t.Name, desc)
	}
	w.Flush()
}

func cliCallTool(handler *RequestHandler, toolName, argsJSON string) {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	if !json.Valid([]byte(argsJSON)) {
		fmt.Fprintf(os.Stderr, "Error: invalid JSON arguments: %s\n", argsJSON)
		os.Exit(1)
	}

	handler.policy = handler.fetchPolicy()

	if !handler.policy.IsAllowed(toolName) {
		fmt.Fprintf(os.Stderr, "Error: tool '%s' is not allowed by the current policy\n", toolName)
		os.Exit(1)
	}

	result, err := executeTool(handler.client, toolName, json.RawMessage(argsJSON), handler.sessionID, handler.conversationID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		os.Exit(1)
	}

	fmt.Print(result)
}
