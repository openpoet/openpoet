package handlers

import (
	"devmanager/internal/files"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

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
