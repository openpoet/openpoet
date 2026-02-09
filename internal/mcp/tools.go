package mcp

import (
	"encoding/json"
	"fmt"
)

// MCPTool represents a tool definition in MCP protocol format.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolsDef returns the list of MCP tools exposed by DevManager.
func toolsDef() []MCPTool {
	return []MCPTool{
		{
			Name:        "devmanager_list_skills",
			Description: "List all skills in DevManager. Skills are markdown instruction templates stored in the database.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "devmanager_create_skill",
			Description: "Create a new skill in DevManager. Skills are markdown instructions synced to projects as .claude/skills/<name>/SKILL.md for Claude Code to follow.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Unique name for the skill. MUST be lowercase with hyphens, no spaces (e.g. 'python-best-practices', 'git-workflow'). Max 64 chars."},"content":{"type":"string","description":"Markdown content with instructions for Claude Code. Do NOT include YAML frontmatter (---) — it is auto-generated during sync."},"category":{"type":"string","description":"Category label (e.g. coding, testing, deployment, documentation, workflow)"}},"required":["name","content"]}`),
		},
		{
			Name:        "devmanager_update_skill",
			Description: "Update an existing skill by ID.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"number","description":"The skill ID"},"name":{"type":"string","description":"New name for the skill"},"content":{"type":"string","description":"New markdown content"},"category":{"type":"string","description":"New category label"}},"required":["id"]}`),
		},
		{
			Name:        "devmanager_delete_skill",
			Description: "Delete a skill by ID.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"number","description":"The skill ID"}},"required":["id"]}`),
		},
		{
			Name:        "devmanager_list_projects",
			Description: "List all projects managed by DevManager.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "devmanager_list_mcp_servers",
			Description: "List all MCP server configurations in DevManager.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "devmanager_create_mcp_server",
			Description: "Create a new MCP server configuration.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Name of the MCP server"},"command":{"type":"string","description":"Command to run the server"},"args":{"type":"string","description":"JSON array of arguments"},"env":{"type":"string","description":"JSON object of environment variables"}},"required":["name","command"]}`),
		},
		{
			Name:        "devmanager_update_setting",
			Description: "Update a DevManager setting.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"Setting key"},"value":{"type":"string","description":"Setting value"}},"required":["key","value"]}`),
		},
		{
			Name:        "devmanager_sync_config",
			Description: "Sync configuration (skills, MCP servers, hooks) to all projects.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "devmanager_list_project_files",
			Description: "List files and directories in a project. Returns names, sizes, and types. Use path parameter to navigate subdirectories.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"},"path":{"type":"string","description":"Relative path within the project (empty or omit for root)"}},"required":["project_id"]}`),
		},
		{
			Name:        "devmanager_read_project_file",
			Description: "Read the content of a text file from a project. Max 2MB, text files only. Returns the file content as text.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"},"path":{"type":"string","description":"Relative path to the file within the project"}},"required":["project_id","path"]}`),
		},
	}
}

