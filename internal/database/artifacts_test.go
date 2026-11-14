package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestArtifact(id string, projectID int64, sessionID string) (*Artifact, *ArtifactVersion) {
	artifact := &Artifact{ID: id, Title: "Plan " + id, SessionID: sessionID, CreatedBy: "session:" + sessionID}
	if projectID > 0 {
		artifact.ProjectID = sql.NullInt64{Int64: projectID, Valid: true}
	}
	html := "<html><body>" + id + " v1</body></html>"
	return artifact, &ArtifactVersion{Content: html, SizeBytes: int64(len(html)), SHA256: "hash-" + id + "-1", CreatedBy: artifact.CreatedBy, SessionID: sessionID}
}

func TestMigrationV81CreatesArtifactTables(t *testing.T) {
	db := setupTestDB(t)
	for _, table := range []string{"artifacts", "artifact_versions", "artifact_annotations"} {
		var name string
		if err := db.Get(&name, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table); err != nil {
			t.Fatalf("table %s missing after migrations: %v", table, err)
		}
	}
	var version int
	if err := db.Get(&version, `SELECT version FROM schema_version LIMIT 1`); err != nil || version < 82 {
		t.Fatalf("schema version = %d (err %v), want >= 82", version, err)
	}
}

func TestArtifactVersioningConflictAndDedup(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	artifact, v1 := newTestArtifact("art00001", 0, "sess-a")
	if err := db.CreateArtifact(ctx, artifact, v1); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetArtifact(ctx, "art00001")
	if err != nil || stored.CurrentVersion != 1 || stored.Status != "active" {
		t.Fatalf("stored artifact = %+v err=%v", stored, err)
	}

	// Stale base version: nothing is written, the current state is reported.
	stale := &ArtifactVersion{Content: "<p>stale</p>", SHA256: "hash-stale"}
	current, created, err := db.AppendArtifactVersion(ctx, "art00001", 7, stale)
	if !errors.Is(err, ErrArtifactVersionConflict) || created || current == nil || current.CurrentVersion != 1 {
		t.Fatalf("stale append: current=%+v created=%v err=%v", current, created, err)
	}

	// Identical HTML (same hash as the current version) is not stored again.
	same := &ArtifactVersion{Content: v1.Content, SHA256: v1.SHA256}
	current, created, err = db.AppendArtifactVersion(ctx, "art00001", 1, same)
	if err != nil || created || current.CurrentVersion != 1 {
		t.Fatalf("identical append: current=%+v created=%v err=%v", current, created, err)
	}

	v2 := &ArtifactVersion{Content: "<p>v2</p>", SizeBytes: 9, SHA256: "hash-2", ChangeNote: "second", CreatedBy: "session:sess-b", SessionID: "sess-b"}
	current, created, err = db.AppendArtifactVersion(ctx, "art00001", 1, v2)
	if err != nil || !created || current.CurrentVersion != 2 {
		t.Fatalf("append v2: current=%+v created=%v err=%v", current, created, err)
	}
	// base version 0 means "whatever is current".
	v3 := &ArtifactVersion{Content: "<p>v3</p>", SizeBytes: 9, SHA256: "hash-3"}
	if current, created, err = db.AppendArtifactVersion(ctx, "art00001", 0, v3); err != nil || !created || current.CurrentVersion != 3 {
		t.Fatalf("append v3: current=%+v created=%v err=%v", current, created, err)
	}

	versions, err := db.ListArtifactVersions(ctx, "art00001")
	if err != nil || len(versions) != 3 || versions[0].Version != 3 || versions[2].Version != 1 {
		t.Fatalf("versions = %+v err=%v", versions, err)
	}
	if versions[0].Content != "" {
		t.Fatal("version listing must not carry the HTML")
	}
	// Every version stays readable with its own HTML.
	first, err := db.GetArtifactVersion(ctx, "art00001", 1)
	if err != nil || first.Content != v1.Content {
		t.Fatalf("v1 = %+v err=%v", first, err)
	}
	second, err := db.GetArtifactVersion(ctx, "art00001", 2)
	if err != nil || second.Content != "<p>v2</p>" || second.ChangeNote != "second" || second.SessionID != "sess-b" {
		t.Fatalf("v2 = %+v err=%v", second, err)
	}
	if _, err := db.GetArtifactVersion(ctx, "art00001", 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing version err = %v", err)
	}
	if _, _, err := db.AppendArtifactVersion(ctx, "missing", 0, v3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("append to missing artifact err = %v", err)
	}
}

