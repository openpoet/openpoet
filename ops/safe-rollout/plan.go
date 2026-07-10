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
		return errors.New("release-id deve conter apenas letras, números, ponto, hífen ou underscore")
	}
	return nil
}

func ValidateApplyAuthorization(execute bool, provided, expected string) error {
	if !execute {
		return nil
	}
	if expected == "" {
		return errors.New("manifest sem confirmation_token")
	}
	if provided == "" {
		return errors.New("apply real exige --confirm-token")
	}
	if provided != expected {
		return errors.New("confirmation token inválido")
	}
	return nil
}

func BuildPlan(operation Operation) ([]Step, error) {
	switch operation {
	case OperationPrepare:
		return []Step{
			{Name: "validate-source", Description: "validar repositório e release-id"},
			{Name: "test", Description: "executar go test ./... antes de gerar qualquer artefato"},
			{Name: "build", Description: "compilar binário candidato em diretório temporário", Mutating: true},
			{Name: "hash", Description: "calcular SHA-256 do candidato"},
			{Name: "publish-release", Description: "publicar diretório imutável e manifest com token", Mutating: true},
		}, nil
	case OperationPreflight:
		return []Step{
			{Name: "verify-manifest", Description: "validar manifest, release-id e hash do artefato"},
			{Name: "verify-runtime-paths", Description: "validar binário live, plist e banco"},
			{Name: "database-check", Description: "executar PRAGMA quick_check no banco live"},
			{Name: "health-check", Description: "validar /api/version da instância atual"},
		}, nil
	case OperationApply:
		return []Step{
			{Name: "authorization", Description: "exigir --execute e token exato do manifest"},
			{Name: "preflight", Description: "revalidar artefato, runtime, banco e saúde atual"},
			{Name: "backup", Description: "criar snapshot SQLite consistente e validá-lo", Mutating: true},
			{Name: "graceful-stop", Description: "enviar SIGTERM via launchctl bootout e aguardar a porta fechar", Mutating: true},
			{Name: "switch-binary", Description: "preservar binário anterior e trocar candidato atomicamente", Mutating: true},
			{Name: "secret-migration", Description: "migrar plaintext legado após backup e antes do restart", Mutating: true},
			{Name: "start", Description: "subir o LaunchAgent com o binário candidato", Mutating: true},
			{Name: "health-gates", Description: "exigir versão esperada, checks consecutivos e quick_check"},
			{Name: "rollback-on-failure", Description: "restaurar binário anterior e reabrir serviço se algum gate falhar", Mutating: true},
		}, nil
	default:
		return nil, fmt.Errorf("operação desconhecida: %q", operation)
	}
}
