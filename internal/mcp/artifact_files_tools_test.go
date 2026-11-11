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

const artifactTestSHA = "0611d38f3ff6d90a2922e48b7d0260cadb28316b67348077e2deb341b8136dd2"

func TestArtifactToolsPublishFromAFilePatchAndExport(t *testing.T) {
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.RequestURI()
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			decoded := map[string]any{}
			_ = json.Unmarshal(raw, &decoded)
			bodies[key] = decoded
		}
		publish := func(version int, extra string) {
			fmt.Fprintf(w, `{"id":"ab12cd34","version":%d,"created":true,"size_bytes":170213,"sha256":%q,"link":"/app/artifact/ab12cd34","url":"https://poet.example.com/app/artifact/ab12cd34"%s,"artifact":{"id":"ab12cd34"}}`, version, artifactTestSHA, extra)
		}
		switch key {
		case "POST /api/artifacts":
			w.WriteHeader(http.StatusCreated)
			publish(1, `,"source_path":"/home/dev/app/plan.html"`)
		case "PUT /api/artifacts/ab12cd34":
			publish(2, `,"source_path":"/home/dev/app/plan.html"`)
		case "POST /api/artifacts/ab12cd34/patch":
			publish(3, `,"replacements":2`)
		case "POST /api/artifacts/ab12cd34/versions/current/export", "POST /api/artifacts/ab12cd34/versions/2/export":
			fmt.Fprintf(w, `{"artifact_id":"ab12cd34","version":3,"path":"/home/dev/app/current.html","size_bytes":170213,"sha256":%q}`, artifactTestSHA)
		case "GET /api/artifacts/ab12cd34":
			fmt.Fprintf(w, `{"id":"ab12cd34","title":"Plan","status":"active","current_version":3,"size_bytes":170213,"link":"/app/artifact/ab12cd34","url":"https://poet.example.com/app/artifact/ab12cd34","versions":[{"version":3,"size_bytes":170213,"sha256":%q,"created_at":"2026-11-10T10:00:00Z"}]}`, artifactTestSHA)
		case "GET /api/artifacts/ab12cd34/versions/current":
			fmt.Fprintf(w, `{"artifact_id":"ab12cd34","version":3,"size_bytes":28,"sha256":"e9c7f4c5a6e9b0c9e9c6a4a3f0b7b0f0b1f0c2d3e4f5a6b7c8d9e0f1a2b3c4d5","content":"<html><body>v3</body></html>"}`)
		case "GET /api/artifacts/ab12cd34/annotations?status=open", "GET /api/artifacts/ab12cd34/annotations?placement=1&status=open":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewAPIClient(server.URL)
	call := func(name, args string) (string, error) {
		return executeTool(client, name, json.RawMessage(args), "sess-1", "")
	}
	integrity := "170213 bytes (166.2 KiB), sha256 " + artifactTestSHA + ", stored byte for byte as received"
	shareable := "Shareable link, if the user asks for one to pass on: https://poet.example.com/app/artifact/ab12cd34"

	// create from a file: the path travels, the document does not.
	result, err := call("openpoet_create_artifact", `{"title":"Plan","html_path":"plan.html"}`)
	if err != nil || !strings.Contains(result, integrity) || !strings.Contains(result, "Read from /home/dev/app/plan.html.") || !strings.Contains(result, shareable) {
		t.Fatalf("create from file = %q err=%v", result, err)
	}
	if body := bodies["POST /api/artifacts"]; body["html_path"] != "plan.html" || body["html"] != "" || body["session_id"] != "sess-1" {
		t.Fatalf("create payload = %v", body)
	}

	// update: exact size, hash and the links are in the result.
	result, err = call("openpoet_update_artifact", `{"artifact_id":"ab12cd34","html_path":"plan.html","base_version":"1"}`)
	if err != nil || !strings.Contains(result, "is now at version 2 ("+integrity+")") || !strings.Contains(result, "Internal link: /app/artifact/ab12cd34") || !strings.Contains(result, shareable) {
		t.Fatalf("update from file = %q err=%v", result, err)
	}
	if body := bodies["PUT /api/artifacts/ab12cd34"]; body["html_path"] != "plan.html" || body["base_version"] != "1" {
		t.Fatalf("update payload = %v", body)
	}
	for name, args := range map[string]string{
		"nothing":      `{"artifact_id":"ab12cd34"}`,
		"two sources":  `{"artifact_id":"ab12cd34","html":"<p>x</p>","html_path":"plan.html"}`,
		"cli-only arg": `{"artifact_id":"ab12cd34","html_file":"/tmp/plan.html"}`,
	} {
		if _, err := call("openpoet_update_artifact", args); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}

	// patch: the edits travel as given; base_version is required.
	result, err = call("openpoet_patch_artifact", `{"artifact_id":"ab12cd34","base_version":2,"change_note":"fix","edits":[{"old":"<h1>A</h1>","new":"<h1>B</h1>"},{"old":"x","new":"","replace_all":true}]}`)
	if err != nil || !strings.Contains(result, "is now at version 3 (170213 bytes (166.2 KiB), sha256 "+artifactTestSHA+"). 2 replacement(s) applied to version 2.") || !strings.Contains(result, shareable) {
		t.Fatalf("patch = %q err=%v", result, err)
	}
	body := bodies["POST /api/artifacts/ab12cd34/patch"]
	edits, _ := body["edits"].([]any)
	if body["base_version"] != "2" || body["change_note"] != "fix" || len(edits) != 2 ||
		edits[0].(map[string]any)["old"] != "<h1>A</h1>" || edits[1].(map[string]any)["new"] != "" || edits[1].(map[string]any)["replace_all"] != true {
		t.Fatalf("patch payload = %v", body)
	}
	// Edits sent as a JSON string are accepted too.
	if _, err := call("openpoet_patch_artifact", `{"artifact_id":"ab12cd34","base_version":"2","edits":"[{\"old\":\"a\",\"new\":\"b\"}]"}`); err != nil {
		t.Fatalf("edits as a JSON string: %v", err)
	}
	for name, args := range map[string]string{
		"no base":     `{"artifact_id":"ab12cd34","edits":[{"old":"a","new":"b"}]}`,
		"no edits":    `{"artifact_id":"ab12cd34","base_version":2}`,
		"missing new": `{"artifact_id":"ab12cd34","base_version":2,"edits":[{"old":"a"}]}`,
		"not a list":  `{"artifact_id":"ab12cd34","base_version":2,"edits":"nope"}`,
	} {
		if _, err := call("openpoet_patch_artifact", args); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}

	// read: the link and the hash of each version; the HTML goes to a file.
	result, err = call("openpoet_read_artifact", `{"artifact_id":"ab12cd34"}`)
	if err != nil || !strings.Contains(result, "| sha256: "+artifactTestSHA) || !strings.Contains(result, "Internal link: /app/artifact/ab12cd34") ||
		!strings.Contains(result, shareable) || !strings.Contains(result, "- v3 (170213 bytes (166.2 KiB), 2026-11-10T10:00:00Z, sha256 "+artifactTestSHA+")") ||
		!strings.Contains(result, "output_path") {
		t.Fatalf("read = %q err=%v", result, err)
	}
	result, err = call("openpoet_read_artifact", `{"artifact_id":"ab12cd34","output_path":"current.html"}`)
	if err != nil || !strings.Contains(result, "written to /home/dev/app/current.html (170213 bytes (166.2 KiB), sha256 "+artifactTestSHA+")") ||
		!strings.Contains(result, "base_version 3") || strings.Contains(result, "<html>") {
		t.Fatalf("read to file = %q err=%v", result, err)
	}
	if body := bodies["POST /api/artifacts/ab12cd34/versions/current/export"]; body["path"] != "current.html" || body["session_id"] != "sess-1" {
		t.Fatalf("export payload = %v", body)
	}
	if _, err := call("openpoet_read_artifact", `{"artifact_id":"ab12cd34","version":"2","output_path":"v2.html"}`); err != nil || bodies["POST /api/artifacts/ab12cd34/versions/2/export"] == nil {
		t.Fatalf("export of a given version: %v", err)
	}
	result, err = call("openpoet_read_artifact", `{"artifact_id":"ab12cd34","include_html":true}`)
	if err != nil || !strings.Contains(result, "### HTML of version 3 (28 bytes, sha256 e9c7") || !strings.Contains(result, "<html><body>v3</body></html>") {
		t.Fatalf("read with html = %q err=%v", result, err)
	}

	// output_file is the CLI's: the MCP server does not write a local path.
	local := filepath.Join(t.TempDir(), "saved.html")
	args, _ := json.Marshal(map[string]string{"artifact_id": "ab12cd34", "output_file": local})
	if _, err := executeTool(client, "openpoet_read_artifact", args, "sess-1", ""); err == nil || !strings.Contains(err.Error(), "only available through the openpoet CLI") {
		t.Fatalf("MCP server honored output_file: %v", err)
	}
	if _, statErr := os.Stat(local); statErr == nil {
		t.Fatal("the MCP server wrote a local file")
	}
	// In the CLI the stored hash is checked against what was written.
	if _, err := executeToolWithOptions(client, "openpoet_read_artifact", args, "sess-1", "", toolExecutionOptions{LocalFiles: true}); err == nil || !strings.Contains(err.Error(), "but the stored version has") {
		t.Fatalf("a hash mismatch was not reported: %v", err)
	}
}

