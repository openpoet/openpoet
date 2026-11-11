package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
)

func TestArtifactMaskingReportsEachPlaceByLineAndKind(t *testing.T) {
	// None of this is a credential. The key markers are written in two pieces
	// so the file itself does not look like it holds a private key.
	content := strings.Join([]string{
		"<!doctype html>",                                  // 1
		"<html><body>",                                     // 2
		"<p>password=hunter2hunter2</p>",                   // 3
		"<pre>Authorization: Bearer abc.def.ghi</pre>",     // 4
		"<p>ordinary text, with the word secret in it</p>", // 5
		"<script>const cfg = { api_key:",                   // 6: the match runs on to line 7
		"  'k-123456' };</script>",                         // 7
		"<pre>-----BEGIN RSA " + "PRIVATE KEY-----",        // 8
		"MIIEow==", // 9
		"-----END RSA " + "PRIVATE KEY-----</pre>", // 10
		"<p>Access_Token: zzz</p>",                 // 11
		"</body></html>",                           // 12
	}, "\n")
	masked, redactions := maskArtifactSecrets(content)
	if masked != redactReportSecrets(content) {
		t.Fatalf("masking differs from the shared redaction:\n%s", masked)
	}
	for _, leaked := range []string{"hunter2", "abc.def.ghi", "k-123456", "MIIEow", "zzz"} {
		if strings.Contains(masked, leaked) {
			t.Fatalf("%q survived masking", leaked)
		}
	}
	got := map[int]string{}
	for _, redaction := range redactions {
		got[redaction.Line] = redaction.Kind
	}
	want := map[int]string{3: "password", 4: "authorization bearer", 6: "api_key", 8: "private key", 11: "access_token"}
	if len(redactions) != len(want) {
		t.Fatalf("redactions = %+v, want %v", redactions, want)
	}
	for line, kind := range want {
		if got[line] != kind {
			t.Fatalf("line %d: kind %q, want %q (all: %+v)", line, got[line], kind, redactions)
		}
	}

	// Masking again changes nothing and reports nothing: a patch of a document
	// that was already masked does not count its old masks again.
	if again, more := maskArtifactSecrets(masked); again != masked || len(more) != 0 {
		t.Fatalf("masking is not idempotent: %+v", more)
	}
	if same, none := maskArtifactSecrets("<p>nothing to hide</p>"); same != "<p>nothing to hide</p>" || none != nil {
		t.Fatalf("clean text reported %+v", none)
	}
}

func TestArtifactPublishReportsMaskingAndDryRunStoresNothing(t *testing.T) {
	ctx := context.Background()
	service, _, effects := artifactTestService(t)
	auth := phase4Authorization()
	html := "<!doctype html>\n<html><body>\n<p>secret: s3cr3tvalue</p>\n</body></html>\n"

	// A dry run of a create: accepted, measured, masking reported, nothing stored.
	dry, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: html, DryRun: true, Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Created || dry.Artifact.ID != "" || dry.RedactionCount != 1 || len(dry.Redactions) != 1 ||
		dry.Redactions[0] != (ArtifactRedaction{Line: 3, Kind: "secret"}) || dry.SHA256 == "" || dry.SizeBytes == 0 {
		t.Fatalf("dry run = %+v", dry)
	}
	if list, err := service.List(ctx, ArtifactListQuery{}); err != nil || len(list) != 0 || len(effects.changes) != 0 {
		t.Fatalf("a dry run stored something: %d artifacts, %d changes, err=%v", len(list), len(effects.changes), err)
	}
	if _, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: "plain text", DryRun: true, Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("a dry run accepted a non-HTML document: %v", err)
	}

	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: html, Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || !created.Redacted || created.RedactionCount != 1 || created.SHA256 != dry.SHA256 {
		t.Fatalf("create = %+v", created)
	}
	id := created.Artifact.ID

	// A dry run of an update: the version it would create, or none, or the conflict.
	next := strings.Replace(html, "</body>", "<p>more</p></body>", 1)
	would, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: next, BaseVersion: 1, DryRun: true, Authorization: auth})
	if err != nil || !would.DryRun || !would.Created || would.Version != 2 || would.Artifact.ID != id {
		t.Fatalf("dry update = %+v err=%v", would, err)
	}
	same, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: html, DryRun: true, Authorization: auth})
	if err != nil || !same.DryRun || same.Created || same.Version != 1 {
		t.Fatalf("dry update with identical html = %+v err=%v", same, err)
	}
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: next, BaseVersion: 7, DryRun: true, Authorization: auth}); !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("dry update on a stale base: %v", err)
	}
	patched, err := service.Patch(ctx, PatchArtifactCommand{ArtifactID: id, BaseVersion: 1, DryRun: true, Authorization: auth,
		Edits: []ArtifactEdit{{Old: "</body>", New: "<p>token</p><p>password = abc12345</p></body>"}}})
	if err != nil || !patched.DryRun || patched.Version != 2 || patched.Replacements != 1 || patched.RedactionCount != 1 || patched.Redactions[0].Kind != "password" {
		t.Fatalf("dry patch = %+v err=%v", patched, err)
	}
	if view, err := service.Get(ctx, id); err != nil || view.CurrentVersion != 1 || len(view.Versions) != 1 {
		t.Fatalf("a dry run published a version: %+v err=%v", view, err)
	}

	// Many masked values: the count is exact, the list is bounded.
	var many strings.Builder
	many.WriteString("<html><body>\n")
	for i := 0; i < maxArtifactRedactionsReported+7; i++ {
		many.WriteString("<p>password=abcdefgh</p>\n")
	}
	many.WriteString("</body></html>")
	big, err := service.Create(ctx, CreateArtifactCommand{Title: "Many", Content: many.String(), DryRun: true, Authorization: auth})
	if err != nil || big.RedactionCount != maxArtifactRedactionsReported+7 || len(big.Redactions) != maxArtifactRedactionsReported {
		t.Fatalf("many redactions: count=%d listed=%d err=%v", big.RedactionCount, len(big.Redactions), err)
	}
}

