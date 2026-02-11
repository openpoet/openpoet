package llm

// ChatTools returns the tool definitions for the AI chat assistant.
func ChatTools() []ToolDefinition {
	return []ToolDefinition{
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
		},
		{
			Name:        "list_skills",
			Description: "List all skills in DevManager.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
		},
		{
			Name:        "list_projects",
			Description: "List all projects in DevManager.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
		},
		{
			Name:        "list_mcp_servers",
			Description: "List all MCP servers in DevManager.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
		},
		{
			Name:        "create_mcp_server",
			Description: "Create a new MCP server configuration.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"name":    {Type: "string", Description: "Name of the MCP server"},
					"command": {Type: "string", Description: "Command to run the server"},
					"args":    {Type: "string", Description: "JSON array of arguments (e.g. '[\"--port\", \"3000\"]')"},
					"env":     {Type: "string", Description: "JSON object of environment variables (e.g. '{\"KEY\": \"value\"}')"},
				},
				Required: []string{"name", "command"},
			},
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
		},
		{
			Name:        "sync_config",
			Description: "Sync configuration (skills, MCP servers, hooks) to all projects.",
			InputSchema: ToolDefinitionInput{
				Type:       "object",
				Properties: map[string]ToolPropertySchema{},
			},
		},
		{
			Name:        "get_memory_doc",
			Description: "Get the memory doc (CLAUDE.md) for a project. Returns a viewer link + internal reference. IMPORTANT: Never paste the content in chat — only share the viewer link with the user.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
				},
				Required: []string{"project_id"},
			},
		},
		{
			Name:        "update_memory_doc",
			Description: "Propose changes to the memory doc (CLAUDE.md) for a project. Creates a preview for user approval — changes are NOT applied immediately. Only use when the user explicitly asks.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"content":    {Type: "string", Description: "Full markdown content for the memory doc"},
					"summary":    {Type: "string", Description: "Brief summary of what changed in this update"},
				},
				Required: []string{"project_id", "content"},
			},
		},
		{
			Name:        "list_tasks",
			Description: "List all tasks for a project. Shows title, status, priority, due date.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
				},
				Required: []string{"project_id"},
			},
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
					"status":      {Type: "string", Description: "Status: todo, in_progress, done, blocked (default: todo)"},
					"priority":    {Type: "string", Description: "Priority: low, medium, high, urgent (default: medium)"},
					"due_date":    {Type: "string", Description: "Due date in ISO 8601 format (e.g. 2025-01-15T14:00)"},
					"parent_id":   {Type: "string", Description: "Parent task ID for subtasks"},
				},
				Required: []string{"project_id", "title"},
			},
		},
		{
			Name:        "update_task",
			Description: "Update an existing task.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id":  {Type: "string", Description: "The project ID (number)"},
					"task_id":     {Type: "string", Description: "The task ID (number)"},
					"title":       {Type: "string", Description: "New title"},
					"description": {Type: "string", Description: "New description"},
					"status":      {Type: "string", Description: "New status: todo, in_progress, done, blocked"},
					"priority":    {Type: "string", Description: "New priority: low, medium, high, urgent"},
					"due_date":    {Type: "string", Description: "New due date (empty string to clear)"},
				},
				Required: []string{"project_id", "task_id"},
			},
		},
		{
			Name:        "delete_task",
			Description: "Delete a task and its subtasks.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"task_id":    {Type: "string", Description: "The task ID (number)"},
				},
				Required: []string{"project_id", "task_id"},
			},
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
		},
		{
			Name:        "create_document",
			Description: "Create a temporary markdown document and return a clickable link. Use this for ANY response that would be longer than 5 lines — lists, explanations, code, reports, etc. This keeps the chat clean.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"title":   {Type: "string", Description: "Short document title"},
					"content": {Type: "string", Description: "Full markdown content of the document"},
				},
				Required: []string{"title", "content"},
			},
		},
		// File exploration tools (used by planning mode and general chat)
		{
			Name:        "list_directory",
			Description: "List files and directories in a project path. Returns names, sizes, and types. Use to browse the project structure.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number)"},
					"path":       {Type: "string", Description: "Relative path within the project (empty or omit for root directory)"},
				},
				Required: []string{"project_id"},
			},
		},
		{
			Name:        "read_file",
			Description: "Read the content of a text file from a project. Supports optional line offset and limit for reading specific sections of large files. Max 2MB file size.",
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
		},
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
		},
		// Planning mode activation tool
		{
			Name:        "activate_planning_mode",
			Description: "Switch this conversation to planning mode for a specific project. This enables the planning workflow with file exploration tools and task creation. Use when the user wants to plan features, refactoring, or any development work that should be broken into tasks.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"project_id": {Type: "string", Description: "The project ID (number) to plan for"},
				},
				Required: []string{"project_id"},
			},
		},
	}
}

// PlanningTools returns the subset of tools available in planning mode.
func PlanningTools() []ToolDefinition {
	all := ChatTools()
	allowed := map[string]bool{
		"list_projects":  true,
		"list_directory": true,
		"read_file":      true,
		"find_files":     true,
		"grep_content":   true,
		"list_tasks":     true,
		"create_task":    true,
		"update_task":    true,
		"get_task_report": true,
		"create_document": true,
	}
	var filtered []ToolDefinition
	for _, t := range all {
		if allowed[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
