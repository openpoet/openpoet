package files

import (
	"bufio"
	"devmanager/internal/database"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type RemoteFileManager struct {
	project     *database.Project
	decryptFunc func(string, string) (string, error)
}

func NewRemoteFileManager(project *database.Project, decryptFunc func(string, string) (string, error)) *RemoteFileManager {
	return &RemoteFileManager{
		project:     project,
		decryptFunc: decryptFunc,
	}
}

func (m *RemoteFileManager) connect() (*ssh.Client, *sftp.Client, error) {
	config, err := m.buildSSHConfig()
	if err != nil {
		return nil, nil, err
	}

	addr := fmt.Sprintf("%s:%d", m.project.SSHHost.String, m.project.SSHPort.Int64)
	sshClient, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH connection failed: %w", err)
	}

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, nil, fmt.Errorf("SFTP connection failed: %w", err)
	}

	return sshClient, sftpClient, nil
}

func (m *RemoteFileManager) buildSSHConfig() (*ssh.ClientConfig, error) {
	authType := m.project.SSHAuthType.String
	user := m.project.SSHUser.String

	var authMethods []ssh.AuthMethod

	if authType == "password" {
		password, err := m.decryptFunc(
			m.project.SSHCredentialEncrypted.String,
			m.project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.Password(password))
	} else if authType == "key" || authType == "key_passphrase" {
		keyData, err := m.decryptFunc(
			m.project.SSHCredentialEncrypted.String,
			m.project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}

		signer, err := ssh.ParsePrivateKey([]byte(keyData))
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	} else {
		// Try default keys
		homeDir, _ := os.UserHomeDir()
		keyPaths := []string{
			homeDir + "/.ssh/id_rsa",
			homeDir + "/.ssh/id_ed25519",
			homeDir + "/.ssh/id_ecdsa",
		}
		for _, keyPath := range keyPaths {
			if keyData, err := os.ReadFile(keyPath); err == nil {
				if signer, err := ssh.ParsePrivateKey(keyData); err == nil {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
					break
				}
			}
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}, nil
}

// List lists files in a directory
func (m *RemoteFileManager) List(relativePath string) ([]FileInfo, error) {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	fullPath := filepath.Join(m.project.Path, relativePath)

	entries, err := sftpClient.ReadDir(fullPath)
	if err != nil {
		return nil, err
	}

	var files []FileInfo
	for _, entry := range entries {
		relPath := filepath.Join(relativePath, entry.Name())
		files = append(files, FileInfo{
			Name:    entry.Name(),
			Path:    relPath,
			Size:    entry.Size(),
			IsDir:   entry.IsDir(),
			ModTime: entry.ModTime(),
			Mode:    entry.Mode().String(),
		})
	}

	// Sort: directories first, then by name
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Name < files[j].Name
	})

	return files, nil
}

// Read reads a file and returns its contents
func (m *RemoteFileManager) Read(relativePath string) ([]byte, error) {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	fullPath := filepath.Join(m.project.Path, relativePath)

	file, err := sftpClient.Open(fullPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(file)
}

// ReadStream returns a reader for streaming file content
// Note: The caller is responsible for closing the returned connections
func (m *RemoteFileManager) ReadStream(relativePath string) (*sftp.File, *FileInfo, *ssh.Client, *sftp.Client, error) {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return nil, nil, nil, nil, err
	}

	fullPath := filepath.Join(m.project.Path, relativePath)

	file, err := sftpClient.Open(fullPath)
	if err != nil {
		sftpClient.Close()
		sshClient.Close()
		return nil, nil, nil, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		sftpClient.Close()
		sshClient.Close()
		return nil, nil, nil, nil, err
	}

	if info.IsDir() {
		file.Close()
		sftpClient.Close()
		sshClient.Close()
		return nil, nil, nil, nil, fmt.Errorf("path is a directory")
	}

	fileInfo := &FileInfo{
		Name:    info.Name(),
		Path:    relativePath,
		Size:    info.Size(),
		IsDir:   false,
		ModTime: info.ModTime(),
		Mode:    info.Mode().String(),
	}

	return file, fileInfo, sshClient, sftpClient, nil
}

