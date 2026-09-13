package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"openpoet/internal/database"
)

// ProjectsRootSettingKey names the setting holding the container directory that
// every OpenPoet-created project folder lives under.
const ProjectsRootSettingKey = "projects_root_path"

// ComposeFileName is the manifest written into every scaffolded project.
const ComposeFileName = "docker-compose.yml"

// SettingsReader is the slice of the settings store the scaffolder needs.
type SettingsReader interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// slugPattern constrains a directory name to something safe to join onto the
// projects root: lowercase, no separators, no dot-dot.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}[a-z0-9]$|^[a-z0-9]$`)

// ScaffoldProjectInput describes a project to create on disk and register.
type ScaffoldProjectInput struct {
	Name        string            `json:"name"`
	DirName     string            `json:"dir_name"`
	ComposeYAML string            `json:"compose_yaml"`
	Files       map[string]string `json:"files"`
	GitInit     bool              `json:"git_init"`
	Backend     string            `json:"backend"`
}

// ScaffoldProjectResult is what the caller gets back once the folder exists and
// the project row is written.
type ScaffoldProjectResult struct {
	Project     *database.Project `json:"project"`
	CreatedPath string            `json:"created_path"`
	Files       []string          `json:"files"`
}

// ProjectScaffoldService creates a project directory under the configured root,
// writes its scaffold files, and registers the project.
type ProjectScaffoldService struct {
	settings SettingsReader
	projects *ProjectService
}

func NewProjectScaffoldService(settings SettingsReader, projects *ProjectService) *ProjectScaffoldService {
	return &ProjectScaffoldService{settings: settings, projects: projects}
}

func (s *ProjectScaffoldService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("project_scaffold")
}

// Root returns the configured projects root, already validated as an existing,
// writable directory.
func (s *ProjectScaffoldService) Root(ctx context.Context) (string, error) {
	// An unset key comes back as sql.ErrNoRows, which is the "not configured"
	// case, not a failure.
	raw, err := s.settings.GetSetting(ctx, ProjectsRootSettingKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	root := strings.TrimSpace(raw)
	if root == "" {
		return "", validationError("projects_root_not_configured",
			"No projects root directory is configured. Set one under Settings → Project Creation.")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", validationError("invalid_projects_root", fmt.Sprintf("Invalid projects root: %s", root))
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", validationError("projects_root_missing", fmt.Sprintf("Projects root does not exist: %s", abs))
	}
	if !info.IsDir() {
		return "", validationError("projects_root_not_a_directory", fmt.Sprintf("Projects root is not a directory: %s", abs))
	}
	// Writability is checked by actually touching the directory — mode bits lie
	// under ACLs, read-only mounts and mismatched ownership.
	probe, err := os.CreateTemp(abs, ".openpoet-write-probe-*")
	if err != nil {
		return "", validationError("projects_root_not_writable",
			fmt.Sprintf("Projects root is not writable: %s", abs))
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)
	return abs, nil
}

// Slug normalizes a project name into a directory name.
func Slug(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range lower {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '.' || r == '_' || r == '-' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-._")
}

// Create builds the directory, writes the files and registers the project.
// On any failure after the directory is created, the directory is removed so a
// half-built project never lingers.
func (s *ProjectScaffoldService) Create(ctx context.Context, input ScaffoldProjectInput) (*ScaffoldProjectResult, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, validationError("project_name_required", "Project name is required")
	}

	root, err := s.Root(ctx)
	if err != nil {
		return nil, err
	}

	// A derived name is slugified; an explicitly supplied one is validated as
	// given, so a caller asking for "../etc" is refused rather than quietly
	// rewritten into something else.
	dirName := strings.TrimSpace(input.DirName)
	if dirName == "" {
		dirName = Slug(name)
	}
	if !slugPattern.MatchString(dirName) {
		return nil, validationError("invalid_directory_name",
			"Directory name must be lowercase letters, digits, '.', '_' or '-' (max 64 chars)")
	}

	target := filepath.Join(root, dirName)
	if err := ensureWithin(root, target); err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); err == nil {
		return nil, conflictError("project_directory_exists",
			fmt.Sprintf("Directory already exists: %s", target))
	}

	compose := strings.TrimSpace(input.ComposeYAML)
	if compose == "" {
		return nil, validationError("compose_required",
			"A docker-compose.yml body is required. Describe the stack so the assistant can generate one.")
	}
	if err := LintCompose(compose); err != nil {
		return nil, err
	}

	files := map[string]string{ComposeFileName: compose + "\n"}
	for rel, content := range input.Files {
		clean := strings.TrimSpace(rel)
		if clean == "" {
			continue
		}
		dest := filepath.Join(target, filepath.Clean(clean))
		if err := ensureWithin(target, dest); err != nil {
			return nil, err
		}
		files[filepath.Clean(clean)] = content
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("create project directory: %w", err)
	}

	written := make([]string, 0, len(files))
	cleanup := func() { _ = os.RemoveAll(target) }

	for rel, content := range files {
		dest := filepath.Join(target, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			cleanup()
			return nil, fmt.Errorf("create %s: %w", rel, err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			cleanup()
			return nil, fmt.Errorf("write %s: %w", rel, err)
		}
		written = append(written, rel)
	}

	if input.GitInit {
		cmd := exec.CommandContext(ctx, "git", "init", "-q")
		cmd.Dir = target
		if out, err := cmd.CombinedOutput(); err != nil {
			cleanup()
			return nil, fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	project, err := s.projects.Create(ctx, database.ProjectInput{
		Name:    name,
		Path:    target,
		Type:    "local",
		Backend: strings.TrimSpace(input.Backend),
	})
	if err != nil {
		cleanup()
		return nil, err
	}

	return &ScaffoldProjectResult{Project: project, CreatedPath: target, Files: written}, nil
}

// ensureWithin rejects any path that escapes base once cleaned — the guard that
// keeps a crafted name or file key from writing outside the projects root.
func ensureWithin(base, candidate string) error {
	rel, err := filepath.Rel(base, filepath.Clean(candidate))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return validationError("path_escapes_root",
			fmt.Sprintf("Path escapes its container directory: %s", candidate))
	}
	return nil
}
