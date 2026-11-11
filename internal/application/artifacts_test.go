package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
)

const artifactTestHTML = `<!doctype html><html><head><style>body{font-family:sans-serif}</style></head><body><h1>Plan</h1><script>document.title='x'</script></body></html>`

func artifactTestService(t *testing.T) (*ArtifactService, *database.DB, *recordingApplicationEffects) {
	t.Helper()
	db := applicationTestDB(t)
	effects := &recordingApplicationEffects{}
	return NewArtifactService(db, effects), db, effects
}

func TestArtifactCreateDerivesLinksFromSessionAndTask(t *testing.T) {
	ctx := context.Background()
	service, db, effects := artifactTestService(t)
	project := createApplicationProject(t, db, "artifact-links")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Doc task", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, &database.Session{ID: "sess-art", ProjectID: project.ID, Status: "running", Name: "s", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkSessionToTask(ctx, "sess-art", task.ID); err != nil {
		t.Fatal(err)
	}

	// Only the session is given: project and task come from it.
	result, err := service.Create(ctx, CreateArtifactCommand{Title: "  Plan  ", Content: artifactTestHTML, SessionID: "sess-art", Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	view := result.Artifact
	if !result.Created || result.Version != 1 || result.Redacted || view.Title != "Plan" || view.CurrentVersion != 1 ||
		view.ProjectID == nil || *view.ProjectID != project.ID || view.TaskID == nil || *view.TaskID != task.ID ||
		view.SessionID != "sess-art" || view.CreatedBy != "automation:assistant" || view.SizeBytes != int64(len(artifactTestHTML)) {
		t.Fatalf("create result = %+v", result)
	}
	if len(effects.changes) != 1 || effects.changes[0].Domain != "artifacts" || effects.changes[0].Action != "created" ||
		effects.changes[0].Meta["artifact_id"] != view.ID {
		t.Fatalf("effects = %+v", effects.changes)
	}

	// An explicit task wins and supplies the project; an unknown session is
	// kept as the label it is, without failing the publish.
	other, err := service.Create(ctx, CreateArtifactCommand{Content: artifactTestHTML, TaskID: &task.ID, SessionID: "unknown-session", Authorization: phase4Authorization()})
	if err != nil || other.Artifact.Title != "Artifact" || other.Artifact.ProjectID == nil || *other.Artifact.ProjectID != project.ID {
		t.Fatalf("task-linked create = %+v err=%v", other, err)
	}

	missing := int64(9999)
	if _, err := service.Create(ctx, CreateArtifactCommand{Content: artifactTestHTML, TaskID: &missing, Authorization: phase4Authorization()}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("missing task err = %v", err)
	}
	if _, err := service.Create(ctx, CreateArtifactCommand{Content: artifactTestHTML, ProjectID: &missing, Authorization: phase4Authorization()}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("missing project err = %v", err)
	}
	otherProject := createApplicationProject(t, db, "artifact-other")
	if _, err := service.Create(ctx, CreateArtifactCommand{Content: artifactTestHTML, ProjectID: &otherProject.ID, TaskID: &task.ID, Authorization: phase4Authorization()}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("task/project mismatch err = %v", err)
	}
	if _, err := service.Create(ctx, CreateArtifactCommand{Content: artifactTestHTML}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("missing actor err = %v", err)
	}

	listed, err := service.List(ctx, ArtifactListQuery{ProjectID: project.ID})
	if err != nil || len(listed) != 2 {
		t.Fatalf("project listing = %+v err=%v", listed, err)
	}
	bySession, err := service.List(ctx, ArtifactListQuery{SessionID: "sess-art"})
	if err != nil || len(bySession) != 1 || bySession[0].ID != view.ID || bySession[0].SizeBytes == 0 {
		t.Fatalf("session listing = %+v err=%v", bySession, err)
	}
}

func TestArtifactContentIsBoundedHTMLAndRedacted(t *testing.T) {
	ctx := context.Background()
	service, db, _ := artifactTestService(t)
	auth := phase4Authorization()

	for name, content := range map[string]string{
		"empty":     "   ",
		"markdown":  "# Plan\n\n- not html",
		"null byte": "<html>\x00</html>",
		"bad utf8":  "<html>\xff\xfe</html>",
		"too large": "<html>" + strings.Repeat("x", MaxArtifactHTMLBytes) + "</html>",
	} {
		if _, err := service.Create(ctx, CreateArtifactCommand{Content: content, Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
			t.Fatalf("%s: expected validation error, got %v", name, err)
		}
	}

	// Exactly at the limit is accepted.
	atLimit := "<p>" + strings.Repeat("x", MaxArtifactHTMLBytes-7) + "</p>"
	if len(atLimit) != MaxArtifactHTMLBytes {
		t.Fatalf("fixture size = %d", len(atLimit))
	}
	if _, err := service.Create(ctx, CreateArtifactCommand{Content: atLimit, Authorization: auth}); err != nil {
		t.Fatalf("content at the limit was rejected: %v", err)
	}

	result, err := service.Create(ctx, CreateArtifactCommand{
		Title: "Leak", Content: "<html><body><pre>api_key=artifact-secret-value</pre></body></html>", Authorization: auth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Redacted {
		t.Fatal("publish result did not report the redaction")
	}
	stored, err := db.GetArtifactVersion(ctx, result.Artifact.ID, 1)
	if err != nil || strings.Contains(stored.Content, "artifact-secret-value") || !strings.Contains(stored.Content, "[REDACTED]") {
		t.Fatalf("secret was persisted: %+v err=%v", stored, err)
	}
	if stored.SizeBytes != int64(len(stored.Content)) || stored.SHA256 != artifactContentHash(stored.Content) {
		t.Fatalf("size/hash do not describe the stored HTML: %+v", stored)
	}
}

func TestArtifactUpdateVersionsConflictAndRead(t *testing.T) {
	ctx := context.Background()
	service, _, effects := artifactTestService(t)
	auth := phase4Authorization()

	created, err := service.Create(ctx, CreateArtifactCommand{Title: "Doc", Content: artifactTestHTML, Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Artifact.ID
	v2HTML := strings.Replace(artifactTestHTML, "<h1>Plan</h1>", "<h1>Plan v2</h1>", 1)

	updated, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: v2HTML, ChangeNote: "retitled", BaseVersion: 1, SessionID: "sess-u", Authorization: auth})
	if err != nil || !updated.Created || updated.Version != 2 || updated.Artifact.CurrentVersion != 2 {
		t.Fatalf("update = %+v err=%v", updated, err)
	}
	// Publishing the same HTML again stores nothing and emits nothing.
	again, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: v2HTML, Authorization: auth})
	if err != nil || again.Created || again.Version != 2 {
		t.Fatalf("identical update = %+v err=%v", again, err)
	}
	// A stale base version is a conflict that names the current version.
	_, err = service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: artifactTestHTML + "<!-- x -->", BaseVersion: 1, Authorization: auth})
	if !ErrorIsKind(err, ErrorConflict) || ErrorCode(err) != "artifact_version_conflict" || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("conflict err = %v", err)
	}
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: "missing", Content: v2HTML, Authorization: auth}); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("missing artifact err = %v", err)
	}
	if _, err := service.Update(ctx, UpdateArtifactCommand{ArtifactID: id, Content: "plain text", Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("non-HTML update err = %v", err)
	}
	if len(effects.changes) != 2 || effects.changes[1].Action != "version_published" || effects.changes[1].Meta["version"] != 2 {
		t.Fatalf("effects = %+v", effects.changes)
	}

	view, err := service.Get(ctx, id)
	if err != nil || view.CurrentVersion != 2 || len(view.Versions) != 2 || view.Versions[0].Version != 2 ||
		view.Versions[0].ChangeNote != "retitled" || view.Versions[0].Content != "" || view.SizeBytes != int64(len(v2HTML)) {
		t.Fatalf("get = %+v err=%v", view, err)
	}
	current, err := service.GetVersion(ctx, id, 0)
	if err != nil || current.Version != 2 || current.Content != v2HTML {
		t.Fatalf("current version = %+v err=%v", current, err)
	}
	first, err := service.GetVersion(ctx, id, 1)
	if err != nil || first.Content != artifactTestHTML {
		t.Fatalf("version 1 = %+v err=%v", first, err)
	}
	if _, err := service.GetVersion(ctx, id, 5); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("missing version err = %v", err)
	}
	if _, err := service.Get(ctx, "nope"); !ErrorIsKind(err, ErrorNotFound) {
		t.Fatalf("missing artifact get err = %v", err)
	}

	archived, title := "archived", "Renamed"
	meta, err := service.UpdateMeta(ctx, UpdateArtifactMetaCommand{ArtifactID: id, Title: &title, Status: &archived, Authorization: auth})
	if err != nil || meta.Title != "Renamed" || meta.Status != "archived" {
		t.Fatalf("meta = %+v err=%v", meta, err)
	}
	bogus := "deleted"
	if _, err := service.UpdateMeta(ctx, UpdateArtifactMetaCommand{ArtifactID: id, Status: &bogus, Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("bogus status err = %v", err)
	}
	if listed, err := service.List(ctx, ArtifactListQuery{}); err != nil || len(listed) != 0 {
		t.Fatalf("archived artifact is still listed: %+v err=%v", listed, err)
	}
	if listed, err := service.List(ctx, ArtifactListQuery{IncludeArchived: true}); err != nil || len(listed) != 1 {
		t.Fatalf("include_archived listing = %+v err=%v", listed, err)
	}
}
