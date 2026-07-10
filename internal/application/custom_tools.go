package application

import (
	"context"
	"strings"

	"openpoet/internal/database"
)

type CustomToolStore interface {
	GetProject(context.Context, int64) (*database.Project, error)
	ListProjectTools(context.Context, int64) ([]database.ProjectTool, error)
	GetProjectTool(context.Context, int64) (*database.ProjectTool, error)
	CreateProjectTool(context.Context, *database.ProjectTool) error
	UpdateProjectTool(context.Context, *database.ProjectTool) error
	DeleteProjectTool(context.Context, int64) error
}

type CustomToolView struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  string `json:"parameters"`
	Confirm     bool   `json:"confirm"`
	WorkingDir  string `json:"working_dir"`
	Enabled     bool   `json:"enabled"`
	HasCommand  bool   `json:"has_command"`
}

type CustomToolInput struct {
	Name        string
	Description string
	Command     string
	Parameters  string
	Confirm     bool
	WorkingDir  string
	Enabled     bool
}

type UpdateCustomToolCommand struct {
	ID          int64
	Name        *string
	Description *string
	Command     *string
	Parameters  *string
	Confirm     *bool
	WorkingDir  *string
	Enabled     *bool
}

type CustomToolService struct {
	store   CustomToolStore
	codec   SecretCodec
	effects ApplicationEffects
}

func NewCustomToolService(store CustomToolStore, codec SecretCodec, effects ApplicationEffects) *CustomToolService {
	return &CustomToolService{store: store, codec: codec, effects: effects}
}

func (s *CustomToolService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("custom_tools")
}

func (s *CustomToolService) List(ctx context.Context, projectID int64) ([]CustomToolView, error) {
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	items, err := s.store.ListProjectTools(ctx, projectID)
	if err != nil {
		return nil, err
	}
	views := make([]CustomToolView, 0, len(items))
	for _, item := range items {
		views = append(views, customToolView(item))
	}
	return views, nil
}

func (s *CustomToolService) Create(ctx context.Context, boundary R4Boundary, projectID int64, input CustomToolInput) (*CustomToolView, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	if err := validateCustomToolInput(input.Name, input.Command, defaultJSON(input.Parameters, "{}")); err != nil {
		return nil, err
	}
	if err := s.ensureName(ctx, projectID, input.Name, 0); err != nil {
		return nil, err
	}
	encrypted, err := encryptEnvelope(s.codec, input.Command)
	if err != nil {
		return nil, err
	}
	item := &database.ProjectTool{ProjectID: projectID, Name: strings.TrimSpace(input.Name), Description: strings.TrimSpace(input.Description), Command: encrypted, Parameters: defaultJSON(input.Parameters, "{}"), Confirm: input.Confirm, WorkingDir: strings.TrimSpace(input.WorkingDir), Enabled: input.Enabled}
	if err = s.store.CreateProjectTool(ctx, item); err != nil {
		return nil, err
	}
	view := customToolView(*item)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "custom_tools", Action: "created", ID: item.ID, Meta: map[string]any{"project_id": projectID}})
	return &view, nil
}

func (s *CustomToolService) Update(ctx context.Context, boundary R4Boundary, projectID int64, command UpdateCustomToolCommand) (*CustomToolView, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	item, err := s.get(ctx, projectID, command.ID)
	if err != nil {
		return nil, err
	}
	if command.Name != nil {
		item.Name = strings.TrimSpace(*command.Name)
	}
	if command.Description != nil {
		item.Description = strings.TrimSpace(*command.Description)
	}
	if command.Command != nil {
		if strings.TrimSpace(*command.Command) == "" {
			return nil, validationError("tool_command_required", "Custom tool command is required")
		}
		item.Command, err = encryptEnvelope(s.codec, *command.Command)
		if err != nil {
			return nil, err
		}
	}
	if command.Parameters != nil {
		if err = validateJSONObject(*command.Parameters, "invalid_tool_parameters", "Custom tool parameters must be a JSON object"); err != nil {
			return nil, err
		}
		item.Parameters = *command.Parameters
	}
	if command.Confirm != nil {
		item.Confirm = *command.Confirm
	}
	if command.WorkingDir != nil {
		item.WorkingDir = strings.TrimSpace(*command.WorkingDir)
	}
	if command.Enabled != nil {
		item.Enabled = *command.Enabled
	}
	commandNeedsEncryption, classifyErr := needsEnvelopeEncryption(item.Command)
	if classifyErr != nil {
		return nil, classifyErr
	}
	if commandNeedsEncryption {
		if strings.TrimSpace(item.Command) == "" {
			return nil, validationError("tool_command_required", "Custom tool command is required")
		}
		item.Command, err = encryptEnvelope(s.codec, item.Command)
		if err != nil {
			return nil, err
		}
	}
	if err = validateCustomToolInput(item.Name, "encrypted", item.Parameters); err != nil {
		return nil, err
	}
	if err = s.ensureName(ctx, projectID, item.Name, item.ID); err != nil {
		return nil, err
	}
	if err = s.store.UpdateProjectTool(ctx, item); err != nil {
		return nil, err
	}
	view := customToolView(*item)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "custom_tools", Action: "updated", ID: item.ID, Meta: map[string]any{"project_id": projectID}})
	return &view, nil
}

func (s *CustomToolService) Delete(ctx context.Context, boundary R4Boundary, projectID, id int64) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if _, err := s.get(ctx, projectID, id); err != nil {
		return err
	}
	if err := s.store.DeleteProjectTool(ctx, id); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "custom_tools", Action: "deleted", ID: id, Meta: map[string]any{"project_id": projectID}})
	return nil
}

func validateCustomToolInput(name, command, parameters string) error {
	if strings.TrimSpace(name) == "" {
		return validationError("tool_name_required", "Custom tool name is required")
	}
	if len(strings.TrimSpace(name)) > 200 {
		return validationError("invalid_tool_name", "Custom tool name is too long")
	}
	if strings.TrimSpace(command) == "" {
		return validationError("tool_command_required", "Custom tool command is required")
	}
	return validateJSONObject(parameters, "invalid_tool_parameters", "Custom tool parameters must be a JSON object")
}

func customToolView(item database.ProjectTool) CustomToolView {
	return CustomToolView{ID: item.ID, ProjectID: item.ProjectID, Name: item.Name, Description: item.Description, Parameters: item.Parameters, Confirm: item.Confirm, WorkingDir: item.WorkingDir, Enabled: item.Enabled, HasCommand: item.Command != ""}
}

func (s *CustomToolService) ensureProject(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid_project_id", "Project ID must be positive")
	}
	item, err := s.store.GetProject(ctx, id)
	if err != nil || item == nil {
		return notFoundError("project_not_found", "Project not found", err)
	}
	return nil
}

func (s *CustomToolService) get(ctx context.Context, projectID, id int64) (*database.ProjectTool, error) {
	if projectID <= 0 || id <= 0 {
		return nil, validationError("invalid_tool_id", "Project and tool IDs must be positive")
	}
	item, err := s.store.GetProjectTool(ctx, id)
	if err != nil || item == nil || item.ProjectID != projectID {
		return nil, notFoundError("project_tool_not_found", "Project custom tool not found", err)
	}
	return item, nil
}

func (s *CustomToolService) ensureName(ctx context.Context, projectID int64, name string, exceptID int64) error {
	items, err := s.store.ListProjectTools(ctx, projectID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(item.Name, strings.TrimSpace(name)) {
			return conflictError("tool_name_conflict", "A custom tool with this name already exists in the project")
		}
	}
	return nil
}
