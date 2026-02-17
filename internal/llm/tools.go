package llm

import "encoding/json"

// ToolContext identifies where a tool is available.
type ToolContext string

const (
	ToolContextChat    ToolContext = "chat"    // AI chat assistant only
	ToolContextSession ToolContext = "session" // MCP session only
	ToolContextBoth    ToolContext = "both"    // Both chat and session
)

// ToolDef is the unified tool definition used across all paths.
// This is the SINGLE SOURCE OF TRUTH for all DevManager tools.
type ToolDef struct {
	Name           string              // Canonical name without prefix (e.g. "create_skill")
	MCPName        string              // Explicit MCP name (if empty, auto = "devmanager_" + Name)
	Description    string              // Default description
	MCPDescription string              // Override description for MCP context (if empty, uses Description)
	InputSchema    ToolDefinitionInput // Typed Go schema (single source of truth)
	Context        ToolContext         // Where this tool is available
	ChatOnly bool // If true, filtered out from MCP sessions (only available in MCP context "chat")
}

// MCPToolDef is the MCP protocol format with JSON raw schema.
type MCPToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// AllToolDefs returns the complete unified tool registry.
// All 33 tools are defined here — chat, session, and shared.
func AllToolDefs() []ToolDef {
	return []ToolDef{
		// ──── Shared tools (both chat + session) ────

		{
			Name:        "create_skill",
			Description: "Create a new skill in DevManager. Skills are markdown instructions synced to projects as .claude/skills/<name>/SKILL.md for Claude Code to follow.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"name":     {Type: "string", Description: "Unique name. MUST be lowercase with hyphens, no spaces (e.g. 'python-best-practices'). Max 64 chars."},
					"content":  {Type: "string", Description: "Markdown content with instructions. Do NOT include YAML frontmatter (---) — it is auto-generated."},
					"category": {Type: "string", Description: "Category label (e.g. coding, testing, deployment, documentation, workflow)"},
				},
				Required: []string{"name", "content"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "update_skill",
			Description: "Update an existing skill by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"id":       {Type: "string", Description: "The skill ID (number)"},
					"name":     {Type: "string", Description: "New name for the skill"},
					"content":  {Type: "string", Description: "New markdown content"},
					"category": {Type: "string", Description: "New category label"},
				},
				Required: []string{"id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "delete_skill",
			Description: "Delete a skill by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"id": {Type: "string", Description: "The skill ID (number)"},
				},
				Required: []string{"id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "list_skills",
			Description:    "List all skills in DevManager.",
			MCPDescription: "List all skills in DevManager. Skills are markdown instruction templates stored in the database.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "list_projects",
			Description:    "List all projects in DevManager.",
			MCPDescription: "List all projects managed by DevManager.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context:      ToolContextBoth,

		},
		{
			Name:           "list_mcp_servers",
			Description:    "List all MCP servers in DevManager.",
			MCPDescription: "List all MCP server configurations in DevManager.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "create_mcp_server",
			Description: "Create a new MCP server configuration.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"name":    {Type: "string", Description: "Name of the MCP server"},
					"command": {Type: "string", Description: "Command to run the server"},
					"args":    {Type: "string", Description: "JSON array of arguments (e.g. '[\"--port\", \"3000\"]'). Can also be passed as a native array."},
					"env":     {Type: "string", Description: "JSON object of environment variables (e.g. '{\"KEY\": \"value\"}'). Can also be passed as a native object."},
				},
				Required: []string{"name", "command"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "update_setting",
			Description: "Update a DevManager setting.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"key":   {Type: "string", Description: "Setting key"},
					"value": {Type: "string", Description: "Setting value"},
				},
				Required: []string{"key", "value"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "sync_config",
			Description:    "Sync configuration (skills, MCP servers, hooks) to all projects.",
			MCPDescription: "Sync configuration (skills and hooks) to all projects. MCP servers are injected via --mcp-config CLI flag at session start.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "get_memory_doc",
			Description:    "Get the memory doc (CLAUDE.md) for a project. Returns a viewer link + internal reference. IMPORTANT: Never paste the content in chat — only share the viewer link with the user.",
			MCPDescription: "Get the memory doc for a project. The memory doc tracks project goals, progress, and key decisions. Returns the markdown content or empty if none exists.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
				},
				Required: []string{"project_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "update_memory_doc",
			Description:    "Propose changes to the memory doc (CLAUDE.md) for a project. Creates a preview for user approval — changes are NOT applied immediately. Only use when the user explicitly asks.",
			MCPDescription: "Propose changes to a project's memory doc. Changes are NOT applied immediately — they create a proposal that the user must approve via the DevManager UI.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"content":    {Type: "string", Description: "Full markdown content for the memory doc"},
					"summary":    {Type: "string", Description: "Brief summary of what changed in this update"},
				},
				Required: []string{"project_id", "content"},
			},
			Context:  ToolContextBoth,
			ChatOnly: true,
		},
		{
			Name:           "list_tasks",
			Description:    "List all tasks for a project. Shows title, status, priority, due date.",
			MCPDescription: "List all tasks for a project. Returns task title, status, priority, due date, and subtask relationships.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
				},
				Required: []string{"project_id"},
			},
			Context:      ToolContextBoth,

		},
		{
			Name:           "get_task",
			Description:    "Get full details of a single task: title, description, status, priority, due date, parent, subtasks.",
			MCPDescription: "Get the full details of a task by project_id and task_id. Returns all metadata including description.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"task_id":    {Type: "string", Description: "The task ID (number)"},
				},
				Required: []string{"project_id", "task_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "create_task",
			Description: "Create a new task in a project.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":  {Type: "string", Description: "The project ID (number)"},
					"title":       {Type: "string", Description: "Task title"},
					"description": {Type: "string", Description: "Task description"},
					"status":      {Type: "string", Description: "Status: todo, in_progress, awaiting_approval, done (default: todo)"},
					"priority":    {Type: "string", Description: "Priority: low, medium, high, urgent (default: medium)"},
					"due_date":    {Type: "string", Description: "Due date in ISO 8601 format (e.g. 2025-01-15T14:00)"},
					"parent_id":   {Type: "string", Description: "Parent task ID for existing subtasks"},
					"parent_ref":  {Type: "string", Description: "Reference to a parent task in the same planning batch (1-based index). Use this instead of parent_id when creating subtasks in planning mode — the parent doesn't exist yet. Example: if the parent is the 1st task you created, use parent_ref=1."},
					"sort_order":  {Type: "string", Description: "Sort order (integer). Lower numbers appear first. Use sequential values (1, 2, 3...) to define execution order."},
				},
				Required: []string{"project_id", "title"},
			},
			Context:      ToolContextBoth,

		},
		{
			Name:           "update_task",
			Description:    "Update an existing task.",
			MCPDescription: "Update an existing task by project_id and task_id.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":  {Type: "string", Description: "The project ID (number)"},
					"task_id":     {Type: "string", Description: "The task ID (number)"},
					"title":       {Type: "string", Description: "New title"},
					"description": {Type: "string", Description: "New description"},
					"status":      {Type: "string", Description: "New status: todo, in_progress, awaiting_approval, done"},
					"priority":    {Type: "string", Description: "New priority: low, medium, high, urgent"},
					"due_date":    {Type: "string", Description: "New due date (empty string to clear)"},
				},
				Required: []string{"project_id", "task_id"},
			},
			Context:      ToolContextBoth,

		},
		{
			Name:           "delete_task",
			Description:    "Delete a task and its subtasks.",
			MCPDescription: "Delete a task by project_id and task_id. Also deletes subtasks.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"task_id":    {Type: "string", Description: "The task ID (number)"},
				},
				Required: []string{"project_id", "task_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "create_document",
			Description:    "Create a temporary markdown document and return a clickable link. Use this for ANY response that would be longer than 5 lines — lists, explanations, code, reports, etc. This keeps the chat clean.",
			MCPDescription: "Create a temporary markdown document for the user to view. Use for ANY response longer than ~5 lines (lists, explanations, code, reports). Keeps the chat clean.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"title":           {Type: "string", Description: "Short document title"},
					"content":         {Type: "string", Description: "Full markdown content of the document"},
					"conversation_id": {Type: "string", Description: "Current conversation ID (if available)"},
					"task_id":         {Type: "string", Description: "Optional task ID to link this document to a task"},
				},
				Required: []string{"content"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "read_document",
			Description:    "Read the full content of a document by ID.",
			MCPDescription: "Read the full content of a document by ID. Use after devmanager_get_task to read specific documents.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"document_id": {Type: "string", Description: "The document ID (8-char ID for documents, or 'plan:sessionId' for session plans)"},
				},
				Required: []string{"document_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "list_directory",
			MCPName:        "devmanager_list_project_files",
			Description:    "List files and directories in a project path. Returns names, sizes, and types. Use to browse the project structure.",
			MCPDescription: "List files and directories in a project. Returns names, sizes, and types. Use path parameter to navigate subdirectories.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"path":       {Type: "string", Description: "Relative path within the project (empty or omit for root directory)"},
				},
				Required: []string{"project_id"},
			},
			Context:      ToolContextBoth,

		},
		{
			Name:           "read_file",
			MCPName:        "devmanager_read_project_file",
			Description:    "Read the content of a text file from a project. Supports optional line offset and limit for reading specific sections of large files. Max 2MB file size.",
			MCPDescription: "Read the content of a text file from a project. Max 2MB, text files only. Returns the file content as text.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"path":       {Type: "string", Description: "Relative path to the file within the project"},
					"offset":     {Type: "string", Description: "Line number to start reading from (1-based, optional)"},
					"limit":      {Type: "string", Description: "Number of lines to read (optional, default: all lines)"},
				},
				Required: []string{"project_id", "path"},
			},
			Context:      ToolContextBoth,

		},

		// ──── Chat-only tools ────

		{
			Name:        "find_files",
			Description: "Find files matching a glob pattern recursively in a project. Returns matching file paths sorted alphabetically. Skips common non-essential directories (.git, node_modules, vendor, etc.). Max 100 results.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"pattern":    {Type: "string", Description: "Glob pattern to match file names (e.g. '*.go', '*.tsx', 'Makefile')"},
				},
				Required: []string{"project_id", "pattern"},
			},
			Context:      ToolContextChat,

		},
		{
			Name:        "grep_content",
			Description: "Search file contents using a regex pattern in a project. Returns matching lines with file paths and line numbers. Skips binary files and files larger than 1MB. Max 50 results.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"pattern":    {Type: "string", Description: "Regular expression pattern to search for (e.g. 'func.*Handler', 'import.*react')"},
					"path":       {Type: "string", Description: "Subdirectory to restrict search to (optional, empty = entire project)"},
					"glob":       {Type: "string", Description: "File name filter pattern (e.g. '*.go', '*.ts') — optional"},
				},
				Required: []string{"project_id", "pattern"},
			},
			Context:      ToolContextChat,

		},
		{
			Name:        "get_task_report",
			Description: "Get a task summary report for a project. Shows status counts, overdue tasks, and recommends the next task to work on.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
				},
				Required: []string{"project_id"},
			},
			Context:      ToolContextChat,

		},
		// ──── Session-only tools ────

		{
			Name:        "update_mcp_server",
			Description: "Update a DevManager MCP server configuration by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"id":      {Type: "string", Description: "The MCP server ID (number)"},
					"name":    {Type: "string", Description: "New name"},
					"command": {Type: "string", Description: "New command"},
					"args":    {Type: "string", Description: "JSON array of arguments (e.g. '[\"--headless\"]'). Can also be passed as a native array."},
					"env":     {Type: "string", Description: "JSON object of environment variables. Can also be passed as a native object."},
					"enabled": {Type: "boolean", Description: "Whether the MCP server is enabled"},
				},
				Required: []string{"id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "delete_mcp_server",
			Description: "Delete a DevManager MCP server configuration by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"id": {Type: "string", Description: "The MCP server ID (number)"},
				},
				Required: []string{"id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "get_my_task",
			Description: "Get the task linked to the current session (if any). Returns the task details including status, title, description. Only works within a DevManager session.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "get_session_info",
			Description: "Get information about the current DevManager session, including session ID, project, status, and linked task. Only works within a DevManager session.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "request_task_evaluation",
			Description: "Request the DevManager AI Assistant to evaluate the current session and proactively suggest task actions (create, update, link, or complete tasks). The AI Assistant will analyze the session output and suggest actions to the user via floating notification cards. Use this when you believe the session's work is relevant to task management.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "dashboard",
			Description: "Compact state summary: projects with task counts, active sessions, recent changes. Use this first to understand the current state before making changes.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "batch",
			Description: "Execute multiple tools in one call. Returns array of results. Max 10 calls per batch.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"calls": {
						Type: "array",
						Items: &ToolPropertySchema{
							Type: "object",
							Properties: map[string]ToolPropertySchema{
								"tool": {Type: "string", Description: "Tool name"},
								"args": {Type: "object", Description: "Tool arguments"},
							},
							Required: []string{"tool"},
						},
						MaxItems: 10,
					},
				},
				Required: []string{"calls"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "start_session",
			Description: "Start a Claude Code session for a project. Returns session ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "Project ID (number)"},
					"task_id":    {Type: "string", Description: "Optional task ID to link"},
					"name":       {Type: "string", Description: "Optional session name"},
				},
				Required: []string{"project_id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "stop_session",
			Description: "Stop a running session.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Session ID to stop"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "list_sessions",
			Description: "List all sessions with status.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"status":     {Type: "string", Description: "Filter: running, stopped, completed"},
					"project_id": {Type: "string", Description: "Filter by project"},
				},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "send_to_session",
			Description: "Send text input to a running Claude Code session terminal.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Session ID"},
					"text":       {Type: "string", Description: "Text to send (Enter appended automatically)"},
				},
				Required: []string{"session_id", "text"},
			},
			Context: ToolContextSession,
		},
	}
}

