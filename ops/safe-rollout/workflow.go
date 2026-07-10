package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/security"
)

type Workflow struct {
	Runner         CommandRunner
	Health         HealthClient
	Now            func() time.Time
	Output         io.Writer
	MigrateSecrets func(context.Context, string, string) (database.LegacyRuntimeSecretMigrationReport, error)
}

type ApplyResult struct {
	ReleaseID       string
	BackupPath      string
	PreviousBinary  string
	SecretMigration database.LegacyRuntimeSecretMigrationReport
}

func NewWorkflow(output io.Writer) Workflow {
	return Workflow{
		Runner: ExecRunner{},
		Health: defaultHealthClient(),
		Now:    time.Now, Output: output, MigrateSecrets: migrateRuntimeSecrets,
	}
}

func (workflow Workflow) Prepare(ctx context.Context, config Config) (Manifest, string, error) {
	var empty Manifest
	if err := validatePrepareConfig(config); err != nil {
		return empty, "", err
	}

	status, err := workflow.Runner.Run(ctx, config.RepoDir, "git", "status", "--porcelain")
	if err != nil {
		return empty, "", err
	}
	if strings.TrimSpace(string(status)) != "" {
		return empty, "", errors.New("prepare recusado: worktree contém alterações não commitadas")
	}
	shaOutput, err := workflow.Runner.Run(ctx, config.RepoDir, "git", "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return empty, "", err
	}
	gitSHA := strings.TrimSpace(string(shaOutput))
	if gitSHA == "" {
		return empty, "", errors.New("git SHA vazio")
	}

	releaseID := config.ReleaseID
	if releaseID == "" {
		releaseID = workflow.Now().UTC().Format("20060102T150405Z") + "-" + gitSHA
	}
	if err := ValidateReleaseID(releaseID); err != nil {
		return empty, "", err
	}
	if strings.ContainsAny(config.RelayURL, " \t\r\n") {
		return empty, "", errors.New("relay-url não pode conter espaços")
	}

	testCommand := []string{"go", "test", "./..."}
	if _, err := workflow.Runner.Run(ctx, config.RepoDir, testCommand[0], testCommand[1:]...); err != nil {
		return empty, "", fmt.Errorf("tests failed: %w", err)
	}

	if err := os.MkdirAll(config.ReleasesDir, 0o700); err != nil {
		return empty, "", err
	}
	finalDir := filepath.Join(config.ReleasesDir, releaseID)
	if _, err := os.Stat(finalDir); !os.IsNotExist(err) {
		if err == nil {
			return empty, "", fmt.Errorf("release já existe: %s", finalDir)
		}
		return empty, "", err
	}
	temporaryDir, err := os.MkdirTemp(config.ReleasesDir, ".prepare-"+releaseID+"-")
	if err != nil {
		return empty, "", err
	}
	keepTemporary := false
	defer func() {
		if !keepTemporary {
			_ = os.RemoveAll(temporaryDir)
		}
	}()

	temporaryArtifact := filepath.Join(temporaryDir, "openpoet")
	ldflags := fmt.Sprintf(
		"-X main.BuildVersion=%s -X main.DefaultRelayURL=%s -X main.DebugDefault=false",
		releaseID,
		config.RelayURL,
	)
	buildCommand := []string{
		"go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", temporaryArtifact, "./cmd/openpoet",
	}
	if _, err := workflow.Runner.Run(ctx, config.RepoDir, buildCommand[0], buildCommand[1:]...); err != nil {
		return empty, "", fmt.Errorf("build failed: %w", err)
	}
	if err := os.Chmod(temporaryArtifact, 0o755); err != nil {
		return empty, "", err
	}
	artifactHash, err := FileSHA256(temporaryArtifact)
	if err != nil {
		return empty, "", err
	}
	token, err := NewConfirmationToken()
	if err != nil {
		return empty, "", err
	}

	finalArtifact := filepath.Join(finalDir, "openpoet")
	manifest := Manifest{
		ReleaseID:         releaseID,
		GitSHA:            gitSHA,
		ArtifactSHA256:    artifactHash,
		ArtifactPath:      finalArtifact,
		ConfirmationToken: token,
		PreparedAt:        workflow.Now().UTC(),
		TestCommand:       testCommand,
		BuildCommand:      buildCommand,
	}
	if err := WriteManifest(filepath.Join(temporaryDir, "manifest.json"), manifest); err != nil {
		return empty, "", err
	}
	if err := syncDir(temporaryDir); err != nil {
		return empty, "", err
	}
	if err := os.Rename(temporaryDir, finalDir); err != nil {
		return empty, "", err
	}
	keepTemporary = true
	if err := syncDir(config.ReleasesDir); err != nil {
		return empty, "", err
	}
	return manifest, filepath.Join(finalDir, "manifest.json"), nil
}

