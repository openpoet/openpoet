package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/session"
	"openpoet/internal/websocket"
)

// TestSessionCreateRunsExplicitModelAndEffort drives the real SessionService
// and session manager: the request's model/effort reach the CLI as flags and
// are recorded with their source; "default" and a missing configuration are
// refused before any process starts.
func TestSessionCreateRunsExplicitModelAndEffort(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	script := filepath.Join(t.TempDir(), "args-backend.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'ARGS:%s\\n' \"$*\"\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := &database.Project{Name: "explicit", Path: t.TempDir(), Type: "local", Backend: string(session.BackendClaudeCode), BackendConfig: "{}"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(db, websocket.NewHub(), "localhost:0")
	api := NewAPI(db, websocket.NewHub(), mgr, nil, nil, nil, nil)
	configureSessionPlatformFixture(t, api, db, mgr, script)
	services, _ := api.platformApplicationServices()
	auth := application.ActionAuthorization{Actor: application.Actor{Type: "agent", ID: "test"}, Reason: "test", Approved: true, ApprovedBy: "user:test"}

	created, err := services.Execution.Sessions.Create(ctx, application.CreateSessionCommand{ProjectID: project.ID, Model: "sonnet", Effort: "max", Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		_ = mgr.StopSession(stopCtx, created.ID)
	})
	if created.RequestedModel != "sonnet" || created.Effort != "max" || created.ModelSource != "request" || created.EffortSource != "request" {
		t.Fatalf("session = model %q (%s) effort %q (%s)", created.RequestedModel, created.ModelSource, created.Effort, created.EffortSource)
	}
	waitForSessionOutput(t, mgr, created.ID, "--model sonnet --effort max")

	_, err = services.Execution.Sessions.Create(ctx, application.CreateSessionCommand{ProjectID: project.ID, Model: "default", Authorization: auth})
	requireApplicationCode(t, err, "runtime_setting_invalid")

	for _, key := range []string{"default_model_claude_code", "default_effort_claude_code"} {
		if err := db.SetSetting(ctx, key, ""); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := db.ListSessions(ctx)
	_, err = services.Execution.Sessions.Create(ctx, application.CreateSessionCommand{ProjectID: project.ID, Authorization: auth})
	requireApplicationCode(t, err, "session_settings_unresolved")
	if after, _ := db.ListSessions(ctx); len(after) != len(before) {
		t.Fatalf("a refused session left a row: %d -> %d", len(before), len(after))
	}
}

func requireApplicationCode(t *testing.T, err error, code string) {
	t.Helper()
	var appErr *application.Error
	if !errors.As(err, &appErr) || appErr.Code != code {
		t.Fatalf("err = %v, want application error %s", err, code)
	}
}
