package application

import (
	"context"
	"strings"

	"openpoet/internal/database"
)

type AIAgentStore interface {
	ListAIAgents(context.Context) ([]database.AIAgent, error)
	GetAIAgent(context.Context, int64) (*database.AIAgent, error)
	CreateAIAgent(context.Context, *database.AIAgent) error
	UpdateAIAgent(context.Context, *database.AIAgent) error
	DeleteAIAgent(context.Context, int64) error
}

type AIAgentInput struct {
	Name          string
	SystemPrompt  string
	ToolPolicy    string
	ProjectFilter string
	Enabled       bool
}

type UpdateAIAgentCommand struct {
	ID            int64
	Name          *string
	SystemPrompt  *string
	ToolPolicy    *string
	ProjectFilter *string
	Enabled       *bool
}

type AIAgentService struct {
	store   AIAgentStore
	effects ApplicationEffects
}

func NewAIAgentService(store AIAgentStore, effects ApplicationEffects) *AIAgentService {
	return &AIAgentService{store: store, effects: effects}
}

func (s *AIAgentService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("ai_agents")
}

func (s *AIAgentService) List(ctx context.Context) ([]database.AIAgent, error) {
	items, err := s.store.ListAIAgents(ctx)
	if items == nil && err == nil {
		items = []database.AIAgent{}
	}
	return items, err
}

func (s *AIAgentService) Create(ctx context.Context, input AIAgentInput) (*database.AIAgent, error) {
	item := &database.AIAgent{Name: strings.TrimSpace(input.Name), SystemPrompt: input.SystemPrompt, ToolPolicy: defaultJSON(input.ToolPolicy, "{}"), ProjectFilter: defaultJSON(input.ProjectFilter, "{}"), Enabled: input.Enabled, IsDefault: false}
	if err := validateAIAgent(item); err != nil {
		return nil, err
	}
	if err := s.ensureName(ctx, item.Name, 0); err != nil {
		return nil, err
	}
	if err := s.store.CreateAIAgent(ctx, item); err != nil {
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai_agents", Action: "created", ID: item.ID})
	return item, nil
}

func (s *AIAgentService) Update(ctx context.Context, command UpdateAIAgentCommand) (*database.AIAgent, error) {
	item, err := s.get(ctx, command.ID)
	if err != nil {
		return nil, err
	}
	if command.Name != nil {
		if item.IsDefault && strings.TrimSpace(*command.Name) != item.Name {
			return nil, validationError("default_agent_name_immutable", "The default agent cannot be renamed")
		}
		item.Name = strings.TrimSpace(*command.Name)
	}
	if command.SystemPrompt != nil {
		item.SystemPrompt = *command.SystemPrompt
	}
	if command.ToolPolicy != nil {
		item.ToolPolicy = defaultJSON(*command.ToolPolicy, "{}")
	}
	if command.ProjectFilter != nil {
		item.ProjectFilter = defaultJSON(*command.ProjectFilter, "{}")
	}
	if command.Enabled != nil {
		item.Enabled = *command.Enabled
	}
	if err = validateAIAgent(item); err != nil {
		return nil, err
	}
	if err = s.ensureName(ctx, item.Name, item.ID); err != nil {
		return nil, err
	}
	if err = s.store.UpdateAIAgent(ctx, item); err != nil {
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai_agents", Action: "updated", ID: item.ID})
	return item, nil
}

func (s *AIAgentService) Delete(ctx context.Context, id int64) error {
	item, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if item.IsDefault {
		return validationError("default_agent_immutable", "The default agent cannot be deleted")
	}
	if err = s.store.DeleteAIAgent(ctx, id); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai_agents", Action: "deleted", ID: id})
	return nil
}

func validateAIAgent(item *database.AIAgent) error {
	if item.Name == "" {
		return validationError("agent_name_required", "Agent name is required")
	}
	if strings.TrimSpace(item.SystemPrompt) == "" {
		return validationError("agent_prompt_required", "Agent system prompt is required")
	}
	if len(item.Name) > 200 {
		return validationError("invalid_agent_name", "Agent name is too long")
	}
	if err := validateJSONObject(defaultJSON(item.ToolPolicy, "{}"), "invalid_agent_tool_policy", "Agent tool policy must be a JSON object"); err != nil {
		return err
	}
	return validateJSONObject(defaultJSON(item.ProjectFilter, "{}"), "invalid_agent_project_filter", "Agent project filter must be a JSON object")
}

func (s *AIAgentService) get(ctx context.Context, id int64) (*database.AIAgent, error) {
	if id <= 0 {
		return nil, validationError("invalid_agent_id", "Agent ID must be positive")
	}
	item, err := s.store.GetAIAgent(ctx, id)
	if err != nil || item == nil {
		return nil, notFoundError("agent_not_found", "Agent not found", err)
	}
	return item, nil
}

func (s *AIAgentService) ensureName(ctx context.Context, name string, exceptID int64) error {
	items, err := s.store.ListAIAgents(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(item.Name, name) {
			return conflictError("agent_name_conflict", "An agent with this name already exists")
		}
	}
	return nil
}
