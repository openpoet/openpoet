package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

const artifactHandlerMarkdown = "# Release notes\n\nFirst line\nsecond line with **bold** text.\n\n- [x] shipped\n\n[The plan](/app/artifact/ab12cd34)\n"

func renderArtifactForTest(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestMarkdownArtifactIsRenderedIntoAPageInsideTheSandbox(t *testing.T) {
	_, router := artifactTestRouter(t)
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts",
		map[string]any{"title": "Notes <1>", "markdown": artifactHandlerMarkdown}, nil)
	if recorder.Code != http.StatusCreated || created["format"] != "markdown" || created["size_bytes"] != float64(len(artifactHandlerMarkdown)) {
		t.Fatalf("create = %d %v", recorder.Code, created)
	}
	id := created["id"].(string)

	// The JSON endpoints give the source back, never the rendered page.
	if recorder, version := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id+"/versions/current", nil, nil); recorder.Code != http.StatusOK ||
		version["content"] != artifactHandlerMarkdown || version["format"] != "markdown" {
		t.Fatalf("version = %d %v", recorder.Code, version)
	}
	if recorder, detail := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id, nil, nil); recorder.Code != http.StatusOK ||
		detail["format"] != "markdown" || strings.Contains(recorder.Body.String(), "<strong>") {
		t.Fatalf("detail = %d %s", recorder.Code, recorder.Body.String())
	}

	rendered := renderArtifactForTest(t, router, "/api/artifacts/"+id+"/render")
	body := rendered.Body.String()
	if rendered.Code != http.StatusOK || !strings.HasPrefix(rendered.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("render = %d %s", rendered.Code, rendered.Header().Get("Content-Type"))
	}
	if csp := rendered.Header().Get("Content-Security-Policy"); csp != artifactContentSecurityPolicy || strings.Contains(csp, "allow-modals") {
		t.Fatalf("a markdown artifact is served under another policy: %s", csp)
	}
	for _, want := range []string{
		"<!doctype html>", "<title>Notes &lt;1&gt;</title>", `<main id="openpoet-markdown">`,
		`<h1 id="release-notes">Release notes</h1>`, "First line<br>", "<strong>bold</strong>",
		`<a href="/app/artifact/ab12cd34">The plan</a>`, "<style data-openpoet-markdown>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page lacks %q", want)
		}
	}
	// Standards mode, with the guard right after the doctype and the bridge last.
	if !strings.HasPrefix(body, "<!doctype html>\n<script data-openpoet-guard>") || strings.Count(body, "<script data-openpoet-bridge>") != 1 {
		t.Fatalf("guard and bridge are not where they belong: %.120q", body)
	}
	// No mermaid block: the library does not travel.
	if strings.Contains(body, "data-openpoet-mermaid") || len(body) > 200<<10 {
		t.Fatalf("a document without diagrams carries the mermaid library (%d bytes)", len(body))
	}
}

func TestMarkdownArtifactCarriesMermaidOnlyWhenItHasADiagram(t *testing.T) {
	_, router := artifactTestRouter(t)
	source := "# Flow\n\n```mermaid\ngraph TD; A-->B;\n```\n"
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Flow", "markdown": source}, nil)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", recorder.Code, recorder.Body.String())
	}
	body := renderArtifactForTest(t, router, "/api/artifacts/"+created["id"].(string)+"/render").Body.String()
	library := strings.Index(body, "<script data-openpoet-mermaid-library>")
	step := strings.Index(body, "<script data-openpoet-mermaid>")
	bridge := strings.Index(body, "<script data-openpoet-bridge>")
	if library < 0 || step < library || bridge < step {
		t.Fatalf("mermaid library, its step and the bridge are out of order: %d %d %d", library, step, bridge)
	}
	if !strings.Contains(body, `<pre><code class="language-mermaid">graph TD; A--&gt;B;`) {
		t.Fatal("the diagram source is not in the page for the step to draw")
	}
	// The library is one inline script: nothing in it ends the element early.
	inline := body[library:step]
	if strings.Count(strings.ToLower(inline), "</script") != 1 {
		t.Fatalf("the inlined library closes its script element %d times", strings.Count(strings.ToLower(inline), "</script"))
	}
}

