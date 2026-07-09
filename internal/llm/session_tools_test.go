package llm

import "testing"

func TestSessionManagementToolsAvailableInChatAndMCP(t *testing.T) {
	wantChat := []string{
		"start_session",
		"stop_session",
		"list_sessions",
		"get_session",
		"read_session_history",
		"send_to_session",
		"link_session_task",
		"unlink_session_task",
		"stop_session_and_update_task",
	}
	chatNames := map[string]bool{}
	for _, tool := range ChatTools() {
		chatNames[tool.Name] = true
	}
	for _, name := range wantChat {
		if !chatNames[name] {
			t.Fatalf("ChatTools missing %q", name)
		}
	}

	wantMCP := []string{
		"openpoet_get_my_task",
		"openpoet_get_session_info",
		"openpoet_request_task_evaluation",
		"openpoet_start_session",
		"openpoet_stop_session",
		"openpoet_list_sessions",
		"openpoet_get_session",
		"openpoet_read_session_history",
		"openpoet_send_to_session",
		"openpoet_link_session_task",
		"openpoet_unlink_session_task",
		"openpoet_stop_session_and_update_task",
	}
	mcpNames := map[string]bool{}
	for _, tool := range MCPTools("chat") {
		mcpNames[tool.Name] = true
	}
	for _, name := range wantMCP {
		if !mcpNames[name] {
			t.Fatalf("MCPTools missing %q", name)
		}
	}
}

func TestSendToSessionToolAllowsPromptAlias(t *testing.T) {
	var sendTool *ToolDefinition
	for _, tool := range ChatTools() {
		if tool.Name == "send_to_session" {
			t := tool
			sendTool = &t
			break
		}
	}
	if sendTool == nil {
		t.Fatal("ChatTools missing send_to_session")
	}
	if _, ok := sendTool.InputSchema.Properties["prompt"]; !ok {
		t.Fatal("send_to_session schema missing prompt alias")
	}
	for _, required := range sendTool.InputSchema.Required {
		if required == "text" {
			t.Fatal("send_to_session should require session_id and accept either text or prompt")
		}
	}
}

func TestStartSessionToolExposesAutoStartTaskPrompt(t *testing.T) {
	var startTool *ToolDefinition
	for _, tool := range ChatTools() {
		if tool.Name == "start_session" {
			t := tool
			startTool = &t
			break
		}
	}
	if startTool == nil {
		t.Fatal("ChatTools missing start_session")
	}
	if _, ok := startTool.InputSchema.Properties["auto_start_task_prompt"]; !ok {
		t.Fatal("start_session schema missing auto_start_task_prompt")
	}
}
