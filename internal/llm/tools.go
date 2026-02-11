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
	}
}
