package llm

// ChatTools returns the tool definitions for the AI chat assistant.
func ChatTools() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "create_skill",
			Description: "Create a new skill in DevManager. Skills are markdown instructions synced to projects for Claude Code to follow.",
			InputSchema: ToolDefinitionInput{
				Type: "object",
				Properties: map[string]ToolPropertySchema{
					"name":     {Type: "string", Description: "Unique name for the skill"},
					"content":  {Type: "string", Description: "Markdown content of the skill"},
					"category": {Type: "string", Description: "Category label (e.g. coding, testing, deployment)"},
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
	}
}
