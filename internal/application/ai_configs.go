package application

import (
	"context"
	"strings"

	"openpoet/internal/database"
)

var validAIConfigSlots = map[string]struct{}{
	"ai_chat": {}, "ai_background": {}, "claude_session": {},
}

type AIConfigStore interface {
	ListAIConfigs(context.Context) ([]database.AIConfig, error)
	GetAIConfig(context.Context, int64) (*database.AIConfig, error)
	CreateAIConfig(context.Context, *database.AIConfig) error
	UpdateAIConfig(context.Context, *database.AIConfig) error
	DeleteAIConfig(context.Context, int64) error
	GetAIConfigAssignments(context.Context) ([]database.AIConfigAssignment, error)
	SetAIConfigAssignmentsAtomic(context.Context, map[string]*int64) error
}

type AIReinitializer interface {
	ReinitializeAI(context.Context)
}

type AIConfigInput struct {
	Name         string
	ProviderType string
	APIKey       string
	Model        string
	BaseURL      string
	ExtraJSON    string
}

type UpdateAIConfigCommand struct {
	ID           int64
	Name         *string
	ProviderType *string
	APIKey       *string
	Model        *string
	BaseURL      *string
	ExtraJSON    *string
}

type AIConfigService struct {
	store         AIConfigStore
	codec         SecretCodec
	effects       ApplicationEffects
	reinitializer AIReinitializer
}

func NewAIConfigService(store AIConfigStore, codec SecretCodec, effects ApplicationEffects, reinitializer AIReinitializer) *AIConfigService {
	return &AIConfigService{store: store, codec: codec, effects: effects, reinitializer: reinitializer}
}

func (s *AIConfigService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("ai_configs")
}

func (s *AIConfigService) List(ctx context.Context) ([]database.AIConfig, error) {
	items, err := s.store.ListAIConfigs(ctx)
	if items == nil && err == nil {
		items = []database.AIConfig{}
	}
	return items, err
}

func (s *AIConfigService) Create(ctx context.Context, boundary R4Boundary, input AIConfigInput) (*database.AIConfig, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	item := &database.AIConfig{Name: strings.TrimSpace(input.Name), ProviderType: strings.TrimSpace(input.ProviderType), Model: strings.TrimSpace(input.Model), BaseURL: strings.TrimSpace(input.BaseURL), ExtraJSON: defaultJSON(input.ExtraJSON, "{}")}
	if err := validateAIConfig(item); err != nil {
		return nil, err
	}
	if err := s.ensureName(ctx, item.Name, 0); err != nil {
		return nil, err
	}
	if input.APIKey != "" {
		if err := s.setAPIKey(item, input.APIKey); err != nil {
			return nil, err
		}
	}
	if err := s.store.CreateAIConfig(ctx, item); err != nil {
		return nil, err
	}
	s.afterCommit(ctx, "created", item.ID)
	return item, nil
}

func (s *AIConfigService) Update(ctx context.Context, boundary R4Boundary, command UpdateAIConfigCommand) (*database.AIConfig, error) {
	if err := requireR4(boundary); err != nil {
		return nil, err
	}
	item, err := s.get(ctx, command.ID)
	if err != nil {
		return nil, err
	}
	if command.Name != nil {
		item.Name = strings.TrimSpace(*command.Name)
	}
	if command.ProviderType != nil {
		item.ProviderType = strings.TrimSpace(*command.ProviderType)
	}
	if command.Model != nil {
		item.Model = strings.TrimSpace(*command.Model)
	}
	if command.BaseURL != nil {
		item.BaseURL = strings.TrimSpace(*command.BaseURL)
	}
	if command.ExtraJSON != nil {
		item.ExtraJSON = defaultJSON(*command.ExtraJSON, "{}")
	}
	// nil or empty means "keep existing". A caller cannot accidentally erase a
	// credential by sending a partial form payload.
	if command.APIKey != nil && *command.APIKey != "" {
		if err = s.setAPIKey(item, *command.APIKey); err != nil {
			return nil, err
		}
	}
	if err = validateAIConfig(item); err != nil {
		return nil, err
	}
	if err = s.ensureName(ctx, item.Name, item.ID); err != nil {
		return nil, err
	}
	if err = s.store.UpdateAIConfig(ctx, item); err != nil {
		return nil, err
	}
	s.afterCommit(ctx, "updated", item.ID)
	return item, nil
}

