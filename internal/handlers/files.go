package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"openpoet/internal/files"

	"github.com/go-chi/chi/v5"
)

type FileHandler struct {
	api *API
}

func NewFileHandler(api *API) *FileHandler {
	return &FileHandler{api: api}
}

func (h *FileHandler) ListFiles(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")

	// Get session and project
	sess, err := h.api.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	var fileList []files.FileInfo
	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		fileList, err = fm.List(path)
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		fileList, err = fm.List(path)
	}

	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, fileList)
}

func (h *FileHandler) DownloadFile(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	filePath := chi.URLParam(r, "*")

	// Get session and project
	sess, err := h.api.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		reader, fileInfo, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer reader.Close()

		w.Header().Set("Content-Type", fm.GetMimeType(filePath))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileInfo.Name))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size))

		io.Copy(w, reader)
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		file, fileInfo, sshClient, sftpClient, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer file.Close()
		defer sftpClient.Close()
		defer sshClient.Close()

		w.Header().Set("Content-Type", fm.GetMimeType(filePath))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileInfo.Name))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size))

		io.Copy(w, file)
	}
}

func (h *FileHandler) UploadFiles(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	targetDir := r.URL.Query().Get("dir")

	// Get session and project
	sess, err := h.api.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	// Parse multipart form
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100MB limit
		respondError(w, http.StatusBadRequest, "Failed to parse form: "+err.Error())
		return
	}

	uploadedFiles := []string{}

	for _, fileHeaders := range r.MultipartForm.File {
		for _, fh := range fileHeaders {
			file, err := fh.Open()
			if err != nil {
				continue
			}

			targetPath := filepath.Join(targetDir, fh.Filename)

			if project.Type == "local" {
				fm := files.NewLocalFileManager(project.Path)
				if err := fm.WriteStream(targetPath, file); err != nil {
					file.Close()
					respondError(w, http.StatusInternalServerError, err.Error())
					return
				}
			} else {
				fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
				if err := fm.WriteStream(targetPath, file); err != nil {
					file.Close()
					respondError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}

			file.Close()
			uploadedFiles = append(uploadedFiles, targetPath)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"uploaded": uploadedFiles,
	})
}

func (h *FileHandler) PasteImage(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	// Get session and project
	sess, err := h.api.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	var input struct {
		Data     string `json:"data"`     // Base64 encoded image
		Filename string `json:"filename"` // Optional filename
		Dir      string `json:"dir"`      // Target directory
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	// Determine file extension from data URL
	ext := ".png"
	if strings.HasPrefix(input.Data, "data:image/jpeg") {
		ext = ".jpg"
	} else if strings.HasPrefix(input.Data, "data:image/gif") {
		ext = ".gif"
	} else if strings.HasPrefix(input.Data, "data:image/webp") {
		ext = ".webp"
	}

	filename := input.Filename
	if filename == "" {
		filename = fmt.Sprintf("paste_%d%s", time.Now().UnixNano(), ext)
	}

	targetPath := filepath.Join(input.Dir, filename)

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		savedPath, err := fm.SaveBase64Image(input.Data, targetPath)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{
			"path": savedPath,
		})
	} else {
		// For remote, decode base64 and upload
		data := input.Data
		if idx := strings.Index(data, ","); idx != -1 {
			data = data[idx+1:]
		}

		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())

		// Decode base64
		var decoded []byte
		decoded = make([]byte, len(data))
		n, err := decodeBase64(data, decoded)
		if err != nil {
			respondError(w, http.StatusBadRequest, "Invalid base64 data")
			return
		}

		if err := fm.Write(targetPath, decoded[:n]); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{
			"path": targetPath,
		})
	}
}

func (h *FileHandler) ViewFile(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	filePath := chi.URLParam(r, "*")

	// Get session and project
	sess, err := h.api.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	const maxSize int64 = 2 * 1024 * 1024 // 2MB limit

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		reader, fileInfo, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer reader.Close()

		if fileInfo.Size > maxSize {
			respondError(w, http.StatusRequestEntityTooLarge, "File too large to view (max 2MB)")
			return
		}

		content, err := io.ReadAll(io.LimitReader(reader, maxSize+1))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to read file")
			return
		}

		// Detect binary: check for null bytes in first 512 bytes
		checkLen := len(content)
		if checkLen > 512 {
			checkLen = 512
		}
		for i := 0; i < checkLen; i++ {
			if content[i] == 0 {
				respondError(w, http.StatusUnsupportedMediaType, "Binary file cannot be viewed")
				return
			}
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"name":      fileInfo.Name,
			"path":      fileInfo.Path,
			"size":      fileInfo.Size,
			"mime_type": fm.GetMimeType(filePath),
			"content":   string(content),
			"mod_time":  fileInfo.ModTime,
		})
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		file, fileInfo, sshClient, sftpClient, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer file.Close()
		defer sftpClient.Close()
		defer sshClient.Close()

		if fileInfo.Size > maxSize {
			respondError(w, http.StatusRequestEntityTooLarge, "File too large to view (max 2MB)")
			return
		}

		content, err := io.ReadAll(io.LimitReader(file, maxSize+1))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to read file")
			return
		}

		// Detect binary
		checkLen := len(content)
		if checkLen > 512 {
			checkLen = 512
		}
		for i := 0; i < checkLen; i++ {
			if content[i] == 0 {
				respondError(w, http.StatusUnsupportedMediaType, "Binary file cannot be viewed")
				return
			}
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"name":      fileInfo.Name,
			"path":      fileInfo.Path,
			"size":      fileInfo.Size,
			"mime_type": fm.GetMimeType(filePath),
			"content":   string(content),
			"mod_time":  fileInfo.ModTime,
		})
	}
}