// ──── Derived view functions ────

// ChatTools returns tool definitions for the AI chat assistant (no prefix).
// Filters to tools with Context == ToolContextChat or ToolContextBoth.
func ChatTools() []ToolDefinition {
	var result []ToolDefinition
	for _, td := range AllToolDefs() {
		if td.Context != ToolContextChat && td.Context != ToolContextBoth {
			continue
		}
		result = append(result, ToolDefinition{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: td.InputSchema,
		})
	}
	return result
}

// MCPTools returns tool definitions for the MCP protocol (with devmanager_ prefix).
// The context parameter controls ChatOnly filtering:
//   - "chat": includes all tools (even ChatOnly ones)
//   - anything else: excludes ChatOnly tools (used by terminal sessions)
func MCPTools(context string) []MCPToolDef {
	var result []MCPToolDef
	for _, td := range AllToolDefs() {
		if td.Context != ToolContextSession && td.Context != ToolContextBoth {
			continue
		}
		if td.ChatOnly && context != "chat" {
			continue
		}
		mcpName := td.MCPName
		if mcpName == "" {
			mcpName = "devmanager_" + td.Name
		}
		desc := td.Description
		if td.MCPDescription != "" {
			desc = td.MCPDescription
		}
		result = append(result, MCPToolDef{
			Name:        mcpName,
			Description: desc,
			InputSchema: ConvertInputSchemaToJSON(td.InputSchema),
		})
	}
	return result
}

