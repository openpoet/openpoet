package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ComposeManifestSource is the source_path recorded in the approval ledger for
// a project's docker-compose file.
const ComposeManifestSource = "docker-compose.yml"

// maxComposeManifestBytes caps how much of the compose file is echoed back in a
// status response.
const maxComposeManifestBytes = 64 * 1024

// ComposeRunner is the docker-compose surface the service drives.
type ComposeRunner interface {
	Available() error
	Status(ctx context.Context, projectPath string) ([]ComposeService, error)
	Up(ctx context.Context, projectPath string) (string, error)
	Down(ctx context.Context, projectPath string) (string, error)
	Restart(ctx context.Context, projectPath string) (string, error)
	Logs(ctx context.Context, projectPath, service string, tail int) (string, error)
	HasManifest(projectPath string) bool
	ReadManifest(projectPath string) (string, error)
	ProjectName(projectPath string) string
}

// ComposeService mirrors one entry of `docker compose ps`.
type ComposeService struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Health  string `json:"health,omitempty"`
	Ports   string `json:"ports,omitempty"`
}

// ComposeApprovalStore is the content-hash ledger gating `up`.
type ComposeApprovalStore interface {
	ApproveManifest(ctx context.Context, projectID int64, sourcePath, content, sha, approvedBy string) error
	IsManifestApproved(ctx context.Context, projectID int64, sha string) (bool, error)
}

// ComposeStatus is what the UI renders for a project's stack.
type ComposeStatus struct {
	ProjectID   int64            `json:"project_id"`
	HasCompose  bool             `json:"has_compose"`
	DockerReady bool             `json:"docker_ready"`
	ComposeName string           `json:"compose_name,omitempty"`
	ManifestSHA string           `json:"manifest_sha,omitempty"`
	Manifest    string           `json:"manifest,omitempty"`
	Approved    bool             `json:"approved"`
	LintError   string           `json:"lint_error,omitempty"`
	Services    []ComposeService `json:"services"`
	StatusError string           `json:"status_error,omitempty"`
	Unsupported string           `json:"unsupported,omitempty"`
}

// ComposeApplicationService owns the container lifecycle of a local project.
type ComposeApplicationService struct {
	projects  *ProjectService
	runner    ComposeRunner
	approvals ComposeApprovalStore
}

func NewComposeApplicationService(projects *ProjectService, runner ComposeRunner, approvals ComposeApprovalStore) *ComposeApplicationService {
	return &ComposeApplicationService{projects: projects, runner: runner, approvals: approvals}
}

func (s *ComposeApplicationService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("compose")
}

// ComposeSHA is the content hash the approval ledger keys on.
func ComposeSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// resolve loads a project and refuses the ones compose cannot drive.
func (s *ComposeApplicationService) resolve(ctx context.Context, projectID int64) (string, error) {
	project, err := s.projects.Get(ctx, projectID)
	if err != nil {
		return "", err
	}
	if project.Type != "local" {
		return "", validationError("compose_requires_local_project",
			"Container management is only available for local projects")
	}
	if strings.TrimSpace(project.Path) == "" {
		return "", validationError("project_path_required", "The project has no path")
	}
	return project.Path, nil
}

// Status reports the stack without ever failing the whole request: a missing
// docker, an unapproved manifest and a stopped stack are all states the UI
// needs to render, not errors that hide the rest.
func (s *ComposeApplicationService) Status(ctx context.Context, projectID int64) (*ComposeStatus, error) {
	project, err := s.projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	status := &ComposeStatus{ProjectID: projectID, Services: []ComposeService{}}
	if project.Type != "local" {
		status.Unsupported = "Container management is only available for local projects"
		return status, nil
	}
	status.DockerReady = s.runner.Available() == nil
	status.HasCompose = s.runner.HasManifest(project.Path)
	if !status.HasCompose {
		return status, nil
	}
	status.ComposeName = s.runner.ProjectName(project.Path)

	body, err := s.runner.ReadManifest(project.Path)
	if err != nil {
		status.StatusError = err.Error()
		return status, nil
	}
	status.ManifestSHA = ComposeSHA(body)
	// The body travels with the status so the approval dialog can show exactly
	// what is being approved, without a second round-trip.
	if len(body) > maxComposeManifestBytes {
		status.Manifest = body[:maxComposeManifestBytes] + "\n… (truncated)"
	} else {
		status.Manifest = body
	}
	if lintErr := LintCompose(body); lintErr != nil {
		status.LintError = lintErr.Error()
	}
	if approved, err := s.approvals.IsManifestApproved(ctx, projectID, status.ManifestSHA); err == nil {
		status.Approved = approved
	}
	if !status.DockerReady {
		status.StatusError = "docker is not installed or not on PATH"
		return status, nil
	}
	services, err := s.runner.Status(ctx, project.Path)
	if err != nil {
		status.StatusError = err.Error()
		return status, nil
	}
	status.Services = services
	return status, nil
}

