package handlers

import (
	"context"

	"openpoet/internal/application"
	"openpoet/internal/compose"
)

// composeRunnerAdapter bridges the compose package to the application port,
// translating the runner's Service type into the application's.
type composeRunnerAdapter struct {
	runner *compose.Runner
}

// NewComposeRunnerAdapter returns the docker-compose driver used by the
// application layer.
func NewComposeRunnerAdapter() application.ComposeRunner {
	return &composeRunnerAdapter{runner: compose.New()}
}

func (a *composeRunnerAdapter) Available() error { return a.runner.Available() }

func (a *composeRunnerAdapter) Status(ctx context.Context, projectPath string) ([]application.ComposeService, error) {
	services, err := a.runner.Status(ctx, projectPath)
	if err != nil {
		return nil, err
	}
	result := make([]application.ComposeService, len(services))
	for i, s := range services {
		result[i] = application.ComposeService{
			Name: s.Name, Service: s.Service, Image: s.Image,
			State: s.State, Status: s.Status, Health: s.Health, Ports: s.Ports,
		}
	}
	return result, nil
}

func (a *composeRunnerAdapter) Up(ctx context.Context, projectPath string) (string, error) {
	return a.runner.Up(ctx, projectPath)
}

func (a *composeRunnerAdapter) Down(ctx context.Context, projectPath string) (string, error) {
	return a.runner.Down(ctx, projectPath)
}

func (a *composeRunnerAdapter) Restart(ctx context.Context, projectPath string) (string, error) {
	return a.runner.Restart(ctx, projectPath)
}

func (a *composeRunnerAdapter) Logs(ctx context.Context, projectPath, service string, tail int) (string, error) {
	return a.runner.Logs(ctx, projectPath, service, tail)
}

func (a *composeRunnerAdapter) HasManifest(projectPath string) bool {
	return compose.HasManifest(projectPath)
}

func (a *composeRunnerAdapter) ReadManifest(projectPath string) (string, error) {
	return compose.ReadManifest(projectPath)
}

func (a *composeRunnerAdapter) ProjectName(projectPath string) string {
	return compose.ProjectName(projectPath)
}
