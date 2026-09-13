package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(_ context.Context, key string) (string, error) {
	return f[key], nil
}

// permissivePathValidator accepts the freshly created directory; the real
// validator also only requires the directory to exist, which it does by then.
type permissivePathValidator struct{}

func (permissivePathValidator) ValidateProjectPath(_ context.Context, path, _ string) error {
	if _, err := os.Stat(path); err != nil {
		return validationError("invalid_project_path", "path does not exist")
	}
	return nil
}

func newScaffolder(t *testing.T, root string) (*ProjectScaffoldService, *fakeProjectStore) {
	t.Helper()
	store := newFakeProjectStore()
	projects := NewProjectService(store, fakeEncryptor{}, &projectEffectsRecorder{}, permissivePathValidator{})
	return NewProjectScaffoldService(fakeSettings{ProjectsRootSettingKey: root}, projects), store
}

const okCompose = "services:\n  app:\n    image: nginx:alpine\n    ports:\n      - \"3000:80\"\n"

func TestScaffoldCreatesDirectoryAndFiles(t *testing.T) {
	root := t.TempDir()
	svc, store := newScaffolder(t, root)

	result, err := svc.Create(context.Background(), ScaffoldProjectInput{
		Name:        "My New App",
		ComposeYAML: okCompose,
		Files:       map[string]string{"README.md": "# hi\n"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := filepath.Join(root, "my-new-app")
	if result.CreatedPath != want {
		t.Errorf("CreatedPath = %q, want %q", result.CreatedPath, want)
	}
	for _, rel := range []string{ComposeFileName, "README.md"} {
		if _, err := os.Stat(filepath.Join(want, rel)); err != nil {
			t.Errorf("expected %s to exist: %v", rel, err)
		}
	}
	if len(store.projects) != 1 {
		t.Fatalf("expected exactly one project row, got %d", len(store.projects))
	}
	if got := store.projects[result.Project.ID].Path; got != want {
		t.Errorf("registered project path = %q, want %q", got, want)
	}
}

func TestScaffoldRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	svc, _ := newScaffolder(t, root)

	cases := []struct{ name, dir string }{
		{"dot dot", ".."},
		{"traversal", "../../etc"},
		{"absolute", "/etc/passwd"},
		{"slash", "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(context.Background(), ScaffoldProjectInput{
				Name: "x", DirName: tc.dir, ComposeYAML: okCompose,
			})
			if err == nil {
				t.Fatal("expected the escaping directory name to be refused")
			}
			if !ErrorIsKind(err, ErrorValidation) {
				t.Errorf("expected a validation error, got %v", err)
			}
		})
	}
}

func TestScaffoldRejectsEscapingFileKey(t *testing.T) {
	root := t.TempDir()
	svc, _ := newScaffolder(t, root)

	_, err := svc.Create(context.Background(), ScaffoldProjectInput{
		Name:        "app",
		ComposeYAML: okCompose,
		Files:       map[string]string{"../../../etc/cron.d/pwn": "* * * * * root sh\n"},
	})
	if err == nil {
		t.Fatal("expected an escaping file key to be refused")
	}
	if _, statErr := os.Stat(filepath.Join(root, "app")); statErr == nil {
		t.Error("the project directory should not survive a refused scaffold")
	}
}

func TestScaffoldRefusesExistingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc, _ := newScaffolder(t, root)

	_, err := svc.Create(context.Background(), ScaffoldProjectInput{
		Name: "taken", ComposeYAML: okCompose,
	})
	if !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("expected a conflict error, got %v", err)
	}
}

func TestScaffoldRequiresConfiguredRoot(t *testing.T) {
	store := newFakeProjectStore()
	projects := NewProjectService(store, fakeEncryptor{}, &projectEffectsRecorder{}, permissivePathValidator{})
	svc := NewProjectScaffoldService(fakeSettings{}, projects)

	_, err := svc.Create(context.Background(), ScaffoldProjectInput{Name: "x", ComposeYAML: okCompose})
	if ErrorCode(err) != "projects_root_not_configured" {
		t.Fatalf("expected projects_root_not_configured, got %v", err)
	}
}

func TestScaffoldRollsBackOnRefusedCompose(t *testing.T) {
	root := t.TempDir()
	svc, store := newScaffolder(t, root)

	_, err := svc.Create(context.Background(), ScaffoldProjectInput{
		Name:        "bad",
		ComposeYAML: "services:\n  app:\n    image: nginx\n    privileged: true\n",
	})
	if err == nil {
		t.Fatal("expected the privileged compose to be refused")
	}
	if _, statErr := os.Stat(filepath.Join(root, "bad")); statErr == nil {
		t.Error("no directory should be left behind")
	}
	if len(store.projects) != 0 {
		t.Error("no project row should be registered")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"My New App":     "my-new-app",
		"  Spaced  Out ": "spaced-out",
		"API_v2":         "api-v2",
		"Já-Existe!!":    "j-existe",
		"---":            "",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLintComposeRefusals(t *testing.T) {
	cases := map[string]struct{ body, code string }{
		"privileged":    {"services:\n  a:\n    privileged: true\n", "compose_privileged"},
		"host network":  {"services:\n  a:\n    network_mode: host\n", "compose_host_namespace"},
		"host pid":      {"services:\n  a:\n    pid: \"host\"\n", "compose_host_namespace"},
		"docker socket": {"services:\n  a:\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n", "compose_docker_socket"},
		"root mount":    {"services:\n  a:\n    volumes:\n      - /:/host\n", "compose_host_bind_mount"},
		"home mount":    {"services:\n  a:\n    volumes:\n      - /home/user/.ssh:/keys\n", "compose_host_bind_mount"},
		"cap_add":       {"services:\n  a:\n    cap_add:\n      - SYS_ADMIN\n", "compose_cap_add"},
		"prod port":     {"services:\n  a:\n    ports:\n      - \"8081:80\"\n", "compose_reserved_port"},
		"dev port":      {"services:\n  a:\n    ports:\n      - \"8080:3000\"\n", "compose_reserved_port"},
		"bound prod":    {"services:\n  a:\n    ports:\n      - \"127.0.0.1:8081:80\"\n", "compose_reserved_port"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := LintCompose(tc.body)
			if err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
			if got := ErrorCode(err); got != tc.code {
				t.Errorf("code = %q, want %q (%v)", got, tc.code, err)
			}
		})
	}
}

func TestLintComposeAccepts(t *testing.T) {
	good := []string{
		okCompose,
		"services:\n  db:\n    image: postgres:16\n    volumes:\n      - ./data:/var/lib/postgresql/data\n    ports:\n      - \"5432:5432\"\n",
		"services:\n  web:\n    build: .\n    ports:\n      - \"127.0.0.1:3000:3000\"\n    environment:\n      NODE_ENV: production\n",
		"volumes:\n  pgdata:\nservices:\n  db:\n    image: postgres:16\n    volumes:\n      - pgdata:/var/lib/postgresql/data\n",
	}
	for i, body := range good {
		if err := LintCompose(body); err != nil {
			t.Errorf("case %d refused unexpectedly: %v\n%s", i, err, strings.TrimSpace(body))
		}
	}
}
