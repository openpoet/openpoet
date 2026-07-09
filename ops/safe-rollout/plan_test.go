package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildPlanOrdersSafetyGates(t *testing.T) {
	prepare, err := BuildPlan(OperationPrepare)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := BuildPlan(OperationApply)
	if err != nil {
		t.Fatal(err)
	}

	if stepIndex(prepare, "test") >= stepIndex(prepare, "build") {
		t.Fatal("tests must run before build")
	}
	if stepIndex(apply, "backup") >= stepIndex(apply, "graceful-stop") {
		t.Fatal("backup must happen before graceful stop")
	}
	if stepIndex(apply, "switch-binary") >= stepIndex(apply, "start") {
		t.Fatal("binary switch must happen before start")
	}
	if stepIndex(apply, "start") >= stepIndex(apply, "health-gates") {
		t.Fatal("health gates must happen after start")
	}
}

func TestApplyAuthorizationRequiresFlagAndExactToken(t *testing.T) {
	if err := ValidateApplyAuthorization(false, "", "expected"); err != nil {
		t.Fatalf("dry-run should not require token: %v", err)
	}
	if err := ValidateApplyAuthorization(true, "", "expected"); err == nil {
		t.Fatal("expected missing token to fail")
	}
	if err := ValidateApplyAuthorization(true, "wrong", "expected"); err == nil {
		t.Fatal("expected wrong token to fail")
	}
	if err := ValidateApplyAuthorization(true, "expected", "expected"); err != nil {
		t.Fatalf("valid authorization failed: %v", err)
	}
}

func TestValidateReleaseIDRejectsPathTraversal(t *testing.T) {
	for _, value := range []string{"../release", "release/child", "", "space here"} {
		if err := ValidateReleaseID(value); err == nil {
			t.Fatalf("expected invalid release ID %q", value)
		}
	}
	if err := ValidateReleaseID("20260709T120000Z-abc123"); err != nil {
		t.Fatal(err)
	}
}

func stepIndex(steps []Step, name string) int {
	for index, step := range steps {
		if step.Name == name {
			return index
		}
	}
	return len(steps) + 1
}

func TestBackupSQLiteCreatesConsistentIndependentSnapshot(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "live.sqlite")
	backupPath := filepath.Join(directory, "backup.sqlite")
	db := createTestDatabase(t, sourcePath)
	defer db.Close()

	if _, err := db.Exec("INSERT INTO entries(value) VALUES ('before')"); err != nil {
		t.Fatal(err)
	}
	if err := BackupSQLite(sourcePath, backupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO entries(value) VALUES ('after')"); err != nil {
		t.Fatal(err)
	}

	backup, err := sql.Open("sqlite", sqliteReadOnlyDSN(backupPath))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var count int
	if err := backup.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backup count = %d, want 1", count)
	}
	if err := QuickCheckSQLite(backupPath); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicInstallAndRestoreBinary(t *testing.T) {
	directory := t.TempDir()
	livePath := filepath.Join(directory, "openpoet")
	candidatePath := filepath.Join(directory, "candidate")
	if err := os.WriteFile(livePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidatePath, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	previousPath, err := AtomicInstall(candidatePath, livePath, "release-1")
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, livePath, "new")
	assertFileContent(t, previousPath, "old")

	if err := RestoreBinary(previousPath, livePath, "release-1"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, livePath, "old")
}

func TestPrepareRunsTestsBeforeBuildAndPublishesManifest(t *testing.T) {
	directory := t.TempDir()
	repoDir := filepath.Join(directory, "repo")
	releasesDir := filepath.Join(directory, "releases")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name == "git" && reflect.DeepEqual(args, []string{"status", "--porcelain"}) {
			return nil, nil
		}
		if name == "git" && reflect.DeepEqual(args, []string{"rev-parse", "--short=12", "HEAD"}) {
			return []byte("abc123def456\n"), nil
		}
		if name == "go" && len(args) > 0 && args[0] == "test" {
			return []byte("ok"), nil
		}
		if name == "go" && len(args) > 0 && args[0] == "build" {
			for index := range args {
				if args[index] == "-o" && index+1 < len(args) {
					return nil, os.WriteFile(args[index+1], []byte("candidate"), 0o755)
				}
			}
		}
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}}
	workflow := NewWorkflow(os.Stdout)
	workflow.Runner = runner
	workflow.Now = func() time.Time {
		return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	}

	manifest, manifestPath, err := workflow.Prepare(context.Background(), Config{
		RepoDir:     repoDir,
		ReleasesDir: releasesDir,
		ReleaseID:   "release-prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ReleaseID != "release-prepare" || manifest.ConfirmationToken == "" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(manifest); err != nil {
		t.Fatal(err)
	}

	if runner.indexOf("go", "test") >= runner.indexOf("go", "build") {
		t.Fatalf("commands out of order: %v", runner.calls)
	}
}

