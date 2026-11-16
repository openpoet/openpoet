package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/database"
)

// The session list with ?view=summary must keep every session and its
// metadata but leave out the plan text; without the parameter it is unchanged.
func TestListSessionsSummaryViewDropsPlanContent(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "openpoet-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	proj := &database.Project{Name: "list-test", Path: t.TempDir(), Type: "local", BackendConfig: "{}"}
	if err := db.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s-plan", "s-noplan"} {
		if err := db.CreateSession(ctx, &database.Session{ID: id, ProjectID: proj.ID, Status: "running", Name: id, StartTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateSessionPlan(ctx, "s-plan", "# a long plan"); err != nil {
		t.Fatal(err)
	}

	api := &API{db: db}
	list := func(target string) []database.Session {
		t.Helper()
		rec := httptest.NewRecorder()
		api.ListSessions(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", target, rec.Code, rec.Body.String())
		}
		var out []database.Session
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	plans := func(sessions []database.Session) map[string]string {
		m := map[string]string{}
		for _, s := range sessions {
			m[s.ID] = s.PlanContent
		}
		return m
	}

	full := plans(list("/api/sessions"))
	if len(full) != 2 || full["s-plan"] != "# a long plan" {
		t.Fatalf("full list = %v, want both sessions with the plan", full)
	}
	summary := list("/api/sessions?view=summary")
	if got := plans(summary); len(got) != 2 || got["s-plan"] != "" {
		t.Fatalf("summary list = %v, want both sessions without plan text", got)
	}
	for _, s := range summary {
		if s.Name != s.ID || s.Status != "running" || s.ProjectID != proj.ID {
			t.Fatalf("summary dropped metadata: %+v", s)
		}
	}
}
