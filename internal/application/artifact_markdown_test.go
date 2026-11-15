package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"openpoet/internal/database"
)

const artifactTestMarkdown = "# Rollout plan\n\nFirst line\nsecond line with **bold** text.\n\n## Steps & risks\n\n" +
	"- [x] done\n- [ ] todo\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n<details><summary>More</summary>hidden</details>\n\n" +
	"```mermaid\ngraph TD; A-->B;\n```\n\n```go\nx := 1 < 2\n```\n\nSee https://example.com and ~~old~~.\n"

// The rendering is pinned: annotations made on a markdown version are placed
// by the text and the elements of this output, so a change in it (a new
// version of the renderer, another option) has to be a decision, not a
// surprise.
func TestRenderArtifactMarkdownIsPinned(t *testing.T) {
	want := "<h1 id=\"rollout-plan\">Rollout plan</h1>\n" +
		"<p>First line<br>\nsecond line with <strong>bold</strong> text.</p>\n" +
		"<h2 id=\"steps--risks\">Steps &amp; risks</h2>\n" +
		"<ul>\n<li><input checked=\"\" disabled=\"\" type=\"checkbox\"> done</li>\n<li><input disabled=\"\" type=\"checkbox\"> todo</li>\n</ul>\n" +
		"<table>\n<thead>\n<tr>\n<th>A</th>\n<th>B</th>\n</tr>\n</thead>\n<tbody>\n<tr>\n<td>1</td>\n<td>2</td>\n</tr>\n</tbody>\n</table>\n" +
		"<details><summary>More</summary>hidden</details>\n" +
		"<pre><code class=\"language-mermaid\">graph TD; A--&gt;B;\n</code></pre>\n" +
		"<pre><code class=\"language-go\">x := 1 &lt; 2\n</code></pre>\n" +
		"<p>See <a href=\"https://example.com\">https://example.com</a> and <del>old</del>.</p>\n"
	body, hasMermaid := RenderArtifactMarkdown(artifactTestMarkdown)
	if body != want {
		t.Fatalf("rendered markdown changed:\n got %q\nwant %q", body, want)
	}
	if !hasMermaid {
		t.Fatal("a ```mermaid block was not reported")
	}
	if _, hasMermaid := RenderArtifactMarkdown("# Title\n\n```go\nx := 1\n```\n"); hasMermaid {
		t.Fatal("a document without a mermaid block reported one")
	}
}

func TestMarkdownArtifactIsStoredAsItsSource(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	auth := phase4Authorization()
	sum := sha256.Sum256([]byte(artifactTestMarkdown))
	want := hex.EncodeToString(sum[:])

	// Markdown is not an HTML document: it is refused as html, accepted as markdown.
	plain := "Just a short answer, no markup at all.\n"
	if _, err := service.Create(ctx, CreateArtifactCommand{Title: "Answer", Content: plain, Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("plain text published as html: err = %v", err)
	}
	if _, err := service.Create(ctx, CreateArtifactCommand{Title: "Answer", Content: plain, Format: "markdown", Authorization: auth}); err != nil {
		t.Fatalf("plain text published as markdown: %v", err)
	}
	for name, command := range map[string]CreateArtifactCommand{
		"empty markdown": {Title: "X", Content: " \n", Format: "markdown", Authorization: auth},
		"unknown format": {Title: "X", Content: "text", Format: "pdf", Authorization: auth},
		"invalid UTF-8":  {Title: "X", Content: "bad \xff bytes", Format: "markdown", Authorization: auth},
	} {
		if _, err := service.Create(ctx, command); !ErrorIsKind(err, ErrorValidation) {
			t.Fatalf("%s: err = %v, want a validation error", name, err)
		}
	}

	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: artifactTestMarkdown, Format: "markdown", Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	if created.Format != database.ArtifactFormatMarkdown || created.SHA256 != want || created.SizeBytes != int64(len(artifactTestMarkdown)) || created.Redacted || created.Questions != 0 {
		t.Fatalf("create result = %+v", created)
	}
	id := created.Artifact.ID
	stored, err := service.GetVersion(ctx, id, 0)
	if err != nil || stored.Content != artifactTestMarkdown || stored.Format != database.ArtifactFormatMarkdown || stored.SHA256 != want {
		t.Fatalf("stored version = %+v err=%v", stored, err)
	}
	view, err := service.Get(ctx, id)
	if err != nil || view.Format != database.ArtifactFormatMarkdown || len(view.Versions) != 1 || view.Versions[0].Format != database.ArtifactFormatMarkdown {
		t.Fatalf("artifact view = %+v err=%v", view, err)
	}
	listed, err := service.List(ctx, ArtifactListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.ID == id && item.Format != database.ArtifactFormatMarkdown {
			t.Fatalf("listed format = %q", item.Format)
		}
	}

	// A patch edits the source and keeps the format.
	patched, err := service.Patch(ctx, PatchArtifactCommand{ArtifactID: id, BaseVersion: 1, Authorization: auth,
		Edits: []ArtifactEdit{{Old: "- [ ] todo", New: "- [x] todo"}, {Old: "**bold**", New: "**strong**"}}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.GetVersion(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if patched.Format != database.ArtifactFormatMarkdown || patched.Version != 2 || patched.Replacements != 2 ||
		current.Format != database.ArtifactFormatMarkdown || !strings.Contains(current.Content, "- [x] todo") || !strings.Contains(current.Content, "**strong** text") {
		t.Fatalf("patched = %+v, stored %q", patched, current.Content)
	}

	// The same content again is not a new version; in another format it is.
	same, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: current.Content, Format: "markdown", Authorization: auth})
	if err != nil || same.Created || same.Version != 2 {
		t.Fatalf("identical markdown update = %+v err=%v", same, err)
	}
	page := "<!doctype html><html><body><h1>Plan</h1></body></html>"
	asHTML, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: page, Authorization: auth})
	if err != nil || !asHTML.Created || asHTML.Version != 3 || asHTML.Format != database.ArtifactFormatHTML {
		t.Fatalf("html version after markdown = %+v err=%v", asHTML, err)
	}
	if view, err = service.Get(ctx, id); err != nil || view.Format != database.ArtifactFormatHTML {
		t.Fatalf("format of the current version = %q err=%v", view.Format, err)
	}
}

