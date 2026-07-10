package application

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"

	"openpoet/internal/database"
)

type ProjectStore interface {
	ListProjects(context.Context) ([]database.Project, error)
	GetProject(context.Context, int64) (*database.Project, error)
	CreateProject(context.Context, *database.Project) error
	UpdateProject(context.Context, *database.Project) error
	DeleteProject(context.Context, int64) error
}

type ProjectCredentialEncryptor interface {
	Encrypt(string) (encrypted string, iv string, err error)
}

type ProjectPathValidator interface {
	ValidateProjectPath(context.Context, string, string) error
}

type ProjectChange struct {
	Action  string
	Project *database.Project
	ID      int64
}

type ProjectEffects interface {
	PublishProjectChange(ProjectChange)
}

type ProjectService struct {
	store     ProjectStore
	encryptor ProjectCredentialEncryptor
	effects   ProjectEffects
	paths     ProjectPathValidator
}

func NewProjectService(store ProjectStore, encryptor ProjectCredentialEncryptor, effects ProjectEffects, paths ...ProjectPathValidator) *ProjectService {
	service := &ProjectService{store: store, encryptor: encryptor, effects: effects}
	if len(paths) > 0 {
		service.paths = paths[0]
	}
	return service
}

func (s *ProjectService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("projects")
}

func (s *ProjectService) List(ctx context.Context) ([]database.Project, error) {
	projects, err := s.store.ListProjects(ctx)
	if projects == nil && err == nil {
		projects = []database.Project{}
	}
	return projects, err
}

func (s *ProjectService) Get(ctx context.Context, id int64) (*database.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}
	project, err := s.store.GetProject(ctx, id)
	if err != nil || project == nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	return project, nil
}

func (s *ProjectService) Create(ctx context.Context, input database.ProjectInput) (*database.Project, error) {
	project, err := s.projectFromInput(ctx, input, nil)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateProject(ctx, project); err != nil {
		return nil, err
	}
	s.publish(ProjectChange{Action: "created", Project: project, ID: project.ID})
	return project, nil
}

func (s *ProjectService) Update(ctx context.Context, id int64, input database.ProjectInput) (*database.Project, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	project, err := s.projectFromInput(ctx, input, current)
	if err != nil {
		return nil, err
	}
	project.ID = id
	if err := s.store.UpdateProject(ctx, project); err != nil {
		return nil, err
	}
	s.publish(ProjectChange{Action: "updated", Project: project, ID: id})
	return project, nil
}

func (s *ProjectService) Delete(ctx context.Context, id int64) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.store.DeleteProject(ctx, id); err != nil {
		return err
	}
	s.publish(ProjectChange{Action: "deleted", ID: id})
	return nil
}

type DuplicateProjectCommand struct {
	ProjectID int64
	Overrides database.ProjectInput
}

func (s *ProjectService) Duplicate(ctx context.Context, command DuplicateProjectCommand) (*database.Project, error) {
	original, err := s.Get(ctx, command.ProjectID)
	if err != nil {
		return nil, err
	}
	clone := *original
	clone.ID = 0
	clone.Name = strings.TrimSpace(command.Overrides.Name)
	if clone.Name == "" {
		clone.Name = original.Name + " (Copy)"
	}
	if value := strings.TrimSpace(command.Overrides.Path); value != "" {
		clone.Path = value
	}
	if value := strings.TrimSpace(command.Overrides.Type); value != "" {
		clone.Type = value
	}
	if value := strings.TrimSpace(command.Overrides.Backend); value != "" {
		clone.Backend = value
	}
	if value := strings.TrimSpace(command.Overrides.BackendConfig); value != "" {
		clone.BackendConfig = value
	}
	if command.Overrides.SSHCredential != "" {
		if err := s.encryptCredential(&clone, command.Overrides.SSHCredential); err != nil {
			return nil, err
		}
	}
	if err := validateProject(&clone); err != nil {
		return nil, err
	}
	if err := s.validatePath(ctx, clone.Path, clone.Type); err != nil {
		return nil, err
	}
	if err := s.store.CreateProject(ctx, &clone); err != nil {
		return nil, err
	}
	s.publish(ProjectChange{Action: "created", Project: &clone, ID: clone.ID})
	return &clone, nil
}