// Write writes data to a file
func (m *RemoteFileManager) Write(relativePath string, data []byte) error {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	fullPath := filepath.Join(m.project.Path, relativePath)

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	sftpClient.MkdirAll(dir)

	file, err := sftpClient.Create(fullPath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(data)
	return err
}

// WriteStream writes from a reader to a file
func (m *RemoteFileManager) WriteStream(relativePath string, reader io.Reader) error {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	fullPath := filepath.Join(m.project.Path, relativePath)

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	sftpClient.MkdirAll(dir)

	file, err := sftpClient.Create(fullPath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	return err
}

// Delete removes a file
func (m *RemoteFileManager) Delete(relativePath string) error {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	fullPath := filepath.Join(m.project.Path, relativePath)
	return sftpClient.Remove(fullPath)
}

// Exists checks if a file exists
func (m *RemoteFileManager) Exists(relativePath string) bool {
	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return false
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	fullPath := filepath.Join(m.project.Path, relativePath)
	_, err = sftpClient.Stat(fullPath)
	return err == nil
}

// GetMimeType returns the MIME type based on file extension
func (m *RemoteFileManager) GetMimeType(relativePath string) string {
	ext := strings.ToLower(filepath.Ext(relativePath))
	mimeTypes := map[string]string{
		".html": "text/html",
		".css":  "text/css",
		".js":   "application/javascript",
		".json": "application/json",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".svg":  "image/svg+xml",
		".pdf":  "application/pdf",
		".txt":  "text/plain",
		".md":   "text/markdown",
		".xml":  "application/xml",
		".zip":  "application/zip",
		".tar":  "application/x-tar",
		".gz":   "application/gzip",
	}

	if mimeType, ok := mimeTypes[ext]; ok {
		return mimeType
	}
	return "application/octet-stream"
}

// Glob finds files matching a pattern recursively via SFTP.
func (m *RemoteFileManager) Glob(pattern string) ([]string, error) {
	const maxResults = 100

	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	var results []string
	walker := sftpClient.Walk(m.project.Path)
	for walker.Step() {
		if len(results) >= maxResults {
			break
		}
		if err := walker.Err(); err != nil {
			continue
		}
		fi := walker.Stat()
		if fi.IsDir() {
			if ignoredDirs[fi.Name()] {
				walker.SkipDir()
				continue
			}
			continue
		}
		matched, _ := filepath.Match(pattern, fi.Name())
		if matched {
			rel, _ := filepath.Rel(m.project.Path, walker.Path())
			results = append(results, rel)
		}
	}
	return results, nil
}

// Grep searches file contents for a regex pattern via SFTP.
func (m *RemoteFileManager) Grep(pattern string, searchPath string, fileGlob string) ([]GrepResult, error) {
	const maxResults = 50
	const maxFileSize int64 = 1 * 1024 * 1024

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}

	sshClient, sftpClient, err := m.connect()
	if err != nil {
		return nil, err
	}
	defer sftpClient.Close()
	defer sshClient.Close()

	searchRoot := m.project.Path
	if searchPath != "" {
		searchRoot = filepath.Join(m.project.Path, searchPath)
	}

	var results []GrepResult
	walker := sftpClient.Walk(searchRoot)
	for walker.Step() {
		if len(results) >= maxResults {
			break
		}
		if err := walker.Err(); err != nil {
			continue
		}
		fi := walker.Stat()
		if fi.IsDir() {
			if ignoredDirs[fi.Name()] {
				walker.SkipDir()
				continue
			}
			continue
		}
		if fi.Size() > maxFileSize {
			continue
		}
		if fileGlob != "" {
			matched, _ := filepath.Match(fileGlob, fi.Name())
			if !matched {
				continue
			}
		}

		relPath, _ := filepath.Rel(m.project.Path, walker.Path())

		file, err := sftpClient.Open(walker.Path())
		if err != nil {
			continue
		}

		// Binary check
		header := make([]byte, 512)
		n, _ := file.Read(header)
		isBinary := false
		for i := 0; i < n; i++ {
			if header[i] == 0 {
				isBinary = true
				break
			}
		}
		if isBinary {
			file.Close()
			continue
		}
		file.Seek(0, 0)

		scanner := bufio.NewScanner(file)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if re.MatchString(line) {
				results = append(results, GrepResult{
					File:    relPath,
					Line:    lineNum,
					Content: line,
				})
				if len(results) >= maxResults {
					break
				}
			}
		}
		file.Close()
	}
	return results, nil
}

// GetBasePath returns the project path
func (m *RemoteFileManager) GetBasePath() string {
	return m.project.Path
}