// ListProjectFiles lists files in a project directory (read-only, no session required).
func (h *FileHandler) ListProjectFiles(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	path := r.URL.Query().Get("path")

	project, err := h.api.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	var fileList []files.FileInfo
	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		fileList, err = fm.List(path)
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		fileList, err = fm.List(path)
	}

	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, fileList)
}

// ViewProjectFile reads a file from a project (read-only, no session required, 2MB limit, text only).
func (h *FileHandler) ViewProjectFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	filePath := chi.URLParam(r, "*")

	project, err := h.api.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	const maxSize int64 = 2 * 1024 * 1024 // 2MB limit

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		reader, fileInfo, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer reader.Close()

		if fileInfo.Size > maxSize {
			respondError(w, http.StatusRequestEntityTooLarge, "File too large to view (max 2MB)")
			return
		}

		content, err := io.ReadAll(io.LimitReader(reader, maxSize+1))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to read file")
			return
		}

		checkLen := len(content)
		if checkLen > 512 {
			checkLen = 512
		}
		for i := 0; i < checkLen; i++ {
			if content[i] == 0 {
				respondError(w, http.StatusUnsupportedMediaType, "Binary file cannot be viewed")
				return
			}
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"name":      fileInfo.Name,
			"path":      fileInfo.Path,
			"size":      fileInfo.Size,
			"mime_type": fm.GetMimeType(filePath),
			"content":   string(content),
			"mod_time":  fileInfo.ModTime,
		})
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		file, fileInfo, sshClient, sftpClient, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer file.Close()
		defer sftpClient.Close()
		defer sshClient.Close()

		if fileInfo.Size > maxSize {
			respondError(w, http.StatusRequestEntityTooLarge, "File too large to view (max 2MB)")
			return
		}

		content, err := io.ReadAll(io.LimitReader(file, maxSize+1))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to read file")
			return
		}

		checkLen := len(content)
		if checkLen > 512 {
			checkLen = 512
		}
		for i := 0; i < checkLen; i++ {
			if content[i] == 0 {
				respondError(w, http.StatusUnsupportedMediaType, "Binary file cannot be viewed")
				return
			}
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"name":      fileInfo.Name,
			"path":      fileInfo.Path,
			"size":      fileInfo.Size,
			"mime_type": fm.GetMimeType(filePath),
			"content":   string(content),
			"mod_time":  fileInfo.ModTime,
		})
	}
}

// WriteProjectFile writes content to a file in a project (used by MCP copy tools).
func (h *FileHandler) WriteProjectFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Path == "" {
		respondError(w, http.StatusBadRequest, "path is required")
		return
	}
	// Reject path traversal
	if strings.Contains(req.Path, "..") {
		respondError(w, http.StatusBadRequest, "path must not contain '..'")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	data := []byte(req.Content)

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		if err := fm.Write(req.Path, data); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		if err := fm.Write(req.Path, data); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"path": req.Path,
		"size": len(data),
	})
}

// DownloadProjectFile streams raw file bytes from a project (no session required).
// Used by MCP copy tools to transfer files including binary ones.
func (h *FileHandler) DownloadProjectFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	filePath := chi.URLParam(r, "*")

	project, err := h.api.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		reader, fileInfo, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer reader.Close()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size))
		io.Copy(w, reader)
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		file, fileInfo, sshClient, sftpClient, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer file.Close()
		defer sftpClient.Close()
		defer sshClient.Close()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size))
		io.Copy(w, file)
	}
}

// UploadProjectFile writes raw bytes to a file in a project (no session required).
// Path is passed as a query parameter. Used by MCP copy tools.
func (h *FileHandler) UploadProjectFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		respondError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if strings.Contains(filePath, "..") {
		respondError(w, http.StatusBadRequest, "path must not contain '..'")
		return
	}

	project, err := h.api.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to read request body")
		return
	}

	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		if err := fm.Write(filePath, data); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		fm := files.NewRemoteFileManager(project, h.api.DecryptFunc())
		if err := fm.Write(filePath, data); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"path": filePath,
		"size": len(data),
	})
}

func parseID(s string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(s, "%d", &id)
	return id, err
}

// Simple base64 decoder
func decodeBase64(src string, dst []byte) (int, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	decoder := make([]int, 256)
	for i := range decoder {
		decoder[i] = -1
	}
	for i, c := range alphabet {
		decoder[c] = i
	}

	n := 0
	buf := 0
	bits := 0

	for _, c := range src {
		if c == '=' {
			break
		}
		if c == '\n' || c == '\r' || c == ' ' {
			continue
		}

		val := decoder[c]
		if val < 0 {
			return 0, fmt.Errorf("invalid character")
		}

		buf = buf<<6 | val
		bits += 6

		if bits >= 8 {
			bits -= 8
			if n < len(dst) {
				dst[n] = byte(buf >> bits)
				n++
			}
		}
	}

	return n, nil
}
