package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	GitHubRepo    = "openpoet/openpoet"
	CheckInterval = 6 * time.Hour
)

// UpdateStatus represents the current update state for the UI.
type UpdateStatus struct {
	Available      bool   `json:"available"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	ReleaseURL     string `json:"release_url,omitempty"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	DownloadURL    string `json:"download_url,omitempty"`
	ChecksumURL    string `json:"checksum_url,omitempty"`
	Error          string `json:"error,omitempty"`
	Managed        string `json:"managed,omitempty"` // "homebrew", "winget", or ""
	CheckedAt      string `json:"checked_at,omitempty"`
}

// githubRelease is the subset of GitHub API release we need.
type githubRelease struct {
	TagName    string        `json:"tag_name"`
	HTMLURL    string        `json:"html_url"`
	Body       string        `json:"body"`
	Prerelease bool          `json:"prerelease"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Updater checks for and applies binary updates from GitHub Releases.
type Updater struct {
	CurrentVersion string

	mu         sync.Mutex
	lastCheck  *UpdateStatus
	httpClient *http.Client
}

// New creates a new Updater for the given build version.
func New(currentVersion string) *Updater {
	return &Updater{
		CurrentVersion: currentVersion,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// CheckForUpdate queries the GitHub Releases API for a newer version.
func (u *Updater) CheckForUpdate(ctx context.Context) (*UpdateStatus, error) {
	status := &UpdateStatus{
		CurrentVersion: u.CurrentVersion,
		CheckedAt:      time.Now().Format(time.RFC3339),
		Managed:        u.DetectPackageManager(),
	}

	if IsDevBuild(u.CurrentVersion) {
		status.Error = "dev build — update check skipped"
		return status, nil
	}

	currentVer, err := Parse(u.CurrentVersion)
	if err != nil {
		return status, fmt.Errorf("cannot parse current version %q: %w", u.CurrentVersion, err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", GitHubRepo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return status, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return status, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return status, fmt.Errorf("cannot parse GitHub response: %w", err)
	}

	latestVer, err := Parse(release.TagName)
	if err != nil {
		return status, fmt.Errorf("cannot parse release version %q: %w", release.TagName, err)
	}

	status.LatestVersion = latestVer.String()
	status.ReleaseURL = release.HTMLURL
	status.ReleaseNotes = release.Body

	if !IsNewer(currentVer, latestVer) {
		return status, nil
	}

	// Find the download asset for current OS/arch
	assetName := AssetName(latestVer.String())
	checksumName := "checksums.txt"

	for _, a := range release.Assets {
		if a.Name == assetName {
			status.DownloadURL = a.BrowserDownloadURL
		}
		if a.Name == checksumName {
			status.ChecksumURL = a.BrowserDownloadURL
		}
	}

	if status.DownloadURL == "" {
		return status, fmt.Errorf("no release asset found for %s/%s (%s)", runtime.GOOS, runtime.GOARCH, assetName)
	}

	status.Available = true

	u.mu.Lock()
	u.lastCheck = status
	u.mu.Unlock()

	return status, nil
}

// LastCheck returns the cached result of the last update check.
func (u *Updater) LastCheck() *UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastCheck
}

// DownloadAndApply downloads the release archive, verifies its checksum,
// extracts the binary, and replaces the running executable.
func (u *Updater) DownloadAndApply(ctx context.Context, status *UpdateStatus) error {
	if status.DownloadURL == "" {
		return fmt.Errorf("no download URL")
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("cannot resolve executable symlinks: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "openpoet-update-*")
	if err != nil {
		return fmt.Errorf("cannot create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Download archive
	archiveName := AssetName(status.LatestVersion)
	archivePath := filepath.Join(tmpDir, archiveName)
	log.Printf("[Updater] Downloading %s", status.DownloadURL)
	if err := u.downloadFile(ctx, status.DownloadURL, archivePath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// Verify checksum if available
	if status.ChecksumURL != "" {
		checksumPath := filepath.Join(tmpDir, "checksums.txt")
		if err := u.downloadFile(ctx, status.ChecksumURL, checksumPath); err != nil {
			return fmt.Errorf("checksum download failed: %w", err)
		}
		if err := verifyChecksum(archivePath, checksumPath, archiveName); err != nil {
			return err
		}
		log.Printf("[Updater] Checksum verified")
	}

	// Extract binary from archive
	binaryName := "openpoet"
	if runtime.GOOS == "windows" { // experimental, Windows not officially supported
		binaryName = "openpoet.exe"
	}
	newBinaryPath := filepath.Join(tmpDir, "extracted-"+binaryName)

	if strings.HasSuffix(archiveName, ".zip") {
		if err := extractFromZip(archivePath, binaryName, newBinaryPath); err != nil {
			return fmt.Errorf("zip extraction failed: %w", err)
		}
	} else {
		if err := extractFromTarGz(archivePath, binaryName, newBinaryPath); err != nil {
			return fmt.Errorf("tar.gz extraction failed: %w", err)
		}
	}

	// Atomic replacement: old → .old backup, new → target
	oldPath := execPath + ".old"
	os.Remove(oldPath) // clean up any previous backup

	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("cannot move old binary: %w (may need elevated permissions)", err)
	}

	if err := copyFile(newBinaryPath, execPath); err != nil {
		// Rollback
		if rbErr := os.Rename(oldPath, execPath); rbErr != nil {
			log.Printf("[Updater] WARNING: rollback failed: %v", rbErr)
		}
		return fmt.Errorf("cannot install new binary: %w", err)
	}

	if err := os.Chmod(execPath, 0755); err != nil {
		log.Printf("[Updater] Warning: chmod failed: %v", err)
	}

	// Clean up old binary (best effort)
	os.Remove(oldPath)

	log.Printf("[Updater] Binary updated to v%s at %s", status.LatestVersion, execPath)
	return nil
}

// DetectPackageManager checks if the running binary is managed by a package manager.
func (u *Updater) DetectPackageManager() string {
	execPath, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		resolved = execPath
	}

	// Homebrew: binary resolves to /opt/homebrew/Cellar/ or /usr/local/Cellar/
	if strings.Contains(resolved, "/Cellar/") {
		return "homebrew"
	}

	// Winget: binary in typical Windows package manager paths (experimental, not officially supported)
	if runtime.GOOS == "windows" {
		lower := strings.ToLower(resolved)
		if strings.Contains(lower, "\\windowsapps\\") || strings.Contains(lower, "\\winget\\") {
			return "winget"
		}
	}

	return ""
}

// AssetName returns the expected archive filename for the current OS/arch.
func AssetName(version string) string {
	version = strings.TrimPrefix(version, "v")
	ext := "tar.gz"
	if runtime.GOOS == "windows" { // experimental, Windows not officially supported
		ext = "zip"
	}
	return fmt.Sprintf("openpoet_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

// downloadFile downloads a URL to a local file path.
func (u *Updater) downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

// verifyChecksum reads a checksums.txt file and verifies the archive hash.
func verifyChecksum(archivePath, checksumFilePath, archiveName string) error {
	data, err := os.ReadFile(checksumFilePath)
	if err != nil {
		return fmt.Errorf("cannot read checksums file: %w", err)
	}

	var expectedHash string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == archiveName {
			expectedHash = parts[0]
			break
		}
	}
	if expectedHash == "" {
		return fmt.Errorf("no checksum found for %s in checksums.txt", archiveName)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

// extractFromTarGz extracts a named file from a .tar.gz archive.
func extractFromTarGz(archivePath, targetName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Match by basename — the archive may have a directory prefix
		if filepath.Base(hdr.Name) == targetName && hdr.Typeflag == tar.TypeReg {
			out, err := os.Create(destPath)
			if err != nil {
				return err
			}
			defer out.Close()
			if _, err := io.Copy(out, tr); err != nil {
				return err
			}
			return out.Close()
		}
	}
	return fmt.Errorf("%s not found in archive", targetName)
}

// extractFromZip extracts a named file from a .zip archive.
func extractFromZip(archivePath, targetName, destPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if filepath.Base(f.Name) == targetName && !f.FileInfo().IsDir() {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			out, err := os.Create(destPath)
			if err != nil {
				return err
			}
			defer out.Close()
			if _, err := io.Copy(out, rc); err != nil {
				return err
			}
			return out.Close()
		}
	}
	return fmt.Errorf("%s not found in archive", targetName)
}

// copyFile copies src to dst (cross-filesystem safe, unlike os.Rename).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
