package application

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"

	"openpoet/internal/database"
)

type SkillStore interface {
	ListSkills(context.Context) ([]database.Skill, error)
	GetSkill(context.Context, int64) (*database.Skill, error)
	CreateSkillWithVersion(context.Context, *database.Skill) error
	UpdateSkillWithVersion(context.Context, *database.Skill, bool) error
	DeleteSkill(context.Context, int64) error
	ListSkillVersions(context.Context, int64) ([]database.SkillVersion, error)
	RestoreSkillVersionAtomic(context.Context, int64, int64) (*database.Skill, error)
	GetProject(context.Context, int64) (*database.Project, error)
	UpdateProject(context.Context, *database.Project) error
	ListProjectSkillConfigs(context.Context, int64) ([]database.ProjectSkillConfig, error)
	SetProjectSkillConfigAtomic(context.Context, int64, string, map[int64]bool) error
	ListProjectSkills(context.Context, int64) ([]database.ProjectSkill, error)
	GetProjectSkill(context.Context, int64) (*database.ProjectSkill, error)
	CreateProjectSkill(context.Context, *database.ProjectSkill) error
	UpdateProjectSkill(context.Context, *database.ProjectSkill) error
	DeleteProjectSkill(context.Context, int64) error
}

type SkillService struct {
	store   SkillStore
	effects ApplicationEffects
}

func NewSkillService(store SkillStore, effects ApplicationEffects) *SkillService {
	return &SkillService{store: store, effects: effects}
}

func (s *SkillService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("skills")
}

type SkillInput struct {
	Name      string
	Content   string
	Enabled   bool
	Category  string
	SortOrder int
}

type UpdateSkillCommand struct {
	ID        int64
	Name      *string
	Content   *string
	Enabled   *bool
	Category  *string
	SortOrder *int
}

func (s *SkillService) List(ctx context.Context) ([]database.Skill, error) {
	items, err := s.store.ListSkills(ctx)
	if items == nil && err == nil {
		items = []database.Skill{}
	}
	return items, err
}

func (s *SkillService) Create(ctx context.Context, input SkillInput) (*database.Skill, error) {
	item := skillFromInput(input)
	if err := validateSkill(item.Name, item.Content); err != nil {
		return nil, err
	}
	if err := s.ensureUniqueName(ctx, item.Name, 0); err != nil {
		return nil, err
	}
	if err := s.store.CreateSkillWithVersion(ctx, item); err != nil {
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "skills", Action: "created", ID: item.ID})
	return item, nil
}

func (s *SkillService) Update(ctx context.Context, command UpdateSkillCommand) (*database.Skill, error) {
	current, err := s.get(ctx, command.ID)
	if err != nil {
		return nil, err
	}
	originalName, originalContent := current.Name, current.Content
	if command.Name != nil {
		current.Name = strings.TrimSpace(*command.Name)
	}
	if command.Content != nil {
		current.Content = *command.Content
	}
	if command.Enabled != nil {
		current.Enabled = *command.Enabled
	}
	if command.Category != nil {
		current.Category = strings.TrimSpace(*command.Category)
	}
	if command.SortOrder != nil {
		current.SortOrder = *command.SortOrder
	}
	if err = validateSkill(current.Name, current.Content); err != nil {
		return nil, err
	}
	if err = s.ensureUniqueName(ctx, current.Name, current.ID); err != nil {
		return nil, err
	}
	snapshot := current.Name != originalName || current.Content != originalContent
	if err = s.store.UpdateSkillWithVersion(ctx, current, snapshot); err != nil {
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "skills", Action: "updated", ID: current.ID})
	return current, nil
}

func (s *SkillService) Delete(ctx context.Context, id int64) error {
	if _, err := s.get(ctx, id); err != nil {
		return err
	}
	if err := s.store.DeleteSkill(ctx, id); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "skills", Action: "deleted", ID: id})
	return nil
}

func (s *SkillService) Duplicate(ctx context.Context, id int64, requestedName string) (*database.Skill, error) {
	original, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(requestedName)
	if name == "" {
		name = original.Name + " (Copy)"
	}
	return s.Create(ctx, SkillInput{Name: name, Content: original.Content, Enabled: original.Enabled, Category: original.Category, SortOrder: original.SortOrder})
}

type SkillImportResult struct {
	Imported []database.Skill
	Skipped  []string
}

func (s *SkillService) Import(ctx context.Context, inputs []SkillInput) (SkillImportResult, error) {
	result := SkillImportResult{Imported: []database.Skill{}, Skipped: []string{}}
	for _, input := range inputs {
		item, err := s.Create(ctx, input)
		if err != nil {
			if ErrorIsKind(err, ErrorValidation) || ErrorIsKind(err, ErrorConflict) {
				result.Skipped = append(result.Skipped, strings.TrimSpace(input.Name))
				continue
			}
			return result, err
		}
		result.Imported = append(result.Imported, *item)
	}
	return result, nil
}

func (s *SkillService) ListVersions(ctx context.Context, skillID int64) ([]database.SkillVersion, error) {
	if _, err := s.get(ctx, skillID); err != nil {
		return nil, err
	}
	versions, err := s.store.ListSkillVersions(ctx, skillID)
	if versions == nil && err == nil {
		versions = []database.SkillVersion{}
	}
	return versions, err
}

func (s *SkillService) Restore(ctx context.Context, skillID, versionID int64) (*database.Skill, error) {
	if skillID <= 0 || versionID <= 0 {
		return nil, validationError("invalid_skill_version", "Skill and version IDs must be positive")
	}
	item, err := s.store.RestoreSkillVersionAtomic(ctx, skillID, versionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, notFoundError("skill_version_not_found", "Skill version not found", err)
		}
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "skills", Action: "restored", ID: skillID, Meta: map[string]any{"version_id": versionID}})
	return item, nil
}