func TestArtifactCLISavesTheHTMLOfAVersion(t *testing.T) {
	const html = "<html><body><h1>Plan</h1></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"artifact_id":"ab12cd34","version":4,"sha256":%q,"content":%q}`, artifactTestSHA, html)
	}))
	t.Cleanup(server.Close)
	local := filepath.Join(t.TempDir(), "saved.html")
	args, _ := json.Marshal(map[string]string{"artifact_id": "ab12cd34", "output_file": local})
	result, err := executeToolWithOptions(NewAPIClient(server.URL), "openpoet_read_artifact", args, "", "", toolExecutionOptions{LocalFiles: true})
	if err != nil || !strings.Contains(result, "version 4") || !strings.Contains(result, "39 bytes, sha256 "+artifactTestSHA) {
		t.Fatalf("result = %q err=%v", result, err)
	}
	if data, err := os.ReadFile(local); err != nil || string(data) != html {
		t.Fatalf("saved file = %q err=%v", data, err)
	}
}

func TestArtifactUpdateWithIdenticalHTMLStillReportsHashAndLink(t *testing.T) {
	body := fmt.Sprintf(`{"id":"ab12cd34","version":3,"created":false,"size_bytes":39,"sha256":%q,"link":"/app/artifact/ab12cd34","artifact":{"id":"ab12cd34"}}`, artifactTestSHA)
	result := FormatArtifactUpdated([]byte(body), 0, "openpoet_")
	if !strings.Contains(result, "No new version") || !strings.Contains(result, "39 bytes, sha256 "+artifactTestSHA) ||
		!strings.Contains(result, "Internal link: /app/artifact/ab12cd34") || !strings.Contains(result, "call openpoet_get_link") {
		t.Fatalf("result = %q", result)
	}
	masked := fmt.Sprintf(`{"id":"ab12cd34","version":4,"created":true,"redacted":true,"size_bytes":39,"sha256":%q,"link":"/app/artifact/ab12cd34","artifact":{"id":"ab12cd34"}}`, artifactTestSHA)
	if result := FormatArtifactUpdated([]byte(masked), 0, ""); !strings.Contains(result, "not the hash of what you sent") || strings.Contains(result, "byte for byte") {
		t.Fatalf("masked result = %q", result)
	}
}
