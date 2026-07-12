package application

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"openpoet/internal/database"
)

const (
	maxGitPaths         = 200
	maxGitCommitRunes   = 4000
	maxGitPathTotalSize = 64 << 10
)

type GitCommitResult struct {
	Message string
	Hash    string
}

type GitMutationPort interface {
	EnsureRepository(context.Context, *database.Project) error
	Stage(context.Context, *database.Project, []string) error
	Unstage(context.Context, *database.Project, []string) error
	Commit(context.Context, *database.Project, string) (GitCommitResult, error)
}

type GitMutationChange struct {
	Action    string
	ProjectID int64
	Paths     []string
	Hash      string
	Actor     Actor
}

type GitMutationEffects interface {
	PublishGitMutation(context.Context, GitMutationChange)
}

type GitMutationService struct {
	store   SessionStore
	git     GitMutationPort
	effects GitMutationEffects
}

func NewGitMutationService(store SessionStore, git GitMutationPort, effects GitMutationEffects) *GitMutationService {
	return &GitMutationService{store: store, git: git, effects: effects}
}

type StageGitFilesCommand struct {
	ProjectID     int64
	Files         []string
	Authorization ActionAuthorization
}

func (s *GitMutationService) Stage(ctx context.Context, command StageGitFilesCommand) error {
	if err := requireActionActor(command.Authorization); err != nil {
		return err
	}
	project, files, err := s.prepare(ctx, command.ProjectID, command.Files)
	if err != nil {
		return err
	}
	if err := s.git.Stage(ctx, project, files); err != nil {
		return err
	}
	s.publish(ctx, GitMutationChange{Action: "staged", ProjectID: project.ID, Paths: files, Actor: command.Authorization.Actor})
	return nil
}

type UnstageGitFilesCommand struct {
	ProjectID     int64
	Files         []string
	Authorization ActionAuthorization
}

func (s *GitMutationService) Unstage(ctx context.Context, command UnstageGitFilesCommand) error {
	if err := requireActionActor(command.Authorization); err != nil {
		return err
	}
	project, files, err := s.prepare(ctx, command.ProjectID, command.Files)
	if err != nil {
		return err
	}
	if err := s.git.Unstage(ctx, project, files); err != nil {
		return err
	}
	s.publish(ctx, GitMutationChange{Action: "unstaged", ProjectID: project.ID, Paths: files, Actor: command.Authorization.Actor})
	return nil
}

type CommitGitCommand struct {
	ProjectID     int64
	Message       string
	Authorization ActionAuthorization
}

func (s *GitMutationService) Commit(ctx context.Context, command CommitGitCommand) (GitCommitResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return GitCommitResult{}, err
	}
	message := strings.TrimSpace(command.Message)
	if message == "" || utf8.RuneCountInString(message) > maxGitCommitRunes || strings.IndexByte(message, 0) >= 0 {
		return GitCommitResult{}, validationError("git_commit_message_invalid", "Commit message is required and must not exceed 4000 characters")
	}
	project, err := s.getProject(ctx, command.ProjectID)
	if err != nil {
		return GitCommitResult{}, err
	}
	if s.git == nil {
		return GitCommitResult{}, validationError("git_mutator_unavailable", "Git mutator is unavailable")
	}
	if err := s.git.EnsureRepository(ctx, project); err != nil {
		return GitCommitResult{}, err
	}
	result, err := s.git.Commit(ctx, project, message)
	if err != nil {
		return GitCommitResult{}, err
	}
	s.publish(ctx, GitMutationChange{Action: "committed", ProjectID: project.ID, Hash: result.Hash, Actor: command.Authorization.Actor})
	return result, nil
}

func (s *GitMutationService) prepare(ctx context.Context, projectID int64, files []string) (*database.Project, []string, error) {
	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	normalized, err := normalizeGitPaths(files)
	if err != nil {
		return nil, nil, err
	}
	if s.git == nil {
		return nil, nil, validationError("git_mutator_unavailable", "Git mutator is unavailable")
	}
	if err := s.git.EnsureRepository(ctx, project); err != nil {
		return nil, nil, err
	}
	return project, normalized, nil
}

func (s *GitMutationService) getProject(ctx context.Context, projectID int64) (*database.Project, error) {
	if projectID <= 0 {
		return nil, validationError("invalid_project_id", "Project ID must be positive")
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		if errors.Is(err, sql.ErrNoRows) || project == nil {
			return nil, notFoundError("project_not_found", "Project not found", err)
		}
		return nil, err
	}
	return project, nil
}

func normalizeGitPaths(files []string) ([]string, error) {
	if len(files) == 0 || len(files) > maxGitPaths {
		return nil, validationError("git_files_count_invalid", "Git mutation must contain between 1 and 200 paths")
	}
	result := make([]string, len(files))
	total := 0
	for i, file := range files {
		if strings.TrimSpace(file) == "." {
			result[i] = "."
			total++
			continue
		}
		normalized, err := normalizeRelativePath(file)
		if err != nil {
			return nil, err
		}
		total += len(normalized)
		if total > maxGitPathTotalSize {
			return nil, validationError("git_paths_too_large", "Git path payload exceeds 64 KiB")
		}
		result[i] = normalized
	}
	return result, nil
}

func (s *GitMutationService) publish(ctx context.Context, change GitMutationChange) {
	if s.effects != nil {
		s.effects.PublishGitMutation(ctx, change)
	}
}