func TestListArtifactsFiltersAndMeta(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	a1, v := newTestArtifact("art0000a", 1, "sess-a")
	a1.TaskID = sql.NullInt64{Int64: 5, Valid: true}
	if err := db.CreateArtifact(ctx, a1, v); err != nil {
		t.Fatal(err)
	}
	a2, v := newTestArtifact("art0000b", 1, "sess-b")
	if err := db.CreateArtifact(ctx, a2, v); err != nil {
		t.Fatal(err)
	}
	a3, v := newTestArtifact("art0000c", 2, "sess-c")
	if err := db.CreateArtifact(ctx, a3, v); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifact_annotations (artifact_id, version, body, status) VALUES ('art0000a', 1, 'fix', 'open'), ('art0000a', 1, 'done', 'resolved')`); err != nil {
		t.Fatal(err)
	}

	count := func(filter ArtifactFilter) int {
		t.Helper()
		items, err := db.ListArtifacts(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	if got := count(ArtifactFilter{}); got != 3 {
		t.Fatalf("all = %d", got)
	}
	if got := count(ArtifactFilter{ProjectID: 1}); got != 2 {
		t.Fatalf("project 1 = %d", got)
	}
	if got := count(ArtifactFilter{SessionID: "sess-c"}); got != 1 {
		t.Fatalf("session c = %d", got)
	}
	items, err := db.ListArtifacts(ctx, ArtifactFilter{TaskID: 5})
	if err != nil || len(items) != 1 || items[0].ID != "art0000a" || items[0].OpenAnnotations != 1 || items[0].SizeBytes == 0 {
		t.Fatalf("task 5 = %+v err=%v", items, err)
	}

	archived, title := "archived", "Renamed"
	if err := db.UpdateArtifactMeta(ctx, "art0000b", &title, nil, &archived); err != nil {
		t.Fatal(err)
	}
	if got := count(ArtifactFilter{ProjectID: 1}); got != 1 {
		t.Fatalf("project 1 without archived = %d", got)
	}
	if got := count(ArtifactFilter{ProjectID: 1, IncludeArchived: true}); got != 2 {
		t.Fatalf("project 1 with archived = %d", got)
	}
	renamed, err := db.GetArtifact(ctx, "art0000b")
	if err != nil || renamed.Title != "Renamed" || renamed.Status != "archived" {
		t.Fatalf("renamed = %+v err=%v", renamed, err)
	}
	if err := db.UpdateArtifactMeta(ctx, "missing", &title, nil, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("meta update on missing artifact err = %v", err)
	}
}

func TestArtifactAnnotationLifecycle(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artifact, v := newTestArtifact("art0000n", 1, "sess-n")
	if err := db.CreateArtifact(ctx, artifact, v); err != nil {
		t.Fatal(err)
	}

	first := &ArtifactAnnotation{ArtifactID: "art0000n", Version: 1, AnchorJSON: `{"type":"text","exact":"quote"}`, Quote: "quote", Body: "fix this", CreatedBy: "user:ui"}
	second := &ArtifactAnnotation{ArtifactID: "art0000n", Version: 1, Body: "and this", CreatedBy: "user:ui"}
	for _, annotation := range []*ArtifactAnnotation{first, second} {
		if err := db.CreateArtifactAnnotation(ctx, annotation); err != nil {
			t.Fatal(err)
		}
	}
	if first.ID == 0 || second.ID <= first.ID {
		t.Fatalf("annotation ids = %d, %d", first.ID, second.ID)
	}
	stored, err := db.GetArtifactAnnotation(ctx, second.ID)
	if err != nil || stored.Status != "open" || stored.AnchorJSON != "{}" || stored.ResolvedAt.Valid {
		t.Fatalf("stored annotation = %+v err=%v", stored, err)
	}
	if count, err := db.CountOpenArtifactAnnotations(ctx, "art0000n"); err != nil || count != 2 {
		t.Fatalf("open count = %d err=%v", count, err)
	}

	if err := db.ResolveArtifactAnnotation(ctx, first.ID, "done in v2", "session:sess-n"); err != nil {
		t.Fatal(err)
	}
	resolved, err := db.GetArtifactAnnotation(ctx, first.ID)
	if err != nil || resolved.Status != "resolved" || resolved.ResolutionNote != "done in v2" || resolved.ResolvedBy != "session:sess-n" || !resolved.ResolvedAt.Valid {
		t.Fatalf("resolved annotation = %+v err=%v", resolved, err)
	}
	listed := func(status string) []int64 {
		t.Helper()
		annotations, err := db.ListArtifactAnnotations(ctx, "art0000n", status)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, 0, len(annotations))
		for _, annotation := range annotations {
			ids = append(ids, annotation.ID)
		}
		return ids
	}
	if open, done, all := listed("open"), listed("resolved"), listed(""); len(open) != 1 || open[0] != second.ID || len(done) != 1 || done[0] != first.ID || len(all) != 2 || all[0] != first.ID {
		t.Fatalf("open=%v resolved=%v all=%v", open, done, all)
	}
	if items, err := db.ListArtifacts(ctx, ArtifactFilter{SessionID: "sess-n"}); err != nil || len(items) != 1 || items[0].OpenAnnotations != 1 {
		t.Fatalf("listing open annotations = %+v err=%v", items, err)
	}

	if err := db.ReopenArtifactAnnotation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.GetArtifactAnnotation(ctx, first.ID)
	if err != nil || reopened.Status != "open" || reopened.ResolutionNote != "" || reopened.ResolvedBy != "" || reopened.ResolvedAt.Valid {
		t.Fatalf("reopened annotation = %+v err=%v", reopened, err)
	}
	// "Send to agent": only the open annotations named are marked, with the
	// session they were sent to. Reopening one clears the mark.
	if marked, err := db.MarkArtifactAnnotationsSent(ctx, "art0000n", nil, "sess-n"); err != nil || marked != 0 {
		t.Fatalf("marking nothing = %d err=%v", marked, err)
	}
	if marked, err := db.MarkArtifactAnnotationsSent(ctx, "other-artifact", []int64{first.ID, second.ID}, "sess-n"); err != nil || marked != 0 {
		t.Fatalf("marking through another artifact = %d err=%v", marked, err)
	}
	if err := db.ResolveArtifactAnnotation(ctx, second.ID, "", "session:sess-n"); err != nil {
		t.Fatal(err)
	}
	if marked, err := db.MarkArtifactAnnotationsSent(ctx, "art0000n", []int64{first.ID, second.ID, 9999}, "sess-n"); err != nil || marked != 1 {
		t.Fatalf("marked = %d err=%v, want only the open annotation", marked, err)
	}
	sent, err := db.GetArtifactAnnotation(ctx, first.ID)
	if err != nil || !sent.SentAt.Valid || time.Since(sent.SentAt.Time) > time.Minute || sent.SentSessionID != "sess-n" {
		t.Fatalf("sent annotation = %+v err=%v", sent, err)
	}
	if unsent, err := db.GetArtifactAnnotation(ctx, second.ID); err != nil || unsent.SentAt.Valid || unsent.SentSessionID != "" {
		t.Fatalf("resolved annotation was marked as sent: %+v err=%v", unsent, err)
	}
	// Reopening an annotation that is already open is not a change to it.
	if err := db.ReopenArtifactAnnotation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if still, err := db.GetArtifactAnnotation(ctx, first.ID); err != nil || !still.SentAt.Valid || still.SentSessionID != "sess-n" {
		t.Fatalf("reopening an open annotation dropped the send record: %+v err=%v", still, err)
	}
	if err := db.ResolveArtifactAnnotation(ctx, first.ID, "done", "session:sess-n"); err != nil {
		t.Fatal(err)
	}
	if kept, err := db.GetArtifactAnnotation(ctx, first.ID); err != nil || !kept.SentAt.Valid || kept.SentSessionID != "sess-n" {
		t.Fatalf("resolving dropped the send record: %+v err=%v", kept, err)
	}
	if err := db.ReopenArtifactAnnotation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if cleared, err := db.GetArtifactAnnotation(ctx, first.ID); err != nil || cleared.SentAt.Valid || cleared.SentSessionID != "" {
		t.Fatalf("reopening kept the send record: %+v err=%v", cleared, err)
	}

	if err := db.ResolveArtifactAnnotation(ctx, 9999, "", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("resolve missing err = %v", err)
	}
	if err := db.ReopenArtifactAnnotation(ctx, 9999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reopen missing err = %v", err)
	}
	if _, err := db.GetArtifactAnnotation(ctx, 9999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get missing err = %v", err)
	}
}

func TestExtendToolPolicyWithArtifactTools(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		want    []string // names that must be in "allowed" afterwards
		absent  []string // names that must not be
		changed bool
	}{
		{
			name:    "session allowlist with both doc tools",
			policy:  `{"mode":"deny_all","allowed":["openpoet_create_document","openpoet_read_document","openpoet_get_my_task"]}`,
			want:    []string{"openpoet_get_my_task", "openpoet_create_artifact", "openpoet_update_artifact", "openpoet_resolve_artifact_annotation", "openpoet_read_artifact", "openpoet_list_artifacts", "openpoet_list_artifact_annotations"},
			absent:  []string{"create_artifact"},
			changed: true,
		},
		{
			name:    "read-only allowlist gets only the read tools",
			policy:  `{"mode":"deny_all","allowed":["openpoet_read_document"]}`,
			want:    []string{"openpoet_read_artifact", "openpoet_list_artifacts", "openpoet_list_artifact_annotations"},
			absent:  []string{"openpoet_create_artifact", "openpoet_update_artifact", "openpoet_resolve_artifact_annotation"},
			changed: true,
		},
		{
			name:    "chat policy uses unprefixed names",
			policy:  `{"mode":"custom","allowed":["create_document","list_tasks"],"denied":["delete_task"]}`,
			want:    []string{"list_tasks", "create_artifact", "update_artifact", "resolve_artifact_annotation"},
			absent:  []string{"openpoet_create_artifact", "read_artifact"},
			changed: true,
		},
		{name: "allowlist without doc tools is left alone", policy: `{"mode":"deny_all","allowed":["openpoet_list_tasks"]}`},
		{name: "allow_all needs nothing", policy: `{"mode":"allow_all","denied":["openpoet_delete_task"]}`},
		{name: "custom with only a deny list allows new tools already", policy: `{"mode":"custom","allowed":[],"denied":["openpoet_delete_task"]}`},
		{name: "deny everything stays deny everything", policy: `{"mode":"deny_all","allowed":[]}`},
		{name: "empty object", policy: `{}`},
		{name: "not JSON", policy: `not json`},
		{
			name:   "already extended is not changed again",
			policy: `{"mode":"deny_all","allowed":["openpoet_read_document","openpoet_read_artifact","openpoet_list_artifacts","openpoet_list_artifact_annotations"]}`,
		},
	}
	for _, tc := range cases {
		updated, changed := extendToolPolicyWithArtifactTools(tc.policy)
		if changed != tc.changed {
			t.Fatalf("%s: changed = %v, want %v (%s)", tc.name, changed, tc.changed, updated)
		}
		if !changed {
			if updated != tc.policy {
				t.Fatalf("%s: unchanged policy was rewritten: %s", tc.name, updated)
			}
			continue
		}
		var parsed struct {
			Mode    string   `json:"mode"`
			Allowed []string `json:"allowed"`
			Denied  []string `json:"denied"`
		}
		if err := json.Unmarshal([]byte(updated), &parsed); err != nil {
			t.Fatalf("%s: result is not JSON: %s", tc.name, updated)
		}
		allowed := map[string]int{}
		for _, name := range parsed.Allowed {
			allowed[name]++
		}
		for _, name := range tc.want {
			if allowed[name] != 1 {
				t.Fatalf("%s: %q appears %d times in %v", tc.name, name, allowed[name], parsed.Allowed)
			}
		}
		for _, name := range tc.absent {
			if allowed[name] != 0 {
				t.Fatalf("%s: %q must not be granted: %v", tc.name, name, parsed.Allowed)
			}
		}
		// Running it again changes nothing.
		if again, changedAgain := extendToolPolicyWithArtifactTools(updated); changedAgain || again != updated {
			t.Fatalf("%s: not idempotent: %s", tc.name, again)
		}
	}
	// The deny list and the mode of a changed policy are preserved.
	updated, _ := extendToolPolicyWithArtifactTools(`{"mode":"custom","allowed":["create_document"],"denied":["delete_task"]}`)
	var kept struct {
		Mode   string   `json:"mode"`
		Denied []string `json:"denied"`
	}
	if err := json.Unmarshal([]byte(updated), &kept); err != nil || kept.Mode != "custom" || len(kept.Denied) != 1 || kept.Denied[0] != "delete_task" {
		t.Fatalf("mode/denied not preserved: %s", updated)
	}
}

// TestMigrationV82ExtendsStoredAllowlists runs the migration against stored
// policies the way an upgraded install has them.
func TestMigrationV82ExtendsStoredAllowlists(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	restricted := &Project{Name: "restricted", Path: "/tmp/restricted", Type: "local"}
	open := &Project{Name: "open", Path: "/tmp/open", Type: "local"}
	for _, project := range []*Project{restricted, open} {
		if err := db.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE projects SET tool_policy = ? WHERE id = ?`,
		`{"mode":"deny_all","allowed":["openpoet_create_document","openpoet_read_document","openpoet_get_my_task"]}`, restricted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('mcp_tool_policy_session', ?), ('mcp_tool_policy_chat', ?), ('unrelated_setting', ?)`,
		`{"mode":"allow_all","denied":["openpoet_delete_task"]}`,
		`{"mode":"custom","allowed":["read_document"],"denied":[]}`,
		`{"mode":"deny_all","allowed":["openpoet_create_document"]}`); err != nil {
		t.Fatal(err)
	}

	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV82(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	read := func(query string, args ...any) string {
		t.Helper()
		var value string
		if err := db.Get(&value, query, args...); err != nil {
			t.Fatal(err)
		}
		return value
	}
	restrictedPolicy := read(`SELECT tool_policy FROM projects WHERE id = ?`, restricted.ID)
	for _, name := range []string{"openpoet_create_artifact", "openpoet_update_artifact", "openpoet_read_artifact", "openpoet_list_artifacts", "openpoet_list_artifact_annotations", "openpoet_resolve_artifact_annotation", "openpoet_get_my_task"} {
		if !strings.Contains(restrictedPolicy, `"`+name+`"`) {
			t.Fatalf("restricted project policy lacks %s: %s", name, restrictedPolicy)
		}
	}
	if policy := read(`SELECT tool_policy FROM projects WHERE id = ?`, open.ID); policy != "" {
		t.Fatalf("project without a policy got one: %s", policy)
	}
	if policy := read(`SELECT value FROM settings WHERE key = 'mcp_tool_policy_session'`); policy != `{"mode":"allow_all","denied":["openpoet_delete_task"]}` {
		t.Fatalf("allow_all global policy was rewritten: %s", policy)
	}
	chat := read(`SELECT value FROM settings WHERE key = 'mcp_tool_policy_chat'`)
	if !strings.Contains(chat, `"read_artifact"`) || strings.Contains(chat, `"create_artifact"`) {
		t.Fatalf("chat policy = %s", chat)
	}
	if policy := read(`SELECT value FROM settings WHERE key = 'unrelated_setting'`); strings.Contains(policy, "artifact") {
		t.Fatalf("a setting that is not a tool policy was rewritten: %s", policy)
	}
}

func TestExtendToolPolicyAllowlistGrantsGetLink(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		want    []string
		absent  []string
		changed bool
	}{
		{
			name:    "session allowlist that reads Docs",
			policy:  `{"mode":"deny_all","allowed":["openpoet_read_document","openpoet_get_my_task"]}`,
			want:    []string{"openpoet_read_document", "openpoet_get_my_task", "openpoet_get_link"},
			absent:  []string{"get_link"},
			changed: true,
		},
		{
			name:    "reading both Docs and artifacts grants it once",
			policy:  `{"mode":"deny_all","allowed":["openpoet_read_document","openpoet_read_artifact"]}`,
			want:    []string{"openpoet_get_link"},
			changed: true,
		},
		{
			name:    "chat policy uses unprefixed names and keeps its deny list",
			policy:  `{"mode":"custom","allowed":["read_artifact"],"denied":["delete_task"]}`,
			want:    []string{"read_artifact", "get_link"},
			absent:  []string{"openpoet_get_link"},
			changed: true,
		},
		{name: "publish-only allowlist is left alone", policy: `{"mode":"deny_all","allowed":["openpoet_create_document","openpoet_create_artifact"]}`},
		{name: "allow_all needs nothing", policy: `{"mode":"allow_all","denied":["openpoet_delete_task"]}`},
		{name: "custom with only a deny list allows it already", policy: `{"mode":"custom","allowed":[],"denied":["openpoet_delete_task"]}`},
		{name: "already granted", policy: `{"mode":"deny_all","allowed":["openpoet_read_document","openpoet_get_link"]}`},
		{name: "not JSON", policy: `not json`},
	}
	for _, tc := range cases {
		updated, changed := extendToolPolicyAllowlist(tc.policy, linkToolFollowing)
		if changed != tc.changed {
			t.Fatalf("%s: changed = %v, want %v (%s)", tc.name, changed, tc.changed, updated)
		}
		if !changed {
			if updated != tc.policy {
				t.Fatalf("%s: unchanged policy was rewritten: %s", tc.name, updated)
			}
			continue
		}
		var parsed struct {
			Allowed []string `json:"allowed"`
			Denied  []string `json:"denied"`
		}
		if err := json.Unmarshal([]byte(updated), &parsed); err != nil {
			t.Fatalf("%s: result is not JSON: %s", tc.name, updated)
		}
		allowed := map[string]int{}
		for _, name := range parsed.Allowed {
			allowed[name]++
		}
		for _, name := range tc.want {
			if allowed[name] != 1 {
				t.Fatalf("%s: %q appears %d times in %v", tc.name, name, allowed[name], parsed.Allowed)
			}
		}
		for _, name := range tc.absent {
			if allowed[name] != 0 {
				t.Fatalf("%s: %q must not be granted: %v", tc.name, name, parsed.Allowed)
			}
		}
		if strings.Contains(tc.policy, `"denied"`) && len(parsed.Denied) == 0 {
			t.Fatalf("%s: the deny list was dropped: %s", tc.name, updated)
		}
	}
}

// TestMigrationV85GrantsPatchWhereUpdateIsGranted checks that an allowlist
// that lets an agent publish a new version also lets it publish one as a patch.
func TestMigrationV85GrantsPatchWhereUpdateIsGranted(t *testing.T) {
	for name, tc := range map[string]struct {
		policy  string
		want    string
		changed bool
	}{
		"session allowlist": {`{"mode":"deny_all","allowed":["openpoet_update_artifact","openpoet_read_artifact"]}`,
			`{"allowed":["openpoet_update_artifact","openpoet_read_artifact","openpoet_patch_artifact"],"mode":"deny_all"}`, true},
		"chat allowlist": {`{"mode":"custom","allowed":["update_artifact"],"denied":[]}`,
			`{"allowed":["update_artifact","patch_artifact"],"denied":[],"mode":"custom"}`, true},
		"already granted": {`{"mode":"deny_all","allowed":["openpoet_update_artifact","openpoet_patch_artifact"]}`, "", false},
		"read only":       {`{"mode":"deny_all","allowed":["openpoet_read_artifact"]}`, "", false},
		"allows all":      {`{"mode":"allow_all","denied":["openpoet_delete_task"]}`, "", false},
	} {
		updated, changed := extendToolPolicyAllowlist(tc.policy, patchToolFollowing)
		if changed != tc.changed || (changed && updated != tc.want) {
			t.Fatalf("%s: updated=%s changed=%v", name, updated, changed)
		}
	}

	db := setupTestDB(t)
	ctx := context.Background()
	project := &Project{Name: "restricted", Path: "/tmp/restricted", Type: "local"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE projects SET tool_policy = ? WHERE id = ?`,
		`{"mode":"deny_all","allowed":["openpoet_create_artifact","openpoet_update_artifact"]}`, project.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV85(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var policy string
	if err := db.Get(&policy, `SELECT tool_policy FROM projects WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(policy, `"openpoet_patch_artifact"`) {
		t.Fatalf("project policy after V85 = %s", policy)
	}
}