// ──── Schema conversion helpers ────

// ConvertInputSchema converts a ToolDefinitionInput to a generic map[string]any,
// suitable for use with the Go Agent SDK's MCP tool registration.
func ConvertInputSchema(schema ToolDefinitionInput) map[string]any {
	return convertPropertySchemaToMap(ToolPropertySchema{
		Type:       schema.Type,
		Properties: schema.Properties,
		Required:   schema.Required,
	})
}

// convertPropertySchemaToMap recursively converts a ToolPropertySchema to a map.
func convertPropertySchemaToMap(prop ToolPropertySchema) map[string]any {
	result := map[string]any{"type": prop.Type}
	if prop.Description != "" {
		result["description"] = prop.Description
	}
	if len(prop.Enum) > 0 {
		result["enum"] = prop.Enum
	}
	if len(prop.Properties) > 0 {
		props := make(map[string]any)
		for name, p := range prop.Properties {
			props[name] = convertPropertySchemaToMap(p)
		}
		result["properties"] = props
	}
	if prop.Items != nil {
		result["items"] = convertPropertySchemaToMap(*prop.Items)
	}
	if len(prop.Required) > 0 {
		result["required"] = prop.Required
	}
	if prop.MaxItems > 0 {
		result["maxItems"] = prop.MaxItems
	}
	return result
}

// ConvertInputSchemaToJSON converts a ToolDefinitionInput to json.RawMessage for MCP protocol.
func ConvertInputSchemaToJSON(schema ToolDefinitionInput) json.RawMessage {
	m := ConvertInputSchema(schema)
	b, _ := json.Marshal(m)
	return b
}
