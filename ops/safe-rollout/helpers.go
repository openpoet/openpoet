package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type HealthClient struct {
	Client *http.Client
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
}

func defaultHealthClient() HealthClient {
	return HealthClient{
		Client: &http.Client{Timeout: 2 * time.Second},
		Now:    time.Now,
		Sleep: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func NewConfirmationToken() (string, error) {
	buffer := make([]byte, 18)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate confirmation token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func WriteManifest(path string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return syncFile(path)
}

func ReadManifest(path string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode manifest: %w", err)
	}
	if err := ValidateReleaseID(manifest.ReleaseID); err != nil {
		return manifest, err
	}
	if manifest.ArtifactPath == "" || manifest.ArtifactSHA256 == "" || manifest.GitSHA == "" {
		return manifest, errors.New("manifest incompleto")
	}
	return manifest, nil
}

func VerifyArtifact(manifest Manifest) error {
	hash, err := FileSHA256(manifest.ArtifactPath)
	if err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}
	if hash != manifest.ArtifactSHA256 {
		return fmt.Errorf("artifact SHA-256 divergente: esperado %s, obtido %s", manifest.ArtifactSHA256, hash)
	}
	return nil
}

func QuickCheckSQLite(path string) error {
	if path == "" {
		return errors.New("db path vazio")
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()

	var result string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check: %s", result)
	}
	return nil
}

func BackupSQLite(sourcePath, destinationPath string) error {
	if sourcePath == "" || destinationPath == "" {
		return errors.New("source e destination são obrigatórios")
	}
	if _, err := os.Stat(sourcePath); err != nil {
		return err
	}
	if _, err := os.Stat(destinationPath); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("backup já existe: %s", destinationPath)
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return err
	}

	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(sourcePath))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", destinationPath); err != nil {
		return fmt.Errorf("SQLite VACUUM INTO: %w", err)
	}
	if err := os.Chmod(destinationPath, 0o600); err != nil {
		return err
	}
	if err := QuickCheckSQLite(destinationPath); err != nil {
		return fmt.Errorf("verify SQLite backup: %w", err)
	}
	return syncFile(destinationPath)
}

func sqliteReadOnlyDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("_pragma", "busy_timeout(10000)")
	u.RawQuery = query.Encode()
	return u.String()
}

func AtomicInstall(candidatePath, livePath, releaseID string) (string, error) {
	if err := ValidateReleaseID(releaseID); err != nil {
		return "", err
	}
	if _, err := os.Stat(candidatePath); err != nil {
		return "", err
	}
	if _, err := os.Stat(livePath); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(livePath), 0o700); err != nil {
		return "", err
	}

	previousPath := livePath + ".previous." + releaseID
	if _, err := os.Stat(previousPath); !os.IsNotExist(err) {
		if err == nil {
			return "", fmt.Errorf("previous binary já existe: %s", previousPath)
		}
		return "", err
	}
	if err := copyExecutable(livePath, previousPath); err != nil {
		return "", fmt.Errorf("preserve previous binary: %w", err)
	}

	temporaryPath := livePath + ".next." + releaseID
	if err := copyExecutable(candidatePath, temporaryPath); err != nil {
		return "", fmt.Errorf("stage candidate: %w", err)
	}
	if err := os.Rename(temporaryPath, livePath); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("atomic binary switch: %w", err)
	}
	if err := syncDir(filepath.Dir(livePath)); err != nil {
		return "", err
	}
	return previousPath, nil
}

func RestoreBinary(previousPath, livePath, releaseID string) error {
	if err := ValidateReleaseID(releaseID); err != nil {
		return err
	}
	if _, err := os.Stat(previousPath); err != nil {
		return err
	}
	temporaryPath := livePath + ".rollback." + releaseID
	if err := copyExecutable(previousPath, temporaryPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, livePath); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return syncDir(filepath.Dir(livePath))
}

func copyExecutable(sourcePath, destinationPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	clean := true
	defer func() {
		_ = destination.Close()
		if clean {
			_ = os.Remove(destinationPath)
		}
	}()

	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	clean = false
	return nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (health HealthClient) CheckVersion(ctx context.Context, endpoint, expectedVersion string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := health.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health status: %s", response.Status)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload); err != nil {
		return err
	}
	if payload.Version == "" {
		return errors.New("health sem version")
	}
	if expectedVersion != "" && payload.Version != expectedVersion {
		return fmt.Errorf("versão inesperada: esperado %q, obtido %q", expectedVersion, payload.Version)
	}
	return nil
}

func (health HealthClient) WaitForHealth(ctx context.Context, endpoint, expectedVersion string, interval time.Duration, consecutive int) error {
	if consecutive < 1 {
		return errors.New("consecutive deve ser positivo")
	}
	passed := 0
	for {
		if err := health.CheckVersion(ctx, endpoint, expectedVersion); err == nil {
			passed++
			if passed >= consecutive {
				return nil
			}
		} else {
			passed = 0
		}
		if err := health.Sleep(ctx, interval); err != nil {
			return fmt.Errorf("health gate: %w", err)
		}
	}
}

func (health HealthClient) WaitForDown(ctx context.Context, endpoint string, interval time.Duration) error {
	for {
		if err := health.CheckVersion(ctx, endpoint, ""); err != nil {
			return nil
		}
		if err := health.Sleep(ctx, interval); err != nil {
			return fmt.Errorf("wait for shutdown: %w", err)
		}
	}
}
