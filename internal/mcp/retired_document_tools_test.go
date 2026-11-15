package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openpoet/internal/llm"
)

// The Docs tools are retired: no agent is offered them. A caller that still
// names one (a session started before, an instruction written before) is
// answered, with a note saying what to use.
func TestRetiredDocumentToolsAreNotListedButStillAnswer(t *testing.T) {
	for _, context := range []string{"session", "http", "chat"} {
		for _, tool := range toolsDef(context) {
			if RetiredDocumentTools[strings.TrimPrefix(tool.Name, "openpoet_")] {
				t.Fatalf("%s is listed to %s clients", tool.Name, context)
			}
		}
	}
	for _, tool := range llm.ChatTools() {
		if RetiredDocumentTools[tool.Name] {
			t.Fatalf("%s is listed to the chat assistant", tool.Name)
		}
	}
	// A policy that allows everything, which is the default, lets the call through.
	for _, name := range []string{"openpoet_create_document", "openpoet_read_document"} {
		if !(ToolPolicy{Mode: "allow_all"}).IsAllowed(name) {
			t.Fatalf("an allow-all policy refuses %s", name)
		}
	}

	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/documents":
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"ab12cd34","link":"/app/doc/ab12cd34","url":""}`)
		case r.URL.Path == "/api/documents/ab12cd34":
			fmt.Fprint(w, `{"id":"ab12cd34","title":"Notes","content":"# Notes","status":"","artifact_id":"ab12cd34","format":"markdown"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewAPIClient(server.URL)

	created, err := executeTool(client, "openpoet_create_document", json.RawMessage(`{"title":"Notes","content":"# Notes"}`), "sess-1", "")
	if err != nil || posted["content"] != "# Notes" || posted["session_id"] != "sess-1" {
		t.Fatalf("create_document: %q err=%v posted=%v", created, err, posted)
	}
	for _, want := range []string{"Internal link: /app/doc/ab12cd34", "openpoet_create_document is retired", "openpoet_create_artifact with the text in markdown"} {
		if !strings.Contains(created, want) {
			t.Fatalf("create_document result lacks %q: %s", want, created)
		}
	}
	read, err := executeTool(client, "openpoet_read_document", json.RawMessage(`{"document_id":"ab12cd34"}`), "sess-1", "")
	if err != nil || !strings.Contains(read, "## Notes") || !strings.Contains(read, "| Artifact: ab12cd34") || !strings.Contains(read, "openpoet_read_document is retired") || !strings.Contains(read, "openpoet_read_artifact, which takes the same id") {
		t.Fatalf("read_document: %q err=%v", read, err)
	}
	// The chat names carry no prefix.
	if note := RetiredDocumentToolNote("create_document", ""); !strings.Contains(note, "call create_artifact with the text in markdown") || strings.Contains(note, "openpoet_") {
		t.Fatalf("chat note = %q", note)
	}
}

// read_artifact reads every document a listing names: a session plan, and the
// id of a document that got another id when it became an artifact.
func TestReadArtifactReadsPlansAndDocumentIDs(t *testing.T) {
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/api/sessions/sess-9/plan":
			fmt.Fprint(w, `{"plan_content":"1. Do the thing","updated_at":"2026-01-02T03:04:05Z"}`)
		case "/api/documents/olddoc01":
			fmt.Fprint(w, `{"id":"olddoc01","title":"Old","content":"old","status":"","artifact_id":"new00001"}`)
		case "/api/artifacts/new00001":
			fmt.Fprint(w, `{"id":"new00001","title":"Old","status":"active","current_version":1,"format":"markdown","size_bytes":3,"versions":[{"version":1,"format":"markdown","size_bytes":3,"sha256":"abc","created_at":"2026-01-01"}]}`)
		case "/api/artifacts/new00001/annotations", "/api/artifacts/new00001/questions":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewAPIClient(server.URL)

	plan, err := executeTool(client, "openpoet_read_artifact", json.RawMessage(`{"artifact_id":"plan:sess-9"}`), "sess-1", "")
	if err != nil || plan != "## Session Plan\n\n1. Do the thing" {
		t.Fatalf("plan = %q err=%v", plan, err)
	}
	old, err := executeTool(client, "openpoet_read_artifact", json.RawMessage(`{"artifact_id":"olddoc01"}`), "sess-1", "")
	if err != nil || !strings.Contains(old, "## Artifact new00001 — Old") || !strings.Contains(old, "| Format: markdown") {
		t.Fatalf("document id = %q err=%v (requested %v)", old, err, requested)
	}
	if _, err := executeTool(client, "openpoet_read_artifact", json.RawMessage(`{"artifact_id":"missing0"}`), "sess-1", ""); err == nil {
		t.Fatal("an id that is neither an artifact nor a document was read")
	}
}
