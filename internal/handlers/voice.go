package handlers

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/voice"
)

type VoiceHandler struct {
	api               *API
	getProviderConfig func() (voice.ProviderType, string, string) // provider, apiKey, model
}

func NewVoiceHandler(api *API, getProviderConfig func() (voice.ProviderType, string, string)) *VoiceHandler {
	return &VoiceHandler{
		api:               api,
		getProviderConfig: getProviderConfig,
	}
}

// base64TranscribeRequest is the JSON payload for base64-encoded audio.
// Used when multipart form data can't be sent (e.g. through the relay tunnel).
type base64TranscribeRequest struct {
	Audio    string `json:"audio"`    // base64-encoded audio data
	Filename string `json:"filename"` // e.g. "recording.webm"
	Language string `json:"language,omitempty"`
}

func (h *VoiceHandler) Transcribe(w http.ResponseWriter, r *http.Request) {
	services, ready := h.api.platformApplicationServices()
	if !ready || services.Execution.Voice == nil {
		respondError(w, http.StatusServiceUnavailable, "platform voice service is unavailable")
		return
	}
	if h.getProviderConfig == nil {
		respondError(w, http.StatusServiceUnavailable, "Voice provider not configured")
		return
	}
	providerType, apiKey, _ := h.getProviderConfig()
	if apiKey == "" {
		respondError(w, http.StatusServiceUnavailable, providerType.String()+" API key not configured")
		return
	}

	ct := r.Header.Get("Content-Type")

	// JSON body with base64-encoded audio (used via relay tunnel)
	if strings.HasPrefix(ct, "application/json") {
		h.transcribeBase64(w, r, services.Execution.Voice)
		return
	}

	// Standard multipart form (direct access)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		respondError(w, http.StatusBadRequest, "Failed to parse form: "+err.Error())
		return
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		respondError(w, http.StatusBadRequest, "No audio file provided")
		return
	}
	defer file.Close()

	audio, err := io.ReadAll(io.LimitReader(file, 32<<20+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Failed to read audio file")
		return
	}
	result, err := services.Execution.Voice.Transcribe(platformUIContext(r), application.TranscribeVoiceCommand{
		Audio:         audio,
		Filename:      header.Filename,
		Language:      r.FormValue("language"),
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, result)
}

func (h *VoiceHandler) transcribeBase64(w http.ResponseWriter, r *http.Request, service *application.VoiceTranscriptionService) {
	var req base64TranscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	if req.Audio == "" {
		respondError(w, http.StatusBadRequest, "Missing audio field")
		return
	}
	if req.Filename == "" {
		req.Filename = "recording.webm"
	}

	audioData, err := base64.StdEncoding.DecodeString(req.Audio)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid base64 audio: "+err.Error())
		return
	}

	result, err := service.Transcribe(platformUIContext(r), application.TranscribeVoiceCommand{
		Audio:         audioData,
		Filename:      req.Filename,
		Language:      req.Language,
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, result)
}
