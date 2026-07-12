package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"openpoet/internal/application"
)

type ConfigurationPlatformServices struct {
	Projects          *application.ProjectService
	ProjectOperations *application.ProjectOperationService
	Tags              *application.TagService
	Skills            *application.SkillService
	Agents            *application.AIAgentService
	AIConfigs         *application.AIConfigService
	MCP               *application.MCPService
	CustomTools       *application.CustomToolService
	Configuration     *application.ConfigurationService
}

func RegisterConfigurationPlatformCapabilities(registry *PlatformCapabilityRegistry, services ConfigurationPlatformServices) error {
	if registry == nil {
		return errors.New("platform capability registry is required")
	}
	if services.Projects == nil || services.ProjectOperations == nil || services.Tags == nil ||
		services.Skills == nil || services.Agents == nil || services.AIConfigs == nil ||
		services.MCP == nil || services.CustomTools == nil || services.Configuration == nil {
		return errors.New("all configuration platform services are required")
	}
	groups := []struct {
		definitions []PlatformCapabilityDefinition
		executor    PlatformDomainExecutor
	}{
		{projectPlatformDefinitions(), &projectPlatformExecutor{service: services.Projects}},
		{projectOperationPlatformDefinitions(), &projectOperationPlatformExecutor{service: services.ProjectOperations}},
		{tagPlatformDefinitions(), &tagPlatformExecutor{service: services.Tags}},
		{skillPlatformDefinitions(), &skillPlatformExecutor{service: services.Skills}},
		{agentPlatformDefinitions(), &agentPlatformExecutor{service: services.Agents}},
		{aiConfigPlatformDefinitions(), &aiConfigPlatformExecutor{service: services.AIConfigs}},
		{mcpPlatformDefinitions(), &mcpPlatformExecutor{service: services.MCP}},
		{customToolPlatformDefinitions(), &customToolPlatformExecutor{service: services.CustomTools}},
		{configurationPlatformDefinitions(), &configurationPlatformExecutor{service: services.Configuration}},
	}
	for _, group := range groups {
		for _, definition := range group.definitions {
			if err := registry.Register(definition, group.executor); err != nil {
				return err
			}
		}
	}
	return nil
}

func configurationCapability(
	name, service string,
	risk application.CapabilityRisk,
	approval application.ApprovalPolicy,
	scopes ...string,
) PlatformCapabilityDefinition {
	required := make([]application.CapabilityScope, len(scopes))
	for i, scope := range scopes {
		required[i] = application.CapabilityScope(scope)
	}
	return PlatformCapabilityDefinition{
		Name: application.CapabilityName(name), Scopes: required, Risk: risk,
		Approval: approval, Mutation: risk != application.CapabilityRiskRead, Handler: application.CapabilityHandler(name),
		Service: application.CapabilityServiceName(service),
	}
}

func readConfigurationCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return configurationCapability(name, service, application.CapabilityRiskRead, application.ApprovalNone, scopes...)
}

func writeConfigurationCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return configurationCapability(name, service, application.CapabilityRiskWrite, application.ApprovalByPolicy, scopes...)
}

func destructiveConfigurationCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return configurationCapability(name, service, application.CapabilityRiskDestructive, application.ApprovalExplicit, scopes...)
}

func unsafeConfigurationCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return configurationCapability(name, service, application.CapabilityRiskUnsafe, application.ApprovalExplicit, scopes...)
}

type configurationCommandTarget struct {
	Type      string          `json:"type,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	ID        json.RawMessage `json:"id,omitempty"`
	ProjectID int64           `json:"project_id,omitempty"`
}

func decodeConfigurationTarget(raw json.RawMessage) (configurationCommandTarget, error) {
	var target configurationCommandTarget
	if err := decodeConfigurationJSON(raw, &target); err != nil {
		return configurationCommandTarget{}, platformFailure("platform_target_invalid", "the configuration target is invalid", false)
	}
	if target.ProjectID < 0 {
		return configurationCommandTarget{}, platformFailure("platform_target_invalid", "project_id must not be negative", false)
	}
	return target, nil
}

func configurationTargetID(target configurationCommandTarget, fallback int64, label string) (int64, error) {
	if len(bytes.TrimSpace(target.ID)) == 0 {
		if fallback > 0 {
			return fallback, nil
		}
		return 0, platformFailure("platform_target_invalid", label+" is required", false)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(target.ID))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0, platformFailure("platform_target_invalid", label+" must be a positive integer", false)
	}
	var raw string
	switch typed := value.(type) {
	case json.Number:
		raw = typed.String()
	case string:
		raw = strings.TrimSpace(typed)
	default:
		return 0, platformFailure("platform_target_invalid", label+" must be a positive integer", false)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, platformFailure("platform_target_invalid", label+" must be a positive integer", false)
	}
	return id, nil
}

func configurationProjectID(target configurationCommandTarget, fallback int64) (int64, error) {
	if target.ProjectID > 0 {
		return target.ProjectID, nil
	}
	if fallback > 0 {
		return fallback, nil
	}
	return 0, platformFailure("platform_target_invalid", "project_id is required", false)
}

func decodeConfigurationPayload[T any](raw json.RawMessage, output *T) error {
	if err := decodeConfigurationJSON(raw, output); err != nil {
		return platformFailure("platform_payload_invalid", "the configuration payload is invalid", false)
	}
	return nil
}

func decodeConfigurationJSON(raw json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func requireEmptyConfigurationPayload(raw json.RawMessage) error {
	var payload map[string]json.RawMessage
	if err := decodeConfigurationPayload(raw, &payload); err != nil {
		return err
	}
	if len(payload) != 0 {
		return platformFailure("platform_payload_invalid", "this capability does not accept payload fields", false)
	}
	return nil
}

type configurationValidatedCommand struct {
	preview any
	execute func(context.Context, application.ActionAuthorization) (any, error)
}

func (c *configurationValidatedCommand) DryRunResult() any {
	if c == nil {
		return nil
	}
	return c.preview
}

func (c *configurationValidatedCommand) Execute(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
	if c == nil || c.execute == nil {
		return nil, errors.New("configuration command executor is unavailable")
	}
	return c.execute(ctx, authorization)
}

func configurationPreview(handler application.CapabilityHandler, values map[string]any) map[string]any {
	result := map[string]any{"handler": handler, "valid": true}
	for key, value := range values {
		result[key] = value
	}
	return result
}

func deletedConfigurationResult(domain string, id, projectID int64) map[string]any {
	result := map[string]any{"deleted": true, "domain": domain, "id": id}
	if projectID > 0 {
		result["project_id"] = projectID
	}
	return result
}
