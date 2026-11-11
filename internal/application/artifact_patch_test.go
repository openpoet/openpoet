package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestArtifactIsStoredAsSentAndReportsItsExactSizeAndHash(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	// A file usually ends with a line break: it is part of the document.
	html := "<!doctype html>\n<html><body><h1>Plan</h1></body></html>\n"
	sum := sha256.Sum256([]byte(html))
	want := hex.EncodeToString(sum[:])

	result, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: html, Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	if result.SizeBytes != int64(len(html)) || result.SHA256 != want || result.Redacted {
		t.Fatalf("create reported %d bytes, sha256 %s, redacted=%v; want %d, %s", result.SizeBytes, result.SHA256, result.Redacted, len(html), want)
	}
	stored, err := service.GetVersion(ctx, result.Artifact.ID, 0)
	if err != nil || stored.Content != html || stored.SHA256 != want || stored.SizeBytes != int64(len(html)) {
		t.Fatalf("stored version differs from what was sent: %q sha=%s err=%v", stored.Content, stored.SHA256, err)
	}

	// Masking is the one thing that changes the document, and it is reported.
	masked, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: result.Artifact.ID,
		Content: html + "<p>password=hunter2hunter2</p>\n", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.GetVersion(ctx, result.Artifact.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !masked.Redacted || masked.SHA256 != current.SHA256 || masked.SizeBytes != int64(len(current.Content)) || strings.Contains(current.Content, "hunter2") {
		t.Fatalf("masked update reported %d bytes sha=%s redacted=%v; stored %d bytes sha=%s", masked.SizeBytes, masked.SHA256, masked.Redacted, len(current.Content), current.SHA256)
	}
}

func TestArtifactPatchAppliesEditsToTheBaseVersion(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	auth := phase4Authorization()
	html := "<!doctype html>\n<html><body>\n<h1>Plan</h1>\n<p>step one</p>\n<p>step two</p>\n<li>todo</li>\n<li>todo</li>\n</body></html>\n"
	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: html, Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Artifact.ID
	patch := func(base int, edits ...ArtifactEdit) (ArtifactPublishResult, error) {
		return service.Patch(ctx, PatchArtifactCommand{ArtifactID: id, BaseVersion: base, Edits: edits, ChangeNote: "patched", Authorization: auth})
	}

	// Edits apply in order, each to the result of the previous one.
	result, err := patch(1,
		ArtifactEdit{Old: "<h1>Plan</h1>", New: "<h1>Rollout</h1>"},
		ArtifactEdit{Old: "<h1>Rollout</h1>\n<p>step one</p>", New: "<h1>Rollout</h1>\n<p>step 1</p>"},
		ArtifactEdit{Old: "<li>todo</li>", New: "<li>done</li>", ReplaceAll: true},
		ArtifactEdit{Old: "<p>step two</p>\n", New: ""})
	if err != nil {
		t.Fatal(err)
	}
	want := "<!doctype html>\n<html><body>\n<h1>Rollout</h1>\n<p>step 1</p>\n<li>done</li>\n<li>done</li>\n</body></html>\n"
	stored, err := service.GetVersion(ctx, id, 0)
	if err != nil || stored.Content != want {
		t.Fatalf("patched content = %q err=%v", stored.Content, err)
	}
	if !result.Created || result.Version != 2 || result.Replacements != 5 || result.SHA256 != stored.SHA256 || result.SizeBytes != int64(len(want)) || stored.ChangeNote != "patched" {
		t.Fatalf("patch result = %+v", result)
	}

	// A patch that does not apply publishes nothing and says which edit.
	for name, tc := range map[string]struct {
		base  int
		edits []ArtifactEdit
		kind  ErrorKind
		text  string
	}{
		"not found":    {2, []ArtifactEdit{{Old: "<h1>Rollout</h1>", New: "<h1>R</h1>"}, {Old: "absent text", New: "x"}}, ErrorValidation, "Edit 2: old text not found"},
		"ambiguous":    {2, []ArtifactEdit{{Old: "<li>done</li>", New: "<li>x</li>"}}, ErrorValidation, "Edit 1: old text occurs 2 times"},
		"empty old":    {2, []ArtifactEdit{{Old: "", New: "x"}}, ErrorValidation, "Edit 1: old is empty"},
		"same text":    {2, []ArtifactEdit{{Old: "<h1>Rollout</h1>", New: "<h1>Rollout</h1>"}}, ErrorValidation, "old and new are the same"},
		"no edits":     {2, nil, ErrorValidation, "at least one edit"},
		"no base":      {0, []ArtifactEdit{{Old: "a", New: "b"}}, ErrorValidation, "base_version is required"},
		"stale base":   {1, []ArtifactEdit{{Old: "<h1>Rollout</h1>", New: "<h1>R</h1>"}}, ErrorConflict, "is at version 2, not base_version 1"},
		"breaks rules": {2, []ArtifactEdit{{Old: want, New: "plain text, no markup"}}, ErrorValidation, "must be an HTML document"},
	} {
		if _, err := patch(tc.base, tc.edits...); !ErrorIsKind(err, tc.kind) || !strings.Contains(err.Error(), tc.text) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.text)
		}
	}
	if after, err := service.Get(ctx, id); err != nil || after.CurrentVersion != 2 {
		t.Fatalf("a refused patch changed the artifact: version %d err=%v", after.CurrentVersion, err)
	}
	if _, err := service.Patch(ctx, PatchArtifactCommand{ArtifactID: "missing0", BaseVersion: 1, Edits: []ArtifactEdit{{Old: "a", New: "b"}}, Authorization: auth}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("patch of a missing artifact: %v", err)
	}
}

func TestArtifactPatchMatchesCRLFDocumentsWithLFEdits(t *testing.T) {
	content, replacements, err := applyArtifactEdits("<p>one</p>\r\n<p>two</p>\r\n", []ArtifactEdit{{Old: "<p>one</p>\n<p>two</p>", New: "<p>1</p>\n<p>2</p>"}})
	if err != nil || replacements != 1 || content != "<p>1</p>\r\n<p>2</p>\r\n" {
		t.Fatalf("content=%q replacements=%d err=%v", content, replacements, err)
	}
}
