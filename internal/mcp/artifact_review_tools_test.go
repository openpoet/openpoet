package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtifactToolsReportMaskingDryRunAndRetries(t *testing.T) {
	bodies := map[string]map[string]any{}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.RequestURI()
		requests = append(requests, key)
		decoded := map[string]any{}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &decoded)
			bodies[key] = decoded
		}
		const tail = `"size_bytes":39,"sha256":"` + artifactTestSHA + `","link":"/app/artifact/ab12cd34"`
		switch key {
		case "POST /api/artifacts":
			switch {
			case decoded["dry_run"] == true:
				fmt.Fprint(w, `{"id":"","version":1,"created":false,"dry_run":true,"questions":2,"redacted":true,"redaction_count":3,"redactions":[{"line":12,"kind":"password"},{"line":40,"kind":"api_key"}],`+tail+`,"artifact":{"id":""}}`)
			case decoded["title"] == "Again":
				fmt.Fprint(w, `{"id":"ab12cd34","version":2,"created":false,"duplicate":true,`+tail+`,"artifact":{"id":"ab12cd34"}}`)
			default:
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"id":"ab12cd34","version":1,"created":true,"redacted":true,"redaction_count":1,"redactions":[{"line":7,"kind":"secret"}],`+tail+`,"artifact":{"id":"ab12cd34"}}`)
			}
		case "PUT /api/artifacts/ab12cd34":
			if decoded["dry_run"] == true {
				fmt.Fprint(w, `{"id":"ab12cd34","version":4,"created":true,"dry_run":true,`+tail+`,"artifact":{"id":"ab12cd34","title":"Old"}}`)
				return
			}
			fmt.Fprint(w, `{"id":"ab12cd34","version":4,"created":true,`+tail+`,"artifact":{"id":"ab12cd34","title":"New title"}}`)
		case "PATCH /api/artifacts/ab12cd34":
			fmt.Fprint(w, `{"id":"ab12cd34","title":"Renamed","current_version":4,"status":"active"}`)
		case "POST /api/artifacts/ab12cd34/patch":
			fmt.Fprint(w, `{"id":"ab12cd34","version":5,"created":true,"replacements":1,"redacted":true,"redaction_count":1,"redactions":[{"line":3,"kind":"password"}],`+tail+`,"artifact":{"id":"ab12cd34","title":"New title"}}`)
		case "GET /api/artifacts/ab12cd34/annotations?status=open":
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

	// create: where the masking happened, by line and kind.
	result, err := call("openpoet_create_artifact", `{"title":"Plan","html":"<html></html>"}`)
	if err != nil || !strings.Contains(result, "NOTE: 1 credential-looking value(s) were masked as [REDACTED] in the stored HTML: line 7 (secret). Lines are those of the document you sent") ||
		!strings.Contains(result, "not the hash of what you sent") {
		t.Fatalf("create = %q err=%v", result, err)
	}
	// dry run: nothing published, and the list of masked places may be shorter than the count.
	result, err = call("openpoet_create_artifact", `{"title":"Plan","html":"<html></html>","dry_run":true}`)
	if err != nil || !strings.HasPrefix(result, "Dry run, nothing was published. The document is accepted: it would be published as a new artifact, stored as 39 bytes, sha256 "+artifactTestSHA) ||
		!strings.Contains(result, "2 question(s) declared") || !strings.Contains(result, "3 credential-looking value(s) were masked") ||
		!strings.Contains(result, "line 12 (password), line 40 (api_key), and 1 more") || strings.Contains(result, "artifact_id") {
		t.Fatalf("dry create = %q err=%v", result, err)
	}
	if bodies["POST /api/artifacts"]["dry_run"] != true {
		t.Fatalf("dry_run was not sent: %v", bodies["POST /api/artifacts"])
	}
	// A repeated create gets the artifact already created.
	result, err = call("openpoet_create_artifact", `{"title":"Again","html":"<html></html>"}`)
	if err != nil || !strings.HasPrefix(result, "Artifact already published: this session created artifact_id ab12cd34 a moment ago") ||
		!strings.Contains(result, "no second artifact was created") || !strings.Contains(result, "base_version 2") {
		t.Fatalf("repeated create = %q err=%v", result, err)
	}

	// update: title and summary go with the publish, and are confirmed.
	result, err = call("openpoet_update_artifact", `{"artifact_id":"ab12cd34","html":"<html></html>","base_version":3,"title":"New title","summary":"What changed"}`)
	if err != nil || !strings.Contains(result, "is now at version 4") || !strings.Contains(result, `Title and summary updated. The title is now "New title".`) {
		t.Fatalf("update with title = %q err=%v", result, err)
	}
	if body := bodies["PUT /api/artifacts/ab12cd34"]; body["title"] != "New title" || body["summary"] != "What changed" {
		t.Fatalf("update payload = %v", body)
	}
	// Without a title the fields are not sent at all (they would blank the stored ones).
	if _, err = call("openpoet_update_artifact", `{"artifact_id":"ab12cd34","html":"<html></html>"}`); err != nil {
		t.Fatal(err)
	}
	if body := bodies["PUT /api/artifacts/ab12cd34"]; body["title"] != nil || body["summary"] != nil {
		t.Fatalf("update without a title sent one: %v", body)
	}
	result, err = call("openpoet_update_artifact", `{"artifact_id":"ab12cd34","html":"<html></html>","title":"X","dry_run":true}`)
	if err != nil || !strings.Contains(result, "it would become version 4 of artifact ab12cd34") || !strings.Contains(result, "Nothing would be masked.") || strings.Contains(result, "Title updated") {
		t.Fatalf("dry update = %q err=%v", result, err)
	}
	// A title on its own renames without publishing.
	requests = nil
	result, err = call("openpoet_update_artifact", `{"artifact_id":"ab12cd34","title":"Renamed"}`)
	if err != nil || result != `Title updated. Artifact ab12cd34 is still at version 4 (no new version was published). The title is now "Renamed".` {
		t.Fatalf("rename = %q err=%v", result, err)
	}
	if len(requests) != 1 || requests[0] != "PATCH /api/artifacts/ab12cd34" || bodies["PATCH /api/artifacts/ab12cd34"]["title"] != "Renamed" {
		t.Fatalf("rename requests = %v %v", requests, bodies["PATCH /api/artifacts/ab12cd34"])
	}

	// patch: the masking lines are those of the patched document.
	result, err = call("openpoet_patch_artifact", `{"artifact_id":"ab12cd34","base_version":4,"summary":"S","edits":[{"old":"a","new":"b"}]}`)
	if err != nil || !strings.Contains(result, "line 3 (password). Lines are those of the patched document") || !strings.Contains(result, "Summary updated.") {
		t.Fatalf("patch = %q err=%v", result, err)
	}
	if body := bodies["POST /api/artifacts/ab12cd34/patch"]; body["summary"] != "S" || body["title"] != nil {
		t.Fatalf("patch payload = %v", body)
	}
}

func TestArtifactAnnotationsAreResolvedInOneCall(t *testing.T) {
	notes := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.RequestURI()
		var body struct {
			Note string `json:"resolution_note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch key {
		case "POST /api/artifacts/ab12cd34/annotations/5/resolve", "POST /api/artifacts/ab12cd34/annotations/6/resolve", "POST /api/artifacts/ab12cd34/annotations/9/resolve":
			notes[key] = body.Note
			fmt.Fprint(w, `{"status":"resolved"}`)
		case "POST /api/artifacts/ab12cd34/annotations/7/resolve":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"Annotation not found on this artifact","code":"annotation_not_found"}`)
		case "GET /api/artifacts/ab12cd34/annotations?status=open":
			fmt.Fprint(w, `[{"id":8}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewAPIClient(server.URL)
	call := func(args string) (string, error) {
		return executeTool(client, "openpoet_resolve_artifact_annotation", json.RawMessage(args), "sess-1", "")
	}

	result, err := call(`{"artifact_id":"ab12cd34","resolution_note":"shared note","resolutions":[
		{"annotation_id":5,"resolution_note":"Renamed the button"},{"annotation_id":"6"},{"annotation_id":7,"resolution_note":"n/a"}]}`)
	if err != nil || result != "Resolved 2 annotation(s) on artifact ab12cd34: #5, #6. NOT resolved (1): #7 (Annotation not found on this artifact). 1 annotation(s) remain open." {
		t.Fatalf("batch = %q err=%v", result, err)
	}
	if notes["POST /api/artifacts/ab12cd34/annotations/5/resolve"] != "Renamed the button" || notes["POST /api/artifacts/ab12cd34/annotations/6/resolve"] != "shared note" {
		t.Fatalf("notes = %v", notes)
	}
	// The single form still works, and a list given as a JSON string too.
	if result, err = call(`{"artifact_id":"ab12cd34","annotation_id":"9","resolution_note":"done"}`); err != nil || result != "Annotation #9 on artifact ab12cd34 resolved. 1 annotation(s) remain open." {
		t.Fatalf("single = %q err=%v", result, err)
	}
	if result, err = call(`{"artifact_id":"ab12cd34","resolutions":"[{\"annotation_id\":5,\"resolution_note\":\"again\"}]"}`); err != nil || !strings.Contains(result, "Annotation #5") {
		t.Fatalf("resolutions as a string = %q err=%v", result, err)
	}
	// None resolved is an error that says why.
	if _, err = call(`{"artifact_id":"ab12cd34","resolutions":[{"annotation_id":7,"resolution_note":"x"}]}`); err == nil || !strings.Contains(err.Error(), "no annotation was resolved") || !strings.Contains(err.Error(), "#7 (Annotation not found") {
		t.Fatalf("all failed: %v", err)
	}
	for name, args := range map[string]string{
		"no note":         `{"artifact_id":"ab12cd34","resolutions":[{"annotation_id":5}]}`,
		"twice":           `{"artifact_id":"ab12cd34","resolutions":[{"annotation_id":5,"resolution_note":"a"},{"annotation_id":5,"resolution_note":"b"}]}`,
		"both forms":      `{"artifact_id":"ab12cd34","annotation_id":5,"resolution_note":"a","resolutions":[{"annotation_id":6,"resolution_note":"b"}]}`,
		"bad id":          `{"artifact_id":"ab12cd34","resolutions":[{"annotation_id":"abc","resolution_note":"a"}]}`,
		"empty list":      `{"artifact_id":"ab12cd34","resolutions":[]}`,
		"nothing":         `{"artifact_id":"ab12cd34"}`,
		"single, no note": `{"artifact_id":"ab12cd34","annotation_id":5}`,
	} {
		if _, err := call(args); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestArtifactAnnotationListingSaysWhereANoteIsAndWhetherItStillApplies(t *testing.T) {
	body := `[
	 {"id":1,"version":1,"status":"open","kind":"note","quote":"Budget is 40k.","body":"Raise it","anchor":{"type":"text","selector":"[data-id=\"TM8\"] > p:nth-of-type(1)","near":"[data-id=\"TM8\"]"},"placement":{"version":3,"checked":true,"found":false}},
	 {"id":2,"version":1,"status":"resolved","kind":"note","quote":"Launch","body":"Rename","resolution_note":"Renamed","resolved_by":"session:s1","resolved_on_version":2,"anchor":{"type":"element","tag":"button","selector":"body > div:nth-of-type(2) > button:nth-of-type(1)","near":"#stage-2"},"placement":{"version":3,"checked":true,"found":true}},
	 {"id":3,"version":3,"status":"open","kind":"note","quote":"Intro","body":"Shorter","anchor":{"type":"text","selector":"#intro > p"}},
	 {"id":4,"version":2,"status":"open","kind":"note","quote":"<div>","body":"What is this","anchor":{"type":"element","tag":"div","selector":"body > div:nth-of-type(4)"},"placement":{"version":3,"checked":false,"found":false}},
	 {"id":5,"version":1,"status":"resolved","kind":"answer","question_id":"D1","quote":"Name?","body":"ok","resolution_note":"Applied","resolved_by":"session:s1","resolved_on_version":2,"answer":{"type":"single","options":[{"id":"a","label":"Routine"}]}}
	]`
	result := FormatArtifactAnnotations("ab12cd34", "all", []byte(body), "openpoet_", nil, nil)
	for _, want := range []string{
		`- #1 [open, made on v1; the quoted text is no longer in v3] on text "Budget is 40k." (at [data-id="TM8"] > p:nth-of-type(1))`,
		`- #2 [resolved on v2, made on v1; its element is still in v3] on <button> element "Launch" (at body > div:nth-of-type(2) > button:nth-of-type(1), inside #stage-2)`,
		`- #3 [open, made on v3] on text "Intro" (at #intro > p)`,
		`- #4 [open, made on v2] on <div> element "<div>" (at body > div:nth-of-type(4))`,
		`- #5 [resolved on v2, answered on v1] answer to question D1 "Name?"`,
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("listing lacks %q:\n%s", want, result)
		}
	}
}
