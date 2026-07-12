package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"openpoet/internal/database"
)

var sensitiveApplicationSettings = map[string]struct{}{
	"anthropic_api_key": {}, "openai_api_key": {}, "groq_api_key": {}, "ollama_api_key": {},
	"tunnel_relay_token": {}, "tunnel_jwt_secret": {},
}

var aiProviderApplicationSettings = map[string]struct{}{
	"ai_provider": {}, "anthropic_api_key": {}, "ollama_api_key": {}, "ollama_base_url": {}, "ollama_model": {}, "ai_model": {},
}

type ConfigurationStore interface {
	GetAllSettings(context.Context) (map[string]string, error)
	SetSettingsAtomic(context.Context, map[string]string, []string) error
	GetProject(context.Context, int64) (*database.Project, error)
	UpdateProject(context.Context, *database.Project) error
	ListProjectShares(context.Context, int64) ([]database.ProjectShare, error)
	ReplaceProjectShares(context.Context, int64, []int64) error
}

type ConfigSynchronizer interface {
	SyncToProject(context.Context, *database.Project) error
	SyncAllProjects(context.Context) error
}

type MCPAPIKeyStatus struct {
	Exists  bool   `json:"exists"`
	Preview string `json:"preview,omitempty"`
	// Secret is an ephemeral, one-time UI delivery value. It is never
	// serialized by Automation, persisted in events, or returned by status.
	Secret string `json:"-"`
}

