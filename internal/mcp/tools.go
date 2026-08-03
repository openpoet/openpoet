package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"openpoet/internal/llm"
	"openpoet/internal/sessionmeta"
)

// MCPTool is a type alias for llm.MCPToolDef — the unified tool definition for MCP protocol.
type MCPTool = llm.MCPToolDef

// toolNumber coerces a JSON tool argument (float64, string, or int) to int64.
func toolNumber(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		var n int64
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}

// coordinatorResponse passes the coordinator surface's JSON straight through:
// success bodies AND typed errors (coordinator_fence_stale,
// platform_project_out_of_scope, ...) both reach the model verbatim.
func coordinatorResponse(body []byte, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// AllTools returns all MCP tool definitions (with openpoet_ prefix).
// Used by the API to expose tool metadata and by session manager for policy checks.
func AllTools() []MCPTool {
	return llm.MCPTools("session")
}

// toolsDef returns MCP tools filtered by context.
// "chat" includes all tools (even ChatOnly); other values exclude ChatOnly tools.
func toolsDef(context string) []MCPTool {
	return llm.MCPTools(context)
}

// executeTool runs a tool by name with the given arguments, calling the OpenPoet API.
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
	case "openpoet_list_skills":
		body, err := client.Get("/api/config/skills")
		if err != nil {
			return "", err
		}
		return formatSkillsList(body)

	case "openpoet_create_skill":
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/config/skills", string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill created: %s", string(body)), nil

	case "openpoet_update_skill":
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

	case "openpoet_delete_skill":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/config/skills/%d", id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill %d deleted", id), nil

	case "openpoet_get_skill":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/config/skills/%d", id))
		if err != nil {
			return "", err
		}
		return string(body), nil

	case "openpoet_create_project_skill":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		delete(params, "project_id")
		payload, _ := json.Marshal(params)
		body, err := client.Post(fmt.Sprintf("/api/projects/%d/skills", projectID), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project skill created: %s", string(body)), nil

	case "openpoet_update_project_skill":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		delete(params, "project_id")
		delete(params, "id")
		payload, _ := json.Marshal(params)
		body, err := client.Put(fmt.Sprintf("/api/projects/%d/skills/%d", projectID, id), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project skill updated: %s", string(body)), nil

	case "openpoet_delete_project_skill":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/projects/%d/skills/%d", projectID, id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project skill %d deleted", id), nil

	case "openpoet_get_project_skill":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		// Use the list endpoint and find the specific skill
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/skills", projectID))
		if err != nil {
			return "", err
		}
		var data struct {
			ProjectSkills []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Content  string `json:"content"`
				Category string `json:"category"`
				Enabled  bool   `json:"enabled"`
			} `json:"project_skills"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return string(body), nil
		}
		for _, ps := range data.ProjectSkills {
			if ps.ID == id {
				return fmt.Sprintf("Name: %s\nCategory: %s\nEnabled: %v\n---\n%s", ps.Name, ps.Category, ps.Enabled, ps.Content), nil
			}
		}
		return "", fmt.Errorf("project skill %d not found", id)

	case "openpoet_list_project_skills":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/skills", projectID))
		if err != nil {
			return "", err
		}
		return formatProjectSkillsList(body)

	case "openpoet_list_projects":
		body, err := client.Get("/api/projects")
		if err != nil {
			return "", err
		}
		return formatProjectsList(body)

	case "openpoet_get_mcp_server":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/config/mcps/%d", id))
		if err != nil {
			return "", err
		}
		return string(body), nil

	case "openpoet_list_mcp_servers":
		body, err := client.Get("/api/config/mcps")
		if err != nil {
			return "", err
		}
		return formatMCPsList(body)

	case "openpoet_create_mcp_server":
		normalizeMCPParams(params)
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/config/mcps", string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server created: %s", string(body)), nil

	case "openpoet_update_mcp_server":
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

	case "openpoet_delete_mcp_server":
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/config/mcps/%d", id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MCP server %d deleted", id), nil

	case "openpoet_create_project_mcp_server":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		delete(params, "project_id")
		normalizeMCPParams(params)
		payload, _ := json.Marshal(params)
		body, err := client.Post(fmt.Sprintf("/api/projects/%d/mcp-servers", projectID), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project MCP server created: %s", string(body)), nil

	case "openpoet_update_project_mcp_server":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		delete(params, "project_id")
		delete(params, "id")
		normalizeMCPParams(params)
		payload, _ := json.Marshal(params)
		body, err := client.Put(fmt.Sprintf("/api/projects/%d/mcp-servers/%d", projectID, id), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project MCP server updated: %s", string(body)), nil

	case "openpoet_delete_project_mcp_server":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/projects/%d/mcp-servers/%d", projectID, id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Project MCP server %d deleted", id), nil

	case "openpoet_get_project_mcp_server":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/mcp-servers/%d", projectID, id))
		if err != nil {
			return "", err
		}
		return string(body), nil

	case "openpoet_list_project_mcp_servers":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/mcp-servers", projectID))
		if err != nil {
			return "", err
		}
		return formatProjectMCPServersList(body)

	case "openpoet_update_setting":
		payload, _ := json.Marshal(params)
		_, err := client.Put("/api/config/settings", string(payload))
		if err != nil {
			return "", err
		}
		key, _ := params["key"].(string)
		return fmt.Sprintf("Setting '%s' updated", key), nil

	case "openpoet_sync_config":
		body, err := client.Post("/api/config/sync-all", "{}")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Config synced: %s", string(body)), nil

	case "openpoet_list_project_files":
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

	case "openpoet_read_project_file":
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

	// ---- Shared project file access tools ----

	case "openpoet_list_shared_projects":
		if sessionID == "" {
			return "", fmt.Errorf("not running within an OpenPoet session")
		}
		projectID, err := getSessionProjectID(client, sessionID)
		if err != nil {
			return "", err
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/shares", projectID))
		if err != nil {
			return "", err
		}
		var projects []struct {
			ProjectID int64  `json:"project_id"`
			Name      string `json:"name"`
			Path      string `json:"path"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal(body, &projects); err != nil {
			return string(body), nil
		}
		if len(projects) == 0 {
			return "No shared projects configured. Ask the user to configure cross-project file access in the OpenPoet project settings.", nil
		}
		result := "Shared projects (read access):\n"
		for _, p := range projects {
			result += fmt.Sprintf("- [%d] %s (%s: %s)\n", p.ProjectID, p.Name, p.Type, p.Path)
		}
		return result, nil

	case "openpoet_list_shared_files":
		if sessionID == "" {
			return "", fmt.Errorf("not running within an OpenPoet session")
		}
		targetID := getProjectID(params)
		if targetID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		if err := verifyShareAccess(client, sessionID, targetID); err != nil {
			return "", err
		}
		path, _ := params["path"].(string)
		endpoint := fmt.Sprintf("/api/projects/%d/files?path=%s", targetID, path)
		body, err := client.Get(endpoint)
		if err != nil {
			return "", err
		}
		return formatFileList(body)

	case "openpoet_read_shared_file":
		if sessionID == "" {
			return "", fmt.Errorf("not running within an OpenPoet session")
		}
		targetID := getProjectID(params)
		if targetID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		path, _ := params["path"].(string)
		if path == "" {
			return "", fmt.Errorf("path is required")
		}
		if err := verifyShareAccess(client, sessionID, targetID); err != nil {
			return "", err
		}
		endpoint := fmt.Sprintf("/api/projects/%d/files/view/%s", targetID, path)
		body, err := client.Get(endpoint)
		if err != nil {
			return "", err
		}
		return formatFileContent(body)

	case "openpoet_copy_shared_file":
		if sessionID == "" {
			return "", fmt.Errorf("not running within an OpenPoet session")
		}
		targetID := getProjectID(params)
		if targetID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		srcPath, _ := params["src_path"].(string)
		if srcPath == "" {
			return "", fmt.Errorf("src_path is required")
		}
		destPath, _ := params["dest_path"].(string)
		if destPath == "" {
			return "", fmt.Errorf("dest_path is required")
		}
		if err := verifyShareAccess(client, sessionID, targetID); err != nil {
			return "", err
		}
		ownProjectID, err := getSessionProjectID(client, sessionID)
		if err != nil {
			return "", err
		}
		// Download raw bytes from shared project
		data, err := client.Get(fmt.Sprintf("/api/projects/%d/files/raw/%s", targetID, srcPath))
		if err != nil {
			return "", fmt.Errorf("failed to read source file: %w", err)
		}
		// Upload raw bytes to own project
		if _, err := client.PostRaw(fmt.Sprintf("/api/projects/%d/files/raw?path=%s", ownProjectID, destPath), data); err != nil {
			return "", fmt.Errorf("failed to write file: %w", err)
		}
		return fmt.Sprintf("Copied %s (%s) → %s", srcPath, formatSize(int64(len(data))), destPath), nil

	case "openpoet_copy_shared_folder":
		if sessionID == "" {
			return "", fmt.Errorf("not running within an OpenPoet session")
		}
		targetID := getProjectID(params)
		if targetID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		srcPath, _ := params["src_path"].(string)
		destPath, _ := params["dest_path"].(string)
		if destPath == "" {
			return "", fmt.Errorf("dest_path is required")
		}
		if err := verifyShareAccess(client, sessionID, targetID); err != nil {
			return "", err
		}
		ownProjectID, err := getSessionProjectID(client, sessionID)
		if err != nil {
			return "", err
		}
		// Recursively collect all files
		filePaths, err := collectSharedFiles(client, targetID, srcPath)
		if err != nil {
			return "", fmt.Errorf("failed to list source folder: %w", err)
		}
		if len(filePaths) == 0 {
			return "Source folder is empty or does not exist.", nil
		}
		// Copy each file
		copied := 0
		var totalSize int64
		var errors []string
		for _, fp := range filePaths {
			// Download raw bytes from shared project
			data, err := client.Get(fmt.Sprintf("/api/projects/%d/files/raw/%s", targetID, fp))
			if err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", fp, err))
				continue
			}
			// Compute destination: replace srcPath prefix with destPath
			relPath := fp
			if srcPath != "" {
				relPath = strings.TrimPrefix(fp, srcPath)
				relPath = strings.TrimPrefix(relPath, "/")
			}
			fileDest := path.Join(destPath, relPath)
			// Upload raw bytes to own project
			if _, err := client.PostRaw(fmt.Sprintf("/api/projects/%d/files/raw?path=%s", ownProjectID, fileDest), data); err != nil {
				errors = append(errors, fmt.Sprintf("%s: write error: %v", fp, err))
				continue
			}
			copied++
			totalSize += int64(len(data))
		}
		result := fmt.Sprintf("Copied %d files (%s) to %s", copied, formatSize(totalSize), destPath)
		if len(errors) > 0 {
			result += fmt.Sprintf("\n%d errors:\n", len(errors))
			for _, e := range errors {
				result += fmt.Sprintf("  - %s\n", e)
			}
		}
		return result, nil

	case "openpoet_get_memory_doc":
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

	case "openpoet_list_tasks":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/tasks", projectID))
		if err != nil {
			return "", err
		}
		return formatTaskList(body)

	case "openpoet_create_task":
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

	case "openpoet_update_task":
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

	case "openpoet_delete_task":
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

	case "openpoet_get_task":
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

	case "openpoet_read_document":
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

	case "openpoet_update_memory_doc":
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
		return "Change proposal created. The user must approve before the change is applied.", nil

	case "openpoet_get_my_task":
		if sessionID == "" {
			return "Not running within a OpenPoet session (no session ID available).", nil
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

	case "openpoet_get_session_info":
		if sessionID == "" {
			return "Not running within a OpenPoet session (no session ID available).", nil
		}
		body, err := client.Get(fmt.Sprintf("/api/sessions/%s", sessionID))
		if err != nil {
			return fmt.Sprintf("Session ID: %s (could not fetch details: %v)", sessionID, err), nil
		}
		var sess struct {
			ID             string `json:"id"`
			ProjectID      int64  `json:"project_id"`
			Status         string `json:"status"`
			Name           string `json:"name"`
			Backend        string `json:"backend"`
			Model          string `json:"model"`
			RequestedModel string `json:"requested_model"`
			Effort         string `json:"effort"`
			Harness        string `json:"harness"`
		}
		if err := json.Unmarshal(body, &sess); err != nil {
			return string(body), nil
		}
		meta := sessionmeta.WithSessionValues(fetchSessionMetadata(client, sess.ProjectID, sess.Backend), sess.Model, sess.Effort, sess.Harness)
		requestedModel := sess.RequestedModel
		if requestedModel == "" {
			requestedModel = meta.Model
		}
		result := fmt.Sprintf("Session: %s\nName: %s\nProject ID: %d\nStatus: %s\nEffective model: %s\nRequested model: %s\nEffort: %s\nHarness: %s", sess.ID, sess.Name, sess.ProjectID, sess.Status, meta.Model, requestedModel, meta.Effort, meta.Harness)
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

	case "openpoet_request_task_evaluation":
		if sessionID == "" {
			return "", fmt.Errorf("not running within a OpenPoet session (no session ID available)")
		}
		_, err := client.Post(fmt.Sprintf("/api/sessions/%s/evaluate", sessionID), "{}")
		if err != nil {
			return "", fmt.Errorf("failed to request evaluation: %w", err)
		}
		return "Task evaluation requested. The AI Assistant will analyze this session and suggest relevant task actions to the user via notification cards.", nil

	// ---- Composite tools ----

	case "openpoet_dashboard":
		return executeDashboard(client)

	case "openpoet_batch":
		return executeBatch(client, args, sessionID, conversationID)

	// ---- Session management tools ----

	case "openpoet_start_session":
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
		if taskID, ok := params["task_id"]; ok && fmt.Sprintf("%v", taskID) != "" {
			// MCP starts are programmatic and must never fall back to the UI
			// task-loaded notification/modal, even if an older caller sends the
			// legacy flag as false.
			params["auto_start_task_prompt"] = true
		}
		payload, _ := json.Marshal(params)
		body, err := client.Post("/api/sessions", string(payload))
		if err != nil {
			return "", err
		}
		var sess struct {
			ID          string `json:"id"`
			ProjectID   int64  `json:"project_id"`
			Status      string `json:"status"`
			WorkspaceID string `json:"workspace_id"`
			WorkDir     string `json:"work_dir"`
		}
		if json.Unmarshal(body, &sess) == nil {
			line := fmt.Sprintf("Session started: %s (project: %d, status: %s)", sess.ID, sess.ProjectID, sess.Status)
			if sess.WorkspaceID != "" {
				// Report the isolated lane: the caller needs its id to merge the work
				// later, and needs to know the edits land in a separate tree.
				line += fmt.Sprintf(", isolated in workspace %s at %s", sess.WorkspaceID, sess.WorkDir)
			}
			return line, nil
		}
		return string(body), nil

	// ---- Coordinator tier (Phase 7.1 — Maestro) ----
	// Thin wrappers over the token-authed /api/coordinator surface; the
	// APIClient carries the verified opst1_ session bearer, which is the
	// coordinator's identity. Raw JSON is returned so the model sees the typed
	// codes (coordinator_fence_stale, platform_project_out_of_scope, ...).

	case "openpoet_coordinator_elect":
		body := map[string]any{"group": toolNumber(params["group"])}
		if ttl := toolNumber(params["ttl_seconds"]); ttl > 0 {
			body["ttl_seconds"] = ttl
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post("/api/coordinator/elect", string(payload)))

	case "openpoet_coordinator_status":
		return coordinatorResponse(client.Get("/api/coordinator/status"))

	case "openpoet_list_group_sessions":
		path := "/api/coordinator/sessions"
		if status, _ := params["status"].(string); status != "" {
			path += "?status=" + url.QueryEscape(status)
		}
		return coordinatorResponse(client.Get(path))

	case "openpoet_await_events":
		q := url.Values{}
		q.Set("after", fmt.Sprintf("%d", toolNumber(params["after"])))
		if filter, _ := params["filter"].(string); filter != "" {
			q.Set("filter", filter)
		}
		if timeout := toolNumber(params["timeout"]); timeout > 0 {
			q.Set("timeout", fmt.Sprintf("%d", timeout))
		}
		if consumer, _ := params["consumer"].(string); consumer != "" {
			q.Set("consumer", consumer)
		}
		return coordinatorResponse(client.Get("/api/coordinator/events/await?" + q.Encode()))

	case "openpoet_wait_for_session":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		q := url.Values{}
		if timeout := toolNumber(params["timeout"]); timeout > 0 {
			q.Set("timeout", fmt.Sprintf("%d", timeout))
		}
		return coordinatorResponse(client.Get(fmt.Sprintf("/api/coordinator/sessions/%s/wait?%s", url.PathEscape(sid), q.Encode())))

	case "openpoet_start_worker":
		body := map[string]any{"project_id": toolNumber(params["project_id"])}
		if fence, ok := params["fence_version"]; ok {
			body["fence_version"] = toolNumber(fence)
		}
		if tid := toolNumber(params["task_id"]); tid > 0 {
			body["task_id"] = tid
		}
		if mid := toolNumber(params["mission_id"]); mid > 0 {
			body["mission_id"] = mid
		}
		if role, _ := params["role"].(string); role != "" {
			body["role"] = role
		}
		for _, key := range []string{"backend", "workspace_id", "isolation", "custom_prompt", "idempotency_key"} {
			if v, _ := params[key].(string); v != "" {
				body[key] = v
			}
		}
		if dry, _ := params["dry_run"].(bool); dry {
			body["dry_run"] = true
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post("/api/coordinator/sessions", string(payload)))

	case "openpoet_start_mission":
		goal, _ := params["goal"].(string)
		if goal == "" {
			return "", fmt.Errorf("goal is required")
		}
		body := map[string]any{"goal": goal}
		if fence, ok := params["fence_version"]; ok {
			body["fence_version"] = toolNumber(fence)
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post("/api/coordinator/missions", string(payload)))

	case "openpoet_get_mission":
		return coordinatorResponse(client.Get(fmt.Sprintf("/api/coordinator/missions/%d", toolNumber(params["mission_id"]))))

	case "openpoet_update_mission_status":
		status, _ := params["status"].(string)
		body := map[string]any{"status": status}
		if fence, ok := params["fence_version"]; ok {
			body["fence_version"] = toolNumber(fence)
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post(fmt.Sprintf("/api/coordinator/missions/%d/status", toolNumber(params["mission_id"])), string(payload)))

	case "openpoet_attach_worker":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		body := map[string]any{"mission_id": toolNumber(params["mission_id"]), "session_id": sid}
		if role, _ := params["role"].(string); role != "" {
			body["role"] = role
		}
		if fence, ok := params["fence_version"]; ok {
			body["fence_version"] = toolNumber(fence)
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post("/api/coordinator/missions/workers/attach", string(payload)))

	case "openpoet_predict_merge":
		wsid, _ := params["workspace_id"].(string)
		if wsid == "" {
			return "", fmt.Errorf("workspace_id is required")
		}
		return coordinatorResponse(client.Get(fmt.Sprintf("/api/coordinator/workspaces/%s/merge_preview", url.PathEscape(wsid))))

	case "openpoet_merge_workspace":
		wsid, _ := params["workspace_id"].(string)
		if wsid == "" {
			return "", fmt.Errorf("workspace_id is required")
		}
		body := map[string]any{"mission_id": toolNumber(params["mission_id"])}
		if fence, ok := params["fence_version"]; ok {
			body["fence_version"] = toolNumber(fence)
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post(fmt.Sprintf("/api/coordinator/workspaces/%s/merge", url.PathEscape(wsid)), string(payload)))

	case "openpoet_emit_session_report":
		payload, _ := json.Marshal(params)
		return coordinatorResponse(client.Post("/api/session/report", string(payload)))

	case "openpoet_get_session_report":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		return coordinatorResponse(client.Get(fmt.Sprintf("/api/coordinator/sessions/%s/report", url.PathEscape(sid))))

	case "openpoet_send_to_worker":
		sid, _ := params["session_id"].(string)
		text, _ := params["text"].(string)
		if sid == "" || text == "" {
			return "", fmt.Errorf("session_id and text are required")
		}
		body := map[string]any{"text": text}
		if fence, ok := params["fence_version"]; ok {
			body["fence_version"] = toolNumber(fence)
		}
		if force, _ := params["force"].(bool); force {
			body["force"] = true
		}
		payload, _ := json.Marshal(body)
		return coordinatorResponse(client.Post(fmt.Sprintf("/api/coordinator/sessions/%s/input", url.PathEscape(sid)), string(payload)))

	case "openpoet_stop_session":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/sessions/%s", sid))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session %s stopped", sid), nil

	case "openpoet_list_sessions":
		body, err := client.Get("/api/sessions")
		if err != nil {
			return "", err
		}
		return formatSessionsList(client, body, params)

	case "openpoet_get_session":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/sessions/%s", sid))
		if err != nil {
			return "", err
		}
		return formatSessionDetail(client, body)

	case "openpoet_set_session_model":
		sid, _ := params["session_id"].(string)
		model, _ := params["model"].(string)
		if strings.TrimSpace(sid) == "" || strings.TrimSpace(model) == "" {
			return "", fmt.Errorf("session_id and model are required")
		}
		payload, _ := json.Marshal(map[string]string{"model": model})
		body, err := client.Post(fmt.Sprintf("/api/sessions/%s/model", sid), string(payload))
		if err != nil {
			return "", err
		}
		var updated struct {
			Model          string `json:"model"`
			RequestedModel string `json:"requested_model"`
			Effort         string `json:"effort"`
			Harness        string `json:"harness"`
		}
		if json.Unmarshal(body, &updated) == nil && updated.Model != "" {
			if updated.RequestedModel == "" {
				updated.RequestedModel = model
			}
			return fmt.Sprintf("Session %s requested model changed to %s (effective model: %s, effort: %s, harness: %s)", shortID(sid), updated.RequestedModel, updated.Model, updated.Effort, updated.Harness), nil
		}
		return string(body), nil

	case "openpoet_set_session_effort":
		sid, _ := params["session_id"].(string)
		effort, _ := params["effort"].(string)
		if strings.TrimSpace(sid) == "" || strings.TrimSpace(effort) == "" {
			return "", fmt.Errorf("session_id and effort are required")
		}
		payload, _ := json.Marshal(map[string]string{"effort": effort})
		body, err := client.Post(fmt.Sprintf("/api/sessions/%s/effort", sid), string(payload))
		if err != nil {
			return "", err
		}
		var updated struct {
			Model   string `json:"model"`
			Effort  string `json:"effort"`
			Harness string `json:"harness"`
		}
		if json.Unmarshal(body, &updated) == nil && updated.Effort != "" {
			return fmt.Sprintf("Session %s effort changed to %s (model: %s, harness: %s)", shortID(sid), updated.Effort, updated.Model, updated.Harness), nil
		}
		return string(body), nil

	case "openpoet_read_session_history":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		q := url.Values{}
		for _, key := range []string{"mode", "lines", "offset", "limit", "query", "context", "max_chars"} {
			if v, ok := params[key]; ok && fmt.Sprintf("%v", v) != "" {
				q.Set(key, fmt.Sprintf("%v", v))
			}
		}
		for _, key := range []string{"regex", "case_sensitive"} {
			if v, ok := params[key]; ok {
				q.Set(key, fmt.Sprintf("%v", v))
			}
		}
		endpoint := fmt.Sprintf("/api/sessions/%s/history", sid)
		if encoded := q.Encode(); encoded != "" {
			endpoint += "?" + encoded
		}
		body, err := client.Get(endpoint)
		if err != nil {
			return "", err
		}
		return formatSessionHistory(body)

	case "openpoet_send_to_session":
		sid, _ := params["session_id"].(string)
		text, _ := params["text"].(string)
		if text == "" {
			text, _ = params["prompt"].(string)
		}
		if sid == "" || text == "" {
			return "", fmt.Errorf("session_id and text are required")
		}
		payload, _ := json.Marshal(map[string]string{"text": text})
		_, err := client.Post(fmt.Sprintf("/api/sessions/%s/input", sid), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Sent to session %s: %s", shortID(sid), truncate(text, 100)), nil

	case "openpoet_link_session_task":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		payloadMap := map[string]interface{}{}
		if taskID, ok := getTaskID(params); ok {
			payloadMap["task_id"] = taskID
		} else {
			title, _ := params["title"].(string)
			if title == "" {
				return "", fmt.Errorf("task_id or title is required")
			}
			taskData := map[string]interface{}{
				"title":       title,
				"description": "",
				"priority":    "medium",
			}
			if desc, ok := params["description"].(string); ok {
				taskData["description"] = desc
			}
			if priority, ok := params["priority"].(string); ok && priority != "" {
				taskData["priority"] = priority
			}
			payloadMap["task_data"] = taskData
		}
		payload, _ := json.Marshal(payloadMap)
		body, err := client.Post(fmt.Sprintf("/api/sessions/%s/link-task", sid), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session task linked: %s", string(body)), nil

	case "openpoet_unlink_session_task":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		body, err := client.Post(fmt.Sprintf("/api/sessions/%s/unlink-task", sid), "{}")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Session task unlinked: %s", string(body)), nil

	case "openpoet_stop_session_and_update_task":
		sid, _ := params["session_id"].(string)
		if sid == "" {
			return "", fmt.Errorf("session_id is required")
		}
		taskBody, err := client.Get(fmt.Sprintf("/api/sessions/%s/task", sid))
		if err != nil {
			return "", fmt.Errorf("session has no linked task: %w", err)
		}
		var task struct {
			ID        int64  `json:"id"`
			ProjectID int64  `json:"project_id"`
			Title     string `json:"title"`
		}
		if err := json.Unmarshal(taskBody, &task); err != nil {
			return "", fmt.Errorf("failed to parse linked task: %w", err)
		}
		if _, err := client.Delete(fmt.Sprintf("/api/sessions/%s", sid)); err != nil {
			return "", err
		}
		update := map[string]interface{}{}
		for _, key := range []string{"status", "title", "description", "priority", "due_date"} {
			if v, ok := params[key]; ok {
				update[key] = v
			}
		}
		if len(update) > 0 {
			payload, _ := json.Marshal(update)
			if _, err := client.Put(fmt.Sprintf("/api/projects/%d/tasks/%d", task.ProjectID, task.ID), string(payload)); err != nil {
				return "", fmt.Errorf("session stopped, but failed to update linked task: %w", err)
			}
		}
		if status, _ := update["status"].(string); status != "" {
			return fmt.Sprintf("Session %s stopped and linked task #%d (%s) updated to %s", shortID(sid), task.ID, task.Title, status), nil
		}
		return fmt.Sprintf("Session %s stopped and linked task #%d (%s) updated", shortID(sid), task.ID, task.Title), nil

	case "openpoet_create_document":
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
			"session_id":      sessionID,
		}
		if taskID, ok := params["task_id"].(string); ok && taskID != "" {
			docPayload["task_id"] = taskID
		}
		// mission link (Phase 7.1): accept string or number, forward as string —
		// the REST handler parses it like task_id/conversation_id.
		switch mv := params["mission_id"].(type) {
		case string:
			if mv != "" {
				docPayload["mission_id"] = mv
			}
		case float64:
			if mv > 0 {
				docPayload["mission_id"] = fmt.Sprintf("%.0f", mv)
			}
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
			return fmt.Sprintf("Document created successfully. A 'View Document' button was automatically displayed to the user. Do NOT generate links — the user will use the native button. Internal link: %s", result.Link), nil
		}
		return string(body), nil

	// ---- Project custom tools management ----

	case "openpoet_list_project_custom_tools":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		body, err := client.Get(fmt.Sprintf("/api/projects/%d/custom-tools", projectID))
		if err != nil {
			return "", err
		}
		return formatCustomToolsList(body)

	case "openpoet_create_project_custom_tool":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		delete(params, "project_id")
		payload, _ := json.Marshal(params)
		body, err := client.Post(fmt.Sprintf("/api/projects/%d/custom-tools", projectID), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Custom tool created: %s", string(body)), nil

	case "openpoet_update_project_custom_tool":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		delete(params, "project_id")
		delete(params, "id")
		payload, _ := json.Marshal(params)
		body, err := client.Put(fmt.Sprintf("/api/projects/%d/custom-tools/%d", projectID, id), string(payload))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Custom tool updated: %s", string(body)), nil

	case "openpoet_delete_project_custom_tool":
		projectID := getProjectID(params)
		if projectID == 0 {
			return "", fmt.Errorf("project_id is required")
		}
		id, ok := getID(params)
		if !ok {
			return "", fmt.Errorf("id is required")
		}
		_, err := client.Delete(fmt.Sprintf("/api/projects/%d/custom-tools/%d", projectID, id))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Custom tool %d deleted", id), nil

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// executeDashboard returns a compact JSON summary of the entire OpenPoet state.
func executeDashboard(client *APIClient) (string, error) {
	type projectSummary struct {
		ID             int64          `json:"id"`
		Name           string         `json:"name"`
		Type           string         `json:"type"`
		Tasks          map[string]int `json:"tasks"`
		ActiveSessions int            `json:"active_sessions"`
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
			ID       int64 `json:"id"`
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
		if call.Tool == "openpoet_batch" {
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
func formatSessionsList(client *APIClient, body []byte, params map[string]interface{}) (string, error) {
	var sessions []struct {
		ID             string `json:"id"`
		ProjectID      int64  `json:"project_id"`
		Status         string `json:"status"`
		Name           string `json:"name"`
		Backend        string `json:"backend"`
		Model          string `json:"model"`
		RequestedModel string `json:"requested_model"`
		Effort         string `json:"effort"`
		Harness        string `json:"harness"`
		TaskID         *struct {
			Int64 int64 `json:"Int64"`
			Valid bool  `json:"Valid"`
		} `json:"task_id"`
	}
	if err := json.Unmarshal(body, &sessions); err != nil {
		return string(body), nil
	}

	filterStatus, _ := params["status"].(string)
	filterProjectID := int64(0)
	if v, ok := params["project_id"].(float64); ok {
		filterProjectID = int64(v)
	} else if v, ok := params["project_id"].(string); ok && v != "" {
		fmt.Sscanf(v, "%d", &filterProjectID)
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
		task := "none"
		if s.TaskID != nil && s.TaskID.Valid {
			task = fmt.Sprintf("%d", s.TaskID.Int64)
		}
		meta := sessionmeta.WithSessionValues(fetchSessionMetadata(client, s.ProjectID, s.Backend), s.Model, s.Effort, s.Harness)
		requestedModel := s.RequestedModel
		if requestedModel == "" {
			requestedModel = meta.Model
		}
		result += fmt.Sprintf("- %s | %s | project: %d | status: %s | task: %s | model: %s | requested_model: %s | effort: %s | harness: %s\n",
			s.ID, name, s.ProjectID, s.Status, task, meta.Model, requestedModel, meta.Effort, meta.Harness)
	}
	if result == "" {
		return "No sessions matching filter.", nil
	}
	return result, nil
}

func formatSessionDetail(client *APIClient, body []byte) (string, error) {
	var sess struct {
		ID        string `json:"id"`
		ProjectID int64  `json:"project_id"`
		Status    string `json:"status"`
		Name      string `json:"name"`
		TaskID    *struct {
			Int64 int64 `json:"Int64"`
			Valid bool  `json:"Valid"`
		} `json:"task_id"`
		Backend        string `json:"backend"`
		Model          string `json:"model"`
		RequestedModel string `json:"requested_model"`
		Effort         string `json:"effort"`
		Harness        string `json:"harness"`
	}
	if err := json.Unmarshal(body, &sess); err != nil {
		return string(body), nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Session: %s\n", sess.ID))
	sb.WriteString(fmt.Sprintf("Name: %s\nProject ID: %d\nStatus: %s\nBackend: %s\n", sess.Name, sess.ProjectID, sess.Status, sess.Backend))
	meta := sessionmeta.WithSessionValues(fetchSessionMetadata(client, sess.ProjectID, sess.Backend), sess.Model, sess.Effort, sess.Harness)
	requestedModel := sess.RequestedModel
	if requestedModel == "" {
		requestedModel = meta.Model
	}
	sb.WriteString(fmt.Sprintf("Effective model: %s\nRequested model: %s\nEffort: %s\nHarness: %s\n", meta.Model, requestedModel, meta.Effort, meta.Harness))
	if meta.HarnessDetails != "" {
		sb.WriteString(fmt.Sprintf("Harness details: %s\n", meta.HarnessDetails))
	}
	if taskBody, err := client.Get(fmt.Sprintf("/api/sessions/%s/task", sess.ID)); err == nil {
		var task struct {
			ID       int64  `json:"id"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
		}
		if json.Unmarshal(taskBody, &task) == nil {
			sb.WriteString(fmt.Sprintf("Linked Task: #%d %s (%s, %s)\n", task.ID, task.Title, task.Status, task.Priority))
		}
	} else {
		sb.WriteString("Linked Task: none\n")
	}
	return sb.String(), nil
}

func fetchSessionMetadata(client *APIClient, projectID int64, backend string) sessionmeta.Metadata {
	meta := sessionmeta.FromProjectConfig(backend, "")
	if client == nil || projectID <= 0 {
		return meta
	}
	body, err := client.Get(fmt.Sprintf("/api/projects/%d", projectID))
	if err != nil {
		return meta
	}
	var project struct {
		Backend       string `json:"backend"`
		BackendConfig string `json:"backend_config"`
	}
	if err := json.Unmarshal(body, &project); err != nil {
		return meta
	}
	projectBackend := backend
	if strings.TrimSpace(projectBackend) == "" {
		projectBackend = project.Backend
	}
	return sessionmeta.FromProjectConfig(projectBackend, project.BackendConfig)
}

func formatSessionHistory(body []byte) (string, error) {
	var result struct {
		SessionID     string `json:"session_id"`
		Source        string `json:"source"`
		Mode          string `json:"mode"`
		TotalLines    int    `json:"total_lines"`
		ReturnedLines int    `json:"returned_lines"`
		Offset        int    `json:"offset"`
		Truncated     bool   `json:"truncated"`
		Content       string `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return string(body), nil
	}
	truncated := ""
	if result.Truncated {
		truncated = " | truncated"
	}
	return fmt.Sprintf(
		"Session history: %s | source: %s | mode: %s | lines: %d/%d | offset: %d%s\n\n%s",
		result.SessionID,
		result.Source,
		result.Mode,
		result.ReturnedLines,
		result.TotalLines,
		result.Offset,
		truncated,
		result.Content,
	), nil
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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

func formatProjectSkillsList(body []byte) (string, error) {
	var data struct {
		SkillPolicy  string `json:"skill_policy"`
		GlobalSkills []struct {
			ID             int64  `json:"id"`
			Name           string `json:"name"`
			Category       string `json:"category"`
			ProjectEnabled bool   `json:"project_enabled"`
		} `json:"global_skills"`
		ProjectSkills []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Category string `json:"category"`
			Enabled  bool   `json:"enabled"`
		} `json:"project_skills"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return string(body), nil
	}
	var result strings.Builder
	if len(data.GlobalSkills) > 0 {
		result.WriteString("Global skills:\n")
		for _, s := range data.GlobalSkills {
			status := "enabled"
			if !s.ProjectEnabled {
				status = "disabled"
			}
			result.WriteString(fmt.Sprintf("- [%d] %s (%s, %s)\n", s.ID, s.Name, s.Category, status))
		}
	}
	if len(data.ProjectSkills) > 0 {
		result.WriteString("\nProject-specific skills:\n")
		for _, ps := range data.ProjectSkills {
			status := "enabled"
			if !ps.Enabled {
				status = "disabled"
			}
			result.WriteString(fmt.Sprintf("- [%d] %s (%s, %s)\n", ps.ID, ps.Name, ps.Category, status))
		}
	}
	if len(data.GlobalSkills) == 0 && len(data.ProjectSkills) == 0 {
		return "No skills found for this project.", nil
	}
	return result.String(), nil
}

func formatProjectMCPServersList(body []byte) (string, error) {
	var data struct {
		GlobalMCPServers []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Command string `json:"command"`
			Enabled bool   `json:"enabled"`
		} `json:"global_mcp_servers"`
		ProjectMCPServers []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Command string `json:"command"`
			Enabled bool   `json:"enabled"`
		} `json:"project_mcp_servers"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return string(body), nil
	}
	var result strings.Builder
	if len(data.GlobalMCPServers) > 0 {
		result.WriteString("Global MCP servers:\n")
		for _, m := range data.GlobalMCPServers {
			status := "enabled"
			if !m.Enabled {
				status = "disabled"
			}
			result.WriteString(fmt.Sprintf("- [%d] %s: %s (%s)\n", m.ID, m.Name, m.Command, status))
		}
	}
	if len(data.ProjectMCPServers) > 0 {
		result.WriteString("\nProject-specific MCP servers:\n")
		for _, m := range data.ProjectMCPServers {
			status := "enabled"
			if !m.Enabled {
				status = "disabled"
			}
			result.WriteString(fmt.Sprintf("- [%d] %s: %s (%s)\n", m.ID, m.Name, m.Command, status))
		}
	}
	if len(data.GlobalMCPServers) == 0 && len(data.ProjectMCPServers) == 0 {
		return "No MCP servers found for this project.", nil
	}
	return result.String(), nil
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
		ID       int64 `json:"id"`
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
				} else if strings.HasPrefix(d.Title, "Verification") {
					icon = "[V]"
				}
				sb.WriteString(fmt.Sprintf("- %s %s (id: %s, status: %s)\n", icon, d.Title, d.ID, d.Status))
			}
			sb.WriteString("\nUse openpoet_read_document to read a document's content.\n")
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

func formatCustomToolsList(body []byte) (string, error) {
	var tools []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Command     string `json:"command"`
		Enabled     bool   `json:"enabled"`
		Confirm     bool   `json:"confirm"`
	}
	if err := json.Unmarshal(body, &tools); err != nil {
		return string(body), nil
	}
	if len(tools) == 0 {
		return "No custom tools found for this project.", nil
	}
	result := ""
	for _, t := range tools {
		status := "enabled"
		if !t.Enabled {
			status = "disabled"
		}
		confirm := ""
		if t.Confirm {
			confirm = ", requires confirmation"
		}
		result += fmt.Sprintf("- [%d] %s (%s%s): %s\n  Command: %s\n", t.ID, t.Name, status, confirm, t.Description, t.Command)
	}
	return result, nil
}

// getSessionProjectID fetches the project ID for the given session.
func getSessionProjectID(client *APIClient, sessionID string) (int64, error) {
	body, err := client.Get(fmt.Sprintf("/api/sessions/%s", sessionID))
	if err != nil {
		return 0, fmt.Errorf("failed to get session info: %w", err)
	}
	var sess struct {
		ProjectID int64 `json:"project_id"`
	}
	if err := json.Unmarshal(body, &sess); err != nil {
		return 0, fmt.Errorf("failed to parse session: %w", err)
	}
	return sess.ProjectID, nil
}

// verifyShareAccess checks that the session's project has share access to the target project.
func verifyShareAccess(client *APIClient, sessionID string, targetProjectID int64) error {
	projectID, err := getSessionProjectID(client, sessionID)
	if err != nil {
		return err
	}
	body, err := client.Get(fmt.Sprintf("/api/projects/%d/shares", projectID))
	if err != nil {
		return fmt.Errorf("failed to get shares: %w", err)
	}
	var shares []struct {
		ProjectID int64 `json:"project_id"`
	}
	if err := json.Unmarshal(body, &shares); err != nil {
		return fmt.Errorf("failed to parse shares: %w", err)
	}
	for _, s := range shares {
		if s.ProjectID == targetProjectID {
			return nil
		}
	}
	return fmt.Errorf("project %d is not in this project's shared access list", targetProjectID)
}

// collectSharedFiles recursively lists all file paths under a directory in a shared project.
func collectSharedFiles(client *APIClient, projectID int64, dirPath string) ([]string, error) {
	endpoint := fmt.Sprintf("/api/projects/%d/files?path=%s", projectID, dirPath)
	body, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		fullPath := e.Path
		if fullPath == "" {
			if dirPath != "" {
				fullPath = dirPath + "/" + e.Name
			} else {
				fullPath = e.Name
			}
		}
		if e.IsDir {
			sub, err := collectSharedFiles(client, projectID, fullPath)
			if err != nil {
				continue // skip unreadable subdirectories
			}
			files = append(files, sub...)
		} else {
			files = append(files, fullPath)
		}
	}
	return files, nil
}
