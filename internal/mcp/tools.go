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

// chatOnlyTools lists tools that should only be available in chat context (no session ID).
var chatOnlyTools = map[string]bool{
	"devmanager_update_memory_doc": true,
}

// toolsDef returns the list of MCP tools exposed by DevManager.
// When context is "chat" (spawned by AI Assistant), all tools are included.
// Otherwise (terminal sessions), chat-only tools are excluded.
func toolsDef(context string) []MCPTool {
	all := allToolsDef()
	if context == "chat" {
		return all
	}
	// Filter out chat-only tools for terminal sessions
	var filtered []MCPTool
	for _, t := range all {
		if !chatOnlyTools[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func allToolsDef() []MCPTool {
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
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Name of the MCP server"},"command":{"type":"string","description":"Command to run the server"},"args":{"type":"string","description":"JSON array of arguments, e.g. '[\"--headless\"]'. Can also be passed as a native array."},"env":{"type":"string","description":"JSON object of environment variables, e.g. '{\"KEY\":\"val\"}'. Can also be passed as a native object."}},"required":["name","command"]}`),
		},
		{
			Name:        "devmanager_update_mcp_server",
			Description: "Update a DevManager MCP server configuration by ID.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"number","description":"The MCP server ID"},"name":{"type":"string","description":"New name"},"command":{"type":"string","description":"New command"},"args":{"type":"string","description":"JSON array of arguments, e.g. '[\"--headless\"]'. Can also be passed as a native array."},"env":{"type":"string","description":"JSON object of environment variables. Can also be passed as a native object."},"enabled":{"type":"boolean","description":"Whether the MCP server is enabled"}},"required":["id"]}`),
		},
		{
			Name:        "devmanager_delete_mcp_server",
			Description: "Delete a DevManager MCP server configuration by ID.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"number","description":"The MCP server ID"}},"required":["id"]}`),
		},
		{
			Name:        "devmanager_update_setting",
			Description: "Update a DevManager setting.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"Setting key"},"value":{"type":"string","description":"Setting value"}},"required":["key","value"]}`),
		},
		{
			Name:        "devmanager_sync_config",
			Description: "Sync configuration (skills and hooks) to all projects. MCP servers are injected via --mcp-config CLI flag at session start.",
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
		{
			Name:        "devmanager_get_memory_doc",
			Description: "Get the memory doc for a project. The memory doc tracks project goals, progress, and key decisions. Returns the markdown content or empty if none exists.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"}},"required":["project_id"]}`),
		},
		{
			Name:        "devmanager_update_memory_doc",
			Description: "Propose changes to a project's memory doc. Changes are NOT applied immediately — they create a proposal that the user must approve via the DevManager UI.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"},"content":{"type":"string","description":"Full markdown content for the memory doc"},"summary":{"type":"string","description":"Brief summary of what changed in this update"}},"required":["project_id","content"]}`),
		},
		{
			Name:        "devmanager_list_tasks",
			Description: "List all tasks for a project. Returns task title, status, priority, due date, and subtask relationships.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"}},"required":["project_id"]}`),
		},
		{
			Name:        "devmanager_create_task",
			Description: "Create a new task in a project.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"},"title":{"type":"string","description":"Task title"},"description":{"type":"string","description":"Task description"},"status":{"type":"string","description":"Status: todo, in_progress, done, blocked"},"priority":{"type":"string","description":"Priority: low, medium, high, urgent"},"due_date":{"type":"string","description":"Due date in ISO 8601 format (e.g. 2025-01-15T14:00)"},"parent_id":{"type":"number","description":"Parent task ID for subtasks"}},"required":["project_id","title"]}`),
		},
		{
			Name:        "devmanager_update_task",
			Description: "Update an existing task by project_id and task_id.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"},"task_id":{"type":"number","description":"The task ID"},"title":{"type":"string","description":"New title"},"description":{"type":"string","description":"New description"},"status":{"type":"string","description":"New status: todo, in_progress, done, blocked"},"priority":{"type":"string","description":"New priority: low, medium, high, urgent"},"due_date":{"type":"string","description":"New due date in ISO 8601 format (empty string to clear)"}},"required":["project_id","task_id"]}`),
		},
		{
			Name:        "devmanager_delete_task",
			Description: "Delete a task by project_id and task_id. Also deletes subtasks.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"number","description":"The project ID"},"task_id":{"type":"number","description":"The task ID"}},"required":["project_id","task_id"]}`),
		},
		// Session-aware tools (available when running inside a DevManager session)
		{
			Name:        "devmanager_get_my_task",
			Description: "Get the task linked to the current session (if any). Returns the task details including status, title, description. Only works within a DevManager session.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "devmanager_get_session_info",
			Description: "Get information about the current DevManager session, including session ID, project, status, and linked task. Only works within a DevManager session.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "devmanager_request_task_evaluation",
			Description: "Request the DevManager AI Assistant to evaluate the current session and proactively suggest task actions (create, update, link, or complete tasks). The AI Assistant will analyze the session output and suggest actions to the user via floating notification cards. Use this when you believe the session's work is relevant to task management.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

// executeTool runs a tool by name with the given arguments, calling the DevManager API.
func executeTool(client *APIClient, name string, args json.RawMessage, sessionID, conversationID string) (string, error) {
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
		normalizeMCPParams(params)
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/config/mcps", string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server created: %s", string(body)), nil

	case "devmanager_update_mcp_server":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		delete(params, "id")
		normalizeMCPParams(params)
		payload, _ := json.Marshal(params)
		body, err := client.Put(fmt.Sprintf("/api/config/mcps/%d", id), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server updated: %s", string(body)), nil

	case "devmanager_delete_mcp_server":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/config/mcps/%d", id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server %d deleted", id), nil

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

	case "devmanager_get_memory_doc":
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
		endpoint := fmt.Sprintf("/api/projects/%d/memory-doc", projectID)
		body, err := client.Get(endpoint)
		if err != nil {
			return "", err
		}
		return formatMemoryDoc(body)

	case "devmanager_list_tasks":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/tasks", projectID))
		if err != nil {
			return "", err
		}
		return formatTaskList(body)

	case "devmanager_create_task":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		delete(params, "project_id")
		payload, _ := json.Marshal(params)
		body, err := client.Post(fmt.Sprintf("/api/projects/%d/tasks", projectID), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Task created: %s", string(body)), nil

	case "devmanager_update_task":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		taskID, ok := getTaskID(params)
		if !ok {
			return "", fmt.Errorf("task_id is required")
		}
		delete(params, "project_id")
		delete(params, "task_id")
		payload, _ := json.Marshal(params)
		body, err := client.Put(fmt.Sprintf("/api/projects/%d/tasks/%d", projectID, taskID), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Task updated: %s", string(body)), nil

	case "devmanager_delete_task":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		taskID, ok := getTaskID(params)
		if !ok {
			return "", fmt.Errorf("task_id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/projects/%d/tasks/%d", projectID, taskID))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Task %d deleted", taskID), nil

	case "devmanager_update_memory_doc":
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
		content, _ := params["content"].(string)
		if content == "" {
			return "", fmt.Errorf("content is required")
		}
		summary, _ := params["summary"].(string)
		payload, _ := json.Marshal(map[string]interface{}{
			"project_id":      projectID,
			"content":         content,
			"summary":         summary,
			"conversation_id": conversationID,
		})
		body, err := client.Post("/api/memory-doc/propose", string(payload))
		if err != nil {
			return "", err
		}
		var result struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &result) == nil && result.Message != "" {
			return result.Message, nil
		}
		return "Proposta de alteração criada. O usuário precisa aprovar antes que a alteração seja aplicada.", nil

	case "devmanager_get_my_task":
		if sessionID == "" {
			return "Not running within a DevManager session (no session ID available).", nil
		}
		body, err := client.Get(fmt.Sprintf("/api/sessions/%s/task", sessionID))
		if err != nil {
			return "No task linked to this session.", nil
		}
		var task struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Status      string `json:"status"`
			Priority    string `json:"priority"`
			ProjectID   int64  `json:"project_id"`
		}
		if err := json.Unmarshal(body, &task); err != nil {
			return string(body), nil
		}
		return fmt.Sprintf("Linked Task [%d]: %s\nStatus: %s | Priority: %s\nProject ID: %d\nDescription: %s",
			task.ID, task.Title, task.Status, task.Priority, task.ProjectID, task.Description), nil

	case "devmanager_get_session_info":
		if sessionID == "" {
			return "Not running within a DevManager session (no session ID available).", nil
		}
		body, err := client.Get(fmt.Sprintf("/api/sessions/%s", sessionID))
		if err != nil {
			return fmt.Sprintf("Session ID: %s (could not fetch details: %v)", sessionID, err), nil
		}
		var sess struct {
			ID        string `json:"id"`
			ProjectID int64  `json:"project_id"`
			Status    string `json:"status"`
			Name      string `json:"name"`
		}
		if err := json.Unmarshal(body, &sess); err != nil {
			return string(body), nil
		}
		result := fmt.Sprintf("Session: %s\nName: %s\nProject ID: %d\nStatus: %s", sess.ID, sess.Name, sess.ProjectID, sess.Status)
		// Try to get linked task
		taskBody, taskErr := client.Get(fmt.Sprintf("/api/sessions/%s/task", sessionID))
		if taskErr == nil {
			var task struct {
				ID    int64  `json:"id"`
				Title string `json:"title"`
			}
			if json.Unmarshal(taskBody, &task) == nil {
				result += fmt.Sprintf("\nLinked Task: [%d] %s", task.ID, task.Title)
			}
		} else {
			result += "\nLinked Task: none"
		}
		return result, nil

	case "devmanager_request_task_evaluation":
		if sessionID == "" {
			return "", fmt.Errorf("not running within a DevManager session (no session ID available)")
		}
		_, err := client.Post(fmt.Sprintf("/api/sessions/%s/evaluate", sessionID), "{}")
		if err != nil {
			return "", fmt.Errorf("failed to request evaluation: %w", err)
		}
		return "Task evaluation requested. The AI Assistant will analyze this session and suggest relevant task actions to the user via notification cards.", nil

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

// formatMemoryDoc formats the memory doc JSON response.
func formatMemoryDoc(body []byte) (string, error) {
	var meta struct {
		ProjectID int64  `json:"project_id"`
		Content   string `json:"content"`
		Version   int    `json:"version"`
		Exists    bool   `json:"exists"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return string(body), nil
	}
	if !meta.Exists || meta.Content == "" {
		return fmt.Sprintf("No memory doc exists for project %d yet. Sync the project to load its CLAUDE.md.", meta.ProjectID), nil
	}
	return fmt.Sprintf("--- Memory Doc (v%d) ---\n%s", meta.Version, meta.Content), nil
}

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}

// getProjectID extracts project_id from params map.
func getProjectID(params map[string]interface{}) int64 {
	if v, ok := params["project_id"]; ok {
		switch id := v.(type) {
		case float64:
			return int64(id)
		case string:
			var n int64
			if _, err := fmt.Sscanf(id, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// getTaskID extracts task_id from params map.
func getTaskID(params map[string]interface{}) (int64, bool) {
	if v, ok := params["task_id"]; ok {
		switch id := v.(type) {
		case float64:
			return int64(id), true
		case string:
			var n int64
			if _, err := fmt.Sscanf(id, "%d", &n); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// formatTaskList formats the tasks JSON response into readable text.
func formatTaskList(body []byte) (string, error) {
	var tasks []struct {
		ID       int64  `json:"id"`
		ParentID *struct {
			Int64 int64 `json:"Int64"`
			Valid bool  `json:"Valid"`
		} `json:"parent_id"`
		Title    string `json:"title"`
		Status   string `json:"status"`
		Priority string `json:"priority"`
		DueDate  *struct {
			Time  string `json:"Time"`
			Valid bool   `json:"Valid"`
		} `json:"due_date"`
	}
	if err := json.Unmarshal(body, &tasks); err != nil {
		return string(body), nil
	}
	if len(tasks) == 0 {
		return "No tasks found.", nil
	}

	statusIcons := map[string]string{
		"todo":        "[ ]",
		"in_progress": "[~]",
		"done":        "[x]",
		"blocked":     "[!]",
	}
	priorityLabels := map[string]string{
		"low":    "LOW",
		"medium": "MED",
		"high":   "HIGH",
		"urgent": "URG",
	}

	result := ""
	for _, t := range tasks {
		icon := statusIcons[t.Status]
		prio := priorityLabels[t.Priority]
		indent := ""
		if t.ParentID != nil && t.ParentID.Valid {
			indent = "  "
		}
		due := ""
		if t.DueDate != nil && t.DueDate.Valid {
			due = " | due: " + t.DueDate.Time[:10]
		}
		result += fmt.Sprintf("%s%s [%d] %s (%s%s)\n", indent, icon, t.ID, t.Title, prio, due)
	}
	return result, nil
}

// normalizeMCPParams converts args and env fields to JSON strings if they were
// sent as native arrays/objects (which happens when Claude sends structured data).
func normalizeMCPParams(params map[string]interface{}) {
	for _, key := range []string{"args", "env"} {
		v, ok := params[key]
		if !ok {
			continue
		}
		// Already a string — keep as-is
		if _, isStr := v.(string); isStr {
			continue
		}
		// Native array or object — marshal to JSON string
		b, err := json.Marshal(v)
		if err == nil {
			params[key] = string(b)
		}
	}
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