// executeTool runs a tool by name with the given arguments, calling the DevManager API.
func executeTool(client *APIClient, name string, args json.RawMessage) (string, error) {
	var params map[string]interface{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if params == nil {
		params = make(map[string]interface{})
	}

	switch name {
	case "devmanager_list_skills":
		body, err := client.Get("/api/config/skills")
		if err != nil {
			return "", err
		}
		return formatSkillsList(body)

	case "devmanager_create_skill":
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/config/skills", string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill created: %s", string(body)), nil

	case "devmanager_update_skill":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		delete(params, "id")
		payload, _ := json.Marshal(params)
		body, err := client.Put(fmt.Sprintf("/api/config/skills/%d", id), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill updated: %s", string(body)), nil

	case "devmanager_delete_skill":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/config/skills/%d", id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill %d deleted", id), nil

	case "devmanager_list_projects":
		body, err := client.Get("/api/projects")
		if err != nil {
			return "", err
		}
		return formatProjectsList(body)

	case "devmanager_list_mcp_servers":
		body, err := client.Get("/api/config/mcps")
		if err != nil {
			return "", err
		}
		return formatMCPsList(body)

	case "devmanager_create_mcp_server":
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/config/mcps", string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server created: %s", string(body)), nil

	case "devmanager_update_setting":
		payload, _ := json.Marshal(params)
		_, err := client.Put("/api/config/settings", string(payload))
		if err != nil {
			return "", err
		}
		key, _ := params["key"].(string)
		return fmt.Sprintf("Setting '%s' updated", key), nil

	case "devmanager_sync_config":
		body, err := client.Post("/api/config/sync-all", "{}")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Config synced: %s", string(body)), nil

	case "devmanager_list_project_files":
		projectID, ok := getID(params)
		if !ok {
			// Try project_id key
			if v, exists := params["project_id"]; exists {
				params["id"] = v
				projectID, ok = getID(params)
			}
			if !ok {
				return "", fmt.Errorf("project_id is required")
			}
		}
		path, _ := params["path"].(string)
		endpoint := fmt.Sprintf("/api/projects/%d/files?path=%s", projectID, path)
		body, err := client.Get(endpoint)
		if err != nil {
			return "", err
		}
		return formatFileList(body)

	case "devmanager_read_project_file":
		projectID, ok := getID(params)
		if !ok {
			if v, exists := params["project_id"]; exists {
				params["id"] = v
				projectID, ok = getID(params)
			}
			if !ok {
				return "", fmt.Errorf("project_id is required")
			}
		}
		path, _ := params["path"].(string)
		if path == "" {
			return "", fmt.Errorf("path is required")
		}
		endpoint := fmt.Sprintf("/api/projects/%d/files/view/%s", projectID, path)
		body, err := client.Get(endpoint)
		if err != nil {
			return "", err
		}
		return formatFileContent(body)

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// getID extracts an integer ID from the params map.
func getID(params map[string]interface{}) (int64, bool) {
	v, ok := params["id"]
	if !ok {
		return 0, false
	}
	switch id := v.(type) {
	case float64:
		return int64(id), true
	case string:
		var n int64
		if _, err := fmt.Sscanf(id, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// formatSkillsList formats the skills JSON response into readable text.
func formatSkillsList(body []byte) (string, error) {
	var skills []struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Category string `json:"category"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.Unmarshal(body, &skills); err != nil {
		return string(body), nil
	}
	if len(skills) == 0 {
		return "No skills found.", nil
	}
	result := ""
	for _, s := range skills {
		status := "enabled"
		if !s.Enabled {
			status = "disabled"
		}
		result += fmt.Sprintf("- [%d] %s (%s, %s)\n", s.ID, s.Name, s.Category, status)
	}
	return result, nil
}

// formatProjectsList formats the projects JSON response into readable text.
func formatProjectsList(body []byte) (string, error) {
	var projects []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &projects); err != nil {
		return string(body), nil
	}
	if len(projects) == 0 {
		return "No projects found.", nil
	}
	result := ""
	for _, p := range projects {
		result += fmt.Sprintf("- [%d] %s (%s: %s)\n", p.ID, p.Name, p.Type, p.Path)
	}
	return result, nil
}

// formatFileList formats the file list JSON response into readable text.
func formatFileList(body []byte) (string, error) {
	var fileList []struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		IsDir bool   `json:"is_dir"`
		Mode  string `json:"mode"`
	}
	if err := json.Unmarshal(body, &fileList); err != nil {
		return string(body), nil
	}
	if len(fileList) == 0 {
		return "Directory is empty.", nil
	}
	result := ""
	for _, f := range fileList {
		if f.IsDir {
			result += fmt.Sprintf("  [DIR]  %s/\n", f.Name)
		} else {
			result += fmt.Sprintf("  %6s  %s\n", formatSize(f.Size), f.Name)
		}
	}
	return result, nil
}

// formatFileContent extracts text content from the view file JSON response.
func formatFileContent(body []byte) (string, error) {
	var file struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return string(body), nil
	}
	return fmt.Sprintf("--- %s (%s) ---\n%s", file.Path, formatSize(file.Size), file.Content), nil
}

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}

// formatMCPsList formats the MCP servers JSON response into readable text.
func formatMCPsList(body []byte) (string, error) {
	var servers []struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Command string `json:"command"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(body, &servers); err != nil {
		return string(body), nil
	}
	if len(servers) == 0 {
		return "No MCP servers found.", nil
	}
	result := ""
	for _, m := range servers {
		status := "enabled"
		if !m.Enabled {
			status = "disabled"
		}
		result += fmt.Sprintf("- [%d] %s: %s (%s)\n", m.ID, m.Name, m.Command, status)
	}
	return result, nil
}