func TestArtifactUpdateCarriesTitleAndSummary(t *testing.T) {
	ctx := context.Background()
	service, _, _ := artifactTestService(t)
	auth := phase4Authorization()
	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Draft", Summary: "first", Content: artifactTestHTML, Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Artifact.ID
	title, summary := "  Rollout\nplan ", "second"
	updated, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: artifactTestHTML + "<p>v2</p>", BaseVersion: 1,
		Title: &title, Summary: &summary, Authorization: auth})
	if err != nil || updated.Version != 2 || updated.Artifact.Title != "Rollout plan" || updated.Artifact.Summary != "second" {
		t.Fatalf("update with title = %+v err=%v", updated.Artifact, err)
	}
	// With identical HTML the title still changes; only the summary when only it is given.
	renamed := "Final plan"
	kept, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: artifactTestHTML + "<p>v2</p>", Title: &renamed, Authorization: auth})
	if err != nil || kept.Created || kept.Version != 2 || kept.Artifact.Title != "Final plan" || kept.Artifact.Summary != "second" {
		t.Fatalf("title with identical html = %+v err=%v", kept, err)
	}
	patchSummary := "third"
	patched, err := service.Patch(ctx, PatchArtifactCommand{ArtifactID: id, BaseVersion: 2, Summary: &patchSummary, Authorization: auth,
		Edits: []ArtifactEdit{{Old: "<p>v2</p>", New: "<p>v3</p>"}}})
	if err != nil || patched.Version != 3 || patched.Artifact.Title != "Final plan" || patched.Artifact.Summary != "third" {
		t.Fatalf("patch with summary = %+v err=%v", patched.Artifact, err)
	}
	// An empty title is refused before anything is published; a dry run changes nothing.
	empty := "   "
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: artifactTestHTML + "<p>v4</p>", Title: &empty, Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("empty title: %v", err)
	}
	dryTitle := "Not applied"
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: artifactTestHTML + "<p>v4</p>", Title: &dryTitle, DryRun: true, Authorization: auth}); err != nil {
		t.Fatal(err)
	}
	if view, err := service.Get(ctx, id); err != nil || view.CurrentVersion != 3 || view.Title != "Final plan" {
		t.Fatalf("after refused and dry updates: %+v err=%v", view, err)
	}
}

