package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
)

// Chunked voice uploads.
//
// The public entrypoint sits behind an nginx proxy whose request body limit
// (1 MiB) rejects any single recording longer than about a minute with an
// HTML 413. The browser therefore sends the recording in small byte slices
// that the server stages on disk and reassembles into the original file before
// transcription. Slices are idempotent (re-sending one overwrites it) and a
// completed upload that is missing slices reports which ones, so the client can
// always resume without re-recording.
const (
	voiceUploadMaxChunkBytes = 1 << 20
	voiceUploadMaxChunks     = 1024
	voiceUploadMaxAge        = 24 * time.Hour
)

var voiceUploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

type voiceChunkRequest struct {
	Data string `json:"data"` // base64-encoded slice
}

type voiceCompleteRequest struct {
	Chunks   int    `json:"chunks"`
	Filename string `json:"filename"`
	Language string `json:"language,omitempty"`
	// Client-measured diagnostics: recorded duration, and what remained after
	// the client shortened long pauses (0 when sent unchanged).
	Seconds     float64 `json:"seconds,omitempty"`
	KeptSeconds float64 `json:"kept_seconds,omitempty"`
}

func (h *VoiceHandler) uploadRoot() string {
	if h.uploadDir != "" {
		return h.uploadDir
	}
	return filepath.Join(os.TempDir(), "openpoet-voice-uploads")
}

func (h *VoiceHandler) uploadPath(r *http.Request) (string, bool) {
	id := chi.URLParam(r, "uploadId")
	if !voiceUploadIDPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(h.uploadRoot(), id), true
}

func voiceChunkName(index int) string {
	return fmt.Sprintf("%05d.part", index)
}

// UploadChunk stores one slice of a recording.
func (h *VoiceHandler) UploadChunk(w http.ResponseWriter, r *http.Request) {
	dir, ok := h.uploadPath(r)
	if !ok {
		respondError(w, http.StatusBadRequest, "Invalid upload id")
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 || index >= voiceUploadMaxChunks {
		respondError(w, http.StatusBadRequest, "Invalid chunk index")
		return
	}

	// base64 JSON inflates the slice by ~4/3; leave headroom for the envelope.
	body := http.MaxBytesReader(w, r.Body, voiceUploadMaxChunkBytes*3/2)
	var data []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req voiceChunkRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
			return
		}
		data, err = base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			respondError(w, http.StatusBadRequest, "Invalid base64 chunk: "+err.Error())
			return
		}
	} else {
		data, err = io.ReadAll(body)
		if err != nil {
			respondError(w, http.StatusBadRequest, "Failed to read chunk: "+err.Error())
			return
		}
	}
	if len(data) == 0 || len(data) > voiceUploadMaxChunkBytes {
		respondError(w, http.StatusBadRequest, "Chunk must contain between 1 byte and 1 MiB")
		return
	}

	h.pruneStaleUploads()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to stage chunk")
		return
	}
	// Write-then-rename so a retried or concurrent slice never leaves a torn file.
	tmp, err := os.CreateTemp(dir, "incoming-*")
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to stage chunk")
		return
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmp.Name())
		respondError(w, http.StatusInternalServerError, "Failed to stage chunk")
		return
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, voiceChunkName(index))); err != nil {
		os.Remove(tmp.Name())
		respondError(w, http.StatusInternalServerError, "Failed to stage chunk")
		return
	}
	respondJSON(w, http.StatusOK, map[string]int{"index": index, "size": len(data)})
}

// CompleteUpload reassembles the staged slices and transcribes the recording.
// The staged slices are kept when transcription fails so a retry does not have
// to upload them again.
func (h *VoiceHandler) CompleteUpload(w http.ResponseWriter, r *http.Request) {
	services, ready := h.api.platformApplicationServices()
	if !ready || services.Execution.Voice == nil {
		respondError(w, http.StatusServiceUnavailable, "platform voice service is unavailable")
		return
	}
	dir, ok := h.uploadPath(r)
	if !ok {
		respondError(w, http.StatusBadRequest, "Invalid upload id")
		return
	}
	var req voiceCompleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.Chunks <= 0 || req.Chunks > voiceUploadMaxChunks {
		respondError(w, http.StatusBadRequest, "Invalid chunk count")
		return
	}

	missing := []int{}
	total := int64(0)
	for i := 0; i < req.Chunks; i++ {
		info, err := os.Stat(filepath.Join(dir, voiceChunkName(i)))
		if err != nil {
			missing = append(missing, i)
			continue
		}
		total += info.Size()
	}
	if len(missing) > 0 {
		respondJSON(w, http.StatusConflict, map[string]any{
			"error":   fmt.Sprintf("Upload is missing %d of %d chunks", len(missing), req.Chunks),
			"code":    "voice_upload_incomplete",
			"missing": missing,
		})
		return
	}
	if total > 32<<20 {
		respondError(w, http.StatusBadRequest, "Audio must contain between 1 byte and 32 MiB")
		return
	}

	audio := make([]byte, 0, total)
	for i := 0; i < req.Chunks; i++ {
		part, err := os.ReadFile(filepath.Join(dir, voiceChunkName(i)))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to read staged chunk")
			return
		}
		audio = append(audio, part...)
	}

	result, err := services.Execution.Voice.Transcribe(platformUIContext(r), application.TranscribeVoiceCommand{
		Audio:         audio,
		Filename:      req.Filename,
		Language:      req.Language,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		log.Printf("[voice] upload %s failed: bytes=%d client_seconds=%.1f err=%v", filepath.Base(dir), total, req.Seconds, err)
		respondApplicationError(w, err)
		return
	}
	log.Printf("[voice] upload %s complete: bytes=%d chunks=%d client_seconds=%.1f kept_seconds=%.1f chars=%d",
		filepath.Base(dir), total, req.Chunks, req.Seconds, req.KeptSeconds, len(result.Text))
	os.RemoveAll(dir)
	respondJSON(w, http.StatusOK, result)
}

// DiscardUpload drops the staged slices of an abandoned recording.
func (h *VoiceHandler) DiscardUpload(w http.ResponseWriter, r *http.Request) {
	dir, ok := h.uploadPath(r)
	if !ok {
		respondError(w, http.StatusBadRequest, "Invalid upload id")
		return
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		respondError(w, http.StatusInternalServerError, "Failed to discard upload")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pruneStaleUploads removes staging directories nobody completed within a day.
func (h *VoiceHandler) pruneStaleUploads() {
	entries, err := os.ReadDir(h.uploadRoot())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-voiceUploadMaxAge)
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && entry.IsDir() && info.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(h.uploadRoot(), entry.Name()))
		}
	}
}