func (s *AIConfigService) Delete(ctx context.Context, boundary R4Boundary, id int64) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if _, err := s.get(ctx, id); err != nil {
		return err
	}
	if err := s.store.DeleteAIConfig(ctx, id); err != nil {
		return err
	}
	s.afterCommit(ctx, "deleted", id)
	return nil
}

func (s *AIConfigService) ListAssignments(ctx context.Context) ([]database.AIConfigAssignment, error) {
	items, err := s.store.GetAIConfigAssignments(ctx)
	if items == nil && err == nil {
		items = []database.AIConfigAssignment{}
	}
	return items, err
}

func (s *AIConfigService) Assign(ctx context.Context, boundary R4Boundary, assignments map[string]*int64) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	for slot, configID := range assignments {
		if _, ok := validAIConfigSlots[slot]; !ok {
			return validationError("invalid_ai_config_slot", "Unknown AI config assignment slot")
		}
		if configID != nil {
			if _, err := s.get(ctx, *configID); err != nil {
				return err
			}
		}
	}
	if err := s.store.SetAIConfigAssignmentsAtomic(ctx, assignments); err != nil {
		return err
	}
	s.afterCommit(ctx, "assignments_updated", 0)
	return nil
}

func validateAIConfig(item *database.AIConfig) error {
	if item.Name == "" {
		return validationError("ai_config_name_required", "AI config name is required")
	}
	if item.ProviderType == "" {
		return validationError("ai_provider_required", "AI provider type is required")
	}
	if len(item.Name) > 200 || len(item.ProviderType) > 100 {
		return validationError("invalid_ai_config", "AI config name or provider type is too long")
	}
	return validateJSONObject(defaultJSON(item.ExtraJSON, "{}"), "invalid_ai_extra", "AI config extra_json must be a JSON object")
}

func (s *AIConfigService) setAPIKey(item *database.AIConfig, value string) error {
	if s.codec == nil {
		return validationError("secret_codec_unavailable", "Secret encryption is unavailable")
	}
	ciphertext, iv, err := s.codec.Encrypt(value)
	if err != nil {
		return err
	}
	item.APIKeyEncrypted, item.APIKeyIV, item.APIKeyPreview = ciphertext, iv, secretPreview(value)
	return nil
}

func secretPreview(value string) string {
	if len(value) < 6 {
		return "***"
	}
	n := 8
	if len(value) < n {
		n = len(value) / 2
	}
	return value[:n] + "..."
}

func (s *AIConfigService) get(ctx context.Context, id int64) (*database.AIConfig, error) {
	if id <= 0 {
		return nil, validationError("invalid_ai_config_id", "AI config ID must be positive")
	}
	item, err := s.store.GetAIConfig(ctx, id)
	if err != nil || item == nil {
		return nil, notFoundError("ai_config_not_found", "AI config not found", err)
	}
	return item, nil
}

func (s *AIConfigService) ensureName(ctx context.Context, name string, exceptID int64) error {
	items, err := s.store.ListAIConfigs(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(item.Name, name) {
			return conflictError("ai_config_name_conflict", "An AI config with this name already exists")
		}
	}
	return nil
}

func (s *AIConfigService) afterCommit(ctx context.Context, action string, id int64) {
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "ai_configs", Action: action, ID: id})
	if s.reinitializer != nil {
		s.reinitializer.ReinitializeAI(ctx)
	}
}
