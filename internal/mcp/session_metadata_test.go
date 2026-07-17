package mcp

import (
	"encoding/json"
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

func TestFormatSessionsListPrefersPersistedRuntimeMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"backend":"codex","backend_config":"{\"runtime\":\"tui\",\"model\":\"gpt-project\",\"reasoning_effort\":\"low\"}"}`)
	}))
	t.Cleanup(server.Close)

	body := []byte(`[{"id":"session-12345678","project_id":1,"status":"running","name":"Work","backend":"codex","model":"gpt-session","effort":"high","harness":"codex/app-server","task_id":null}]`)
	got, err := formatSessionsList(NewAPIClient(server.URL), body, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"model: gpt-session", "effort: high", "harness: codex/app-server"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted sessions missing %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{"gpt-project", "effort: low", "harness: codex/tui"} {
		if strings.Contains(got, stale) {
			t.Fatalf("formatted sessions contains stale project metadata %q:\n%s", stale, got)
		}
	}
}

func TestFormatSessionsListDistinguishesRequestedAndEffectiveModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"backend":"claude_code","backend_config":"{}"}`)
	}))
	t.Cleanup(server.Close)

	body := []byte(`[{"id":"session-12345678","project_id":1,"status":"running","name":"Work","backend":"claude_code","model":"claude-fable-5","requested_model":"fable","effort":"xhigh","harness":"claude_code","task_id":null}]`)
	got, err := formatSessionsList(NewAPIClient(server.URL), body, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"model: claude-fable-5", "requested_model: fable", "harness: claude_code"} {
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
		"Effective model: gpt-5.1",
		"Requested model: gpt-5.1",
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

func TestSetSessionToolsCallRuntimeSettingEndpoints(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/api/sessions/session-12345678/model":
			if input["model"] != "fable" {
				t.Fatalf("model payload = %#v", input)
			}
			fmt.Fprint(w, `{"model":"fable","effort":"medium","harness":"claude_code"}`)
		case "/api/sessions/session-12345678/effort":
			if input["effort"] != "max" {
				t.Fatalf("effort payload = %#v", input)
			}
			fmt.Fprint(w, `{"model":"fable","effort":"max","harness":"claude_code"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewAPIClient(server.URL)

	modelResult, err := executeTool(client, "openpoet_set_session_model", json.RawMessage(`{"session_id":"session-12345678","model":"fable"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(modelResult, "model changed to fable") {
		t.Fatalf("model result = %q", modelResult)
	}
	effortResult, err := executeTool(client, "openpoet_set_session_effort", json.RawMessage(`{"session_id":"session-12345678","effort":"max"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(effortResult, "effort changed to max") {
		t.Fatalf("effort result = %q", effortResult)
	}
	wantCalls := []string{
		"POST /api/sessions/session-12345678/model",
		"POST /api/sessions/session-12345678/effort",
	}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}
