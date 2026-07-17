package providerbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/security"
)

func openAIStoreFixture(t *testing.T) (*EncryptedAIConfigStore, *database.DB, int64) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "openpoet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	profile := &database.AIConfig{Name: "OpenAI OAuth", ProviderType: ProviderTypeOpenAIOAuth, Model: "gpt-test", ExtraJSON: "{}"}
	if err := db.CreateAIConfig(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	encryptor, err := security.NewEncryptor("providerbridge-test-key")
	if err != nil {
		t.Fatal(err)
	}
	return NewEncryptedAIConfigStore(db, encryptor), db, profile.ID
}

func authFixture(access, refresh string) string {
	raw, _ := json.Marshal(storedAuth{Access: access, Refresh: refresh, Expires: 4102444800000, AccountID: "acct_test"})
	return string(raw)
}

func TestEncryptedAIConfigStoreRoundTripAndClear(t *testing.T) {
	store, db, profileID := openAIStoreFixture(t)
	raw := authFixture("access-private", "refresh-private")
	if err := store.Save(context.Background(), profileID, raw); err != nil {
		t.Fatal(err)
	}
	profile, err := db.GetAIConfig(context.Background(), profileID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(profile.APIKeyEncrypted, "access-private") || strings.Contains(profile.APIKeyEncrypted, "refresh-private") {
		t.Fatal("OAuth credential was stored as plaintext")
	}
	if profile.APIKeyPreview != "OAuth connected" {
		t.Fatalf("preview = %q", profile.APIKeyPreview)
	}
	loaded, err := store.Load(context.Background(), profileID)
	if err != nil || loaded != raw {
		t.Fatalf("Load() = %q, %v", loaded, err)
	}
	if err := store.Clear(context.Background(), profileID); err != nil {
		t.Fatal(err)
	}
	if connected, err := store.Connected(context.Background(), profileID); err != nil || connected {
		t.Fatalf("Connected() = %t, %v", connected, err)
	}
}

func TestTrustedOpenAIVerificationURL(t *testing.T) {
	got, err := trustedOpenAIVerificationURL("https://auth.openai.com/codex/device")
	if err != nil || got != "https://auth.openai.com/codex/device" {
		t.Fatalf("trusted URL = %q, %v", got, err)
	}
	for _, candidate := range []string{
		"http://auth.openai.com/codex/device",
		"https://auth.openai.com.evil.example/codex/device",
		"javascript:alert(1)",
		"https://user@auth.openai.com/codex/device",
	} {
		if _, err := trustedOpenAIVerificationURL(candidate); err == nil {
			t.Fatalf("untrusted verification URL accepted: %s", candidate)
		}
	}
}

func TestSafeHelperErrorNeverEchoesRawOutput(t *testing.T) {
	secret := "access_token=private-value"
	if got := safeHelperError(secret); strings.Contains(got, "private-value") || strings.Contains(got, "access_token") {
		t.Fatalf("sensitive helper output leaked: %q", got)
	}
	if got := safeHelperError("Device auth timed out after 300 seconds"); got != "device authorization timed out" {
		t.Fatalf("timeout error = %q", got)
	}
}

func TestManagerDeviceLoginUsesIsolatedProfileAndEncryptsResult(t *testing.T) {
	store, _, profileID := openAIStoreFixture(t)
	helper := providerBridgeHelper(t)
	manager := NewManager(store, helper)
	t.Cleanup(manager.Close)

	login, err := manager.StartDeviceLogin(context.Background(), profileID)
	if err != nil {
		t.Fatal(err)
	}
	if login.ID == "" {
		t.Fatal("login ID is empty")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		login, err = manager.DeviceLoginStatus(login.ID)
		if err != nil {
			t.Fatal(err)
		}
		if login.Status == "succeeded" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if login.Status != "succeeded" {
		t.Fatalf("login status = %+v", login)
	}
	loaded, err := store.Load(context.Background(), profileID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded, "isolated-access") || !strings.Contains(loaded, "isolated-refresh") {
		t.Fatal("OAuth helper result was not imported")
	}
}

func TestManagerStartsLoopbackProxyAndPersistsRefresh(t *testing.T) {
	store, _, profileID := openAIStoreFixture(t)
	if err := store.Save(context.Background(), profileID, authFixture("initial-access", "initial-refresh")); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, providerBridgeHelper(t))
	defer manager.Close()
	baseURL, err := manager.EnsureProxy(context.Background(), profileID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Fatalf("proxy URL is not loopback-only: %s", baseURL)
	}
	response, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}

	manager.StopProxy(profileID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		loaded, loadErr := store.Load(context.Background(), profileID)
		if loadErr == nil && strings.Contains(loaded, "refreshed-by-helper") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("refreshed OAuth credential was not encrypted back into OpenPoet")
}

func providerBridgeHelper(t *testing.T) string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "claude-code-proxy")
	script := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestProviderBridgeHelperProcess -- \"$@\"\n", shellQuote(testBinary))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestProviderBridgeHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	args := os.Args[separator+1:]
	configDir := os.Getenv("CCP_CONFIG_DIR")
	if configDir == "" || !strings.Contains(filepath.Base(os.Getenv("HOME")), "openpoet-") {
		os.Exit(21)
	}
	authDir := filepath.Join(configDir, "codex")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		os.Exit(22)
	}
	if len(args) >= 3 && args[0] == "codex" && args[1] == "auth" && args[2] == "device" {
		fmt.Println("Visit: https://auth.openai.com/codex/device")
		fmt.Println("Enter code: TEST-CODE")
		if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(authFixture("isolated-access", "isolated-refresh")), 0o600); err != nil {
			os.Exit(23)
		}
		os.Exit(0)
	}
	if len(args) >= 1 && args[0] == "serve" {
		port := 0
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "--port" {
				port, _ = strconv.Atoi(args[i+1])
			}
		}
		if port <= 0 {
			os.Exit(24)
		}
		_ = os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(authFixture("refreshed-by-helper", "refreshed-by-helper")), 0o600)
		handler := http.NewServeMux()
		handler.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		if err := http.ListenAndServe("127.0.0.1:"+strconv.Itoa(port), handler); err != nil {
			os.Exit(25)
		}
	}
	os.Exit(26)
}