func TestArtifactPrintRenderAloneMayOpenThePrintDialog(t *testing.T) {
	_, router := artifactTestRouter(t)
	ids := map[string]string{}
	for name, body := range map[string]map[string]any{
		"html":     {"title": "Page", "html": artifactHandlerHTML},
		"markdown": {"title": "Notes", "markdown": artifactHandlerMarkdown},
	} {
		recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", body, nil)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("%s: create = %d", name, recorder.Code)
		}
		ids[name] = created["id"].(string)
	}
	for name, id := range ids {
		shown := renderArtifactForTest(t, router, "/api/artifacts/"+id+"/render?v=1")
		if csp := shown.Header().Get("Content-Security-Policy"); strings.Contains(csp, "allow-modals") {
			t.Fatalf("%s: the frame the artifact is read in may open dialogs: %s", name, csp)
		}
		if strings.Contains(shown.Body.String(), "data-openpoet-print") {
			t.Fatalf("%s: the print step is in the page the artifact is read in", name)
		}

		printed := renderArtifactForTest(t, router, "/api/artifacts/"+id+"/render?v=1&print=1")
		csp := printed.Header().Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "sandbox allow-scripts allow-modals; ") || strings.TrimPrefix(csp, "sandbox allow-scripts allow-modals; ") != strings.TrimPrefix(artifactContentSecurityPolicy, "sandbox allow-scripts; ") {
			t.Fatalf("%s: print policy = %s", name, csp)
		}
		body := printed.Body.String()
		if strings.Count(body, "<script data-openpoet-print>") != 1 || strings.Contains(body, "<script data-openpoet-bridge>") || strings.Count(body, "<script data-openpoet-guard>") != 1 {
			t.Fatalf("%s: a page to print carries the print step in place of the bridge, and the guard", name)
		}
	}
	// A markdown page being printed says so, for its diagrams and its colors.
	if body := renderArtifactForTest(t, router, "/api/artifacts/"+ids["markdown"]+"/render?print=1").Body.String(); !strings.Contains(body, `<html lang="en" data-openpoet-print>`) {
		t.Fatal("the markdown page to print is not marked as such")
	}
	// Errors under the route keep a sandboxed policy too.
	if missing := renderArtifactForTest(t, router, "/api/artifacts/missing0/render?print=1"); missing.Code != http.StatusNotFound || !strings.HasPrefix(missing.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("missing artifact = %d %s", missing.Code, missing.Header().Get("Content-Security-Policy"))
	}
}

