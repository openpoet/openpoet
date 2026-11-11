package application

import (
	"context"
	"strconv"
	"testing"
	"time"

	"openpoet/internal/database"
)

func TestNormalizePublicBaseURL(t *testing.T) {
	valid := map[string]string{
		"":                                "",
		"   ":                             "",
		"https://openpoet.example.com":    "https://openpoet.example.com",
		"https://openpoet.example.com/":   "https://openpoet.example.com",
		" http://192.0.2.10:8081/ ":       "http://192.0.2.10:8081",
		"https://example.com/openpoet///": "https://example.com/openpoet",
	}
	for raw, want := range valid {
		got, err := NormalizePublicBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("NormalizePublicBaseURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"openpoet.example.com",                 // no scheme
		"ftp://openpoet.example.com",           // not http(s)
		"javascript:alert(1)",                  // not an address
		"https://user:pw@openpoet.example.com", // credentials
		"https://openpoet.example.com/?a=1",    // query
		"https://openpoet.example.com/#frag",   // fragment
		"https://",                             // no host
	} {
		if got, err := NormalizePublicBaseURL(raw); err == nil || !ErrorIsKind(err, ErrorValidation) {
			t.Errorf("NormalizePublicBaseURL(%q) = %q, %v; want a validation error", raw, got, err)
		}
	}
}

func TestLinkServiceBuildsLinksOnThePublicBaseURL(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	t.Setenv(PublicBaseURLEnv, "")
	links := NewLinkService(db)

	project := createApplicationProject(t, db, "links")
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Ship\nit", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, &database.Session{ID: "sess-link", ProjectID: project.ID, Status: "running", Name: "worker", StartTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTempDocument(ctx, &database.TempDocument{ID: "doc12345", Title: "Notes", Content: "# Notes"}); err != nil {
		t.Fatal(err)
	}
	artifacts := NewArtifactService(db, &recordingApplicationEffects{})
	published, err := artifacts.Create(ctx, CreateArtifactCommand{Title: "Plan", Content: artifactTestHTML, Authorization: phase4Authorization()})
	if err != nil {
		t.Fatal(err)
	}
	artifactID := published.Artifact.ID

	// Nothing configured: the path and the local link exist, no public URL.
	view, err := links.Link(ctx, LinkQuery{Kind: "artifact", ID: "artifact:" + artifactID, LocalBase: "http://localhost:8080/"})
	if err != nil || view.Public || view.URL != "" || view.Path != "/app/artifact/"+artifactID ||
		view.LocalURL != "http://localhost:8080/app/artifact/"+artifactID || view.Title != "Plan" || view.ID != artifactID {
		t.Fatalf("unconfigured link = %+v err=%v", view, err)
	}

	if err := db.SetSetting(ctx, PublicBaseURLSetting, "https://openpoet.example.com/"); err != nil {
		t.Fatal(err)
	}
	// The cached "not configured" answer is still served until invalidated.
	if base := links.PublicBaseURL(ctx); base != "" {
		t.Fatalf("cached base = %q, want the cached empty value", base)
	}
	links.Invalidate()
	if base, host := links.PublicBaseURL(ctx), links.PublicHost(ctx); base != "https://openpoet.example.com" || host != "openpoet.example.com" {
		t.Fatalf("base = %q host = %q", base, host)
	}

	cases := []struct {
		query LinkQuery
		path  string
		title string
	}{
		{LinkQuery{Kind: "artifact", ID: artifactID}, "/app/artifact/" + artifactID, "Plan"},
		{LinkQuery{Kind: "ARTIFACT", ID: artifactID, Version: 1}, "/app/artifact/" + artifactID + "?v=1", "Plan"},
		{LinkQuery{Kind: "document", ID: "doc12345"}, "/app/doc/doc12345", "Notes"},
		{LinkQuery{Kind: "doc", ID: "artifact:" + artifactID}, "/app/artifact/" + artifactID, "Plan"},
		{LinkQuery{Kind: "session", ID: "sess-link"}, "/app/session/sess-link", "worker"},
		{LinkQuery{Kind: "task", ID: "#" + strconv.FormatInt(task.ID, 10)}, "/app/task/" + strconv.FormatInt(project.ID, 10) + "/" + strconv.FormatInt(task.ID, 10), "Ship it"},
		{LinkQuery{Kind: "project", ID: strconv.FormatInt(project.ID, 10)}, "/app/project/" + strconv.FormatInt(project.ID, 10), "links"},
	}
	for _, c := range cases {
		view, err := links.Link(ctx, c.query)
		if err != nil || !view.Public || view.Path != c.path || view.URL != "https://openpoet.example.com"+c.path || view.Title != c.title {
			t.Errorf("Link(%+v) = %+v err=%v; want path %q title %q", c.query, view, err, c.path, c.title)
		}
	}

	// A link to something that does not exist is refused, not invented.
	for _, query := range []LinkQuery{
		{Kind: "artifact", ID: "nope0000"},
		{Kind: "artifact", ID: artifactID, Version: 9},
		{Kind: "document", ID: "nope0000"},
		{Kind: "session", ID: "nope"},
		{Kind: "task", ID: "999999"},
		{Kind: "project", ID: "999999"},
	} {
		if view, err := links.Link(ctx, query); !ErrorIsKind(err, ErrorNotFound) {
			t.Errorf("Link(%+v) = %+v err=%v; want not found", query, view, err)
		}
	}
	for _, query := range []LinkQuery{
		{Kind: "artifact", ID: ""},
		{Kind: "task", ID: "abc"},
		{Kind: "file", ID: "x"},
		{Kind: "", ID: "x"},
	} {
		if view, err := links.Link(ctx, query); !ErrorIsKind(err, ErrorValidation) {
			t.Errorf("Link(%+v) = %+v err=%v; want a validation error", query, view, err)
		}
	}

	// The environment wins over the setting; an invalid value means "none".
	t.Setenv(PublicBaseURLEnv, "https://env.example.com/base/")
	links.Invalidate()
	if got := links.Absolute(ctx, "/app/doc/x"); got != "https://env.example.com/base/app/doc/x" {
		t.Fatalf("env base link = %q", got)
	}
	t.Setenv(PublicBaseURLEnv, "not a url")
	links.Invalidate()
	if got := links.Absolute(ctx, "/app/doc/x"); got != "" {
		t.Fatalf("invalid base produced %q", got)
	}
}

