package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/configsync"
	"openpoet/internal/database"
	"openpoet/internal/llm"
	"openpoet/internal/notifications"
	"openpoet/internal/security"
	"openpoet/internal/session"
	"openpoet/internal/updater"
	"openpoet/internal/voice"
	"openpoet/internal/websocket"
)

func platformCompositionFixture(t *testing.T) (*API, PlatformServices) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "platform-composition.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := websocket.NewHub()
	t.Cleanup(hub.Stop)
	encryptor, err := security.NewEncryptor("platform-composition-test-key")
	if err != nil {
		t.Fatal(err)
	}
	manager := session.NewManager(db, hub, "localhost:0")
	syncer := configsync.NewConfigSyncer(db, encryptor.Decrypt, "localhost:0")
	push, err := notifications.NewWebPushService(db, "test@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	notificationService := notifications.NewService(db, hub, push)
	hook := NewHookHandler(hub, notificationService, manager)
	api := NewAPI(db, hub, manager, syncer, encryptor, notificationService, hook)
	fileHandler := NewFileHandler(api)
	gitHandler := NewGitHandler(api)
	voiceHandler := NewVoiceHandler(api, func() (voice.ProviderType, string, string) {
		return voice.ProviderOpenAI, "", ""
	})
	structured := NewStructuredViewHandler(db, hub, api.DecryptFunc())
	appUpdater := updater.New("test")
	providerManager := llm.NewProviderManager("http://localhost:0")
	aiHandler := NewAIHandler(api, providerManager)
	api.SetAIHandler(aiHandler)
	api.SetUpdater(appUpdater)
	api.SetStructuredView(structured)
	return api, PlatformServices{
		DB: db, Hub: hub, SessionManager: manager, ConfigSync: syncer, Encryptor: encryptor,
		HookHandler: hook, FileHandler: fileHandler, GitHandler: gitHandler, VoiceHandler: voiceHandler,
		StructuredView: structured, Updater: appUpdater, AIHandler: aiHandler,
		Notifications: notificationService, WebPush: push, ReinitializeAI: func() {},
	}
}

func TestConfigurePlatformServicesRegistersCompleteInventory(t *testing.T) {
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	registry := api.PlatformCapabilityRegistry()
	if registry == nil {
		t.Fatal("platform registry was not exposed")
	}
	descriptors := registry.ListForActor(automation.Actor{})
	if len(descriptors) != expectedPlatformCapabilities {
		t.Fatalf("capability total = %d, want %d", len(descriptors), expectedPlatformCapabilities)
	}
	names := make(map[application.CapabilityName]struct{}, len(descriptors))
	mutations := 0
	for _, descriptor := range descriptors {
		if descriptor.Name == "" || descriptor.Handler == "" || descriptor.Service == "" || len(descriptor.Scopes) == 0 {
			t.Fatalf("incomplete descriptor: %+v", descriptor)
		}
		if _, exists := names[descriptor.Name]; exists {
			t.Fatalf("duplicate capability %q", descriptor.Name)
		}
		names[descriptor.Name] = struct{}{}
		if descriptor.Mutation {
			mutations++
		}
	}
	if mutations != expectedPlatformMutations || len(descriptors)-mutations != expectedPlatformReads {
		t.Fatalf("inventory = %d mutations/%d reads, want %d/%d", mutations, len(descriptors)-mutations, expectedPlatformMutations, expectedPlatformReads)
	}
	bundle, ready := api.platformApplicationServices()
	if !ready || bundle == nil || bundle.Configuration.Projects == nil || bundle.Execution.Sessions == nil || bundle.Collaboration.AI == nil {
		t.Fatalf("typed platform application-service bundle is incomplete: ready=%v bundle=%+v", ready, bundle)
	}
	readiness := api.PlatformAutomationReadiness()
	if !readiness.Ready || readiness.Total != expectedPlatformCapabilities || readiness.Mutations != expectedPlatformMutations || readiness.Reads != expectedPlatformReads {
		t.Fatalf("platform readiness mismatch: %+v", readiness)
	}
}

func TestConfigurePlatformServicesFailsClosedWithoutBreakingLegacyRegistry(t *testing.T) {
	api, services := platformCompositionFixture(t)
	services.WebPush = nil
	if err := api.ConfigurePlatformServices(services); err == nil {
		t.Fatal("expected missing web push dependency to fail composition")
	}
	if api.PlatformCapabilityRegistry() != nil {
		t.Fatal("platform registry must remain unavailable after failed composition")
	}
	if bundle, ready := api.platformApplicationServices(); ready || bundle != nil || api.PlatformAutomationReadiness().Ready {
		t.Fatalf("platform bundle/readiness must remain unavailable: ready=%v bundle=%+v status=%+v", ready, bundle, api.PlatformAutomationReadiness())
	}
	if api.CapabilityRegistry() == nil {
		t.Fatal("legacy capability registry must remain available")
	}
	if _, ok := api.CapabilityRegistry().Lookup("tasks.create"); !ok {
		t.Fatal("legacy task capability disappeared after platform failure")
	}
}

func TestPlatformEffectsNeverPersistApplicationMetaSecrets(t *testing.T) {
	api, services := platformCompositionFixture(t)
	effects := &platformEffects{api: api, db: services.DB, hub: services.Hub}
	ctx := application.WithEventMetadata(context.Background(), application.EventMetadata{Actor: application.Actor{Type: "automation", ID: "helena"}, CorrelationID: "ain:test"})
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	effects.PublishApplicationChange(ctx, application.ApplicationChange{Domain: "configuration", Action: "updated", ID: 1, Meta: map[string]any{"api_key": secret}})
	events, err := services.DB.ListEventOutboxAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	encoded, _ := json.Marshal(events[0])
	if strings.Contains(string(encoded), secret) || strings.Contains(events[0].PayloadJSON, "api_key") {
		t.Fatalf("secret metadata crossed audit boundary: %s", encoded)
	}
	if events[0].CorrelationID != "ain:test" || events[0].Actor != "automation:helena" {
		t.Fatalf("event identity mismatch: %+v", events[0])
	}
}

func TestPlatformFileReadPortRedactsTextSecrets(t *testing.T) {
	_, services := platformCompositionFixture(t)
	projectDir := t.TempDir()
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	if err := os.WriteFile(filepath.Join(projectDir, "notes.txt"), []byte("API_KEY="+secret+"\nkeep=this"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := &database.Project{Name: "redaction", Path: projectDir, Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := services.DB.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	result, err := (platformFileReader{handler: services.FileHandler}).ReadOperationalFile(context.Background(), automation.OperationalFileScope{ProjectID: project.ID}, "notes.txt", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Data), secret) || !strings.Contains(string(result.Data), "[REDACTED]") {
		t.Fatalf("text file secret was not redacted: %q", result.Data)
	}
}