func (workflow Workflow) Preflight(ctx context.Context, config Config) (Manifest, error) {
	manifest, err := ReadManifest(config.ManifestPath)
	if err != nil {
		return manifest, err
	}
	if config.ReleaseID != "" && config.ReleaseID != manifest.ReleaseID {
		return manifest, fmt.Errorf("release-id %q difere do manifest %q", config.ReleaseID, manifest.ReleaseID)
	}
	expectedArtifact := filepath.Join(filepath.Dir(config.ManifestPath), "openpoet")
	if filepath.Clean(manifest.ArtifactPath) != filepath.Clean(expectedArtifact) {
		return manifest, errors.New("artifact_path precisa apontar para openpoet ao lado do manifest")
	}
	if err := VerifyArtifact(manifest); err != nil {
		return manifest, err
	}
	for name, path := range map[string]string{
		"live-binary": config.LiveBinary,
		"db":          config.DBPath,
		"plist":       config.PlistPath,
	} {
		if path == "" {
			return manifest, fmt.Errorf("%s é obrigatório", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return manifest, fmt.Errorf("%s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return manifest, fmt.Errorf("%s não é arquivo regular: %s", name, path)
		}
	}
	if err := QuickCheckSQLite(config.DBPath); err != nil {
		return manifest, fmt.Errorf("live database: %w", err)
	}
	if config.HealthURL == "" {
		return manifest, errors.New("health-url é obrigatória")
	}
	healthCtx, cancel := context.WithTimeout(ctx, minDuration(config.HealthTimeout, 5*time.Second))
	defer cancel()
	if err := workflow.Health.CheckVersion(healthCtx, config.HealthURL, ""); err != nil {
		return manifest, fmt.Errorf("current service health: %w", err)
	}
	return manifest, nil
}

func (workflow Workflow) Apply(ctx context.Context, config Config) (ApplyResult, error) {
	var result ApplyResult
	manifest, err := ReadManifest(config.ManifestPath)
	if err != nil {
		return result, err
	}
	if err := ValidateApplyAuthorization(config.Execute, config.ConfirmToken, manifest.ConfirmationToken); err != nil {
		return result, err
	}
	if !config.Execute {
		return result, errors.New("apply mutante exige --execute")
	}
	if config.ServiceLabel == "" || config.LaunchDomain == "" {
		return result, errors.New("service-label e launch-domain são obrigatórios")
	}
	if config.BackupDir == "" {
		return result, errors.New("backup-dir é obrigatório")
	}

	manifest, err = workflow.Preflight(ctx, config)
	if err != nil {
		return result, fmt.Errorf("preflight: %w", err)
	}
	result.ReleaseID = manifest.ReleaseID

	if err := os.MkdirAll(config.BackupDir, 0o700); err != nil {
		return result, err
	}
	lockPath := filepath.Join(config.BackupDir, "apply.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return result, fmt.Errorf("acquire apply lock: %w", err)
	}
	_, _ = fmt.Fprintf(lock, "%s %s\n", manifest.ReleaseID, workflow.Now().UTC().Format(time.RFC3339))
	_ = lock.Close()
	defer os.Remove(lockPath)

	backupName := fmt.Sprintf(
		"openpoet-%s-%s.sqlite",
		workflow.Now().UTC().Format("20060102T150405Z"),
		manifest.ReleaseID,
	)
	result.BackupPath = filepath.Join(config.BackupDir, backupName)
	if err := BackupSQLite(config.DBPath, result.BackupPath); err != nil {
		return result, fmt.Errorf("backup: %w", err)
	}

	target := config.LaunchDomain + "/" + config.ServiceLabel
	if _, err := workflow.Runner.Run(ctx, "", "launchctl", "bootout", target); err != nil {
		return result, fmt.Errorf("graceful stop: %w", err)
	}
	stopCtx, cancelStop := context.WithTimeout(ctx, config.StopTimeout)
	err = workflow.Health.WaitForDown(stopCtx, config.HealthURL, config.HealthInterval)
	cancelStop()
	if err != nil {
		_, startErr := workflow.Runner.Run(ctx, "", "launchctl", "bootstrap", config.LaunchDomain, config.PlistPath)
		return result, errors.Join(fmt.Errorf("graceful stop gate: %w", err), startErr)
	}

	result.PreviousBinary, err = AtomicInstall(manifest.ArtifactPath, config.LiveBinary, manifest.ReleaseID)
	if err != nil {
		_, startErr := workflow.Runner.Run(ctx, "", "launchctl", "bootstrap", config.LaunchDomain, config.PlistPath)
		return result, errors.Join(fmt.Errorf("binary switch: %w", err), startErr)
	}
	if config.MigrateSecrets {
		if workflow.MigrateSecrets == nil {
			rollbackErr := workflow.rollback(ctx, config, result.PreviousBinary, manifest.ReleaseID, "")
			return result, errors.Join(errors.New("secret migration runner unavailable"), rollbackErr)
		}
		result.SecretMigration, err = workflow.MigrateSecrets(ctx, config.DBPath, config.EncryptKey)
		if err != nil {
			rollbackErr := workflow.rollback(ctx, config, result.PreviousBinary, manifest.ReleaseID, "")
			return result, errors.Join(fmt.Errorf("secret migration: %w", err), rollbackErr)
		}
		fmt.Fprintf(
			workflow.Output,
			"secret migration: global_mcp=%d project_mcp=%d custom_tools=%d fields=%d\n",
			result.SecretMigration.GlobalMCPRecords,
			result.SecretMigration.ProjectMCPRecords,
			result.SecretMigration.CustomToolRecords,
			result.SecretMigration.Fields,
		)
	}

	startAndGate := func() error {
		if _, err := workflow.Runner.Run(ctx, "", "launchctl", "bootstrap", config.LaunchDomain, config.PlistPath); err != nil {
			return err
		}
		healthCtx, cancelHealth := context.WithTimeout(ctx, config.HealthTimeout)
		defer cancelHealth()
		if err := workflow.Health.WaitForHealth(
			healthCtx,
			config.HealthURL,
			manifest.ReleaseID,
			config.HealthInterval,
			3,
		); err != nil {
			return err
		}
		return QuickCheckSQLite(config.DBPath)
	}
	if err := startAndGate(); err != nil {
		databaseBackup := ""
		if result.SecretMigration.Executed {
			databaseBackup = result.BackupPath
		}
		rollbackErr := workflow.rollback(ctx, config, result.PreviousBinary, manifest.ReleaseID, databaseBackup)
		return result, errors.Join(fmt.Errorf("candidate health gates: %w", err), rollbackErr)
	}

	return result, nil
}

func (workflow Workflow) rollback(ctx context.Context, config Config, previousPath, releaseID, databaseBackup string) error {
	target := config.LaunchDomain + "/" + config.ServiceLabel
	_, stopErr := workflow.Runner.Run(ctx, "", "launchctl", "bootout", target)
	restoreErr := RestoreBinary(previousPath, config.LiveBinary, releaseID)
	var restoreDatabaseErr error
	if databaseBackup != "" {
		restoreDatabaseErr = RestoreSQLiteBackup(databaseBackup, config.DBPath)
	}
	_, startErr := workflow.Runner.Run(ctx, "", "launchctl", "bootstrap", config.LaunchDomain, config.PlistPath)
	if restoreErr == nil && restoreDatabaseErr == nil && startErr == nil {
		healthCtx, cancel := context.WithTimeout(ctx, config.HealthTimeout)
		defer cancel()
		startErr = workflow.Health.WaitForHealth(
			healthCtx,
			config.HealthURL,
			"",
			config.HealthInterval,
			2,
		)
	}
	return errors.Join(stopErr, restoreErr, restoreDatabaseErr, startErr)
}

func migrateRuntimeSecrets(ctx context.Context, dbPath, encryptKey string) (database.LegacyRuntimeSecretMigrationReport, error) {
	db, err := database.OpenExisting(dbPath)
	if err != nil {
		return database.LegacyRuntimeSecretMigrationReport{}, err
	}
	defer db.Close()
	encryptor, err := security.NewEncryptor(encryptKey)
	if err != nil {
		return database.LegacyRuntimeSecretMigrationReport{}, errors.New("initialize rollout secret encryption")
	}
	return database.MigrateLegacyRuntimeSecrets(ctx, db, encryptor, true)
}

func validatePrepareConfig(config Config) error {
	if config.RepoDir == "" || config.ReleasesDir == "" {
		return errors.New("repo e releases-dir são obrigatórios")
	}
	info, err := os.Stat(filepath.Join(config.RepoDir, "go.mod"))
	if err != nil {
		return fmt.Errorf("repo inválido: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("go.mod não é arquivo regular")
	}
	return nil
}

func minDuration(left, right time.Duration) time.Duration {
	if left <= 0 || left > right {
		return right
	}
	return left
}
