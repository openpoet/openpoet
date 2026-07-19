package application

import (
	"context"
	"strings"

	"openpoet/internal/database"
)

const defaultTagColor = "#7aa2f7"

type TagStore interface {
	ListTags(context.Context) ([]database.Tag, error)
	ListCoordinationTags(context.Context) ([]database.Tag, error)
	GetTag(context.Context, int64) (*database.Tag, error)
	CreateTag(context.Context, *database.Tag) error
	UpdateTag(context.Context, *database.Tag) error
	DeleteTag(context.Context, int64) error
	ListProjectTagDetails(context.Context, int64) ([]database.ProjectTagWithDetails, error)
	ReplaceProjectTagIDs(context.Context, int64, []int64) error
}

type TagService struct {
	store TagStore
}

func NewTagService(store TagStore) *TagService {
	return &TagService{store: store}
}

func (s *TagService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("tags")
}

func (s *TagService) List(ctx context.Context) ([]database.Tag, error) {
	tags, err := s.store.ListTags(ctx)
	if tags == nil && err == nil {
		tags = []database.Tag{}
	}
	return tags, err
}

// ListCoordination returns tags flagged as coordination groups (coordination=1).
func (s *TagService) ListCoordination(ctx context.Context) ([]database.Tag, error) {
	tags, err := s.store.ListCoordinationTags(ctx)
	if tags == nil && err == nil {
		tags = []database.Tag{}
	}
	return tags, err
}

func (s *TagService) Create(ctx context.Context, name, color string) (*database.Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, validationError("tag_name_required", "Tag name is required")
	}
	color = normalizeTagColor(color)
	tag := &database.Tag{Name: name, Color: color}
	if err := s.store.CreateTag(ctx, tag); err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return nil, conflictError("tag_name_conflict", "A tag with this name already exists")
		}
		return nil, err
	}
	return tag, nil
}

type UpdateTagCommand struct {
	ID    int64
	Name  *string
	Color *string
}

func (s *TagService) Update(ctx context.Context, command UpdateTagCommand) (*database.Tag, error) {
	if command.ID <= 0 {
		return nil, validationError("invalid_tag_id", "Tag ID must be positive")
	}
	tag, err := s.store.GetTag(ctx, command.ID)
	if err != nil || tag == nil {
		return nil, notFoundError("tag_not_found", "Tag not found", err)
	}
	if command.Name != nil {
		name := strings.TrimSpace(*command.Name)
		if name == "" {
			return nil, validationError("tag_name_required", "Tag name is required")
		}
		tag.Name = name
	}
	if command.Color != nil {
		tag.Color = normalizeTagColor(*command.Color)
	}
	if err := s.store.UpdateTag(ctx, tag); err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return nil, conflictError("tag_name_conflict", "A tag with this name already exists")
		}
		return nil, err
	}
	return tag, nil
}

func (s *TagService) Delete(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid_tag_id", "Tag ID must be positive")
	}
	if _, err := s.store.GetTag(ctx, id); err != nil {
		return notFoundError("tag_not_found", "Tag not found", err)
	}
	return s.store.DeleteTag(ctx, id)
}

func (s *TagService) ListProject(ctx context.Context, projectID int64) ([]database.ProjectTagWithDetails, error) {
	if projectID <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}
	tags, err := s.store.ListProjectTagDetails(ctx, projectID)
	if tags == nil && err == nil {
		tags = []database.ProjectTagWithDetails{}
	}
	return tags, err
}

func (s *TagService) ReplaceProject(ctx context.Context, projectID int64, tagIDs []int64) error {
	if projectID <= 0 {
		return validationError("invalid_project_id", "Project ID must be positive")
	}
	seen := make(map[int64]struct{}, len(tagIDs))
	unique := make([]int64, 0, len(tagIDs))
	for _, id := range tagIDs {
		if id <= 0 {
			return validationError("invalid_tag_id", "Tag IDs must be positive")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return s.store.ReplaceProjectTagIDs(ctx, projectID, unique)
}

func normalizeTagColor(color string) string {
	color = strings.TrimSpace(color)
	if color == "" {
		return defaultTagColor
	}
	return color
}
