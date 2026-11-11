package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func localArtifactWorkspace(root string, temp ...string) *artifactWorkspace {
	return &artifactWorkspace{fs: localWorkspaceFS{}, roots: append([]string{root}, temp...), close: func() {}}
}

func TestArtifactWorkspaceReadsOnlyHTMLFilesInsideItsRoots(t *testing.T) {
	root, temp, outside := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "docs", "plan.html"), "<html>plan</html>\n")
	write(filepath.Join(temp, "scratch.HTM"), "<html>scratch</html>")
	write(filepath.Join(outside, "secret.html"), "<html>secret</html>")
	write(filepath.Join(root, "notes.txt"), "<html>notes</html>")
	write(filepath.Join(root, "big.html"), strings.Repeat("x", 65))
	if err := os.Symlink(filepath.Join(outside, "secret.html"), filepath.Join(root, "link.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "notes.txt"), filepath.Join(root, "notes.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := localArtifactWorkspace(root, temp)

	for name, path := range map[string]string{
		"relative": "docs/plan.html", "absolute": filepath.Join(root, "docs", "plan.html"), "dot segments": "docs/../docs/plan.html",
	} {
		content, resolved, err := workspace.read(path, 64)
		if err != nil || content != "<html>plan</html>\n" || !strings.HasSuffix(resolved, filepath.Join("docs", "plan.html")) {
			t.Fatalf("%s: content=%q resolved=%q err=%v", name, content, resolved, err)
		}
	}
	if content, _, err := workspace.read(filepath.Join(temp, "scratch.HTM"), 64); err != nil || content != "<html>scratch</html>" {
		t.Fatalf("temporary directory: content=%q err=%v", content, err)
	}

	for name, tc := range map[string]struct{ path, code string }{
		"outside":             {filepath.Join(outside, "secret.html"), "path_not_allowed"},
		"parent traversal":    {"../" + filepath.Base(outside) + "/secret.html", "path_not_allowed"},
		"link to outside":     {"link.html", "path_not_allowed"},
		"through linked dir":  {"escape/secret.html", "path_not_allowed"},
		"not html":            {"notes.txt", "invalid_path"},
		"link to a non-html":  {"notes.html", "invalid_path"},
		"directory":           {"dir.html", "invalid_path"},
		"missing":             {"absent.html", "file_not_found"},
		"too large":           {"big.html", "content_too_large"},
		"empty":               {"  ", "invalid_path"},
		"prefix of the root":  {root + "x/plan.html", "file_not_found"},
		"file system root":    {"/etc/hostname.html", "file_not_found"},
		"sensitive, not html": {"/etc/passwd", "invalid_path"},
	} {
		_, _, err := workspace.read(tc.path, 64)
		fileErr, ok := err.(*artifactFileError)
		if !ok || fileErr.code != tc.code {
			t.Fatalf("%s: err = %v, want code %s", name, err, tc.code)
		}
	}

	// A root that is a prefix of another directory's name does not contain it.
	sibling := root + "-sibling"
	write(filepath.Join(sibling, "plan.html"), "<html>sibling</html>")
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	if _, _, err := workspace.read(filepath.Join(sibling, "plan.html"), 64); err == nil {
		t.Fatal("a sibling directory sharing the root's prefix was readable")
	}
}

func TestArtifactWorkspaceWritesOnlyHTMLFilesInsideItsRoots(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target.html"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.html"), filepath.Join(root, "link.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	workspace := localArtifactWorkspace(root)

	written, err := workspace.write("out/page.html", "<html>v1</html>\n")
	if err != nil || written != filepath.Join(root, "out", "page.html") {
		t.Fatalf("written=%q err=%v", written, err)
	}
	// An existing regular file is replaced.
	if _, err := workspace.write("out/page.html", "<html>v2</html>"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(written); string(data) != "<html>v2</html>" {
		t.Fatalf("file content = %q", data)
	}
	for name, tc := range map[string]struct{ path, code string }{
		"outside":            {filepath.Join(outside, "new.html"), "path_not_allowed"},
		"through linked dir": {"escape/new.html", "path_not_allowed"},
		"over a link":        {"link.html", "invalid_path"},
		"not html":           {"out/page.txt", "invalid_path"},
		"missing directory":  {"absent/page.html", "file_not_found"},
	} {
		_, err := workspace.write(tc.path, "<html>x</html>")
		fileErr, ok := err.(*artifactFileError)
		if !ok || fileErr.code != tc.code {
			t.Fatalf("%s: err = %v, want code %s", name, err, tc.code)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "target.html")); string(data) != "keep" {
		t.Fatalf("a write went through a link: %q", data)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.html")); err == nil {
		t.Fatal("a file was written outside the workspace")
	}
}

func TestArtifactPublishFromFilePatchAndExport(t *testing.T) {
	api, router := artifactTestRouter(t)
	ctx := context.Background()
	workspaceDir := t.TempDir()
	project := &database.Project{Name: "files", Path: workspaceDir, Type: "local"}
	if err := api.db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := api.db.CreateSession(ctx, &database.Session{ID: "sess-files", ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session := &application.Actor{Type: "session", ID: "sess-files"}
	html := "<!doctype html>\n<html><body><h1>Plan</h1><p>first</p></body></html>\n"
	if err := os.WriteFile(filepath.Join(workspaceDir, "plan.html"), []byte(html), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}

	// create from a file of the session's workspace: stored byte for byte.
	recorder, created := artifactRequest(t, router, http.MethodPost, "/api/artifacts", map[string]any{"title": "Plan", "html_path": "plan.html"}, session)
	if recorder.Code != http.StatusCreated || created["sha256"] != hash(html) || created["size_bytes"] != float64(len(html)) ||
		created["source_path"] != filepath.Join(workspaceDir, "plan.html") || created["redacted"] != false {
		t.Fatalf("create from file = %d %v", recorder.Code, created)
	}
	id := created["id"].(string)

	// The detail carries the link of the artifact.
	if recorder, detail := artifactRequest(t, router, http.MethodGet, "/api/artifacts/"+id, nil, session); recorder.Code != http.StatusOK ||
		detail["link"] != "/app/artifact/"+id || detail["title"] != "Plan" {
		t.Fatalf("detail = %d %v", recorder.Code, detail)
	}

	// A path is refused without a session, with both sources, and outside the workspace.
	for name, tc := range map[string]struct {
		body  map[string]any
		actor *application.Actor
		code  string
	}{
		"no session":   {map[string]any{"title": "x", "html_path": "plan.html"}, nil, "session_required"},
		"both sources": {map[string]any{"title": "x", "html_path": "plan.html", "html": "<p>x</p>"}, session, "invalid_request"},
		"outside":      {map[string]any{"title": "x", "html_path": "/etc/hosts.html"}, session, "file_not_found"},
		"not html":     {map[string]any{"title": "x", "html_path": "/etc/passwd"}, session, "invalid_path"},
	} {
		if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts", tc.body, tc.actor); recorder.Code != http.StatusBadRequest || body["code"] != tc.code {
			t.Fatalf("%s: %d %v", name, recorder.Code, body)
		}
	}

	// patch: only the replacements travel.
	recorder, patched := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/patch", map[string]any{
		"base_version": 1, "change_note": "second", "edits": []map[string]any{{"old": "<p>first</p>", "new": "<p>second</p>"}},
	}, session)
	want := strings.Replace(html, "<p>first</p>", "<p>second</p>", 1)
	if recorder.Code != http.StatusOK || patched["version"] != float64(2) || patched["sha256"] != hash(want) || patched["replacements"] != float64(1) ||
		patched["link"] != "/app/artifact/"+id {
		t.Fatalf("patch = %d %v", recorder.Code, patched)
	}
	if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/patch", map[string]any{
		"base_version": 1, "edits": []map[string]any{{"old": "<p>second</p>", "new": "<p>third</p>"}},
	}, session); recorder.Code != http.StatusConflict || body["code"] != "artifact_version_conflict" {
		t.Fatalf("stale patch = %d %v", recorder.Code, body)
	}
	if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/patch", map[string]any{
		"base_version": 2, "edits": []map[string]any{{"old": "<p>absent</p>", "new": "<p>third</p>"}},
	}, session); recorder.Code != http.StatusBadRequest || body["code"] != "edit_not_found" {
		t.Fatalf("patch that does not match = %d %v", recorder.Code, body)
	}

	// export: the stored HTML lands in the workspace byte for byte, and the
	// file publishes again through html_path.
	recorder, exported := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/versions/current/export", map[string]any{"path": "current.html"}, session)
	target := filepath.Join(workspaceDir, "current.html")
	if recorder.Code != http.StatusOK || exported["path"] != target || exported["sha256"] != hash(want) || exported["version"] != float64(2) {
		t.Fatalf("export = %d %v", recorder.Code, exported)
	}
	if _, has := exported["content"]; has {
		t.Fatal("export returned the HTML")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != want {
		t.Fatalf("exported file = %q err=%v", data, err)
	}
	if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/versions/1/export", map[string]any{"path": "/etc/openpoet-test.html"}, session); recorder.Code != http.StatusBadRequest || body["code"] != "path_not_allowed" {
		t.Fatalf("export outside the workspace = %d %v", recorder.Code, body)
	}
	if recorder, body := artifactRequest(t, router, http.MethodPost, "/api/artifacts/"+id+"/versions/current/export", map[string]any{"path": "current.html"}, nil); recorder.Code != http.StatusBadRequest || body["code"] != "session_required" {
		t.Fatalf("export without a session = %d %v", recorder.Code, body)
	}
	recorder, same := artifactRequest(t, router, http.MethodPut, "/api/artifacts/"+id, map[string]any{"html_path": "current.html", "base_version": 2}, session)
	if recorder.Code != http.StatusOK || same["created"] != false || same["sha256"] != hash(want) {
		t.Fatalf("republishing the exported file = %d %v", recorder.Code, same)
	}
}