type ShareView struct {
	ProjectID int64  `json:"project_id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Type      string `json:"type"`
}

type ToolPolicy struct {
	Mode    string   `json:"mode"`
	Allowed []string `json:"allowed,omitempty"`
	Denied  []string `json:"denied,omitempty"`
}

type ConfigurationService struct {
	store         ConfigurationStore
	codec         SecretCodec
	effects       ApplicationEffects
	reinitializer AIReinitializer
	synchronizer  ConfigSynchronizer
}

func NewConfigurationService(store ConfigurationStore, codec SecretCodec, effects ApplicationEffects, reinitializer AIReinitializer, synchronizer ConfigSynchronizer) *ConfigurationService {
	return &ConfigurationService{store: store, codec: codec, effects: effects, reinitializer: reinitializer, synchronizer: synchronizer}
}

func (s *ConfigurationService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("configuration")
}

func (s *ConfigurationService) Settings(ctx context.Context) (map[string]string, error) {
	values, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	delete(values, "vapid_private_key")
	for key := range sensitiveApplicationSettings {
		delete(values, key)
		delete(values, key+"_iv")
	}
	delete(values, "mcp_api_key")
	delete(values, "mcp_api_key_iv")
	return values, nil
}

func (s *ConfigurationService) UpdateSettings(ctx context.Context, boundary R4Boundary, input map[string]string) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	values := make(map[string]string, len(input)*3)
	reinitialize := false
	for key, value := range input {
		key = strings.TrimSpace(key)
		if key == "" || key == "vapid_private_key" || strings.HasSuffix(key, "_iv") || strings.HasSuffix(key, "_preview") || key == "mcp_api_key" {
			return validationError("reserved_setting_key", "Reserved setting keys cannot be updated directly")
		}
		if _, ok := sensitiveApplicationSettings[key]; ok {
			if value == "" {
				continue
			}
			if s.codec == nil {
				return validationError("secret_codec_unavailable", "Secret encryption is unavailable")
			}
			ciphertext, iv, err := s.codec.Encrypt(value)
			if err != nil {
				return err
			}
			values[key], values[key+"_iv"], values[key+"_preview"] = ciphertext, iv, secretPreview(value)
		} else {
			values[key] = value
		}
		if _, ok := aiProviderApplicationSettings[key]; ok {
			reinitialize = true
		}
	}
	if err := s.store.SetSettingsAtomic(ctx, values, nil); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "settings", Action: "updated"})
	if reinitialize && s.reinitializer != nil {
		s.reinitializer.ReinitializeAI(ctx)
	}
	return nil
}

func (s *ConfigurationService) MCPAPIKeyStatus(ctx context.Context) (MCPAPIKeyStatus, error) {
	values, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return MCPAPIKeyStatus{}, err
	}
	return MCPAPIKeyStatus{Exists: values["mcp_api_key"] != "", Preview: values["mcp_api_key_preview"]}, nil
}

// GenerateMCPAPIKey persists only the encrypted credential. Secret is returned
// ephemerally with json:"-" so the local UI can show it once while Automation
// and ordinary JSON serialization remain metadata-only.
func (s *ConfigurationService) GenerateMCPAPIKey(ctx context.Context, boundary R4Boundary) (MCPAPIKeyStatus, error) {
	if err := requireR4(boundary); err != nil {
		return MCPAPIKeyStatus{}, err
	}
	if s.codec == nil {
		return MCPAPIKeyStatus{}, validationError("secret_codec_unavailable", "Secret encryption is unavailable")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return MCPAPIKeyStatus{}, err
	}
	secret := "dm_" + hex.EncodeToString(random)
	ciphertext, iv, err := s.codec.Encrypt(secret)
	if err != nil {
		return MCPAPIKeyStatus{}, err
	}
	preview := secretPreview(secret)
	if err = s.store.SetSettingsAtomic(ctx, map[string]string{"mcp_api_key": ciphertext, "mcp_api_key_iv": iv, "mcp_api_key_preview": preview}, nil); err != nil {
		return MCPAPIKeyStatus{}, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "api_key_generated"})
	return MCPAPIKeyStatus{Exists: true, Preview: preview, Secret: secret}, nil
}

func (s *ConfigurationService) RevokeMCPAPIKey(ctx context.Context, boundary R4Boundary) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if err := s.store.SetSettingsAtomic(ctx, nil, []string{"mcp_api_key", "mcp_api_key_iv", "mcp_api_key_preview"}); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "mcp", Action: "api_key_revoked"})
	return nil
}

func (s *ConfigurationService) ToolPolicies(ctx context.Context) (map[string]string, error) {
	values, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"session": values["mcp_tool_policy_session"], "chat": values["mcp_tool_policy_chat"], "http": values["mcp_tool_policy_http"]}, nil
}

func (s *ConfigurationService) UpdateToolPolicies(ctx context.Context, boundary R4Boundary, policies map[string]string) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	values := make(map[string]string, len(policies))
	for contextName, raw := range policies {
		switch contextName {
		case "session", "chat", "http":
		default:
			return validationError("invalid_tool_policy_context", "Tool policy context must be session, chat or http")
		}
		if err := validateToolPolicy(raw); err != nil {
			return err
		}
		values["mcp_tool_policy_"+contextName] = raw
	}
	if err := s.store.SetSettingsAtomic(ctx, values, nil); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "tools", Action: "policies_updated"})
	return nil
}

func (s *ConfigurationService) ProjectToolPolicy(ctx context.Context, projectID int64) (string, error) {
	project, err := s.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	return project.ToolPolicy, nil
}

func (s *ConfigurationService) UpdateProjectToolPolicy(ctx context.Context, boundary R4Boundary, projectID int64, raw string) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if err := validateToolPolicy(raw); err != nil {
		return err
	}
	project, err := s.project(ctx, projectID)
	if err != nil {
		return err
	}
	project.ToolPolicy = raw
	if err = s.store.UpdateProject(ctx, project); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "tools", Action: "project_policy_updated", ID: projectID})
	return nil
}

func (s *ConfigurationService) ProjectShares(ctx context.Context, projectID int64) ([]ShareView, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return nil, err
	}
	shares, err := s.store.ListProjectShares(ctx, projectID)
	if err != nil {
		return nil, err
	}
	result := make([]ShareView, 0, len(shares))
	for _, share := range shares {
		project, getErr := s.project(ctx, share.SharedProjectID)
		if getErr != nil {
			return nil, getErr
		}
		result = append(result, ShareView{ProjectID: project.ID, Name: project.Name, Path: project.Path, Type: project.Type})
	}
	return result, nil
}

func (s *ConfigurationService) ReplaceProjectShares(ctx context.Context, projectID int64, sharedIDs []int64) error {
	if _, err := s.project(ctx, projectID); err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(sharedIDs))
	for _, id := range sharedIDs {
		if id <= 0 || id == projectID {
			return validationError("invalid_project_share", "A project cannot share itself and all IDs must be positive")
		}
		if _, exists := seen[id]; exists {
			return validationError("duplicate_project_share", "Duplicate shared project ID")
		}
		seen[id] = struct{}{}
		if _, err := s.project(ctx, id); err != nil {
			return err
		}
	}
	if err := s.store.ReplaceProjectShares(ctx, projectID, sharedIDs); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "projects", Action: "shares_replaced", ID: projectID})
	return nil
}

func (s *ConfigurationService) SyncProject(ctx context.Context, boundary R4Boundary, projectID int64) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if s.synchronizer == nil {
		return validationError("config_sync_unavailable", "Configuration synchronizer is unavailable")
	}
	project, err := s.project(ctx, projectID)
	if err != nil {
		return err
	}
	if err = s.synchronizer.SyncToProject(ctx, project); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "config", Action: "project_synced", ID: projectID})
	return nil
}

func (s *ConfigurationService) SyncAll(ctx context.Context, boundary R4Boundary) error {
	if err := requireR4(boundary); err != nil {
		return err
	}
	if s.synchronizer == nil {
		return validationError("config_sync_unavailable", "Configuration synchronizer is unavailable")
	}
	if err := s.synchronizer.SyncAllProjects(ctx); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "config", Action: "all_synced"})
	return nil
}

func validateToolPolicy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var policy ToolPolicy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return validationError("invalid_tool_policy", "Tool policy must be valid JSON")
	}
	switch policy.Mode {
	case "allow_all", "deny_all", "custom":
	default:
		return validationError("invalid_tool_policy_mode", "Tool policy mode must be allow_all, deny_all or custom")
	}
	seen := map[string]struct{}{}
	for _, names := range [][]string{policy.Allowed, policy.Denied} {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				return validationError("invalid_tool_policy_tool", "Tool policy tool names cannot be empty")
			}
			if _, exists := seen[name]; exists {
				return validationError("duplicate_tool_policy_tool", "Tool policy cannot repeat a tool")
			}
			seen[name] = struct{}{}
		}
	}
	return nil
}

func (s *ConfigurationService) project(ctx context.Context, id int64) (*database.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}
	item, err := s.store.GetProject(ctx, id)
	if err != nil || item == nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	return item, nil
}
