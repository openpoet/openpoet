package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"devmanager/internal/llm"
)

// MCPTool is a type alias for llm.MCPToolDef — the unified tool definition for MCP protocol.
type MCPTool = llm.MCPToolDef

// AllTools returns all MCP tool definitions (with devmanager_ prefix).
// Used by the API to expose tool metadata and by session manager for policy checks.
func AllTools() []MCPTool {
	return llm.MCPTools("session")
}

// toolsDef returns MCP tools filtered by context.
// "chat" includes all tools (even ChatOnly); other values exclude ChatOnly tools.
func toolsDef(context string) []MCPTool {
	return llm.MCPTools(context)
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

	case "devmanager_get_task":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		taskID, ok := getTaskID(params)
		if !ok {
			return "", fmt.Errorf("task_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/tasks/%d", projectID, taskID))
		if err != nil {
			return "", err
		}
		return formatTaskDetail(client, projectID, taskID, body)

	case "devmanager_read_document":
		docID, _ := params["document_id"].(string)
		if docID == "" {
			return "", fmt.Errorf("document_id is required")
		}
		if strings.HasPrefix(docID, "plan:") {
			sessionID := strings.TrimPrefix(docID, "plan:")
			body, err := client.Get(fmt.Sprintf("/api/sessions/%s/plan", sessionID))
			if err != nil {
				return "", err
			}
			var plan struct {
				Content   string `json:"content"`
				UpdatedAt string `json:"updated_at"`
			}
			if json.Unmarshal(body, &plan) == nil && plan.Content != "" {
				return fmt.Sprintf("## Session Plan\n\n%s", plan.Content), nil
			}
			return string(body), nil
		}
		body, err := client.Get(fmt.Sprintf("/api/documents/%s", docID))
		if err != nil {
			return "", err
		}
		var doc struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Content string `json:"content"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(body, &doc) == nil {
			return fmt.Sprintf("## %s\n\nStatus: %s | ID: %s\n\n%s", doc.Title, doc.Status, doc.ID, doc.Content), nil
		}
		return string(body), nil

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

	// ---- Composite tools ----

	case "devmanager_dashboard":
		return executeDashboard(client)

	case "devmanager_batch":
		return executeBatch(client, args, sessionID, conversationID)

	// ---- Session management tools ----

	case "devmanager_start_session":
		// Convert string IDs to numbers for API compatibility
		if pid, ok := params["project_id"].(string); ok {
			var id int64
			fmt.Sscanf(pid, "%d", &id)
			params["project_id"] = id
		}
		if tid, ok := params["task_id"].(string); ok && tid != "" {
			var id int64
			fmt.Sscanf(tid, "%d", &id)
			params["task_id"] = id
		}
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/sessions", string(payload))
		if err != nil {
			return "", err
		}
		var sess struct {
			ID        string `json:"id"`
			ProjectID int64  `json:"project_id"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(body, &sess) == nil {
			return fmt.Sprintf("Session started: %s (project: %d, status: %s)", sess.ID, sess.ProjectID, sess.Status), nil
		}
		return string(body), nil

	case "devmanager_stop_session":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/sessions/%s", sid))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session %s stopped", sid), nil

	case "devmanager_list_sessions":
		body, err := client.Get("/api/sessions")
		if err != nil {
			return "", err
		}
		return formatSessionsList(body, params)

	case "devmanager_send_to_session":
		sid, _ := params["session_id"].(string)
		text, _ := params["text"].(string)
		if sid == "" || text == "" {
			return "", fmt.Errorf("session_id and text are required")
		}
		payload, _ := json.Marshal(map[string]string{"text": text})
		_, err := client.Post(fmt.Sprintf("/api/sessions/%s/input", sid), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Sent to session %s: %s", sid[:8], truncate(text, 100)), nil

	case "devmanager_create_document":
		title, _ := params["title"].(string)
		content, _ := params["content"].(string)
		convID := conversationID
		if v, ok := params["conversation_id"].(string); ok && v != "" {
			convID = v
		}
		if content == "" {
			return "", fmt.Errorf("content is required")
		}
		docPayload := map[string]interface{}{
			"title":           title,
			"content":         content,
			"conversation_id": convID,
		}
		if taskID, ok := params["task_id"].(string); ok && taskID != "" {
			docPayload["task_id"] = taskID
		}
		payload, _ := json.Marshal(docPayload)
		body, err := client.Post("/api/documents", string(payload))
		if err != nil {
			return "", err
		}
		var result struct {
			ID   string `json:"id"`
			Link string `json:"link"`
		}
		if json.Unmarshal(body, &result) == nil {
			return fmt.Sprintf("Documento criado com sucesso. Um botão 'Ver Documento' foi exibido automaticamente no chat. NÃO gere links — o usuário usará o botão nativo. Link interno: %s", result.Link), nil
		}
		return string(body), nil

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// executeDashboard returns a compact JSON summary of the entire DevManager state.
func executeDashboard(client *APIClient) (string, error) {
	type projectSummary struct {
		ID             int64            `json:"id"`
		Name           string           `json:"name"`
		Type           string           `json:"type"`
		Tasks          map[string]int   `json:"tasks"`
		ActiveSessions int              `json:"active_sessions"`
	}

	// Fetch projects
	projectsBody, err := client.Get("/api/projects")
	if err != nil {
		return "", fmt.Errorf("failed to fetch projects: %w", err)
	}
	var projects []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	json.Unmarshal(projectsBody, &projects)

	// Fetch sessions
	sessionsBody, err := client.Get("/api/sessions")
	if err != nil {
		return "", fmt.Errorf("failed to fetch sessions: %w", err)
	}
	var sessions []struct {
		ID        string `json:"id"`
		ProjectID int64  `json:"project_id"`
		Status    string `json:"status"`
		Name      string `json:"name"`
	}
	json.Unmarshal(sessionsBody, &sessions)

	// Count active sessions per project
	activeByProject := make(map[int64]int)
	var activeSessions []map[string]interface{}
	for _, s := range sessions {
		if s.Status == "running" || s.Status == "starting" {
			activeByProject[s.ProjectID]++
			activeSessions = append(activeSessions, map[string]interface{}{
				"id": s.ID, "project_id": s.ProjectID, "status": s.Status, "name": s.Name,
			})
		}
	}

	// Build project summaries with task counts
	totalTasks := map[string]int{"todo": 0, "in_progress": 0, "done": 0, "awaiting_approval": 0}
	var projectSummaries []projectSummary
	for _, p := range projects {
		tasksBody, _ := client.Get(fmt.Sprintf("/api/projects/%d/tasks", p.ID))
		var tasks []struct {
			ID       int64  `json:"id"`
			ParentID *struct {
				Int64 int64 `json:"Int64"`
				Valid bool  `json:"Valid"`
			} `json:"parent_id"`
			Status string `json:"status"`
		}
		json.Unmarshal(tasksBody, &tasks)

		// Collect IDs that are parents (umbrella tasks)
		parentIDs := make(map[int64]bool)
		for _, t := range tasks {
			if t.ParentID != nil && t.ParentID.Valid {
				parentIDs[t.ParentID.Int64] = true
			}
		}

		taskCounts := map[string]int{"todo": 0, "in_progress": 0, "done": 0, "awaiting_approval": 0}
		for _, t := range tasks {
			if parentIDs[t.ID] {
				continue // skip umbrella tasks
			}
			taskCounts[t.Status]++
			totalTasks[t.Status]++
		}

		projectSummaries = append(projectSummaries, projectSummary{
			ID: p.ID, Name: p.Name, Type: p.Type,
			Tasks: taskCounts, ActiveSessions: activeByProject[p.ID],
		})
	}

	dashboard := map[string]interface{}{
		"projects":        projectSummaries,
		"active_sessions": activeSessions,
		"total_tasks":     totalTasks,
	}

	result, _ := json.Marshal(dashboard)
	return string(result), nil
}

// executeBatch runs multiple tool calls in a single request.
func executeBatch(client *APIClient, args json.RawMessage, sessionID, conversationID string) (string, error) {
	var batchParams struct {
		Calls []struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		} `json:"calls"`
	}
	if err := json.Unmarshal(args, &batchParams); err != nil {
		return "", fmt.Errorf("invalid batch params: %w", err)
	}

	if len(batchParams.Calls) > 10 {
		return "", fmt.Errorf("batch limited to 10 calls, got %d", len(batchParams.Calls))
	}

	var results []map[string]interface{}
	for _, call := range batchParams.Calls {
		// Prevent recursion
		if call.Tool == "devmanager_batch" {
			results = append(results, map[string]interface{}{
				"tool": call.Tool, "error": "cannot nest batch calls",
			})
			continue
		}

		result, err := executeTool(client, call.Tool, call.Args, sessionID, conversationID)
		if err != nil {
			results = append(results, map[string]interface{}{
				"tool": call.Tool, "error": err.Error(),
			})
		} else {
			results = append(results, map[string]interface{}{
				"tool": call.Tool, "result": result,
			})
		}
	}

	out, _ := json.Marshal(results)
	return string(out), nil
}

