package files

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
	Mode    string    `json:"mode"`
}

type LocalFileManager struct {
	basePath string
}

func NewLocalFileManager(basePath string) *LocalFileManager {
	return &LocalFileManager{basePath: basePath}
}

// List lists files in a directory
func (m *LocalFileManager) List(relativePath string) ([]FileInfo, error) {
	fullPath := filepath.Join(m.basePath, relativePath)

	// Ensure we don't escape the base path
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, err
	}
	absBase, _ := filepath.Abs(m.basePath)
	if !strings.HasPrefix(absPath, absBase) {
		return nil, fmt.Errorf("path outside base directory")
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, err
	}

	var files []FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		relPath := filepath.Join(relativePath, entry.Name())
		files = append(files, FileInfo{
			Name:    entry.Name(),
			Path:    relPath,
			Size:    info.Size(),
			IsDir:   entry.IsDir(),
			ModTime: info.ModTime(),
			Mode:    info.Mode().String(),
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
func (m *LocalFileManager) Read(relativePath string) ([]byte, error) {
	fullPath := filepath.Join(m.basePath, relativePath)

	// Security check
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, err
	}
	absBase, _ := filepath.Abs(m.basePath)
	if !strings.HasPrefix(absPath, absBase) {
		return nil, fmt.Errorf("path outside base directory")
	}

	return os.ReadFile(fullPath)
}

// ReadStream returns a reader for streaming file content
func (m *LocalFileManager) ReadStream(relativePath string) (io.ReadCloser, *FileInfo, error) {
	fullPath := filepath.Join(m.basePath, relativePath)

	// Security check
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, nil, err
	}
	absBase, _ := filepath.Abs(m.basePath)
	if !strings.HasPrefix(absPath, absBase) {
		return nil, nil, fmt.Errorf("path outside base directory")
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return nil, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}

	if info.IsDir() {
		file.Close()
		return nil, nil, fmt.Errorf("path is a directory")
	}

	fileInfo := &FileInfo{
		Name:    info.Name(),
		Path:    relativePath,
		Size:    info.Size(),
		IsDir:   false,
		ModTime: info.ModTime(),
		Mode:    info.Mode().String(),
	}

	return file, fileInfo, nil
}

// Write writes data to a file
func (m *LocalFileManager) Write(relativePath string, data []byte) error {
	fullPath := filepath.Join(m.basePath, relativePath)

	// Security check
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return err
	}
	absBase, _ := filepath.Abs(m.basePath)
	if !strings.HasPrefix(absPath, absBase) {
		return fmt.Errorf("path outside base directory")
	}

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(fullPath, data, 0644)
}

// WriteStream writes from a reader to a file
func (m *LocalFileManager) WriteStream(relativePath string, reader io.Reader) error {
	fullPath := filepath.Join(m.basePath, relativePath)

	// Security check
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return err
	}
	absBase, _ := filepath.Abs(m.basePath)
	if !strings.HasPrefix(absPath, absBase) {
		return fmt.Errorf("path outside base directory")
	}

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	return err
}

// Delete removes a file
func (m *LocalFileManager) Delete(relativePath string) error {
	fullPath := filepath.Join(m.basePath, relativePath)

	// Security check
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return err
	}
	absBase, _ := filepath.Abs(m.basePath)
	if !strings.HasPrefix(absPath, absBase) {
		return fmt.Errorf("path outside base directory")
	}

	return os.Remove(fullPath)
}

// Exists checks if a file exists
func (m *LocalFileManager) Exists(relativePath string) bool {
	fullPath := filepath.Join(m.basePath, relativePath)
	_, err := os.Stat(fullPath)
	return err == nil
}

// GetMimeType returns the MIME type of a file
func (m *LocalFileManager) GetMimeType(relativePath string) string {
	ext := filepath.Ext(relativePath)
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		return "application/octet-stream"
	}
	return mimeType
}

// SaveBase64Image saves a base64-encoded image to a file
func (m *LocalFileManager) SaveBase64Image(base64Data string, filename string) (string, error) {
	// Remove data URL prefix if present
	if idx := strings.Index(base64Data, ","); idx != -1 {
		base64Data = base64Data[idx+1:]
	}

	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("invalid base64 data: %w", err)
	}

	// Generate unique filename if needed
	if filename == "" {
		filename = fmt.Sprintf("paste_%d.png", time.Now().UnixNano())
	}

	if err := m.Write(filename, data); err != nil {
		return "", err
	}

	return filename, nil
}

// GetBasePath returns the base path
func (m *LocalFileManager) GetBasePath() string {
	return m.basePath
}
