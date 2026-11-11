package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openpoet/internal/llm"
)

func TestGetLinkToolIsRegisteredForSessionsAndChat(t *testing.T) {
	var found *MCPTool
	for _, tool := range toolsDef("session") {
		if tool.Name == "openpoet_get_link" {
			tool := tool
			found = &tool
		}
	}
	if found == nil {
		t.Fatal("session MCP tools lack openpoet_get_link")
	}
	for _, phrase := range []string{"never build a URL yourself", "localhost", "artifact", "Doc", "session", "task"} {
		if !strings.Contains(found.Description, phrase) {
			t.Fatalf("get_link description lacks %q: %s", phrase, found.Description)
		}
	}
	chat := false
	for _, tool := range llm.ChatTools() {
		chat = chat || tool.Name == "get_link"
	}
	if !chat {
		t.Fatal("chat tools lack get_link")
	}
}

func TestGetLinkToolAsksTheServerAndExplainsTheLink(t *testing.T) {
	var requested []string
	configured := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("id") == "missing0" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Artifact not found","code":"artifact_not_found"}`))
			return
		}
		if configured {
			_, _ = w.Write([]byte(`{"kind":"artifact","id":"ab12cd34","title":"Rollout\nplan","path":"/app/artifact/ab12cd34","url":"https://openpoet.example.com/app/artifact/ab12cd34","local_url":"http://localhost:8080/app/artifact/ab12cd34","public":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"kind":"session","id":"sess-1","title":"worker","path":"/app/session/sess-1","local_url":"http://localhost:8080/app/session/sess-1","public":false}`))
	}))
	defer server.Close()
	client := NewAPIClient(server.URL)
	call := func(args string) (string, error) {
		return executeTool(client, "openpoet_get_link", []byte(args), "sess-1", "")
	}

	result, err := call(`{"kind":"Artifact","id":"ab12cd34","version":2}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		`Link to artifact ab12cd34 "Rollout plan": https://openpoet.example.com/app/artifact/ab12cd34`,
		"the artifact viewer", "Pass on this exact link",
	} {
		if !strings.Contains(result, phrase) {
			t.Fatalf("result lacks %q: %s", phrase, result)
		}
	}
	if strings.Contains(result, "localhost") {
		t.Fatalf("a public link result mentions localhost: %s", result)
	}
	if got := requested[len(requested)-1]; got != "/api/links?id=ab12cd34&kind=artifact&version=2" {
		t.Fatalf("request = %s", got)
	}

	// Not configured: the tool says so instead of handing out localhost as if
	// it were a link. A session without an id means "this session".
	configured = false
	result, err = call(`{"kind":"session"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"No public address is configured", "Public base URL", "Local link (only on that machine): http://localhost:8080/app/session/sess-1",
		"In-app path: /app/session/sess-1", "Do not present either as a link",
	} {
		if !strings.Contains(result, phrase) {
			t.Fatalf("result lacks %q: %s", phrase, result)
		}
	}
	if got := requested[len(requested)-1]; got != "/api/links?id=sess-1&kind=session" {
		t.Fatalf("request = %s", got)
	}

	if _, err = call(`{"id":"ab12cd34"}`); err == nil || !strings.Contains(err.Error(), "kind is required") {
		t.Fatalf("missing kind err = %v", err)
	}
	if _, err = call(`{"kind":"artifact"}`); err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("missing id err = %v", err)
	}
	if _, err = call(`{"kind":"artifact","id":"missing0"}`); err == nil || err.Error() != "Artifact not found" {
		t.Fatalf("not found err = %v", err)
	}
}

func TestPublishResultsNameTheShareableLink(t *testing.T) {
	withURL := FormatArtifactPublished([]byte(`{"id":"ab12cd34","version":1,"created":true,"link":"/app/artifact/ab12cd34","url":"https://openpoet.example.com/app/artifact/ab12cd34","artifact":{"id":"ab12cd34","size_bytes":10}}`), "openpoet_")
	for _, phrase := range []string{"Internal link: /app/artifact/ab12cd34", "Shareable link, if the user asks for one to pass on: https://openpoet.example.com/app/artifact/ab12cd34", "never build a URL yourself"} {
		if !strings.Contains(withURL, phrase) {
			t.Fatalf("published result lacks %q: %s", phrase, withURL)
		}
	}
	without := FormatArtifactPublished([]byte(`{"id":"ab12cd34","version":1,"created":true,"link":"/app/artifact/ab12cd34","artifact":{"id":"ab12cd34","size_bytes":10}}`), "")
	if !strings.Contains(without, "call get_link; never build a URL yourself") || strings.Contains(without, "Shareable link") {
		t.Fatalf("published result without a public base = %s", without)
	}
}