func (s *ProjectService) projectFromInput(ctx context.Context, input database.ProjectInput, current *database.Project) (*database.Project, error) {
	project := &database.Project{}
	if current != nil {
		*project = *current
	}
	project.Name = strings.TrimSpace(input.Name)
	project.Path = strings.TrimSpace(input.Path)
	project.Type = strings.TrimSpace(input.Type)
	project.ToolPolicy = input.ToolPolicy
	project.SkillPolicy = input.SkillPolicy
	project.DangerouslySkipPermissions = input.DangerouslySkipPermissions
	if input.Backend != "" || current == nil {
		project.Backend = strings.TrimSpace(input.Backend)
	}
	if project.Backend == "" {
		project.Backend = "claude_code"
	}
	if input.BackendConfig != "" || current == nil {
		project.BackendConfig = input.BackendConfig
	}
	if project.BackendConfig == "" {
		project.BackendConfig = "{}"
	}
	if project.Type != "remote" && project.Path != "" {
		if absolute, err := filepath.Abs(project.Path); err == nil {
			project.Path = absolute
		}
	}
	if project.Type == "remote" {
		project.SSHHost = sql.NullString{String: strings.TrimSpace(input.SSHHost), Valid: strings.TrimSpace(input.SSHHost) != ""}
		project.SSHPort = sql.NullInt64{Int64: int64(input.SSHPort), Valid: input.SSHPort > 0}
		project.SSHUser = sql.NullString{String: strings.TrimSpace(input.SSHUser), Valid: strings.TrimSpace(input.SSHUser) != ""}
		project.SSHAuthType = sql.NullString{String: strings.TrimSpace(input.SSHAuthType), Valid: strings.TrimSpace(input.SSHAuthType) != ""}
		if input.SSHCredential != "" {
			if err := s.encryptCredential(project, input.SSHCredential); err != nil {
				return nil, err
			}
		}
	}
	if err := validateProject(project); err != nil {
		return nil, err
	}
	if err := s.validatePath(ctx, project.Path, project.Type); err != nil {
		return nil, err
	}
	return project, nil
}

func (s *ProjectService) validatePath(ctx context.Context, path, projectType string) error {
	if s.paths == nil {
		return nil
	}
	if err := s.paths.ValidateProjectPath(ctx, path, projectType); err != nil {
		return validationError("invalid_project_path", err.Error())
	}
	return nil
}

func (s *ProjectService) encryptCredential(project *database.Project, credential string) error {
	if s.encryptor == nil {
		return validationError("credential_encryptor_unavailable", "Credential encryptor is unavailable")
	}
	encrypted, iv, err := s.encryptor.Encrypt(credential)
	if err != nil {
		return err
	}
	project.SSHCredentialEncrypted = sql.NullString{String: encrypted, Valid: true}
	project.SSHCredentialIV = sql.NullString{String: iv, Valid: true}
	return nil
}

func validateProject(project *database.Project) error {
	if project.Name == "" {
		return validationError("project_name_required", "Project name is required")
	}
	if project.Path == "" {
		return validationError("project_path_required", "Project path is required")
	}
	if project.Type != "local" && project.Type != "remote" {
		return validationError("invalid_project_type", "Project type must be local or remote")
	}
	switch project.Backend {
	case "claude_code", "copilot", "acp", "codex", "opencode":
	default:
		return validationError("invalid_project_backend", "Unsupported project backend")
	}
	return nil
}

func (s *ProjectService) publish(change ProjectChange) {
	if s.effects != nil {
		s.effects.PublishProjectChange(change)
	}
}
