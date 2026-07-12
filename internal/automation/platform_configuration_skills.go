package automation

import (
	"context"

	"openpoet/internal/application"
)

func skillPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		readConfigurationCapability("skills.list", "skills", "skills:read"),
		readConfigurationCapability("skills.list_versions", "skills", "skills:read"),
		readConfigurationCapability("skills.list_project_config", "skills", "skills:read", "projects:read"),
		readConfigurationCapability("skills.list_project", "skills", "skills:read", "projects:read"),
		writeConfigurationCapability("skills.create", "skills", "skills:write"),
		destructiveConfigurationCapability("skills.import", "skills", "skills:write"),
		writeConfigurationCapability("skills.update", "skills", "skills:write"),
		destructiveConfigurationCapability("skills.delete", "skills", "skills:write"),
		writeConfigurationCapability("skills.duplicate", "skills", "skills:write"),
		destructiveConfigurationCapability("skills.restore_version", "skills", "skills:write"),
		writeConfigurationCapability("skills.update_project_config", "skills", "skills:write", "projects:write"),
		writeConfigurationCapability("skills.create_project", "skills", "skills:write", "projects:write"),
		writeConfigurationCapability("skills.update_project", "skills", "skills:write", "projects:write"),
		destructiveConfigurationCapability("skills.delete_project", "skills", "skills:write", "projects:write"),
	}
}

type skillPlatformExecutor struct{ service *application.SkillService }

type skillPayload struct {
	ID        int64  `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Content   string `json:"content,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
	Category  string `json:"category,omitempty"`
	SortOrder int    `json:"sort_order,omitempty"`
	ProjectID int64  `json:"project_id,omitempty"`
}

type skillUpdatePayload struct {
	ID        int64   `json:"id,omitempty"`
	Name      *string `json:"name,omitempty"`
	Content   *string `json:"content,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty"`
	Category  *string `json:"category,omitempty"`
	SortOrder *int    `json:"sort_order,omitempty"`
	ProjectID int64   `json:"project_id,omitempty"`
}

type skillImportPayload struct {
	Skills []application.SkillInput `json:"skills"`
}

type skillDuplicatePayload struct {
	Name string `json:"name,omitempty"`
}

type skillVersionPayload struct {
	SkillID   int64 `json:"skill_id,omitempty"`
	VersionID int64 `json:"version_id"`
}

type skillProjectConfigPayload struct {
	ProjectID int64          `json:"project_id,omitempty"`
	Configs   map[int64]bool `json:"configs"`
}

func skillInputFromPayload(payload skillPayload) application.SkillInput {
	return application.SkillInput{
		Name: payload.Name, Content: payload.Content, Enabled: payload.Enabled,
		Category: payload.Category, SortOrder: payload.SortOrder,
	}
}

func skillUpdateCommand(id int64, payload skillUpdatePayload) application.UpdateSkillCommand {
	return application.UpdateSkillCommand{
		ID: id, Name: payload.Name, Content: payload.Content, Enabled: payload.Enabled,
		Category: payload.Category, SortOrder: payload.SortOrder,
	}
}

func (e *skillPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeConfigurationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "skills.list":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) { return e.service.List(ctx) }}, nil
	case "skills.list_versions":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		skillID, err := configurationTargetID(target, 0, "skill id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"skill_id": skillID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListVersions(ctx, skillID)
		}}, nil
	case "skills.list_project_config":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListProjectConfig(ctx, projectID)
		}}, nil
	case "skills.list_project":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListProjectSkills(ctx, projectID)
		}}, nil
	case "skills.create":
		var payload skillPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		skillInput := skillInputFromPayload(payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Create(ctx, skillInput)
		}}, nil
	case "skills.import":
		var payload skillImportPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if len(payload.Skills) == 0 || len(payload.Skills) > 100 {
			return nil, platformFailure("platform_payload_invalid", "skills import must contain between 1 and 100 skills", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"skill_count": len(payload.Skills)}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Import(ctx, payload.Skills)
		}}, nil
	case "skills.update":
		var payload skillUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "skill id")
		if err != nil {
			return nil, err
		}
		command := skillUpdateCommand(id, payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"skill_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Update(ctx, command)
		}}, nil
	case "skills.delete":
		if err := requireEmptyConfigurationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "skill id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"skill_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.Delete(ctx, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("skill", id, 0), nil
		}}, nil
	case "skills.duplicate":
		id, err := configurationTargetID(target, 0, "skill id")
		if err != nil {
			return nil, err
		}
		var payload skillDuplicatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"skill_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Duplicate(ctx, id, payload.Name)
		}}, nil
	case "skills.restore_version":
		var payload skillVersionPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		skillID, err := configurationTargetID(target, payload.SkillID, "skill id")
		if err != nil {
			return nil, err
		}
		if payload.VersionID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "version_id must be positive", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"skill_id": skillID, "version_id": payload.VersionID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Restore(ctx, skillID, payload.VersionID)
		}}, nil
	case "skills.update_project_config":
		var payload skillProjectConfigPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		if len(payload.Configs) > 1000 {
			return nil, platformFailure("platform_payload_invalid", "project skill config exceeds 1000 entries", false)
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "config_count": len(payload.Configs)}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.ReplaceProjectConfig(ctx, projectID, payload.Configs); err != nil {
				return nil, err
			}
			return map[string]any{"updated": true, "project_id": projectID}, nil
		}}, nil
	case "skills.create_project":
		var payload skillPayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		skillInput := skillInputFromPayload(payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.CreateProjectSkill(ctx, projectID, skillInput)
		}}, nil
	case "skills.update_project":
		var payload skillUpdatePayload
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, payload.ID, "project skill id")
		if err != nil {
			return nil, err
		}
		command := skillUpdateCommand(id, payload)
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "skill_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.UpdateProjectSkill(ctx, projectID, command)
		}}, nil
	case "skills.delete_project":
		var payload struct {
			ProjectID int64 `json:"project_id,omitempty"`
		}
		if err := decodeConfigurationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := configurationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		id, err := configurationTargetID(target, 0, "project skill id")
		if err != nil {
			return nil, err
		}
		return &configurationValidatedCommand{preview: configurationPreview(input.Handler, map[string]any{"project_id": projectID, "skill_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.DeleteProjectSkill(ctx, projectID, id); err != nil {
				return nil, err
			}
			return deletedConfigurationResult("project_skill", id, projectID), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the skill capability handler is unsupported", false)
	}
}