// Approve records the current compose content as approved to run. Any later
// edit changes the hash and revokes the approval implicitly.
func (s *ComposeApplicationService) Approve(ctx context.Context, projectID int64, expectedSHA, approvedBy string) (*ComposeStatus, error) {
	path, err := s.resolve(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !s.runner.HasManifest(path) {
		return nil, validationError("compose_missing", "The project has no docker-compose.yml")
	}
	body, err := s.runner.ReadManifest(path)
	if err != nil {
		return nil, err
	}
	sha := ComposeSHA(body)
	// The UI sends back the hash it showed the user; a mismatch means the file
	// changed under them and they would be approving something unseen.
	if expected := strings.TrimSpace(expectedSHA); expected != "" && !strings.EqualFold(expected, sha) {
		return nil, conflictError("compose_changed",
			"The compose file changed since it was displayed. Review it again before approving.")
	}
	if err := LintCompose(body); err != nil {
		return nil, err
	}
	if err := s.approvals.ApproveManifest(ctx, projectID, ComposeManifestSource, body, sha, approvedBy); err != nil {
		return nil, err
	}
	return s.Status(ctx, projectID)
}

// ComposeActionResult carries the command output back to the UI.
type ComposeActionResult struct {
	Action string         `json:"action"`
	Output string         `json:"output"`
	Status *ComposeStatus `json:"status,omitempty"`
}

// Up starts the stack. It is the one action behind the approval gate, because
// it is the only one that executes the manifest's contents.
func (s *ComposeApplicationService) Up(ctx context.Context, projectID int64) (*ComposeActionResult, error) {
	path, err := s.resolve(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !s.runner.HasManifest(path) {
		return nil, validationError("compose_missing", "The project has no docker-compose.yml")
	}
	body, err := s.runner.ReadManifest(path)
	if err != nil {
		return nil, err
	}
	if err := LintCompose(body); err != nil {
		return nil, err
	}
	sha := ComposeSHA(body)
	approved, err := s.approvals.IsManifestApproved(ctx, projectID, sha)
	if err != nil {
		return nil, err
	}
	if !approved {
		return nil, validationError("compose_not_approved", fmt.Sprintf(
			"This docker-compose.yml has not been approved to run (%s). Review and approve it first.", sha[:12]))
	}
	return s.act(ctx, projectID, "up", func() (string, error) { return s.runner.Up(ctx, path) })
}

func (s *ComposeApplicationService) Down(ctx context.Context, projectID int64) (*ComposeActionResult, error) {
	path, err := s.resolve(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return s.act(ctx, projectID, "down", func() (string, error) { return s.runner.Down(ctx, path) })
}

// Restart restarts containers that are already running from an approved
// manifest, so it needs no approval of its own.
func (s *ComposeApplicationService) Restart(ctx context.Context, projectID int64) (*ComposeActionResult, error) {
	path, err := s.resolve(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return s.act(ctx, projectID, "restart", func() (string, error) { return s.runner.Restart(ctx, path) })
}

func (s *ComposeApplicationService) Logs(ctx context.Context, projectID int64, service string, tail int) (string, error) {
	path, err := s.resolve(ctx, projectID)
	if err != nil {
		return "", err
	}
	return s.runner.Logs(ctx, path, service, tail)
}

func (s *ComposeApplicationService) act(ctx context.Context, projectID int64, action string, run func() (string, error)) (*ComposeActionResult, error) {
	output, err := run()
	if err != nil {
		return nil, err
	}
	status, statusErr := s.Status(ctx, projectID)
	result := &ComposeActionResult{Action: action, Output: output}
	if statusErr == nil {
		result.Status = status
	}
	return result, nil
}
