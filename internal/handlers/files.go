package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/files"

	"github.com/go-chi/chi/v5"
)

type FileHandler struct {
	api *API
}

var (
	htmlRootURLAttrRE = regexp.MustCompile(`(?i)\b(href|src|action|poster|data)\s*=\s*(['"])(/[^'"]*)['"]`)
	htmlSrcsetAttrRE  = regexp.MustCompile(`(?i)\bsrcset\s*=\s*(['"])([^'"]*)['"]`)
	cssRootURLRE      = regexp.MustCompile(`(?i)url\(\s*(['"]?)(/[^'")]*)['"]?\s*\)`)
	cssRootImportRE   = regexp.MustCompile(`(?i)@import\s+(['"])(/[^'"]*)['"]`)
)

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

	h.streamProjectFile(w, project, filePath, true, "")
}

// PreviewFile streams a session file inline so HTML previews can load relative
// CSS, JS, images, and other artifacts through the same URL prefix.
func (h *FileHandler) PreviewFile(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	filePath := chi.URLParam(r, "*")

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

	previewRoot := "/api/sessions/" + url.PathEscape(sessionID) + "/files/preview"
	h.streamProjectFile(w, project, filePath, false, previewRoot)
}

// ServePreviewReferrerAsset serves root-relative artifacts requested by an HTML
// preview iframe. Browsers resolve URLs like "/assets/app.css" against the app
// origin, so use the preview document Referer to map that root path back to the
// project/session being previewed.
func (h *FileHandler) ServePreviewReferrerAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/static/") {
		return false
	}

	ref := r.Referer()
	if ref == "" {
		return false
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return false
	}
	if refURL.Host != "" && refURL.Host != r.Host {
		return false
	}

	project, ok := h.projectFromPreviewReferer(r, refURL.Path)
	if !ok {
		return false
	}

	filePath := strings.TrimPrefix(r.URL.Path, "/")
	if filePath == "" {
		return false
	}
	h.streamProjectFile(w, project, filePath, false, "")
	return true
}

func (h *FileHandler) projectFromPreviewReferer(r *http.Request, refPath string) (*database.Project, bool) {
	if rest, ok := strings.CutPrefix(refPath, "/api/projects/"); ok {
		idPart, _, ok := strings.Cut(rest, "/files/preview/")
		if !ok {
			return nil, false
		}
		projectID, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			return nil, false
		}
		project, err := h.api.db.GetProject(r.Context(), projectID)
		return project, err == nil
	}

	if rest, ok := strings.CutPrefix(refPath, "/api/sessions/"); ok {
		sessionID, _, ok := strings.Cut(rest, "/files/preview/")
		if !ok || sessionID == "" {
			return nil, false
		}
		sess, err := h.api.db.GetSession(r.Context(), sessionID)
		if err != nil {
			return nil, false
		}
		project, err := h.api.db.GetProject(r.Context(), sess.ProjectID)
		return project, err == nil
	}

	return nil, false
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

	h.streamProjectFile(w, project, filePath, false, "")
}

// PreviewProjectFile streams project files inline for embedded browser previews.
func (h *FileHandler) PreviewProjectFile(w http.ResponseWriter, r *http.Request) {
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

	previewRoot := fmt.Sprintf("/api/projects/%d/files/preview", id)
	h.streamProjectFile(w, project, filePath, false, previewRoot)
}

func (h *FileHandler) streamProjectFile(w http.ResponseWriter, project *database.Project, filePath string, attachment bool, previewRoot string) {
	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		reader, fileInfo, err := fm.ReadStream(filePath)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		defer reader.Close()

		contentType := fm.GetMimeType(filePath)
		w.Header().Set("Content-Type", contentType)
		if attachment {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileInfo.Name))
		}
		if h.serveRewrittenPreviewFile(w, reader, filePath, contentType, previewRoot) {
			return
		}
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

		contentType := fm.GetMimeType(filePath)
		w.Header().Set("Content-Type", contentType)
		if attachment {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileInfo.Name))
		}
		if h.serveRewrittenPreviewFile(w, file, filePath, contentType, previewRoot) {
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size))
		io.Copy(w, file)
	}
}

func (h *FileHandler) serveRewrittenPreviewFile(w http.ResponseWriter, reader io.Reader, filePath, contentType, previewRoot string) bool {
	if previewRoot == "" || !isPreviewRewriteType(filePath, contentType) {
		return false
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to read file")
		return true
	}
	data = rewritePreviewAssetRoots(data, filePath, contentType, previewRoot)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
	return true
}

func isPreviewRewriteType(filePath, contentType string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	return ext == ".html" || ext == ".htm" || ext == ".css" ||
		strings.HasPrefix(contentType, "text/html") || strings.HasPrefix(contentType, "text/css")
}

func rewritePreviewAssetRoots(data []byte, filePath, contentType, previewRoot string) []byte {
	text := string(data)
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".html" || ext == ".htm" || strings.HasPrefix(contentType, "text/html") {
		text = rewriteHTMLRootURLs(text, previewRoot)
	}
	if ext == ".css" || strings.HasPrefix(contentType, "text/css") ||
		ext == ".html" || ext == ".htm" || strings.HasPrefix(contentType, "text/html") {
		text = rewriteCSSRootURLs(text, previewRoot)
	}
	return []byte(text)
}

func rewriteHTMLRootURLs(text, previewRoot string) string {
	text = htmlRootURLAttrRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := htmlRootURLAttrRE.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		return parts[1] + "=" + parts[2] + rewritePreviewRootURL(parts[3], previewRoot) + parts[2]
	})
	text = htmlSrcsetAttrRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := htmlSrcsetAttrRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return "srcset=" + parts[1] + rewritePreviewSrcset(parts[2], previewRoot) + parts[1]
	})
	return text
}

func rewriteCSSRootURLs(text, previewRoot string) string {
	text = cssRootURLRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := cssRootURLRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		quote := parts[1]
		if quote == "" {
			quote = `"`
		}
		return "url(" + quote + rewritePreviewRootURL(parts[2], previewRoot) + quote + ")"
	})
	text = cssRootImportRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := cssRootImportRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return "@import " + parts[1] + rewritePreviewRootURL(parts[2], previewRoot) + parts[1]
	})
	return text
}

func rewritePreviewSrcset(value, previewRoot string) string {
	candidates := strings.Split(value, ",")
	for i, candidate := range candidates {
		leadingLen := len(candidate) - len(strings.TrimLeft(candidate, " \t\r\n"))
		leading := candidate[:leadingLen]
		rest := candidate[leadingLen:]
		if rest == "" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		fields[0] = rewritePreviewRootURL(fields[0], previewRoot)
		candidates[i] = leading + strings.Join(fields, " ")
	}
	return strings.Join(candidates, ",")
}

func rewritePreviewRootURL(value, previewRoot string) string {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return value
	}
	return previewRoot + value
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
