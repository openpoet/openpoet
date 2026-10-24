package main

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Operation string

const (
	OperationPrepare   Operation = "prepare"
	OperationPreflight Operation = "preflight"
	OperationApply     Operation = "apply"
)

type Step struct {
	Name        string
	Description string
	Mutating    bool
}

type Manifest struct {
	ReleaseID         string    `json:"release_id"`
	GitSHA            string    `json:"git_sha"`
	ArtifactSHA256    string    `json:"artifact_sha256"`
	ArtifactPath      string    `json:"artifact_path"`
	ConfirmationToken string    `json:"confirmation_token"`
	PreparedAt        time.Time `json:"prepared_at"`
	TestCommand       []string  `json:"test_command"`
	BuildCommand      []string  `json:"build_command"`
}

type Config struct {
	Operation      Operation
	Execute        bool
	ConfirmToken   string
	RepoDir        string
	ReleasesDir    string
	ReleaseID      string
	ManifestPath   string
	LiveBinary     string
	DBPath         string
	BackupDir      string
	PlistPath      string
	ServiceLabel   string
	LaunchDomain   string
	HealthURL      string
	RelayURL       string
	StopTimeout    time.Duration
	HealthTimeout  time.Duration
	HealthInterval time.Duration
	EncryptKey     string
	MigrateSecrets bool
}

var releaseIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func ValidateReleaseID(value string) error {
	if !releaseIDPattern.MatchString(value) {
		return errors.New("release-id may only contain letters, digits, dot, hyphen or underscore")
	}
	return nil
}

func ValidateApplyAuthorization(execute bool, provided, expected string) error {
	if !execute {
		return nil
	}
	if expected == "" {
		return errors.New("manifest has no confirmation_token")
	}
	if provided == "" {
		return errors.New("a real apply requires --confirm-token")
	}
	if provided != expected {
		return errors.New("invalid confirmation token")
	}
	return nil
}

func BuildPlan(operation Operation) ([]Step, error) {
	switch operation {
	case OperationPrepare:
		return []Step{
			{Name: "validate-source", Description: "validate repository and release-id"},
			{Name: "test", Description: "run go test ./... before producing any artifact"},
			{Name: "build", Description: "build the candidate binary in a temporary directory", Mutating: true},
			{Name: "hash", Description: "compute the candidate SHA-256"},
			{Name: "publish-release", Description: "publish the immutable directory and the manifest with its token", Mutating: true},
		}, nil
	case OperationPreflight:
		return []Step{
			{Name: "verify-manifest", Description: "validate manifest, release-id and artifact hash"},
			{Name: "verify-runtime-paths", Description: "validate live binary, plist and database"},
			{Name: "database-check", Description: "run PRAGMA quick_check on the live database"},
			{Name: "health-check", Description: "validate /api/version of the current instance"},
		}, nil
	case OperationApply:
		return []Step{
			{Name: "authorization", Description: "require --execute and the exact manifest token"},
			{Name: "preflight", Description: "revalidate artifact, runtime, database and current health"},
			{Name: "backup", Description: "create a consistent SQLite snapshot and verify it", Mutating: true},
			{Name: "graceful-stop", Description: "send SIGTERM via launchctl bootout and wait for the port to close", Mutating: true},
			{Name: "switch-binary", Description: "keep the previous binary and switch to the candidate atomically", Mutating: true},
			{Name: "secret-migration", Description: "migrate legacy plaintext after the backup and before the restart", Mutating: true},
			{Name: "start", Description: "start the LaunchAgent with the candidate binary", Mutating: true},
			{Name: "health-gates", Description: "require the expected version, consecutive checks and quick_check"},
			{Name: "rollback-on-failure", Description: "restore the previous binary and reopen the service if any gate fails", Mutating: true},
		}, nil
	default:
		return nil, fmt.Errorf("unknown operation: %q", operation)
	}
}