// formatSessionsList formats and optionally filters the sessions list.
func formatSessionsList(body []byte, params map[string]interface{}) (string, error) {
	var sessions []struct {
		ID        string `json:"id"`
		ProjectID int64  `json:"project_id"`
		Status    string `json:"status"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(body, &sessions); err != nil {
		return string(body), nil
	}

	filterStatus, _ := params["status"].(string)
	filterProjectID := int64(0)
	if v, ok := params["project_id"].(float64); ok {
		filterProjectID = int64(v)
	}

	if len(sessions) == 0 {
		return "No sessions found.", nil
	}

	result := ""
	for _, s := range sessions {
		if filterStatus != "" && s.Status != filterStatus {
			continue
		}
		if filterProjectID > 0 && s.ProjectID != filterProjectID {
			continue
		}
		name := s.Name
		if name == "" {
			name = s.ID[:8]
		}
		result += fmt.Sprintf("- [%s] %s (project: %d, status: %s)\n", s.ID[:8], name, s.ProjectID, s.Status)
	}
	if result == "" {
		return "No sessions matching filter.", nil
	}
	return result, nil
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
		"todo":              "[ ]",
		"in_progress":       "[~]",
		"done":              "[x]",
		"awaiting_approval": "[?]",
	}
	priorityLabels := map[string]string{
		"low":    "LOW",
		"medium": "MED",
		"high":   "HIGH",
		"urgent": "URG",
	}

	// Identify umbrella tasks (tasks that have children)
	parentIDs := make(map[int64]bool)
	childrenByParent := make(map[int64][]struct{ Status string })
	for _, t := range tasks {
		if t.ParentID != nil && t.ParentID.Valid {
			parentIDs[t.ParentID.Int64] = true
			childrenByParent[t.ParentID.Int64] = append(childrenByParent[t.ParentID.Int64], struct{ Status string }{t.Status})
		}
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
		umbrella := ""
		if parentIDs[t.ID] {
			kids := childrenByParent[t.ID]
			doneCount := 0
			for _, k := range kids {
				if k.Status == "done" {
					doneCount++
				}
			}
			umbrella = fmt.Sprintf(" [umbrella: %d/%d done]", doneCount, len(kids))
		}
		result += fmt.Sprintf("%s%s [%d] %s (%s%s)%s\n", indent, icon, t.ID, t.Title, prio, due, umbrella)
	}
	return result, nil
}

// formatTaskDetail fetches task details, history, documents, and sessions,
// returning a rich text summary with document metadata (no content).
func formatTaskDetail(client *APIClient, projectID int64, taskID int64, taskBody []byte) (string, error) {
	var task struct {
		ID          int64  `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Priority    string `json:"priority"`
		DueDate     *struct {
			Time  string `json:"Time"`
			Valid bool   `json:"Valid"`
		} `json:"due_date"`
		ParentID *struct {
			Int64 int64 `json:"Int64"`
			Valid bool  `json:"Valid"`
		} `json:"parent_id"`
	}
	if err := json.Unmarshal(taskBody, &task); err != nil {
		return string(taskBody), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Task #%d: %s\n", task.ID, task.Title))
	sb.WriteString(fmt.Sprintf("Status: %s | Priority: %s", task.Status, task.Priority))
	if task.DueDate != nil && task.DueDate.Valid {
		sb.WriteString(fmt.Sprintf(" | Due: %s", task.DueDate.Time[:10]))
	}
	if task.ParentID != nil && task.ParentID.Valid {
		sb.WriteString(fmt.Sprintf(" | Parent: #%d", task.ParentID.Int64))
	}
	sb.WriteString("\n")

	if task.Description != "" {
		sb.WriteString(fmt.Sprintf("\n### Description\n%s\n", task.Description))
	}

	// Fetch documents
	docsBody, err := client.Get(fmt.Sprintf("/api/projects/%d/tasks/%d/documents", projectID, taskID))
	if err == nil {
		var docs []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		}
		if json.Unmarshal(docsBody, &docs) == nil && len(docs) > 0 {
			sb.WriteString(fmt.Sprintf("\n### Documents (%d)\n", len(docs)))
			for _, d := range docs {
				icon := "[D]"
				if d.Type == "plan" {
					icon = "[P]"
				} else if strings.HasPrefix(d.Title, "Verificação") || strings.HasPrefix(d.Title, "Verificacao") {
					icon = "[V]"
				}
				sb.WriteString(fmt.Sprintf("- %s %s (id: %s, status: %s)\n", icon, d.Title, d.ID, d.Status))
			}
			sb.WriteString("\nUse devmanager_read_document to read a document's content.\n")
		}
	}

	// Fetch history (recent 20)
	historyBody, err := client.Get(fmt.Sprintf("/api/projects/%d/tasks/%d/history?limit=20", projectID, taskID))
	if err == nil {
		var entries []struct {
			EventType string          `json:"event_type"`
			Details   json.RawMessage `json:"details"`
			Actor     string          `json:"actor"`
			CreatedAt string          `json:"created_at"`
		}
		if json.Unmarshal(historyBody, &entries) == nil && len(entries) > 0 {
			sb.WriteString(fmt.Sprintf("\n### History (recent %d)\n", len(entries)))
			eventIcons := map[string]string{
				"task_created":             "[+]",
				"status_change":            "[~]",
				"priority_change":          "[~]",
				"comment_added":            "[C]",
				"session_linked":           "[S]",
				"session_started":          "[>]",
				"session_ended":            "[.]",
				"verification_doc_created": "[V]",
				"verification_approved":    "[A]",
				"verification_rejected":    "[R]",
				"task_assigned":            "[@]",
			}
			for _, e := range entries {
				icon := eventIcons[e.EventType]
				if icon == "" {
					icon = "[-]"
				}
				// Parse details for extra context
				detail := ""
				var detailMap map[string]interface{}
				if json.Unmarshal(e.Details, &detailMap) == nil {
					if from, ok := detailMap["from"].(string); ok {
						if to, ok := detailMap["to"].(string); ok {
							detail = fmt.Sprintf(": %s → %s", from, to)
						}
					}
					if comment, ok := detailMap["comment"].(string); ok {
						if len(comment) > 80 {
							comment = comment[:80] + "..."
						}
						detail = fmt.Sprintf(": %s", comment)
					}
				}
				ts := e.CreatedAt
				if len(ts) > 16 {
					ts = ts[:16]
				}
				sb.WriteString(fmt.Sprintf("%s %s%s — %s\n", icon, e.EventType, detail, ts))
			}
		}
	}

	// Fetch sessions
	sessionsBody, err := client.Get(fmt.Sprintf("/api/projects/%d/tasks/%d/sessions", projectID, taskID))
	if err == nil {
		var sessions []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if json.Unmarshal(sessionsBody, &sessions) == nil && len(sessions) > 0 {
			sb.WriteString(fmt.Sprintf("\n### Sessions (%d)\n", len(sessions)))
			for _, s := range sessions {
				sb.WriteString(fmt.Sprintf("- %s (%s) — %s\n", s.Name, s.Status, s.ID[:8]))
			}
		}
	}

	return sb.String(), nil
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
