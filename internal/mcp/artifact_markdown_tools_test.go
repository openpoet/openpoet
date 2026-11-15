package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactMarkdownFileIsHonoredOnlyByTheCLI(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received = map[string]any{}
		_ = json.Unmarshal(raw, &received)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"ab12cd34","version":1,"created":true,"format":"markdown","link":"/app/artifact/ab12cd34","artifact":{"id":"ab12cd34"}}`)
	}))
	t.Cleanup(server.Close)
	client := NewAPIClient(server.URL)
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("# Notes\n\nfrom file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"title": "Notes", "markdown_file": path})

	if _, err := executeTool(client, "openpoet_create_artifact", args, "", ""); err == nil || !strings.Contains(err.Error(), "markdown_file is only available through the openpoet CLI") || received != nil {
		t.Fatalf("MCP server honored markdown_file: err=%v received=%v", err, received)
	}
	result, err := executeToolWithOptions(client, "openpoet_create_artifact", args, "", "", toolExecutionOptions{LocalFiles: true})
	if err != nil || received["markdown"] != "# Notes\n\nfrom file\n" || received["html"] != nil {
		t.Fatalf("CLI publish from a markdown file: err=%v received=%v", err, received)
	}
	// The result speaks of what was published.
	if !strings.Contains(result, "do NOT paste the markdown") || !strings.Contains(result, "update_artifact (the whole markdown)") {
		t.Fatalf("result of a markdown publish = %q", result)
	}
}

// read_document names the artifact a document id stands for, which is what
// the artifact tools take.
func TestReadDocumentNamesTheArtifactADocumentIDStandsFor(t *testing.T) {
	document := FormatDocument([]byte(`{"id":"ab12cd34","title":"Verification: Ship it","content":"# Verification\n","status":"pending","artifact_id":"ab12cd34","format":"markdown"}`))
	if document != "## Verification: Ship it\n\nStatus: pending | ID: ab12cd34 | Artifact: ab12cd34\n\n# Verification\n" {
		t.Fatalf("artifact-backed document = %q", document)
	}
	doc := FormatDocument([]byte(`{"id":"0f1e2d3c","title":"Notes","content":"text","status":"pending"}`))
	if doc != "## Notes\n\nStatus: pending | ID: 0f1e2d3c\n\ntext" {
		t.Fatalf("Doc = %q", doc)
	}
	if raw := FormatDocument([]byte(`not json`)); raw != "not json" {
		t.Fatalf("unreadable body = %q", raw)
	}
}

func TestArtifactToolResultsNameMarkdownVersions(t *testing.T) {
	updated := FormatArtifactUpdated([]byte(`{"id":"ab12cd34","version":2,"created":false,"format":"markdown","link":"/app/artifact/ab12cd34","artifact":{"id":"ab12cd34"}}`), 0, "openpoet_")
	if !strings.Contains(updated, "No new version: the markdown is identical to version 2") {
		t.Fatalf("identical markdown update = %q", updated)
	}
	unchanged := FormatArtifactUpdated([]byte(`{"id":"ab12cd34","version":2,"created":false,"format":"html","link":"/app/artifact/ab12cd34","artifact":{"id":"ab12cd34"}}`), 0, "openpoet_")
	if !strings.Contains(unchanged, "No new version: the HTML is identical to version 2") {
		t.Fatalf("identical HTML update = %q", unchanged)
	}

	exported := FormatArtifactExported([]byte(`{"artifact_id":"ab12cd34","version":3,"path":"/home/dev/app/notes.md","format":"markdown","size_bytes":20,"sha256":"abc"}`), "openpoet_")
	if !strings.HasPrefix(exported, "Markdown of version 3 of artifact ab12cd34 written to /home/dev/app/notes.md") || !strings.Contains(exported, "openpoet_update_artifact (markdown_path, base_version 3)") {
		t.Fatalf("markdown export = %q", exported)
	}
	page := FormatArtifactExported([]byte(`{"artifact_id":"ab12cd34","version":3,"path":"/home/dev/app/plan.html","format":"html","size_bytes":20,"sha256":"abc"}`), "openpoet_")
	if !strings.HasPrefix(page, "HTML of version 3") || !strings.Contains(page, "(html_path, base_version 3)") {
		t.Fatalf("HTML export = %q", page)
	}

	artifact := `{"id":"ab12cd34","title":"Notes","status":"active","current_version":2,"format":"markdown","size_bytes":20,
		"versions":[{"version":2,"format":"markdown","size_bytes":20,"sha256":"abc","created_at":"2026-01-02"},{"version":1,"format":"html","size_bytes":90,"sha256":"def","created_at":"2026-01-01"}]}`
	detail := FormatArtifactDetail([]byte(artifact), nil, nil, "openpoet_", nil)
	for _, want := range []string{"| Format: markdown", "- v2 (20 bytes, 2026-01-02, markdown, sha256 abc)", "- v1 (90 bytes, 2026-01-01, sha256 def)", "To get the markdown of a version", ".md file"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	withContent := FormatArtifactDetail([]byte(artifact), nil, []byte(`{"version":2,"format":"markdown","content":"# Notes\n","size_bytes":8,"sha256":"abc"}`), "openpoet_", nil)
	if !strings.Contains(withContent, "### Markdown of version 2 (8 bytes, sha256 abc)\n\n# Notes\n") {
		t.Fatalf("detail with content = %q", withContent)
	}
}