func TestApplyRejectsInvalidTokenBeforeExternalCommands(t *testing.T) {
	directory := t.TempDir()
	manifestPath, _ := writeTestRelease(t, directory, "release-token", "expected")
	runner := &fakeRunner{run: func(_ context.Context, _ string, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("should not run")
	}}
	workflow := NewWorkflow(os.Stdout)
	workflow.Runner = runner

	_, err := workflow.Apply(context.Background(), Config{
		Execute:      true,
		ConfirmToken: "wrong",
		ManifestPath: manifestPath,
	})
	if err == nil || !strings.Contains(err.Error(), "token inválido") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("external commands executed: %v", runner.calls)
	}
}

func TestApplyUsesFakeLaunchdAndPassesHealthGates(t *testing.T) {
	directory := t.TempDir()
	manifestPath, manifest := writeTestRelease(t, directory, "release-apply", "token-apply")
	livePath := filepath.Join(directory, "runtime", "openpoet")
	plistPath := filepath.Join(directory, "runtime", "openpoet.plist")
	dbPath := filepath.Join(directory, "runtime", "openpoet.sqlite")
	backupDir := filepath.Join(directory, "backups")
	if err := os.MkdirAll(filepath.Dir(livePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := createTestDatabase(t, dbPath)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var serviceUp atomic.Bool
	serviceUp.Store(true)
	var version atomic.Value
	version.Store("old-release")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if !serviceUp.Load() {
			http.Error(writer, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(writer, `{"version":%q}`, version.Load().(string))
	}))
	defer server.Close()

	runner := &fakeRunner{run: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name != "launchctl" {
			return nil, fmt.Errorf("unexpected command: %s", name)
		}
		switch args[0] {
		case "bootout":
			serviceUp.Store(false)
		case "bootstrap":
			version.Store(manifest.ReleaseID)
			serviceUp.Store(true)
		default:
			return nil, fmt.Errorf("unexpected launchctl args: %v", args)
		}
		return nil, nil
	}}
	workflow := NewWorkflow(os.Stdout)
	workflow.Runner = runner
	workflow.Now = func() time.Time {
		return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	}

	result, err := workflow.Apply(context.Background(), Config{
		Execute:        true,
		ConfirmToken:   manifest.ConfirmationToken,
		ManifestPath:   manifestPath,
		LiveBinary:     livePath,
		DBPath:         dbPath,
		BackupDir:      backupDir,
		PlistPath:      plistPath,
		ServiceLabel:   "test.openpoet",
		LaunchDomain:   "gui/501",
		HealthURL:      server.URL,
		StopTimeout:    time.Second,
		HealthTimeout:  time.Second,
		HealthInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, livePath, "candidate-binary")
	assertFileContent(t, result.PreviousBinary, "old-binary")
	if err := QuickCheckSQLite(result.BackupPath); err != nil {
		t.Fatal(err)
	}
	if runner.indexOf("launchctl", "bootout") >= runner.indexOf("launchctl", "bootstrap") {
		t.Fatalf("launchctl commands out of order: %v", runner.calls)
	}
}

type commandCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []commandCall
	run   func(context.Context, string, string, ...string) ([]byte, error)
}

func (runner *fakeRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, commandCall{name: name, args: append([]string(nil), args...)})
	return runner.run(ctx, dir, name, args...)
}

func (runner *fakeRunner) indexOf(name, firstArg string) int {
	for index, call := range runner.calls {
		if call.name == name && len(call.args) > 0 && call.args[0] == firstArg {
			return index
		}
	}
	return len(runner.calls) + 1
}

func createTestDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE entries (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func writeTestRelease(t *testing.T, root, releaseID, token string) (string, Manifest) {
	t.Helper()
	releaseDir := filepath.Join(root, "releases", releaseID)
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(releaseDir, "openpoet")
	if err := os.WriteFile(artifactPath, []byte("candidate-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, err := FileSHA256(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		ReleaseID:         releaseID,
		GitSHA:            "abc123",
		ArtifactSHA256:    hash,
		ArtifactPath:      artifactPath,
		ConfirmationToken: token,
		PreparedAt:        time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	manifestPath := filepath.Join(releaseDir, "manifest.json")
	if err := WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	return manifestPath, manifest
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != expected {
		t.Fatalf("%s = %q, want %q", path, content, expected)
	}
}
