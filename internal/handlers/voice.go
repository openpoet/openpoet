package handlers

import (
	"devmanager/internal/voice"
	"net/http"
)

type VoiceHandler struct {
	api              *API
	getProviderConfig func() (voice.ProviderType, string)
}

func NewVoiceHandler(api *API, getProviderConfig func() (voice.ProviderType, string)) *VoiceHandler {
	return &VoiceHandler{
		api:              api,
		getProviderConfig: getProviderConfig,
	}
}

func (h *VoiceHandler) Transcribe(w http.ResponseWriter, r *http.Request) {
	providerType, apiKey := h.getProviderConfig()
	if apiKey == "" {
		respondError(w, http.StatusServiceUnavailable, providerType.String()+" API key not configured")
		return
	}

	// Parse multipart form
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 32MB limit
		respondError(w, http.StatusBadRequest, "Failed to parse form: "+err.Error())
		return
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		respondError(w, http.StatusBadRequest, "No audio file provided")
		return
	}
	defer file.Close()

	// Create transcription provider
	provider, err := voice.NewTranscriptionProvider(providerType, apiKey)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Get optional language parameter
	if lang := r.FormValue("language"); lang != "" {
		provider.SetLanguage(lang)
	}

	// Transcribe
	result, err := provider.TranscribeMultipart(r.Context(), file, header)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, result)
}