func TestArtifactCreateRepeatedByTheSameSessionReturnsTheFirst(t *testing.T) {
	ctx := context.Background()
	service, db, effects := artifactTestService(t)
	project := createApplicationProject(t, db, "artifact-retry")
	for _, sessionID := range []string{"sess-a", "sess-b"} {
		if err := db.CreateSession(ctx, &database.Session{ID: sessionID, ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	auth := phase4Authorization()
	create := func(sessionID, title, html string) ArtifactPublishResult {
		t.Helper()
		result, err := service.Create(ctx, CreateArtifactCommand{Title: title, Content: html, SessionID: sessionID, Authorization: auth})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := create("sess-a", "Plan", artifactTestHTML)
	if !first.Created || first.Duplicate {
		t.Fatalf("first create = %+v", first)
	}
	changes := len(effects.changes)

	// The retry: same session, same title, same HTML.
	retry := create("sess-a", " Plan ", artifactTestHTML)
	if retry.Created || !retry.Duplicate || retry.Artifact.ID != first.Artifact.ID || retry.Version != 1 || retry.SHA256 != first.SHA256 || len(effects.changes) != changes {
		t.Fatalf("retry = %+v (changes %d -> %d)", retry, changes, len(effects.changes))
	}
	// It still is that artifact after it moved on.
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: first.Artifact.ID, Content: artifactTestHTML + "<p>v2</p>", Authorization: auth}); err != nil {
		t.Fatal(err)
	}
	if again := create("sess-a", "Plan", artifactTestHTML); !again.Duplicate || again.Artifact.ID != first.Artifact.ID || again.Version != 2 {
		t.Fatalf("retry after an update = %+v", again)
	}

	// Anything that differs is a new artifact.
	for name, result := range map[string]ArtifactPublishResult{
		"another session": create("sess-b", "Plan", artifactTestHTML),
		"another title":   create("sess-a", "Plan B", artifactTestHTML),
		"another html":    create("sess-a", "Plan", artifactTestHTML+"<p>x</p>"),
		"no session":      create("", "Plan", artifactTestHTML),
	} {
		if !result.Created || result.Duplicate || result.Artifact.ID == first.Artifact.ID {
			t.Fatalf("%s: %+v", name, result)
		}
	}
	// An archived artifact and one older than the window are not returned.
	archived := "archived"
	if _, err := service.UpdateMeta(ctx, UpdateArtifactMetaCommand{ArtifactID: first.Artifact.ID, Status: &archived, Authorization: auth}); err != nil {
		t.Fatal(err)
	}
	afterArchive := create("sess-a", "Plan", artifactTestHTML)
	if !afterArchive.Created || afterArchive.Artifact.ID == first.Artifact.ID {
		t.Fatalf("an archived artifact was returned: %+v", afterArchive)
	}
	if _, err := db.Exec(`UPDATE artifacts SET created_at = datetime('now', '-11 minutes') WHERE id = ?`, afterArchive.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	if late := create("sess-a", "Plan", artifactTestHTML); !late.Created || late.Artifact.ID == afterArchive.Artifact.ID {
		t.Fatalf("an artifact older than the window was returned: %+v", late)
	}
}

func TestArtifactAnnotationKeepsItsPanelAndReportsPlacement(t *testing.T) {
	ctx := context.Background()
	service, db, _ := artifactTestService(t)
	user := ActionAuthorization{Actor: Actor{Type: "user", ID: "ui"}}
	agent := phase4Authorization()
	v1 := `<!doctype html><html><body>
<section id="intro"><h1>Rollout &amp; rollback</h1><p>The   <b>first</b> stage takes a week.</p></section>
<div data-id="TM8" class="panel"><button>Launch</button><p>Budget is 40k.</p></div>
<div data-id="TM9" class="panel"><button>Hold</button></div>
<script>const hidden = "only in a script";</script>
</body></html>`
	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Doc", Content: v1, Authorization: agent})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Artifact.ID
	note := func(anchor ArtifactAnchor) ArtifactAnnotationView {
		t.Helper()
		view, err := service.CreateAnnotation(ctx, CreateArtifactAnnotationCommand{ArtifactID: id, Anchor: anchor, Body: "note", Authorization: user})
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	// The mark of the panel is stored in its known forms only.
	kept := note(ArtifactAnchor{Type: "text", Selector: "#intro > p:nth-of-type(1)", Exact: "The first stage takes a week.", Near: "#intro"})
	budget := note(ArtifactAnchor{Type: "text", Selector: `[data-id="TM8"] > p:nth-of-type(1)`, Exact: "Budget is 40k.", Near: `[data-id="TM8"]`})
	launch := note(ArtifactAnchor{Type: "element", Selector: `[data-id="TM8"] > button:nth-of-type(1)`, Tag: "button", Label: "Launch", Near: `[data-id="TM8"]`})
	hold := note(ArtifactAnchor{Type: "element", Selector: `[data-id="TM9"] > button:nth-of-type(1)`, Tag: "button", Label: "Hold", Near: `[data-id="TM9"]`})
	loose := note(ArtifactAnchor{Type: "element", Selector: "body > div:nth-of-type(1)", Tag: "div", Near: `#x"] , body`})
	entity := note(ArtifactAnchor{Type: "text", Selector: "#intro > h1", Exact: "Rollout & rollback"})
	if kept.Anchor.Near != "#intro" || budget.Anchor.Near != `[data-id="TM8"]` || loose.Anchor.Near != "" {
		t.Fatalf("near = %q, %q, %q", kept.Anchor.Near, budget.Anchor.Near, loose.Anchor.Near)
	}

	// On the version they were made on there is nothing to report.
	views, err := service.ListAnnotationsWithPlacement(ctx, id, "open")
	if err != nil || len(views) != 6 {
		t.Fatalf("views=%d err=%v", len(views), err)
	}
	for _, view := range views {
		if view.Placement != nil || view.ResolvedOnVersion != 0 {
			t.Fatalf("annotation #%d on the current version has placement %+v", view.ID, view.Placement)
		}
	}

	// v2 rewrites the budget sentence, drops panel TM9 and keeps the rest.
	v2 := `<!doctype html><html><body>
<section id="intro"><h1>Rollout &amp; rollback</h1><p>The <b>first</b>
 stage takes a week.</p></section>
<div data-id='TM8' class="panel"><button>Launch</button><p>Budget is 55k.</p></div>
<script>const hidden = "Budget is 40k.";</script>
</body></html>`
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: v2, BaseVersion: 1, Authorization: agent}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveAnnotation(ctx, ResolveArtifactAnnotationCommand{ArtifactID: id, AnnotationID: budget.ID, ResolutionNote: "raised", Authorization: agent}); err != nil {
		t.Fatal(err)
	}
	// Resolved while v2 was current; v3 comes later.
	if _, err := db.Exec(`UPDATE artifact_versions SET created_at = datetime('now', '-2 hours') WHERE artifact_id = ? AND version = 1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifact_versions SET created_at = datetime('now', '-1 hours') WHERE artifact_id = ? AND version = 2`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifact_annotations SET resolved_at = datetime('now', '-30 minutes') WHERE id = ?`, budget.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: v2 + "\n<!-- v3 -->", BaseVersion: 2, Authorization: agent}); err != nil {
		t.Fatal(err)
	}

	views, err = service.ListAnnotationsWithPlacement(ctx, id, "all")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]ArtifactAnnotationView{}
	for _, view := range views {
		byID[view.ID] = view
	}
	for name, tc := range map[string]struct {
		id             int64
		checked, found bool
	}{
		"text kept across tags and line breaks":    {kept.ID, true, true},
		"text rewritten (a script does not count)": {budget.ID, true, false},
		"element in a panel that is still there":   {launch.ID, true, true},
		"element in a panel that was dropped":      {hold.ID, true, false},
		"element with nothing to look for":         {loose.ID, false, false},
		"text with an entity":                      {entity.ID, true, true},
	} {
		placement := byID[tc.id].Placement
		if placement == nil || placement.Version != 3 || placement.Checked != tc.checked || placement.Found != tc.found {
			t.Fatalf("%s: placement = %+v", name, placement)
		}
	}
	if byID[budget.ID].ResolvedOnVersion != 2 || byID[kept.ID].ResolvedOnVersion != 0 {
		t.Fatalf("resolved on v%d (open one: v%d)", byID[budget.ID].ResolvedOnVersion, byID[kept.ID].ResolvedOnVersion)
	}
	// The plain listing, which the viewer uses, does not read the HTML.
	plain, err := service.ListAnnotations(ctx, id, "all")
	if err != nil || len(plain) != 6 || plain[0].Placement != nil {
		t.Fatalf("plain listing: %d err=%v", len(plain), err)
	}
}
