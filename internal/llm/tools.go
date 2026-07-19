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
// This is the SINGLE SOURCE OF TRUTH for all OpenPoet tools.
type ToolDef struct {
	Name           string              // Canonical name without prefix (e.g. "create_skill")
	MCPName        string              // Explicit MCP name (if empty, auto = "openpoet_" + Name)
	Description    string              // Default description
	MCPDescription string              // Override description for MCP context (if empty, uses Description)
	InputSchema    ToolDefinitionInput // Typed Go schema (single source of truth)
	Context        ToolContext         // Where this tool is available
	ChatOnly       bool                // If true, filtered out from MCP sessions (only available in MCP context "chat")
}

// MCPToolDef is the MCP protocol format with JSON raw schema.
type MCPToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// AllToolDefs returns the complete unified tool registry.
// All 37 tools are defined here — chat, session, and shared.
func AllToolDefs() []ToolDef {
	return []ToolDef{
		// ──── Shared tools (both chat + session) ────

		{
			Name:        "create_skill",
			Description: "Create a new skill in OpenPoet. Skills are markdown instructions synced to projects as .claude/skills/<name>/SKILL.md for Claude Code to follow.",
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
			Name:           "get_skill",
			Description:    "Get the full content of a skill by ID. Returns name, content, category, and enabled status.",
			MCPDescription: "Get a skill's full content by ID.",
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
			Description:    "List all skills in OpenPoet.",
			MCPDescription: "List all skills in OpenPoet. Skills are markdown instruction templates stored in the database.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextBoth,
		},

		// ──── Project-scoped skill tools ────

		{
			Name:        "create_project_skill",
			Description: "Create a project-specific skill. Unlike global skills, project skills only apply to the specified project.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"name":       {Type: "string", Description: "Unique name within the project. MUST be lowercase with hyphens, no spaces (e.g. 'python-best-practices'). Max 64 chars."},
					"content":    {Type: "string", Description: "Markdown content with instructions. Do NOT include YAML frontmatter (---) — it is auto-generated."},
					"category":   {Type: "string", Description: "Category label (e.g. coding, testing, deployment, documentation, workflow)"},
				},
				Required: []string{"project_id", "name", "content"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "update_project_skill",
			Description: "Update an existing project-specific skill by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The project skill ID (number)"},
					"name":       {Type: "string", Description: "New name for the skill"},
					"content":    {Type: "string", Description: "New markdown content"},
					"category":   {Type: "string", Description: "New category label"},
					"enabled":    {Type: "boolean", Description: "Whether the skill is enabled"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "delete_project_skill",
			Description: "Delete a project-specific skill by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The project skill ID (number)"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "get_project_skill",
			Description:    "Get the full content of a project-specific skill by ID.",
			MCPDescription: "Get a project skill's full content by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The project skill ID (number)"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "list_project_skills",
			Description:    "List all skills for a project, including both global skills (with project-specific enabled state) and project-specific skills.",
			MCPDescription: "List all skills for a project. Returns global skills with project-level overrides and project-specific skills.",
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
			Name:           "list_projects",
			Description:    "List all projects in OpenPoet.",
			MCPDescription: "List all projects managed by OpenPoet.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "get_mcp_server",
			Description:    "Get the full details of a global MCP server by ID. Returns name, command, args, env, and enabled status.",
			MCPDescription: "Get a global MCP server's full details by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"id": {Type: "string", Description: "The MCP server ID (number)"},
				},
				Required: []string{"id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "list_mcp_servers",
			Description:    "List all MCP servers in OpenPoet.",
			MCPDescription: "List all MCP server configurations in OpenPoet.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextBoth,
		},

		// ──── Project-scoped MCP server tools ────

		{
			Name:        "create_project_mcp_server",
			Description: "Create a project-specific MCP server configuration. Unlike global MCP servers, project MCP servers only apply to sessions of the specified project. If a project server has the same name as a global one, it overrides the global for that project.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"name":       {Type: "string", Description: "Name of the MCP server"},
					"command":    {Type: "string", Description: "Command to run the server"},
					"args":       {Type: "string", Description: "JSON array of arguments (e.g. '[\"--port\", \"3000\"]'). Can also be passed as a native array."},
					"env":        {Type: "string", Description: "JSON object of environment variables (e.g. '{\"KEY\": \"value\"}'). Can also be passed as a native object."},
				},
				Required: []string{"project_id", "name", "command"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "update_project_mcp_server",
			Description: "Update an existing project-specific MCP server configuration by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The project MCP server ID (number)"},
					"name":       {Type: "string", Description: "New name"},
					"command":    {Type: "string", Description: "New command"},
					"args":       {Type: "string", Description: "JSON array of arguments. Can also be passed as a native array."},
					"env":        {Type: "string", Description: "JSON object of environment variables. Can also be passed as a native object."},
					"enabled":    {Type: "boolean", Description: "Whether the MCP server is enabled"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "delete_project_mcp_server",
			Description: "Delete a project-specific MCP server configuration by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The project MCP server ID (number)"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "get_project_mcp_server",
			Description:    "Get the full details of a project-specific MCP server by ID.",
			MCPDescription: "Get a project MCP server's full details by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The project MCP server ID (number)"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "list_project_mcp_servers",
			Description:    "List all MCP servers for a project, including both global MCP servers and project-specific ones.",
			MCPDescription: "List all MCP server configurations for a project. Returns global servers and project-specific servers.",
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
			Description: "Update a OpenPoet setting.",
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
			MCPDescription: "Propose changes to a project's memory doc. Changes are NOT applied immediately — they create a proposal that the user must approve via the OpenPoet UI.",
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
			Context: ToolContextBoth,
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
			Context: ToolContextBoth,
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
			Context: ToolContextBoth,
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
					"mission_id":      {Type: "string", Description: "Optional mission ID to link this document to an orchestration mission (Maestro)"},
				},
				Required: []string{"content"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:           "read_document",
			Description:    "Read the full content of a document by ID.",
			MCPDescription: "Read the full content of a document by ID. Use after openpoet_get_task to read specific documents.",
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
			MCPName:        "openpoet_list_project_files",
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
			Context: ToolContextBoth,
		},
		{
			Name:           "read_file",
			MCPName:        "openpoet_read_project_file",
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
			Context: ToolContextBoth,
		},

		// ──── Shared project file access (session-only) ────

		{
			Name:           "list_shared_projects",
			MCPName:        "openpoet_list_shared_projects",
			Description:    "List projects that this session has read access to. Returns project IDs, names, paths, and types.",
			MCPDescription: "List projects that this session's project has been granted read access to.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:           "list_shared_files",
			MCPName:        "openpoet_list_shared_files",
			Description:    "List files and directories in a shared project. Requires the target project to be in this project's shared access list.",
			MCPDescription: "List files in a shared project directory. Requires share access. Use path parameter to navigate subdirectories.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The shared project's ID (number)"},
					"path":       {Type: "string", Description: "Relative path within the shared project (empty for root)"},
				},
				Required: []string{"project_id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:           "read_shared_file",
			MCPName:        "openpoet_read_shared_file",
			Description:    "Read a file from a shared project. Requires the target project to be in this project's shared access list. Max 2MB, text files only.",
			MCPDescription: "Read a text file from a shared project. Requires share access. Max 2MB.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The shared project's ID (number)"},
					"path":       {Type: "string", Description: "Relative path to the file within the shared project"},
				},
				Required: []string{"project_id", "path"},
			},
			Context: ToolContextSession,
		},
		{
			Name:           "copy_shared_file",
			MCPName:        "openpoet_copy_shared_file",
			Description:    "Copy a single file from a shared project into the current project. Reads from the source and writes to the destination path.",
			MCPDescription: "Copy a file from a shared project to the current project. Requires share access.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The shared project's ID (number)"},
					"src_path":   {Type: "string", Description: "Relative path to the file in the shared project"},
					"dest_path":  {Type: "string", Description: "Relative path where the file should be written in the current project"},
				},
				Required: []string{"project_id", "src_path", "dest_path"},
			},
			Context: ToolContextSession,
		},
		{
			Name:           "copy_shared_folder",
			MCPName:        "openpoet_copy_shared_folder",
			Description:    "Recursively copy an entire folder from a shared project into the current project. Copies all files preserving directory structure.",
			MCPDescription: "Copy a folder from a shared project to the current project. Requires share access. Copies all files recursively.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The shared project's ID (number)"},
					"src_path":   {Type: "string", Description: "Relative path to the folder in the shared project (empty for root)"},
					"dest_path":  {Type: "string", Description: "Relative path where the folder should be written in the current project"},
				},
				Required: []string{"project_id", "src_path", "dest_path"},
			},
			Context: ToolContextSession,
		},

		// ──── Project custom tools management ────

		{
			Name:           "list_project_custom_tools",
			Description:    "List all custom tools defined for a project. Custom tools are shell commands that the AI assistant can execute in the project context.",
			MCPDescription: "List all custom tools for a project. Returns tool names, descriptions, commands, and enabled status.",
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
			Name:        "create_project_custom_tool",
			Description: "Create a custom tool for a project. Custom tools are shell commands executed in the project directory. Parameters are passed as TOOL_<NAME> environment variables.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":  {Type: "string", Description: "The project ID (number)"},
					"name":        {Type: "string", Description: "Tool name (lowercase, underscores). Example: search_code"},
					"description": {Type: "string", Description: "What the tool does — the AI reads this to decide when to use it"},
					"command":     {Type: "string", Description: "Shell command to execute. Use $TOOL_<PARAM> for parameters (e.g. $TOOL_PATTERN)"},
					"parameters":  {Type: "string", Description: "JSON array of parameters: [{\"name\": \"pattern\", \"description\": \"Search pattern\", \"required\": true}]"},
					"working_dir": {Type: "string", Description: "Override working directory (optional, defaults to project path)"},
					"confirm":     {Type: "boolean", Description: "Require user confirmation before execution (default: false)"},
				},
				Required: []string{"project_id", "name", "description", "command"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "update_project_custom_tool",
			Description: "Update an existing custom tool by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":  {Type: "string", Description: "The project ID (number)"},
					"id":          {Type: "string", Description: "The custom tool ID (number)"},
					"name":        {Type: "string", Description: "New tool name"},
					"description": {Type: "string", Description: "New description"},
					"command":     {Type: "string", Description: "New shell command"},
					"parameters":  {Type: "string", Description: "New JSON array of parameters"},
					"working_dir": {Type: "string", Description: "New working directory"},
					"confirm":     {Type: "boolean", Description: "Require user confirmation"},
					"enabled":     {Type: "boolean", Description: "Whether the tool is enabled"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "delete_project_custom_tool",
			Description: "Delete a custom tool by ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"id":         {Type: "string", Description: "The custom tool ID (number)"},
				},
				Required: []string{"project_id", "id"},
			},
			Context: ToolContextBoth,
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
			Context: ToolContextChat,
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
			Context: ToolContextChat,
		},
		{
			Name:        "open_file",
			Description: "Open one or more project files for the user to view. Creates clickable file cards in chat. Does NOT return file content (saves context). Use read_file if you need to analyze content yourself.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"paths": {
						Type:        "array",
						Description: "File paths relative to the project root",
						Items:       &ToolPropertySchema{Type: "string"},
					},
				},
				Required: []string{"project_id", "paths"},
			},
			Context: ToolContextChat,
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
			Context: ToolContextChat,
		},
		// ──── Session-only tools ────

		{
			Name:        "update_mcp_server",
			Description: "Update a OpenPoet MCP server configuration by ID.",
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
			Description: "Delete a OpenPoet MCP server configuration by ID.",
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
			Description: "Get the task linked to the current session (if any). Returns the task details including status, title, description. Only works within a OpenPoet session.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "get_session_info",
			Description: "Get information about the current OpenPoet session, including session ID, project, status, and linked task. Only works within a OpenPoet session.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "request_task_evaluation",
			Description: "Request the OpenPoet AI Assistant to evaluate the current session and proactively suggest task actions (create, update, link, or complete tasks). The AI Assistant will analyze the session output and suggest actions to the user via floating notification cards. Use this when you believe the session's work is relevant to task management.",
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
			Context: ToolContextBoth,
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
			Context: ToolContextBoth,
		},
		{
			Name:        "start_session",
			Description: "Start an OpenPoet coding session for a project. Optionally link it to an existing task. Returns the session ID.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":                   {Type: "string", Description: "Project ID (number)"},
					"task_id":                      {Type: "string", Description: "Optional existing task ID to link to the new session"},
					"dangerously_skip_permissions": {Type: "boolean", Description: "Request skip-permissions mode. Only takes effect if the project allows it."},
					"auto_start_task_prompt":       {Type: "boolean", Description: "Legacy compatibility flag. MCP task-linked starts always begin immediately without UI confirmation."},
					"planning_mode":                {Type: "boolean", Description: "Start immediately with a planning prompt instead of the default task prompt."},
					"custom_prompt":                {Type: "string", Description: "Start immediately with this prompt instead of the default task prompt. Cannot be combined with planning_mode."},
					"workspace_id":                 {Type: "string", Description: "Run the session inside an existing ready workspace (isolated git-worktree lane) instead of the project's main checkout."},
				},
				Required: []string{"project_id"},
			},
			Context: ToolContextBoth,
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
			Context: ToolContextBoth,
		},
		{
			Name:        "list_sessions",
			Description: "List all sessions with status, current model, reasoning effort, and harness/backend runtime.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"status":     {Type: "string", Description: "Filter: running, stopped, completed"},
					"project_id": {Type: "string", Description: "Filter by project"},
				},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "get_session",
			Description: "Get details for a session, including project, status, name, current model, reasoning effort, harness/backend runtime, and linked task if any.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Session ID"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "set_session_model",
			Description: "Request a model for future turns of an active OpenPoet session. Codex app-server IDs are validated against its live catalog. Claude Code accepts only Anthropic aliases/IDs; the effective model is reconciled from the runtime hook/transcript.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Active OpenPoet session ID"},
					"model":      {Type: "string", Description: "Harness-compatible model ID or alias; Claude Code examples: fable, claude-opus-4-5, or default"},
				},
				Required: []string{"session_id", "model"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "set_session_effort",
			Description: "Change the reasoning/thinking effort used by future turns of an active OpenPoet session. Accepted levels are default, minimal, low, medium, high, xhigh, and max; backend/model support is validated before applying when the live catalog is available.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Active OpenPoet session ID"},
					"effort": {
						Type:        "string",
						Description: "Reasoning/thinking level for future turns",
						Enum:        []string{"default", "minimal", "low", "medium", "high", "xhigh", "max"},
					},
				},
				Required: []string{"session_id", "effort"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "read_session_history",
			Description: "Read a compact slice of a session's terminal/message history without loading the full transcript. Supports tail, head, window, and search modes.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id":     {Type: "string", Description: "Session ID"},
					"mode":           {Type: "string", Description: "Read mode: tail, head, window, or search. Defaults to tail. Providing query automatically uses search."},
					"lines":          {Type: "string", Description: "Number of lines for head/tail, or max matches for search. Default: 80."},
					"offset":         {Type: "string", Description: "1-based starting line for window mode."},
					"limit":          {Type: "string", Description: "Number of lines for window mode, or max matches for search."},
					"query":          {Type: "string", Description: "Plain text or regex query for search mode."},
					"regex":          {Type: "boolean", Description: "Treat query as a regular expression."},
					"case_sensitive": {Type: "boolean", Description: "Use case-sensitive search. Default false."},
					"context":        {Type: "string", Description: "Context lines before/after each search match. Default: 2."},
					"max_chars":      {Type: "string", Description: "Maximum returned characters. Default: 12000, hard cap: 50000."},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "send_to_session",
			Description: "Send a prompt or text input to a running OpenPoet session terminal. Enter is appended automatically. Provide either text or prompt.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Session ID"},
					"text":       {Type: "string", Description: "Text or prompt to send (Enter appended automatically)"},
					"prompt":     {Type: "string", Description: "Prompt to send. Alias for text."},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "link_session_task",
			Description: "Link a session to an existing task, or create a new task and link it to the session.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id":  {Type: "string", Description: "Session ID"},
					"task_id":     {Type: "string", Description: "Existing task ID to link"},
					"title":       {Type: "string", Description: "Title for a new task when task_id is omitted"},
					"description": {Type: "string", Description: "Description for a new task"},
					"priority":    {Type: "string", Description: "Priority for a new task: low, medium, high, urgent"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextBoth,
		},
		// ──── Coordinator tier (Phase 7.1 — Maestro) ────
		// The session that starts a mission elects itself coordinator of a
		// coordination group and gains scoped cross-project reach. Local and
		// remote (SSH) projects participate identically.
		{
			Name:        "coordinator_elect",
			Description: "Elect this session as the coordinator of a coordination group (or renew the lease it already holds). Returns the fence_version that MUST accompany every coordinator mutation.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"group":       {Type: "number", Description: "Coordination group (tag id with coordination=1). The session's project must be a member."},
					"ttl_seconds": {Type: "number", Description: "Lease TTL in seconds (default 300). Renew before it lapses or another session can take over."},
				},
				Required: []string{"group"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "coordinator_status",
			Description: "Report whether this session holds a coordinator lease, for which group, the current fence_version, and the member project ids.",
			InputSchema: ToolDefinitionInput{Type: "object", Properties: map[string]ToolPropertySchema{}},
			Context:     ToolContextSession,
		},
		{
			Name:        "list_group_sessions",
			Description: "List every session across ALL projects of the coordinated group (local and remote). Requires holding the coordinator lease.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"status": {Type: "string", Description: "Optional status filter (running, stopped, ...)"},
				},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "await_events",
			Description: "Long-poll the durable event outbox for the coordinated group (session.turn_completed, session.awaiting_input, workspace.*, ...). Returns matching events past the cursor or an empty page on timeout. Events from projects outside the group never appear.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"after":    {Type: "number", Description: "Cursor: only events with sequence > after. Use next_cursor from the previous call."},
					"filter":   {Type: "string", Description: "Event type prefix filter, e.g. 'session.'"},
					"timeout":  {Type: "number", Description: "Long-poll timeout in seconds (default 20, max 60)"},
					"consumer": {Type: "string", Description: "Reserved for automation clients; coordinator sessions page with the explicit after cursor"},
				},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "wait_for_session",
			Description: "Block until a group session settles (turn_complete / awaiting_input / stopped) or the timeout. Reports signal_quality: exact (event-derived) vs heuristic (timeout fallback).",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Target session id (must belong to the coordinated group)"},
					"timeout":    {Type: "number", Description: "Timeout in seconds (default 20, max 60)"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "start_worker",
			Description: "Start a worker session in any member project of the coordinated group — local or remote (SSH), any backend. Lineage records this session as parent. Requires the current fence_version.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":      {Type: "number", Description: "Target project id (must be a group member)"},
					"fence_version":   {Type: "number", Description: "Current fence token from coordinator_elect/status"},
					"task_id":         {Type: "number", Description: "Optional task to bind the worker to"},
					"backend":         {Type: "string", Description: "Optional backend override (claude_code, codex, copilot, acp, opencode); defaults to the project's backend"},
					"workspace_id":    {Type: "string", Description: "Optional workspace (worktree lane) to run in"},
					"isolation":       {Type: "string", Description: "Optional: 'auto' leases a pooled workspace"},
					"custom_prompt":   {Type: "string", Description: "Optional initial brief for the worker"},
					"mission_id":      {Type: "number", Description: "Enroll the worker in this mission (briefing injected server-side)"},
					"role":            {Type: "string", Description: "Worker role label within the mission"},
					"idempotency_key": {Type: "string", Description: "Optional spawn fence: retrying with the same key returns the SAME session instead of double-spawning"},
					"dry_run":         {Type: "boolean", Description: "Validate without creating the session"},
				},
				Required: []string{"project_id", "fence_version"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "start_mission",
			Description: "Start a goal-driven mission for the coordinated group (this session becomes the mission's coordinator). One active mission per group. Requires the current fence_version.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"goal":          {Type: "string", Description: "The mission goal (what done looks like)"},
					"fence_version": {Type: "number", Description: "Current fence token from coordinator_elect/status"},
				},
				Required: []string{"goal", "fence_version"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "get_mission",
			Description: "Read a mission of the coordinated group: goal, status, and the worker roster (session, project, backend, role, status, last_report_ref).",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"mission_id": {Type: "number", Description: "Mission id"},
				},
				Required: []string{"mission_id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "update_mission_status",
			Description: "Transition a mission of the coordinated group (active|paused|completed|failed|archived). Requires the current fence_version.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"mission_id":    {Type: "number", Description: "Mission id"},
					"status":        {Type: "string", Description: "active, paused, completed, failed or archived"},
					"fence_version": {Type: "number", Description: "Current fence token"},
				},
				Required: []string{"mission_id", "status", "fence_version"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "attach_worker",
			Description: "Adopt an EXISTING group session into a mission's worker roster (workers spawned via start_worker enroll automatically). Backfills its last dense report.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"mission_id":    {Type: "number", Description: "Mission id"},
					"session_id":    {Type: "string", Description: "Existing session to adopt (must belong to the group)"},
					"role":          {Type: "string", Description: "Worker role label (e.g. impl, qa)"},
					"fence_version": {Type: "number", Description: "Current fence token"},
				},
				Required: []string{"mission_id", "session_id", "fence_version"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "emit_session_report",
			Description: "Emit THIS session's dense structured milestone report (condensed communication — the coordinator reads reports, not transcripts). Re-emitting the same turn_id updates the milestone; finalize:true closes it.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"turn_id":                {Type: "string", Description: "Stable milestone id (m1, m2, ...). Default: 'milestone'"},
					"objective":              {Type: "string", Description: "What this milestone set out to do"},
					"summary":                {Type: "string", Description: "Tight summary of what actually happened"},
					"outcome":                {Type: "string", Description: "Optional outcome statement"},
					"decisions":              {Type: "array", Description: "Key decisions made (strings)"},
					"files":                  {Type: "array", Description: "Files changed (strings)"},
					"verification":           {Type: "object", Description: "{status: passed|failed|partial|not_run, summary}"},
					"blockers":               {Type: "array", Description: "Current blockers (marks the report incomplete)"},
					"needs_from_coordinator": {Type: "array", Description: "What you need decided/unblocked by the coordinator"},
					"next":                   {Type: "string", Description: "Next step you will take"},
					"finalize":               {Type: "boolean", Description: "true on the last report of this milestone"},
				},
				Required: []string{"objective", "summary"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "get_session_report",
			Description: "Read the latest dense structured report of a coordinated-group session (progressive disclosure — use read_session_history only to debug). Requires holding the coordinator lease.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Target session id (must belong to the coordinated group)"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "send_to_worker",
			Description: "Send steering input to a worker session of the coordinated group, awaiting the prompt ack. Requires the current fence_version.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id":    {Type: "string", Description: "Target worker session id (must belong to the group)"},
					"text":          {Type: "string", Description: "Text to send (Enter appended automatically)"},
					"fence_version": {Type: "number", Description: "Current fence token from coordinator_elect/status"},
					"force":         {Type: "boolean", Description: "Send even if the worker is mid-turn"},
				},
				Required: []string{"session_id", "text", "fence_version"},
			},
			Context: ToolContextSession,
		},
		{
			Name:        "unlink_session_task",
			Description: "Remove the task link from a session without deleting the task.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id": {Type: "string", Description: "Session ID"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextBoth,
		},
		{
			Name:        "stop_session_and_update_task",
			Description: "Stop a session and update its linked task in one operation. Use this when finishing or pausing work from an active session.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"session_id":  {Type: "string", Description: "Session ID to stop"},
					"status":      {Type: "string", Description: "Optional new task status: todo, in_progress, awaiting_approval, done"},
					"title":       {Type: "string", Description: "Optional new task title"},
					"description": {Type: "string", Description: "Optional new task description"},
					"priority":    {Type: "string", Description: "Optional new task priority: low, medium, high, urgent"},
					"due_date":    {Type: "string", Description: "Optional new due date, or empty string to clear"},
				},
				Required: []string{"session_id"},
			},
			Context: ToolContextBoth,
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

// MCPTools returns tool definitions for the MCP protocol (with openpoet_ prefix).
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
			mcpName = "openpoet_" + td.Name
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
