package automation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// TestArtifactCapabilitiesPublishVersionAndReadThroughTheSharedService runs the
// artifact capabilities against a real database: the automation plane must
// reach the same ArtifactService (and its invariants) as the REST routes.
func TestArtifactCapabilitiesPublishVersionAndReadThroughTheSharedService(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "artifact-capabilities.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	services := collaborationTestServices()
	services.Artifacts = application.NewArtifactService(db, nil)
	_, registry := collaborationTestRegistry(t, services)
	actor := collaborationActor(artifactCollaborationDefinitions())
	ctx := context.Background()

	dispatch := func(capability, target, payload string) (map[string]any, error) {
		t.Helper()
		result, err := DispatchPlatformCapability(ctx, registry, PlatformDispatchRequest{
			Capability: application.CapabilityName(capability), Target: json.RawMessage(target), Payload: json.RawMessage(payload),
			Actor: actor, Reason: "artifact capability test", Approval: collaborationApproval(t),
		})
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(result.Result)
		decoded := map[string]any{}
		_ = json.Unmarshal(encoded, &decoded)
		return decoded, nil
	}

	created, err := dispatch("artifacts.create", `{}`, `{"title":"Plan","html":"<html><body><h1>v1</h1></body></html>","session_id":"sess-auto"}`)
	if err != nil {
		t.Fatal(err)
	}
	artifact, _ := created["artifact"].(map[string]any)
	id, _ := artifact["id"].(string)
	if id == "" || created["version"] != float64(1) || artifact["created_by"] != "automation_client:assistant" || artifact["session_id"] != "sess-auto" {
		t.Fatalf("create result = %v", created)
	}
	target := `{"id":"` + id + `"}`

	updated, err := dispatch("artifacts.update", target, `{"html":"<html><body><h1>v2</h1></body></html>","base_version":1,"change_note":"second"}`)
	if err != nil || updated["version"] != float64(2) || updated["created"] != true {
		t.Fatalf("update result = %v err=%v", updated, err)
	}
	// A stale base version surfaces the application conflict, not a new version.
	_, err = dispatch("artifacts.update", target, `{"html":"<html><body><h1>v3</h1></body></html>","base_version":1}`)
	if dispatchCode(err) != "artifact_version_conflict" || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("conflict err = %v", err)
	}
	// Non-HTML content is rejected by the service.
	if _, err = dispatch("artifacts.create", `{}`, `{"html":"# markdown only"}`); dispatchCode(err) != "invalid_artifact_html" {
		t.Fatalf("markdown err = %v", err)
	}
	// Unknown payload fields are rejected at the capability boundary.
	var dispatchErr *PlatformDispatchError
	if _, err = dispatch("artifacts.create", `{}`, `{"html":"<p>x</p>","content":"x"}`); !errors.As(err, &dispatchErr) || dispatchErr.Code != "platform_payload_invalid" {
		t.Fatalf("unknown field err = %v", err)
	}

	detail, err := dispatch("artifacts.get", target, `{}`)
	versions, _ := detail["versions"].([]any)
	if err != nil || detail["current_version"] != float64(2) || len(versions) != 2 {
		t.Fatalf("get result = %v err=%v", detail, err)
	}
	current, err := dispatch("artifacts.get_version", target, `{}`)
	if err != nil || current["version"] != float64(2) || !strings.Contains(current["content"].(string), "<h1>v2</h1>") {
		t.Fatalf("get_version current = %v err=%v", current, err)
	}
	first, err := dispatch("artifacts.get_version", target, `{"version":1}`)
	if err != nil || !strings.Contains(first["content"].(string), "<h1>v1</h1>") {
		t.Fatalf("get_version 1 = %v err=%v", first, err)
	}

	list := func(payload string) int {
		t.Helper()
		result, err := DispatchPlatformCapability(ctx, registry, PlatformDispatchRequest{
			Capability: "artifacts.list", Target: json.RawMessage(`{}`), Payload: json.RawMessage(payload), Actor: actor,
		})
		if err != nil {
			t.Fatal(err)
		}
		items, ok := result.Result.([]application.ArtifactView)
		if !ok {
			t.Fatalf("list result type = %T", result.Result)
		}
		return len(items)
	}
	if got := list(`{"session_id":"sess-auto"}`); got != 1 {
		t.Fatalf("session listing = %d", got)
	}
	if _, err = dispatch("artifacts.update_meta", target, `{"status":"archived"}`); err != nil {
		t.Fatal(err)
	}
	if got := list(`{}`); got != 0 {
		t.Fatalf("archived artifact still listed: %d", got)
	}
	if got := list(`{"include_archived":true}`); got != 1 {
		t.Fatalf("include_archived listing = %d", got)
	}
	if _, err = dispatch("artifacts.update_meta", target, `{}`); !errors.As(err, &dispatchErr) || dispatchErr.Code != "platform_payload_invalid" {
		t.Fatalf("empty update_meta err = %v", err)
	}

	// Annotations: created on the artifact, listed open by default, resolved
	// and reopened by annotation id through the artifact in the target.
	note, err := dispatch("artifacts.annotations.create", target, `{"anchor":{"type":"text","selector":"h1","exact":"v2"},"body":"Rename the heading"}`)
	if err != nil || note["status"] != "open" || note["quote"] != "v2" || note["version"] != float64(2) || note["created_by"] != "automation_client:assistant" {
		t.Fatalf("annotation create = %v err=%v", note, err)
	}
	annotationID := int64(note["id"].(float64))
	listAnnotations := func(payload string) []application.ArtifactAnnotationView {
		t.Helper()
		result, err := DispatchPlatformCapability(ctx, registry, PlatformDispatchRequest{
			Capability: "artifacts.annotations.list", Target: json.RawMessage(target), Payload: json.RawMessage(payload), Actor: actor,
		})
		if err != nil {
			t.Fatal(err)
		}
		items, ok := result.Result.([]application.ArtifactAnnotationView)
		if !ok {
			t.Fatalf("annotation list result type = %T", result.Result)
		}
		return items
	}
	if open := listAnnotations(`{}`); len(open) != 1 || open[0].ID != annotationID || open[0].Body != "Rename the heading" {
		t.Fatalf("open annotations = %+v", open)
	}
	change := `{"annotation_id":` + strconv.FormatInt(annotationID, 10) + `,"resolution_note":"Renamed in v3"}`
	resolvedNote, err := dispatch("artifacts.annotations.resolve", target, change)
	if err != nil || resolvedNote["status"] != "resolved" || resolvedNote["resolution_note"] != "Renamed in v3" || resolvedNote["resolved_by"] != "automation_client:assistant" {
		t.Fatalf("annotation resolve = %v err=%v", resolvedNote, err)
	}
	if open := listAnnotations(`{}`); len(open) != 0 {
		t.Fatalf("resolved annotation still listed as open: %+v", open)
	}
	if done := listAnnotations(`{"status":"resolved"}`); len(done) != 1 {
		t.Fatalf("resolved annotations = %+v", done)
	}
	if _, err = dispatch("artifacts.annotations.resolve", `{"id":"another"}`, change); dispatchCode(err) != "annotation_not_found" {
		t.Fatalf("resolve through another artifact err = %v", err)
	}
	if reopened, err := dispatch("artifacts.annotations.reopen", target, change); err != nil || reopened["status"] != "open" {
		t.Fatalf("annotation reopen = %v err=%v", reopened, err)
	}
	if _, err = dispatch("artifacts.annotations.resolve", target, `{}`); dispatchCode(err) != "platform_payload_invalid" {
		t.Fatalf("resolve without annotation_id err = %v", err)
	}
	if _, err = dispatch("artifacts.annotations.create", target, `{"anchor":{"type":"shape"},"body":"x"}`); dispatchCode(err) != "invalid_annotation_anchor" {
		t.Fatalf("bad anchor err = %v", err)
	}

	// Without documents:write the mutations are refused.
	readOnly := Actor{Type: "automation_client", ID: "reader", ClientID: "reader", Name: "Reader", Scopes: ScopeSet{ScopeDocumentsRead: {}}}
	_, err = DispatchPlatformCapability(ctx, registry, PlatformDispatchRequest{
		Capability: "artifacts.create", Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{"html":"<p>x</p>"}`), Actor: readOnly,
		Reason: "artifact capability test", Approval: collaborationApproval(t),
	})
	if dispatchCode(err) != "platform_insufficient_scope" {
		t.Fatalf("read-only actor create err = %v", err)
	}
}
