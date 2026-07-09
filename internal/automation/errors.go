package automation

import (
	"encoding/json"
	"net/http"
)

type errorEnvelope struct {
	Error automationError `json:"error"`
}

type automationError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: automationError{
		Code:      code,
		Message:   message,
		Retryable: retryable,
	}})
}