func TestUpdateSettingsNormalizesAndValidatesThePublicBaseURL(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	service := NewConfigurationService(db, nil, &recordingApplicationEffects{}, nil, nil)
	if err := service.UpdateSettings(ctx, approvedR4(), map[string]string{PublicBaseURLSetting: " https://openpoet.example.com/ "}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := db.GetSetting(ctx, PublicBaseURLSetting); stored != "https://openpoet.example.com" {
		t.Fatalf("stored = %q", stored)
	}
	if err := service.UpdateSettings(ctx, approvedR4(), map[string]string{PublicBaseURLSetting: "openpoet.example.com"}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("invalid base err = %v", err)
	}
	if stored, _ := db.GetSetting(ctx, PublicBaseURLSetting); stored != "https://openpoet.example.com" {
		t.Fatalf("an invalid value replaced the stored one: %q", stored)
	}
	// Clearing it is allowed.
	if err := service.UpdateSettings(ctx, approvedR4(), map[string]string{PublicBaseURLSetting: ""}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := db.GetSetting(ctx, PublicBaseURLSetting); stored != "" {
		t.Fatalf("stored after clear = %q", stored)
	}
	settings, err := service.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, listed := settings[PublicBaseURLSetting]; !listed {
		t.Fatalf("settings do not list %s: the UI could not show it", PublicBaseURLSetting)
	}
}
