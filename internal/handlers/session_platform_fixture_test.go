package handlers

import (
	"context"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/database"
	"openpoet/internal/session"
)

type sessionFixtureEnvironment struct{ binary string }

func (p sessionFixtureEnvironment) SessionEnvironment(context.Context, *database.Project) (map[string]string, error) {
	return map[string]string{"OPENPOET_BACKEND_BINARY": p.binary}, nil
}

func configureSessionPlatformFixture(t *testing.T, api *API, db *database.DB, manager *session.Manager, binary string) {
	t.Helper()
	// Sessions never start on the CLI's account default: give the fixture
	// explicit global defaults (tests that check refusal clear them).
	for key, value := range map[string]string{
		"default_model_claude_code": "opus", "default_effort_claude_code": "high",
		"default_model_codex": "gpt-6.1-sol", "default_effort_codex": "medium",
	} {
		if err := db.SetSetting(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := automation.NewPlatformCapabilityRegistry(api.capabilities)
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewSessionService(
		db, manager, nil, api.taskService, nil, nil, nil, nil,
		application.SessionCreationCollaborators{
			Environment:  sessionFixtureEnvironment{binary: binary},
			Names:        platformSessionNameStore{db: db},
			Input:        platformSessionInputSubmitter{api: api},
			InitialInput: platformSessionInitialPromptSubmitter{api: api},
			Settings:     platformSessionRuntimeSettings{api: api},
		},
	)
	api.platformMu.Lock()
	api.platformCapabilities = registry
	api.platformServices = &PlatformApplicationServices{Execution: automation.ExecutionPlatformServices{Sessions: service}}
	api.platformMu.Unlock()
}
