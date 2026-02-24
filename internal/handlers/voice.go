package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"openpoet/internal/voice"
	"strings"
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
	providerType, apiKey, model := h.getProviderConfig()
	if apiKey == "" {
		respondError(w, http.StatusServiceUnavailable, providerType.String()+" API key not configured")
		return
	}

	ct := r.Header.Get("Content-Type")

	// JSON body with base64-encoded audio (used via relay tunnel)
	if strings.HasPrefix(ct, "application/json") {
		h.transcribeBase64(w, r, providerType, apiKey, model)
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

	provider, err := voice.NewTranscriptionProvider(providerType, apiKey, model)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if lang := r.FormValue("language"); lang != "" {
		provider.SetLanguage(lang)
	}

	result, err := provider.TranscribeMultipart(r.Context(), file, header)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, result)
}

func (h *VoiceHandler) transcribeBase64(w http.ResponseWriter, r *http.Request, providerType voice.ProviderType, apiKey string, model string) {
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

	provider, err := voice.NewTranscriptionProvider(providerType, apiKey, model)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.Language != "" {
		provider.SetLanguage(req.Language)
	}

	result, err := provider.TranscribeBytes(r.Context(), audioData, req.Filename)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, result)
}
