package application

import (
	"context"
	"database/sql"
	"testing"

	"openpoet/internal/database"
)

type fakeProjectStore struct {
	projects map[int64]*database.Project
	nextID   int64
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{projects: make(map[int64]*database.Project), nextID: 1}
}

func (s *fakeProjectStore) ListProjects(context.Context) ([]database.Project, error) {
	result := make([]database.Project, 0, len(s.projects))
	for _, project := range s.projects {
		result = append(result, *project)
	}
	return result, nil
}

func (s *fakeProjectStore) GetProject(_ context.Context, id int64) (*database.Project, error) {
	project := s.projects[id]
	if project == nil {
		return nil, sql.ErrNoRows
	}
	copy := *project
	return &copy, nil
}

func (s *fakeProjectStore) CreateProject(_ context.Context, project *database.Project) error {
	project.ID = s.nextID
	s.nextID++
	copy := *project
	s.projects[project.ID] = &copy
	return nil
}

func (s *fakeProjectStore) UpdateProject(_ context.Context, project *database.Project) error {
	copy := *project
	s.projects[project.ID] = &copy
	return nil
}

func (s *fakeProjectStore) DeleteProject(_ context.Context, id int64) error {
	delete(s.projects, id)
	return nil
}

type fakeEncryptor struct{}

func (fakeEncryptor) Encrypt(value string) (string, string, error) {
	return "encrypted:" + value, "iv", nil
}

type projectEffectsRecorder struct{ changes []ProjectChange }

func (r *projectEffectsRecorder) PublishProjectChange(change ProjectChange) {
	r.changes = append(r.changes, change)
}

func TestProjectServiceOwnsValidationCredentialBoundaryAndEffects(t *testing.T) {
	store := newFakeProjectStore()
	effects := &projectEffectsRecorder{}
	service := NewProjectService(store, fakeEncryptor{}, effects)

	project, err := service.Create(context.Background(), database.ProjectInput{
		Name: " Remote ", Path: "/srv/app", Type: "remote", Backend: "codex",
		SSHHost: "host", SSHPort: 22, SSHUser: "user", SSHAuthType: "password", SSHCredential: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "Remote" || project.SSHCredentialEncrypted.String != "encrypted:secret" {
		t.Fatalf("unexpected project: %+v", project)
	}
	duplicate, err := service.Duplicate(context.Background(), DuplicateProjectCommand{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Name != "Remote (Copy)" || duplicate.ID == project.ID {
		t.Fatalf("unexpected duplicate: %+v", duplicate)
	}
	if err := service.Delete(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	if len(effects.changes) != 3 || effects.changes[2].Action != "deleted" {
		t.Fatalf("effects=%+v", effects.changes)
	}
	if _, err := service.Create(context.Background(), database.ProjectInput{Name: "bad", Type: "local"}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

type fakeTagStore struct {
	tags        map[int64]*database.Tag
	nextID      int64
	projectTags []int64
}

func newFakeTagStore() *fakeTagStore {
	return &fakeTagStore{tags: make(map[int64]*database.Tag), nextID: 1}
}

func (s *fakeTagStore) ListTags(context.Context) ([]database.Tag, error) {
	result := make([]database.Tag, 0, len(s.tags))
	for _, tag := range s.tags {
		result = append(result, *tag)
	}
	return result, nil
}

func (s *fakeTagStore) ListCoordinationTags(context.Context) ([]database.Tag, error) {
	result := make([]database.Tag, 0)
	for _, tag := range s.tags {
		if tag.Coordination == 1 {
			result = append(result, *tag)
		}
	}
	return result, nil
}

func (s *fakeTagStore) GetTag(_ context.Context, id int64) (*database.Tag, error) {
	tag := s.tags[id]
	if tag == nil {
		return nil, sql.ErrNoRows
	}
	copy := *tag
	return &copy, nil
}

func (s *fakeTagStore) CreateTag(_ context.Context, tag *database.Tag) error {
	tag.ID = s.nextID
	s.nextID++
	copy := *tag
	s.tags[tag.ID] = &copy
	return nil
}

func (s *fakeTagStore) UpdateTag(_ context.Context, tag *database.Tag) error {
	copy := *tag
	s.tags[tag.ID] = &copy
	return nil
}

func (s *fakeTagStore) DeleteTag(_ context.Context, id int64) error {
	delete(s.tags, id)
	return nil
}

func (s *fakeTagStore) ListProjectTagDetails(context.Context, int64) ([]database.ProjectTagWithDetails, error) {
	return []database.ProjectTagWithDetails{}, nil
}

func (s *fakeTagStore) ReplaceProjectTagIDs(_ context.Context, _ int64, ids []int64) error {
	s.projectTags = append([]int64(nil), ids...)
	return nil
}

func TestTagServiceNormalizesAndDeduplicatesAssignments(t *testing.T) {
	store := newFakeTagStore()
	service := NewTagService(store)
	tag, err := service.Create(context.Background(), " Backend ", "")
	if err != nil {
		t.Fatal(err)
	}
	if tag.Name != "Backend" || tag.Color != defaultTagColor {
		t.Fatalf("unexpected tag: %+v", tag)
	}
	if err := service.ReplaceProject(context.Background(), 9, []int64{tag.ID, tag.ID, 2}); err != nil {
		t.Fatal(err)
	}
	if len(store.projectTags) != 2 || store.projectTags[0] != tag.ID || store.projectTags[1] != 2 {
		t.Fatalf("assignments=%v", store.projectTags)
	}
	if err := service.Delete(context.Background(), tag.ID); err != nil {
		t.Fatal(err)
	}
}

type fakeNotificationBackend struct {
	limit         int
	marked        int64
	allRead       bool
	notifications []database.Notification
}

func (b *fakeNotificationBackend) GetRecent(_ context.Context, limit int) ([]database.Notification, error) {
	b.limit = limit
	return b.notifications, nil
}

func (b *fakeNotificationBackend) GetActive(context.Context) ([]database.Notification, error) {
	return b.notifications, nil
}

func (b *fakeNotificationBackend) GetUnreadCount(context.Context) (int, error) {
	return len(b.notifications), nil
}

func (b *fakeNotificationBackend) MarkRead(_ context.Context, id int64) error {
	b.marked = id
	return nil
}

func (b *fakeNotificationBackend) MarkAllRead(context.Context) error {
	b.allRead = true
	return nil
}

func TestNotificationServiceBoundsQueriesAndValidatesMutation(t *testing.T) {
	backend := &fakeNotificationBackend{notifications: []database.Notification{{ID: 1}}}
	service := NewNotificationService(backend)
	items, err := service.List(context.Background(), 10_000)
	if err != nil || len(items) != 1 || backend.limit != 200 {
		t.Fatalf("items=%v limit=%d err=%v", items, backend.limit, err)
	}
	if err := service.MarkRead(context.Background(), 0); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if err := service.MarkRead(context.Background(), 1); err != nil || backend.marked != 1 {
		t.Fatalf("marked=%d err=%v", backend.marked, err)
	}
	if err := service.MarkAllRead(context.Background()); err != nil || !backend.allRead {
		t.Fatalf("allRead=%v err=%v", backend.allRead, err)
	}
}
