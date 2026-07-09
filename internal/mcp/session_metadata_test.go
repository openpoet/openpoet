package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFormatSessionsListIncludesModelEffortAndHarness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/1" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"backend":"codex","backend_config":"{\"runtime\":\"app-server\",\"model\":\"gpt-5.1\",\"reasoning_effort\":\"high\",\"approval_policy\":\"on-request\",\"sandbox_mode\":\"workspace-write\"}"}`)
	}))
	t.Cleanup(server.Close)

	body := []byte(`[{"id":"session-12345678","project_id":1,"status":"running","name":"Work","backend":"codex","task_id":null}]`)
	got, err := formatSessionsList(NewAPIClient(server.URL), body, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"model: gpt-5.1",
		"effort: high",
		"harness: codex/app-server",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted sessions missing %q:\n%s", want, got)
		}
	}
}

func TestFormatSessionDetailIncludesHarnessDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects/1":
			fmt.Fprint(w, `{"backend":"codex","backend_config":"{\"runtime\":\"tui\",\"model\":\"gpt-5.1\",\"reasoning_effort\":\"medium\",\"approval_policy\":\"never\",\"sandbox_mode\":\"danger-full-access\"}"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	body := []byte(`{"id":"session-12345678","project_id":1,"status":"running","name":"Work","backend":"codex","task_id":null}`)
	got, err := formatSessionDetail(NewAPIClient(server.URL), body)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"Model: gpt-5.1",
		"Effort: medium",
		"Harness: codex/tui",
		"Harness details: runtime: tui | approval: never | sandbox: danger-full-access",
		"Linked Task: none",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted session detail missing %q:\n%s", want, got)
		}
	}
}
