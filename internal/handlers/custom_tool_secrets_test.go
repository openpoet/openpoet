package handlers

import (
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/secretvalue"
	"openpoet/internal/security"
)

func TestResolveCustomToolForExecutionDecryptsCopyAndSupportsLegacy(t *testing.T) {
	encryptor, err := security.NewEncryptor("custom-tool-runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secretvalue.Encrypt(encryptor, "printf private-runtime-value")
	if err != nil {
		t.Fatal(err)
	}
	handler := &AIHandler{api: &API{encryptor: encryptor}}
	stored := &database.ProjectTool{ID: 1, Command: envelope}
	resolved, err := handler.resolveCustomToolForExecution(stored)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Command != "printf private-runtime-value" || stored.Command != envelope {
		t.Fatalf("resolved/stored commands were not isolated")
	}
	legacy := &database.ProjectTool{ID: 2, Command: "printf legacy"}
	resolved, err = (&AIHandler{}).resolveCustomToolForExecution(legacy)
	if err != nil || resolved.Command != legacy.Command {
		t.Fatalf("legacy resolve = %q, err=%v", resolved.Command, err)
	}
}