func TestMarkdownArtifactMasksCredentialsAndDeclaresNoQuestions(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	// In an HTML document this block is malformed and refuses the publish; in
	// markdown it is text.
	source := "# Notes\n\npassword=hunter2hunter2\n\n<script type=\"application/json\" id=\"openpoet-questions\">nope</script>\n"
	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Notes", Content: source, Format: "markdown", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.GetVersion(ctx, created.Artifact.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Redacted || created.RedactionCount != 1 || created.Redactions[0].Line != 3 || strings.Contains(stored.Content, "hunter2") {
		t.Fatalf("masking: result %+v, stored %q", created, stored.Content)
	}
	if created.Questions != 0 {
		t.Fatalf("a markdown version declared %d questions", created.Questions)
	}
}

func TestMarkdownAnnotationPlacementIsCheckedOnTheRenderedPage(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	auth := phase4Authorization()
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "local-ui"}}
	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: artifactTestMarkdown, Format: "markdown", Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Artifact.ID
	// The reader quotes the page: "second line with bold text" is not in the
	// source, where the word is wrapped in asterisks.
	notes := map[string]ArtifactAnchor{
		"kept":    {Type: "text", Selector: "#openpoet-markdown > p", Exact: "second line with bold text."},
		"removed": {Type: "text", Selector: "#openpoet-markdown > p", Exact: "See https://example.com and old."},
		"heading": {Type: "element", Selector: "#steps--risks", Tag: "h2", Label: "Steps & risks"},
	}
	ids := map[string]int64{}
	for name, anchor := range notes {
		view, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{ArtifactID: id, Version: 1, Anchor: anchor, Body: name, Authorization: user})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ids[name] = view.ID
	}
	if _, err := service.Patch(ctx, PatchArtifactCommand{ArtifactID: id, BaseVersion: 1, Authorization: auth,
		Edits: []ArtifactEdit{{Old: "See https://example.com and ~~old~~.\n", New: "Nothing else.\n"}}}); err != nil {
		t.Fatal(err)
	}
	views, err := service.ListAnnotationsWithPlacement(ctx, id, "open")
	if err != nil {
		t.Fatal(err)
	}
	found := map[int64]*ArtifactAnnotationPlacement{}
	for i := range views {
		found[views[i].ID] = views[i].Placement
	}
	for name, want := range map[string]bool{"kept": true, "removed": false, "heading": true} {
		placement := found[ids[name]]
		if placement == nil || !placement.Checked || placement.Found != want || placement.Version != 2 {
			t.Fatalf("%s: placement = %+v, want found=%v on version 2", name, placement, want)
		}
	}
}