// The chat assistant has no workspace: it publishes markdown inline, and reads
// it back, through the same tools.
func TestArtifactChatToolsPublishMarkdown(t *testing.T) {
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "user", ID: "ui"}, Approved: true, ApprovedBy: "ui", Reason: "test"}
	call := func(name string, input map[string]interface{}) (string, error) {
		return services.AIHandler.executeTool(ctx, name, input, 0, 0, nil, "", auth)
	}

	published, err := call("create_artifact", map[string]interface{}{"title": "Chat notes", "markdown": "# Notes\n\nA short answer.\n"})
	if err != nil {
		t.Fatal(err)
	}
	match := artifactIDInToolResult.FindStringSubmatch(published)
	if match == nil || !strings.Contains(published, "do NOT paste the markdown") || strings.Contains(published, "openpoet_") {
		t.Fatalf("chat markdown create result = %s", published)
	}
	id := match[1]
	if stored, err := api.db.GetArtifactVersion(ctx, id, 1); err != nil || stored.Format != database.ArtifactFormatMarkdown || stored.Content != "# Notes\n\nA short answer.\n" {
		t.Fatalf("stored version = %+v err=%v", stored, err)
	}
	if detail, err := call("read_artifact", map[string]interface{}{"artifact_id": id, "include_html": true}); err != nil ||
		!strings.Contains(detail, "| Format: markdown") || !strings.Contains(detail, "### Markdown of version 1") || !strings.Contains(detail, "A short answer.") {
		t.Fatalf("chat read = %q err=%v", detail, err)
	}
	if updated, err := call("update_artifact", map[string]interface{}{"artifact_id": id, "markdown": "# Notes\n\nA longer answer.\n", "base_version": float64(1)}); err != nil || !strings.Contains(updated, "now at version 2") {
		t.Fatalf("chat markdown update = %q err=%v", updated, err)
	}
	if patched, err := call("patch_artifact", map[string]interface{}{"artifact_id": id, "base_version": float64(2),
		"edits": []interface{}{map[string]interface{}{"old": "longer", "new": "better"}}}); err != nil || !strings.Contains(patched, "now at version 3") {
		t.Fatalf("chat markdown patch = %q err=%v", patched, err)
	}
	if stored, err := api.db.GetArtifactVersion(ctx, id, 3); err != nil || stored.Format != database.ArtifactFormatMarkdown || !strings.Contains(stored.Content, "A better answer.") {
		t.Fatalf("patched version = %+v err=%v", stored, err)
	}
	// A title alone still renames without publishing a version.
	if renamed, err := call("update_artifact", map[string]interface{}{"artifact_id": id, "title": "Chat answer"}); err != nil || strings.Contains(renamed, "version 4") {
		t.Fatalf("chat rename = %q err=%v", renamed, err)
	}
	for name, input := range map[string]map[string]interface{}{
		"both":          {"title": "x", "html": "<p>x</p>", "markdown": "x"},
		"markdown path": {"title": "x", "markdown_path": "notes.md"},
	} {
		if _, err := call("create_artifact", input); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestMarkdownArtifactPublishesFromAndExportsToMarkdownFiles(t *testing.T) {
	api, router := artifactTestRouter(t)
	ctx := context.Background()
	workspaceDir := t.TempDir()
	project := &database.Project{Name: "markdown-files", Path: workspaceDir, Type: "local"}
	if err := api.db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-md", ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session := &application.Actor{Type: "session", ID: "sess-md"}
	if err := os.WriteFile(filepath.Join(workspaceDir, "notes.md"), []byte(artifactHandlerMarkdown), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "page.html"), []byte(artifactHandlerHTML), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(artifactHandlerMarkdown))
	hash := hex.EncodeToString(sum[:])

	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Notes", "markdown_path": "notes.md"}, session)
	if recorder.Code != http.StatusCreated || created["format"] != "markdown" || created["sha256"] != hash ||
		created["source_path"] != filepath.Join(workspaceDir, "notes.md") {
		t.Fatalf("create from a markdown file = %d %v", recorder.Code, created)
	}
	id := created["id"].(string)

	// A file is read as what its name says, and one source is given.
	for name, tc := range map[string]struct {
		body map[string]any
		code string
	}{
		"html file as markdown": {map[string]any{"title": "x", "markdown_path": "page.html"}, "invalid_path"},
		"markdown file as html": {map[string]any{"title": "x", "html_path": "notes.md"}, "invalid_path"},
		"two sources":           {map[string]any{"title": "x", "markdown_path": "notes.md", "html": "<p>x</p>"}, "invalid_request"},
		"both inline":           {map[string]any{"title": "x", "markdown": "x", "html": "<p>x</p>"}, "invalid_request"},
	} {
		if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts", tc.body, session); recorder.Code != http.StatusBadRequest || body["code"] != tc.code {
			t.Fatalf("%s: %d %v", name, recorder.Code, body)
		}
	}

	// export: a markdown version goes to a .md file, byte for byte.
	if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/versions/current/export", map[string]any{"path": "out.html"}, session); recorder.Code != http.StatusBadRequest || body["code"] != "invalid_path" {
		t.Fatalf("markdown exported to an .html file = %d %v", recorder.Code, body)
	}
	recorder, exported := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/versions/current/export", map[string]any{"path": "out.md"}, session)
	if recorder.Code != http.StatusOK || exported["format"] != "markdown" || exported["sha256"] != hash {
		t.Fatalf("export = %d %v", recorder.Code, exported)
	}
	written, err := os.ReadFile(filepath.Join(workspaceDir, "out.md"))
	if err != nil || string(written) != artifactHandlerMarkdown {
		t.Fatalf("exported file = %q err=%v", written, err)
	}

	// The exported file publishes again as the next version, and a patch keeps the format.
	if err := os.WriteFile(filepath.Join(workspaceDir, "out.md"), []byte(artifactHandlerMarkdown+"\nMore.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recorder, updated := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id, map[string]any{"markdown_path": "out.md", "base_version": 1}, session); recorder.Code != http.StatusOK ||
		updated["version"] != float64(2) || updated["format"] != "markdown" {
		t.Fatalf("update from the exported file = %d %v", recorder.Code, updated)
	}
	if recorder, patched := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/patch", map[string]any{
		"base_version": 2, "edits": []map[string]any{{"old": "More.", "new": "Much more."}},
	}, session); recorder.Code != http.StatusOK || patched["version"] != float64(3) || patched["format"] != "markdown" {
		t.Fatalf("patch = %d %v", recorder.Code, patched)
	}
}
