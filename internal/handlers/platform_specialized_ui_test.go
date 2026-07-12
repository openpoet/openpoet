package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/database"
	"openpoet/internal/voice"
)

func TestSpecializedUIProjectFileUsesSharedApplicationService(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	project := createSpecializedUIProject(t, platform.DB, projectDir)

	router := chi.NewRouter()
	router.Post("/projects/{id}/files/write", platform.FileHandler.WriteProjectFile)
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/projects/%d/files/write", project.ID), strings.NewReader(`{
		"path":"nested/note.txt","content":"shared service"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "ain:ui-file-write")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("write status = %d, body=%s", response.Code, response.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(projectDir, "nested", "note.txt"))
	if err != nil || string(content) != "shared service" {
		t.Fatalf("written content = %q, err=%v", content, err)
	}
	assertSpecializedUIEvent(t, platform.DB, "platform.file.project_file_written", "ain:ui-file-write")
}

func TestSpecializedUIProjectFileAppliesApplicationLimitWithoutWriting(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	project := createSpecializedUIProject(t, platform.DB, projectDir)

	payload, err := json.Marshal(map[string]string{
		"path": "too-large.txt", "content": strings.Repeat("x", (2<<20)+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Post("/projects/{id}/files/write", platform.FileHandler.WriteProjectFile)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost, fmt.Sprintf("/projects/%d/files/write", project.ID), bytes.NewReader(payload),
	))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized write status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(projectDir, "too-large.txt")); !os.IsNotExist(err) {
		t.Fatalf("oversized file should not exist, err=%v", err)
	}
	events, err := platform.DB.ListEventOutboxAfter(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == "platform.file.project_file_written" {
			t.Fatalf("rejected write emitted mutation event: %+v", event)
		}
	}
}

func TestSpecializedUISessionUploadKeepsMultipartAtHTTPBoundary(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	project := createSpecializedUIProject(t, platform.DB, projectDir)
	session := &database.Session{
		ID: "multipart-session", ProjectID: project.ID, Status: "running", Name: "multipart",
		StartTime: time.Now(), Backend: "claude_code",
	}
	if err := platform.DB.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("multipart through service")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	router.Post("/sessions/{id}/files", platform.FileHandler.UploadFiles)
	request := httptest.NewRequest(http.MethodPost, "/sessions/multipart-session/files?dir=nested", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Request-ID", "ain:ui-multipart-upload")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("multipart status = %d, body=%s", response.Code, response.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(projectDir, "nested", "note.txt"))
	if err != nil || string(content) != "multipart through service" {
		t.Fatalf("multipart content = %q, err=%v", content, err)
	}
	assertSpecializedUIEvent(t, platform.DB, "platform.file.session_files_uploaded", "ain:ui-multipart-upload")
}

func TestSpecializedUIGitStageUsesSharedApplicationService(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	runSpecializedUIGit(t, projectDir, "init")
	if err := os.WriteFile(filepath.Join(projectDir, "note.txt"), []byte("stage me"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := createSpecializedUIProject(t, platform.DB, projectDir)

	router := chi.NewRouter()
	router.Post("/projects/{id}/git/stage", platform.GitHandler.StageFiles)
	request := httptest.NewRequest(
		http.MethodPost, fmt.Sprintf("/projects/%d/git/stage", project.ID), strings.NewReader(`{"files":["note.txt"]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "ain:ui-git-stage")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("stage status = %d, body=%s", response.Code, response.Body.String())
	}
	if staged := strings.TrimSpace(runSpecializedUIGit(t, projectDir, "diff", "--cached", "--name-only")); staged != "note.txt" {
		t.Fatalf("staged paths = %q", staged)
	}
	assertSpecializedUIEvent(t, platform.DB, "platform.git.staged", "ain:ui-git-stage")
}

func TestSpecializedUINotificationPreferenceUsesSharedApplicationService(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	handler := NewWebSocketHandler(platform.Hub, api, platform.WebPush)
	request := httptest.NewRequest(http.MethodPut, "/notifications/preference", strings.NewReader(`{"disabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "ain:ui-push-preference")
	response := httptest.NewRecorder()
	handler.HandleSetNotificationPreference(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("preference status = %d, body=%s", response.Code, response.Body.String())
	}
	value, err := platform.DB.GetSetting(context.Background(), "push_notifications_disabled")
	if err != nil || value != "true" {
		t.Fatalf("stored preference = %q, err=%v", value, err)
	}
	assertSpecializedUIEvent(t, platform.DB, "platform.notifications.preference_updated", "ain:ui-push-preference")
}

func TestSpecializedUIPermissionResponseUsesSharedApplicationService(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	responseChannel := make(chan PermissionResponse, 1)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform.HookHandler.mu.Lock()
	platform.HookHandler.pending["session-1"] = &pendingPermission{responseCh: responseChannel, cancel: cancel}
	platform.HookHandler.mu.Unlock()

	router := chi.NewRouter()
	router.Post("/hooks/permission/{sessionId}/respond", platform.HookHandler.HandlePermissionRespond)
	request := httptest.NewRequest(
		http.MethodPost, "/hooks/permission/session-1/respond", strings.NewReader(`{"behavior":"allow","tool_name":"Read"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "ain:ui-permission")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("permission status = %d, body=%s", response.Code, response.Body.String())
	}
	select {
	case received := <-responseChannel:
		if received.Behavior != "allow" || received.ToolName != "Read" {
			t.Fatalf("permission response = %+v", received)
		}
	default:
		t.Fatal("permission response did not reach hook port")
	}
	assertSpecializedUIEvent(t, platform.DB, "platform.hook_response.permission_responded", "ain:ui-permission")
}

func TestSpecializedUIVoiceKeepsJSONAtBoundaryAndUsesApplicationValidation(t *testing.T) {
	api, platform := platformCompositionFixture(t)
	platform.VoiceHandler.getProviderConfig = func() (voice.ProviderType, string, string) {
		return voice.ProviderOpenAI, "test-key", "test-model"
	}
	if err := api.ConfigurePlatformServices(platform); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(base64TranscribeRequest{
		Audio: base64.StdEncoding.EncodeToString([]byte("not real audio")), Filename: "recording.exe",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/voice/transcribe", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	platform.VoiceHandler.Transcribe(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "extension is not supported") {
		t.Fatalf("voice validation status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestSpecializedUIMutationDoesNotFallbackWithoutComposition(t *testing.T) {
	handler := NewFileHandler(&API{})
	router := chi.NewRouter()
	router.Post("/projects/{id}/files/write", handler.WriteProjectFile)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost, "/projects/1/files/write", strings.NewReader(`{"path":"note.txt","content":"no fallback"}`),
	))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, body=%s", response.Code, response.Body.String())
	}
}

func createSpecializedUIProject(t *testing.T, db *database.DB, path string) *database.Project {
	t.Helper()
	project := &database.Project{
		Name: "specialized-ui", Path: path, Type: "local", Backend: "claude_code", BackendConfig: "{}",
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	return project
}

func assertSpecializedUIEvent(t *testing.T, db *database.DB, eventType, correlationID string) {
	t.Helper()
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == eventType {
			if event.Actor != "user:"+platformUIActorID || event.CorrelationID != correlationID {
				t.Fatalf("event identity = actor:%q correlation:%q", event.Actor, event.CorrelationID)
			}
			return
		}
	}
	t.Fatalf("event %q was not emitted: %+v", eventType, events)
}

func runSpecializedUIGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
