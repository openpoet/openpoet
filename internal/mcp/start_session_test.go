package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartSessionToolForwardsProgrammaticStartOptions(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions" {
			t.Fatalf("request = %s %s, want POST /api/sessions", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"id":"session-1","project_id":7,"status":"running"}`)
	}))
	t.Cleanup(server.Close)

	result, err := executeTool(
		NewAPIClient(server.URL),
		"openpoet_start_session",
		json.RawMessage(`{"project_id":"7","task_id":"9","auto_start_task_prompt":false,"custom_prompt":"Start with the regression test."}`),
		"",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if payload["project_id"] != float64(7) || payload["task_id"] != float64(9) {
		t.Fatalf("numeric IDs were not normalized: %#v", payload)
	}
	if payload["auto_start_task_prompt"] != true || payload["custom_prompt"] != "Start with the regression test." {
		t.Fatalf("programmatic start options were not forwarded: %#v", payload)
	}
	if result != "Session started: session-1 (project: 7, status: running)" {
		t.Fatalf("result = %q", result)
	}
}

func TestStartSessionToolSchemaExposesPlanningAndCustomPrompts(t *testing.T) {
	for _, tool := range AllTools() {
		if tool.Name != "openpoet_start_session" {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"planning_mode", "custom_prompt"} {
			if _, exists := schema.Properties[name]; !exists {
				t.Fatalf("start_session schema is missing %q", name)
			}
		}
		return
	}
	t.Fatal("openpoet_start_session tool not found")
}
