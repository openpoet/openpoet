package handlers

import (
	"errors"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
)

// resolveCustomToolForExecution returns a short-lived copy whose command is
// plaintext. Callers must use it only at the final execution boundary and must
// never place Command in API responses, proposal payloads, events, or logs.
// Legacy plaintext commands remain executable during the cutover window.
func (h *AIHandler) resolveCustomToolForExecution(tool *database.ProjectTool) (*database.ProjectTool, error) {
	if tool == nil {
		return nil, errors.New("custom tool is required")
	}
	var decrypt secretvalue.DecryptFunc
	if h != nil && h.api != nil && h.api.encryptor != nil {
		decrypt = h.api.encryptor.Decrypt
	}
	command, err := secretvalue.Resolve(tool.Command, decrypt)
	if err != nil {
		return nil, errors.New("custom tool command could not be resolved")
	}
	resolved := *tool
	resolved.Command = command
	return &resolved, nil
}
