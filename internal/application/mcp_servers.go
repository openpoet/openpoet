package application

import (
	"context"
	"strings"

	"openpoet/internal/database"
)

type MCPStore interface {
	ListMCPServers(context.Context) ([]database.MCPServer, error)
	GetMCPServer(context.Context, int64) (*database.MCPServer, error)
	CreateMCPServer(context.Context, *database.MCPServer) error
	UpdateMCPServer(context.Context, *database.MCPServer) error
	DeleteMCPServer(context.Context, int64) error
	GetProject(context.Context, int64) (*database.Project, error)
	ListProjectMCPServers(context.Context, int64) ([]database.ProjectMCPServer, error)
	GetProjectMCPServer(context.Context, int64) (*database.ProjectMCPServer, error)
	CreateProjectMCPServer(context.Context, *database.ProjectMCPServer) error
	UpdateProjectMCPServer(context.Context, *database.ProjectMCPServer) error
	DeleteProjectMCPServer(context.Context, int64) error
}

type MCPServerView struct {
	ID         int64  `json:"id"`
	ProjectID  int64  `json:"project_id,omitempty"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	HasCommand bool   `json:"has_command"`
	HasArgs    bool   `json:"has_args"`
	HasEnv     bool   `json:"has_env"`
}

type MCPServerInput struct {
	Name    string
	Command string
	Args    string
	Env     string
	Enabled bool
}

type UpdateMCPServerCommand struct {
	ID      int64
	Name    *string
	Command *string
	Args    *string
	Env     *string
	Enabled *bool
}

type MCPService struct {
	store   MCPStore
	codec   SecretCodec
	effects ApplicationEffects
}

func NewMCPService(store MCPStore, codec SecretCodec, effects ApplicationEffects) *MCPService {
	return &MCPService{store: store, codec: codec, effects: effects}
}

func (s *MCPService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("mcp_servers")
}

func (s *MCPService) ListGlobal(ctx context.Context) ([]MCPServerView, error) {
	items, err := s.store.ListMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]MCPServerView, 0, len(items))
	for _, item := range items {
		views = append(views, globalMCPView(item))
	}
	return views, nil
}

func (s *MCPService) CreateGlobal(ctx context.Context, boundary R4Boundary, input MCPServerInput) (*MCPServerView, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	item, err := s.newGlobal(input)
	if err != nil {
		return nil, err
	}
	if err = s.ensureGlobalName(ctx, item.Name, 0); err != nil {
		return nil, err
	}
	if err = s.store.CreateMCPServer(ctx, item); err != nil {
		return nil, err
	}
	view := globalMCPView(*item)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "global_created", ID: item.ID})
	return &view, nil
}

func (s *MCPService) UpdateGlobal(ctx context.Context, boundary R4Boundary, command UpdateMCPServerCommand) (*MCPServerView, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	item, err := s.global(ctx, command.ID)
	if err != nil {
		return nil, err
	}
	if command.Name != nil {
		item.Name = strings.TrimSpace(*command.Name)
	}
	if command.Command != nil {
		if strings.TrimSpace(*command.Command) == "" {
			return nil, validationError("mcp_command_required", "MCP command is required")
		}
		item.Command, err = encryptEnvelope(s.codec, *command.Command)
		if err != nil {
			return nil, err
		}
	}
	if command.Args != nil {
		if err = validateJSONArray(*command.Args, "invalid_mcp_args", "MCP args must be a JSON array"); err != nil {
			return nil, err
		}
		item.Args, err = encryptEnvelope(s.codec, *command.Args)
		if err != nil {
			return nil, err
		}
	}
	if command.Env != nil {
		if err = validateJSONObject(*command.Env, "invalid_mcp_env", "MCP env must be a JSON object"); err != nil {
			return nil, err
		}
		item.Env, err = encryptEnvelope(s.codec, *command.Env)
		if err != nil {
			return nil, err
		}
	}
	if command.Enabled != nil {
		item.Enabled = *command.Enabled
	}
	if err = s.encryptLegacyGlobal(item); err != nil {
		return nil, err
	}
	if err = validateMCPName(item.Name); err != nil {
		return nil, err
	}
	if err = s.ensureGlobalName(ctx, item.Name, item.ID); err != nil {
		return nil, err
	}
	if err = s.store.UpdateMCPServer(ctx, item); err != nil {
		return nil, err
	}
	view := globalMCPView(*item)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "global_updated", ID: item.ID})
	return &view, nil
}

func (s *MCPService) DeleteGlobal(ctx context.Context, boundary R4Boundary, id int64) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if _, err := s.global(ctx, id); err != nil {
		return err
	}
	if err := s.store.DeleteMCPServer(ctx, id); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "global_deleted", ID: id})
	return nil
}

func (s *MCPService) ListProject(ctx context.Context, projectID int64) ([]MCPServerView, error) {
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	items, err := s.store.ListProjectMCPServers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	views := make([]MCPServerView, 0, len(items))
	for _, item := range items {
		views = append(views, projectMCPView(item))
	}
	return views, nil
}

func (s *MCPService) CreateProject(ctx context.Context, boundary R4Boundary, projectID int64, input MCPServerInput) (*MCPServerView, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	item, err := s.newProject(projectID, input)
	if err != nil {
		return nil, err
	}
	if err = s.ensureProjectName(ctx, projectID, item.Name, 0); err != nil {
		return nil, err
	}
	if err = s.store.CreateProjectMCPServer(ctx, item); err != nil {
		return nil, err
	}
	view := projectMCPView(*item)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "project_created", ID: item.ID, Meta: map[string]any{"project_id": projectID}})
	return &view, nil
}

func (s *MCPService) UpdateProject(ctx context.Context, boundary R4Boundary, projectID int64, command UpdateMCPServerCommand) (*MCPServerView, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	item, err := s.project(ctx, projectID, command.ID)
	if err != nil {
		return nil, err
	}
	if command.Name != nil {
		item.Name = strings.TrimSpace(*command.Name)
	}
	if command.Command != nil {
		if strings.TrimSpace(*command.Command) == "" {
			return nil, validationError("mcp_command_required", "MCP command is required")
		}
		item.Command, err = encryptEnvelope(s.codec, *command.Command)
		if err != nil {
			return nil, err
		}
	}
	if command.Args != nil {
		if err = validateJSONArray(*command.Args, "invalid_mcp_args", "MCP args must be a JSON array"); err != nil {
			return nil, err
		}
		item.Args, err = encryptEnvelope(s.codec, *command.Args)
		if err != nil {
			return nil, err
		}
	}
	if command.Env != nil {
		if err = validateJSONObject(*command.Env, "invalid_mcp_env", "MCP env must be a JSON object"); err != nil {
			return nil, err
		}
		item.Env, err = encryptEnvelope(s.codec, *command.Env)
		if err != nil {
			return nil, err
		}
	}
	if command.Enabled != nil {
		item.Enabled = *command.Enabled
	}
	if err = s.encryptLegacyProject(item); err != nil {
		return nil, err
	}
	if err = validateMCPName(item.Name); err != nil {
		return nil, err
	}
	if err = s.ensureProjectName(ctx, projectID, item.Name, item.ID); err != nil {
		return nil, err
	}
	if err = s.store.UpdateProjectMCPServer(ctx, item); err != nil {
		return nil, err
	}
	view := projectMCPView(*item)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "project_updated", ID: item.ID, Meta: map[string]any{"project_id": projectID}})
	return &view, nil
}

func (s *MCPService) DeleteProject(ctx context.Context, boundary R4Boundary, projectID, id int64) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if _, err := s.project(ctx, projectID, id); err != nil {
		return err
	}
	if err := s.store.DeleteProjectMCPServer(ctx, id); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "project_deleted", ID: id, Meta: map[string]any{"project_id": projectID}})
	return nil
}

func (s *MCPService) newGlobal(input MCPServerInput) (*database.MCPServer, error) {
	if err := validateMCPInput(input); err != nil {
		return nil, err
	}
	command, err := encryptEnvelope(s.codec, input.Command)
	if err != nil {
		return nil, err
	}
	args, err := encryptEnvelope(s.codec, defaultJSON(input.Args, "[]"))
	if err != nil {
		return nil, err
	}
	env, err := encryptEnvelope(s.codec, defaultJSON(input.Env, "{}"))
	if err != nil {
		return nil, err
	}
	return &database.MCPServer{Name: strings.TrimSpace(input.Name), Command: command, Args: args, Env: env, Enabled: input.Enabled}, nil
}

func (s *MCPService) newProject(projectID int64, input MCPServerInput) (*database.ProjectMCPServer, error) {
	global, err := s.newGlobal(input)
	if err != nil {
		return nil, err
	}
	return &database.ProjectMCPServer{ProjectID: projectID, Name: global.Name, Command: global.Command, Args: global.Args, Env: global.Env, Enabled: global.Enabled}, nil
}

func (s *MCPService) encryptLegacyGlobal(item *database.MCPServer) error {
	command, args, env, err := s.encryptLegacyFields(item.Command, item.Args, item.Env)
	if err != nil {
		return err
	}
	item.Command, item.Args, item.Env = command, args, env
	return nil
}

func (s *MCPService) encryptLegacyProject(item *database.ProjectMCPServer) error {
	command, args, env, err := s.encryptLegacyFields(item.Command, item.Args, item.Env)
	if err != nil {
		return err
	}
	item.Command, item.Args, item.Env = command, args, env
	return nil
}

func (s *MCPService) encryptLegacyFields(command, args, env string) (string, string, string, error) {
	var err error
	commandNeedsEncryption, err := needsEnvelopeEncryption(command)
	if err != nil {
		return "", "", "", err
	}
	if commandNeedsEncryption {
		if strings.TrimSpace(command) == "" {
			return "", "", "", validationError("mcp_command_required", "MCP command is required")
		}
		command, err = encryptEnvelope(s.codec, command)
		if err != nil {
			return "", "", "", err
		}
	}
	argsNeedsEncryption, err := needsEnvelopeEncryption(args)
	if err != nil {
		return "", "", "", err
	}
	if argsNeedsEncryption {
		args = defaultJSON(args, "[]")
		if err = validateJSONArray(args, "invalid_mcp_args", "MCP args must be a JSON array"); err != nil {
			return "", "", "", err
		}
		args, err = encryptEnvelope(s.codec, args)
		if err != nil {
			return "", "", "", err
		}
	}
	envNeedsEncryption, err := needsEnvelopeEncryption(env)
	if err != nil {
		return "", "", "", err
	}
	if envNeedsEncryption {
		env = defaultJSON(env, "{}")
		if err = validateJSONObject(env, "invalid_mcp_env", "MCP env must be a JSON object"); err != nil {
			return "", "", "", err
		}
		env, err = encryptEnvelope(s.codec, env)
		if err != nil {
			return "", "", "", err
		}
	}
	return command, args, env, nil
}

func validateMCPInput(input MCPServerInput) error {
	if err := validateMCPName(input.Name); err != nil {
		return err
	}
	if strings.TrimSpace(input.Command) == "" {
		return validationError("mcp_command_required", "MCP command is required")
	}
	if err := validateJSONArray(defaultJSON(input.Args, "[]"), "invalid_mcp_args", "MCP args must be a JSON array"); err != nil {
		return err
	}
	return validateJSONObject(defaultJSON(input.Env, "{}"), "invalid_mcp_env", "MCP env must be a JSON object")
}

func validateMCPName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return validationError("mcp_name_required", "MCP server name is required")
	}
	if len(name) > 200 {
		return validationError("invalid_mcp_name", "MCP server name is too long")
	}
	return nil
}

func defaultJSON(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func globalMCPView(item database.MCPServer) MCPServerView {
	return MCPServerView{ID: item.ID, Name: item.Name, Enabled: item.Enabled, HasCommand: item.Command != "", HasArgs: item.Args != "", HasEnv: item.Env != ""}
}

func projectMCPView(item database.ProjectMCPServer) MCPServerView {
	return MCPServerView{ID: item.ID, ProjectID: item.ProjectID, Name: item.Name, Enabled: item.Enabled, HasCommand: item.Command != "", HasArgs: item.Args != "", HasEnv: item.Env != ""}
}

func (s *MCPService) global(ctx context.Context, id int64) (*database.MCPServer, error) {
	if id <= 0 {
		return nil, validationError("invalid_mcp_id", "MCP server ID must be positive")
	}
	item, err := s.store.GetMCPServer(ctx, id)
	if err != nil || item == nil {
		return nil, notFoundError("mcp_not_found", "MCP server not found", err)
	}
	return item, nil
}

func (s *MCPService) project(ctx context.Context, projectID, id int64) (*database.ProjectMCPServer, error) {
	if projectID <= 0 || id <= 0 {
		return nil, validationError("invalid_mcp_id", "Project and MCP server IDs must be positive")
	}
	item, err := s.store.GetProjectMCPServer(ctx, id)
	if err != nil || item == nil || item.ProjectID != projectID {
		return nil, notFoundError("project_mcp_not_found", "Project MCP server not found", err)
	}
	return item, nil
}

func (s *MCPService) ensureProject(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid_project_id", "Project ID must be positive")
	}
	item, err := s.store.GetProject(ctx, id)
	if err != nil || item == nil {
		return notFoundError("project_not_found", "Project not found", err)
	}
	return nil
}

func (s *MCPService) ensureGlobalName(ctx context.Context, name string, exceptID int64) error {
	items, err := s.store.ListMCPServers(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(item.Name, name) {
			return conflictError("mcp_name_conflict", "An MCP server with this name already exists")
		}
	}
	return nil
}

func (s *MCPService) ensureProjectName(ctx context.Context, projectID int64, name string, exceptID int64) error {
	items, err := s.store.ListProjectMCPServers(ctx, projectID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(item.Name, name) {
			return conflictError("mcp_name_conflict", "An MCP server with this name already exists in the project")
		}
	}
	return nil
}