func (s *SkillService) ListProjectConfig(ctx context.Context, projectID int64) ([]database.ProjectSkillConfig, error) {
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	items, err := s.store.ListProjectSkillConfigs(ctx, projectID)
	if items == nil && err == nil {
		items = []database.ProjectSkillConfig{}
	}
	return items, err
}

func (s *SkillService) ReplaceProjectConfig(ctx context.Context, projectID int64, configs map[int64]bool) error {
	return s.SetProjectConfig(ctx, projectID, false, configs)
}

// SetProjectConfig keeps the project policy and its overrides on the same
// application path for both local UI and Automation callers.
func (s *SkillService) SetProjectConfig(ctx context.Context, projectID int64, inherit bool, configs map[int64]bool) error {
	if err := s.ensureProject(ctx, projectID); err != nil {
		return err
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return notFoundError("project_not_found", "Project not found", err)
	}
	if inherit {
		configs = map[int64]bool{}
		project.SkillPolicy = ""
	} else {
		project.SkillPolicy = "custom"
		for skillID := range configs {
			if _, err := s.get(ctx, skillID); err != nil {
				return err
			}
		}
	}
	if err := s.store.SetProjectSkillConfigAtomic(ctx, projectID, project.SkillPolicy, configs); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "skills", Action: "project_config_replaced", ID: projectID})
	return nil
}

func (s *SkillService) ListProjectSkills(ctx context.Context, projectID int64) ([]database.ProjectSkill, error) {
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	items, err := s.store.ListProjectSkills(ctx, projectID)
	if items == nil && err == nil {
		items = []database.ProjectSkill{}
	}
	return items, err
}

func (s *SkillService) CreateProjectSkill(ctx context.Context, projectID int64, input SkillInput) (*database.ProjectSkill, error) {
	if err := s.ensureProject(ctx, projectID); err != nil {
		return nil, err
	}
	if err := validateSkill(input.Name, input.Content); err != nil {
		return nil, err
	}
	item := &database.ProjectSkill{ProjectID: projectID, Name: strings.TrimSpace(input.Name), Content: input.Content, Enabled: input.Enabled, Category: strings.TrimSpace(input.Category), SortOrder: input.SortOrder}
	if err := s.store.CreateProjectSkill(ctx, item); err != nil {
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "project_skills", Action: "created", ID: item.ID, Meta: map[string]any{"project_id": projectID}})
	return item, nil
}

func (s *SkillService) UpdateProjectSkill(ctx context.Context, projectID int64, command UpdateSkillCommand) (*database.ProjectSkill, error) {
	item, err := s.store.GetProjectSkill(ctx, command.ID)
	if err != nil || item == nil || item.ProjectID != projectID {
		return nil, notFoundError("project_skill_not_found", "Project skill not found", err)
	}
	if command.Name != nil {
		item.Name = strings.TrimSpace(*command.Name)
	}
	if command.Content != nil {
		item.Content = *command.Content
	}
	if command.Enabled != nil {
		item.Enabled = *command.Enabled
	}
	if command.Category != nil {
		item.Category = strings.TrimSpace(*command.Category)
	}
	if command.SortOrder != nil {
		item.SortOrder = *command.SortOrder
	}
	if err = validateSkill(item.Name, item.Content); err != nil {
		return nil, err
	}
	if err = s.store.UpdateProjectSkill(ctx, item); err != nil {
		return nil, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "project_skills", Action: "updated", ID: item.ID, Meta: map[string]any{"project_id": projectID}})
	return item, nil
}

func (s *SkillService) DeleteProjectSkill(ctx context.Context, projectID, id int64) error {
	item, err := s.store.GetProjectSkill(ctx, id)
	if err != nil || item == nil || item.ProjectID != projectID {
		return notFoundError("project_skill_not_found", "Project skill not found", err)
	}
	if err = s.store.DeleteProjectSkill(ctx, id); err != nil {
		return err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "project_skills", Action: "deleted", ID: id, Meta: map[string]any{"project_id": projectID}})
	return nil
}

func skillFromInput(input SkillInput) *database.Skill {
	return &database.Skill{Name: strings.TrimSpace(input.Name), Content: input.Content, Enabled: input.Enabled, Category: strings.TrimSpace(input.Category), SortOrder: input.SortOrder}
}

func validateSkill(name, content string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return validationError("skill_name_required", "Skill name is required")
	}
	if len(name) > 200 || strings.ContainsAny(name, `/\\`) || strings.Contains(name, "..") || filepath.Base(name) != name {
		return validationError("invalid_skill_name", "Skill name contains an invalid path sequence")
	}
	if strings.TrimSpace(content) == "" {
		return validationError("skill_content_required", "Skill content is required")
	}
	return nil
}

func (s *SkillService) get(ctx context.Context, id int64) (*database.Skill, error) {
	if id <= 0 {
		return nil, validationError("invalid_skill_id", "Skill ID must be positive")
	}
	item, err := s.store.GetSkill(ctx, id)
	if err != nil || item == nil {
		return nil, notFoundError("skill_not_found", "Skill not found", err)
	}
	return item, nil
}

func (s *SkillService) ensureUniqueName(ctx context.Context, name string, exceptID int64) error {
	items, err := s.store.ListSkills(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(item.Name, name) {
			return conflictError("skill_name_conflict", "A skill with this name already exists")
		}
	}
	return nil
}

func (s *SkillService) ensureProject(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid_project_id", "Project ID must be positive")
	}
	project, err := s.store.GetProject(ctx, id)
	if err != nil || project == nil {
		return notFoundError("project_not_found", "Project not found", err)
	}
	return nil
}
