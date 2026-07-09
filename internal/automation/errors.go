package automation

import (
	"encoding/json"
	"net/http"
)

type errorEnvelope struct {
	APIVersion    string          `json:"api_version"`
	CommandID     string          `json:"command_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Error         automationError `json:"error"`
}

type automationError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeErrorWithMetadata(w, status, code, message, retryable, "", "", nil)
}

func writeErrorWithMetadata(
	w http.ResponseWriter,
	status int,
	code, message string,
	retryable bool,
	commandID, correlationID string,
	details map[string]any,
) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		APIVersion: APIVersion, CommandID: commandID, CorrelationID: correlationID,
		Error: automationError{Code: code, Message: message, Retryable: retryable, Details: details},
	})
}
