package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openpoet/internal/application"
)

func TestMCPAPIKeyUIRevealsSecretOnceAndStoredValueRemainsEncrypted(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}

	generated := httptest.NewRecorder()
	api.GenerateMCPAPIKey(generated, httptest.NewRequest(http.MethodPost, "/api/settings/mcp-key", nil))
	if generated.Code != http.StatusOK {
		t.Fatalf("generate status=%d body=%s", generated.Code, generated.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(generated.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	secret := response["key"]
	if !strings.HasPrefix(secret, "dm_") {
		t.Fatalf("one-time secret missing: %#v", response)
	}
	stored, err := platform.DB.GetSetting(context.Background(), "mcp_api_key")
	if err != nil {
		t.Fatal(err)
	}
	iv, err := platform.DB.GetSetting(context.Background(), "mcp_api_key_iv")
	if err != nil || iv == "" || stored == secret {
		t.Fatalf("key was not encrypted at rest: stored=%q iv=%q err=%v", stored, iv, err)
	}

	statusResponse := httptest.NewRecorder()
	api.GetMCPAPIKeyStatus(statusResponse, httptest.NewRequest(http.MethodGet, "/api/settings/mcp-key", nil))
	if statusResponse.Code != http.StatusOK || strings.Contains(statusResponse.Body.String(), secret) {
		t.Fatalf("status leaked generated key: status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	status := application.MCPAPIKeyStatus{Exists: true, Preview: "dm_preview...", Secret: secret}
	serialized, err := json.Marshal(status)
	if err != nil || strings.Contains(string(serialized), secret) || strings.Contains(string(serialized), "Secret") {
		t.Fatalf("automation-safe status serialized secret: %s err=%v", serialized, err)
	}
}
