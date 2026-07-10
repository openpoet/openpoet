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
	registry, err := automation.NewPlatformCapabilityRegistry(api.capabilities)
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewSessionService(
		db, manager, nil, api.taskService, nil, nil, nil, nil,
		application.SessionCreationCollaborators{
			Environment: sessionFixtureEnvironment{binary: binary},
			Names:       platformSessionNameStore{db: db},
		},
	)
	api.platformMu.Lock()
	api.platformCapabilities = registry
	api.platformServices = &PlatformApplicationServices{Execution: automation.ExecutionPlatformServices{Sessions: service}}
	api.platformMu.Unlock()
}
