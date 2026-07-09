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
