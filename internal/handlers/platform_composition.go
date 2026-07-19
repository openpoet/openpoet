package handlers

import (
	"context"
	"errors"
	"fmt"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/configsync"
	"openpoet/internal/database"
	"openpoet/internal/notifications"
	"openpoet/internal/security"
	runtime "openpoet/internal/session"
	"openpoet/internal/updater"
	"openpoet/internal/websocket"
	"openpoet/internal/workspace"
)

const (
	// Phase 5 added groups.list (config read), blackboard.get (exec read),
	// blackboard.put (exec write): +3 capabilities, +1 mutation, +2 reads.
	// Phase 6 added environments.approve_manifest (unsafe) + workspaces.discard
	// (destructive): +2 capabilities, +2 mutations, +0 reads.
	expectedPlatformCapabilities = 168
	expectedPlatformMutations    = 112
	expectedPlatformReads        = 56
)

// PlatformServices is the explicit runtime composition root for Automation.
// It deliberately accepts already-created process components so no adapter
// reaches back through HTTP or creates a second credential/provider owner.
type PlatformServices struct {
	DB             *database.DB
	Hub            *websocket.Hub
	SessionManager *runtime.Manager
	ConfigSync     *configsync.ConfigSyncer
	Encryptor      *security.Encryptor
	HookHandler    *HookHandler
	FileHandler    *FileHandler
	GitHandler     *GitHandler
	VoiceHandler   *VoiceHandler
	StructuredView *StructuredViewHandler
	Updater        *updater.Updater
	AIHandler      *AIHandler
	Notifications  *notifications.Service
	WebPush        *notifications.WebPushService
	ReinitializeAI func()
}

// PlatformApplicationServices is the immutable typed bundle shared by the
// Automation registry today and by legacy UI handlers as they migrate off
// their dual paths. It is published only after the complete inventory passes.
type PlatformApplicationServices struct {
	Configuration automation.ConfigurationPlatformServices
	Execution     automation.ExecutionPlatformServices
	Collaboration automation.CollaborationPlatformServices
}

type PlatformAutomationReadiness struct {
	Ready     bool `json:"ready"`
	Total     int  `json:"total"`
	Mutations int  `json:"mutations"`
	Reads     int  `json:"reads"`
}

func (a *API) ConfigurePlatformServices(services PlatformServices) error {
	if a == nil || a.capabilities == nil || a.taskService == nil {
		return errors.New("platform composition requires an initialized API")
	}
	a.clearPlatformComposition()
	if err := validatePlatformServices(services); err != nil {
		return err
	}

	effects := &platformEffects{api: a, db: services.DB, hub: services.Hub}
	reinitializer := platformAIReinitializer{callback: services.ReinitializeAI}
	configuration := automation.ConfigurationPlatformServices{
		Projects: application.NewProjectService(services.DB, services.Encryptor, effects, platformProjectPathValidator{}),
		ProjectOperations: application.NewProjectOperationService(
			services.DB, NewProjectOperationAdapter(a), effects,
		),
		Tags:          application.NewTagService(services.DB),
		Skills:        application.NewSkillService(services.DB, effects),
		Agents:        application.NewAIAgentService(services.DB, effects),
		AIConfigs:     application.NewAIConfigService(services.DB, services.Encryptor, effects, reinitializer),
		MCP:           application.NewMCPService(services.DB, services.Encryptor, effects),
		CustomTools:   application.NewCustomToolService(services.DB, services.Encryptor, effects),
		Configuration: application.NewConfigurationService(services.DB, services.Encryptor, effects, reinitializer, services.ConfigSync),
	}

	workspaceService := application.NewWorkspaceService(services.DB, NewGitCommandAdapter(services.GitHandler), services.ConfigSync)
	workspaceService.SetEnvironmentProvisioner(workspace.NewProvisioner(services.DB)) // Phase 6: environment.yaml provisioning
	workRunService := application.NewWorkRunService(services.DB)
	sessionService := application.NewSessionService(
		services.DB, services.SessionManager, services.ConfigSync, a.taskService,
		services.HookHandler, services.HookHandler, a.DecryptFunc(), effects,
		application.SessionCreationCollaborators{
			Environment:  platformSessionEnvironmentProvider{handler: services.AIHandler},
			Names:        platformSessionNameStore{db: services.DB},
			Tasks:        platformSessionTaskNotifier{hook: services.HookHandler},
			Input:        platformSessionInputSubmitter{api: a},
			InitialInput: platformSessionInitialPromptSubmitter{api: a},
			Settings:     platformSessionRuntimeSettings{api: a},
			Workspaces:   workspaceService,
			Signals:      services.HookHandler,
			WorkRuns:     workRunService,
		},
	)
	execution := automation.ExecutionPlatformServices{
		Sessions:           sessionService,
		SessionWatchers:    application.NewSessionEventWatcherService(services.DB, NewSessionEventWatcherAdapter(services.StructuredView), effects),
		SessionSuggestions: application.NewSessionTaskSuggestionService(services.DB, services.SessionManager, platformSessionSuggestionProvider{handler: services.AIHandler}, effects),
		FileMutations:      application.NewFileMutationService(services.DB, services.FileHandler, effects),
		GitMutations:       application.NewGitMutationService(services.DB, NewGitMutationAdapter(services.GitHandler), effects),
		HookResponses:      application.NewHookResponseService(services.HookHandler, effects),
		Voice:              application.NewVoiceTranscriptionService(services.VoiceHandler),
		TunnelMutations:    application.NewTunnelMutationService(NewTunnelMutationAdapter(a), effects),
		UpdateMutations:    application.NewUpdateMutationService(services.Updater, a, effects),
		SessionQueries:     services.DB,
		SessionRuntime:     services.SessionManager,
		SessionEvents:      platformSessionEventReader{handler: services.StructuredView},
		Files:              platformFileReader{handler: services.FileHandler},
		Git:                platformGitReader{handler: services.GitHandler},
		Tunnel:             platformTunnelReader{api: a},
		Updates:            services.Updater,
		Conflicts:          services.DB,
		Workspaces:         workspaceService,
		Blackboard:         services.DB,
		Environments:       application.NewEnvironmentService(services.DB),
	}

	collaboration := automation.CollaborationPlatformServices{
		Documents: application.NewDocumentService(services.DB, platformMemoryMirror{api: a}, effects),
		Proposals: application.NewProposalService(platformProposalBackend{api: a}, effects),
		AI: application.NewAIAssistantService(
			platformAIProvider{handler: services.AIHandler},
			platformAIConversationBackend{api: a},
			platformAIToolExecutor{handler: services.AIHandler}, effects,
		),
		Notifications:        application.NewNotificationService(services.Notifications),
		NotificationDelivery: application.NewNotificationDeliveryService(platformNotificationDeliveryBackend{db: services.DB, push: services.WebPush}, effects),
		TokenUsage:           application.NewTokenUsageService(services.DB, effects),
		AIQueries:            services.DB,
		TokenUsageQueries:    services.DB,
	}

	registry, err := automation.NewPlatformCapabilityRegistry(a.capabilities)
	if err != nil {
		return fmt.Errorf("create platform registry: %w", err)
	}
	if err := automation.RegisterConfigurationPlatformCapabilities(registry, configuration); err != nil {
		return fmt.Errorf("register configuration platform: %w", err)
	}
	if err := automation.RegisterExecutionPlatformCapabilities(registry, execution); err != nil {
		return fmt.Errorf("register execution platform: %w", err)
	}
	if err := automation.RegisterCollaborationPlatformCapabilities(registry, collaboration); err != nil {
		return fmt.Errorf("register collaboration platform: %w", err)
	}
	if err := validatePlatformCapabilityInventory(registry); err != nil {
		return err
	}
	a.platformMu.Lock()
	a.platformCapabilities = registry
	a.platformServices = &PlatformApplicationServices{
		Configuration: configuration,
		Execution:     execution,
		Collaboration: collaboration,
	}
	a.platformMu.Unlock()
	if binder, ok := any(services.HookHandler).(interface{ BindPlatformAPI(*API) }); ok {
		binder.BindPlatformAPI(a)
	}
	if decryptor, ok := any(services.SessionManager).(interface {
		SetSecretDecryptor(func(string, string) (string, error))
	}); ok {
		decryptor.SetSecretDecryptor(services.Encryptor.Decrypt)
	}
	services.ConfigSync.SetSecretEncryptor(services.Encryptor)
	return nil
}

func (a *API) clearPlatformComposition() {
	if a == nil {
		return
	}
	a.platformMu.Lock()
	a.platformCapabilities = nil
	a.platformServices = nil
	a.platformMu.Unlock()
}

// platformApplicationServices is intentionally package-private: HTTP handlers
// can converge on the same services without exposing mutable composition state
// to transports or external packages.
func (a *API) platformApplicationServices() (*PlatformApplicationServices, bool) {
	if a == nil {
		return nil, false
	}
	a.platformMu.RLock()
	defer a.platformMu.RUnlock()
	if a.platformCapabilities == nil || a.platformServices == nil {
		return nil, false
	}
	copy := *a.platformServices
	return &copy, true
}

func (a *API) PlatformAutomationReadiness() PlatformAutomationReadiness {
	if a == nil {
		return PlatformAutomationReadiness{}
	}
	a.platformMu.RLock()
	registry := a.platformCapabilities
	hasServices := a.platformServices != nil
	a.platformMu.RUnlock()
	if registry == nil || !hasServices {
		return PlatformAutomationReadiness{}
	}
	descriptors := registry.ListForActor(automation.Actor{})
	mutations := 0
	for _, descriptor := range descriptors {
		if descriptor.Mutation {
			mutations++
		}
	}
	readiness := PlatformAutomationReadiness{
		Total: len(descriptors), Mutations: mutations, Reads: len(descriptors) - mutations,
	}
	readiness.Ready = readiness.Total == expectedPlatformCapabilities &&
		readiness.Mutations == expectedPlatformMutations && readiness.Reads == expectedPlatformReads
	return readiness
}

func validatePlatformServices(services PlatformServices) error {
	switch {
	case services.DB == nil:
		return errors.New("platform DB is required")
	case services.Hub == nil:
		return errors.New("platform hub is required")
	case services.SessionManager == nil:
		return errors.New("platform session manager is required")
	case services.ConfigSync == nil:
		return errors.New("platform config syncer is required")
	case services.Encryptor == nil:
		return errors.New("platform encryptor is required")
	case services.HookHandler == nil:
		return errors.New("platform hook handler is required")
	case services.FileHandler == nil || services.GitHandler == nil || services.VoiceHandler == nil:
		return errors.New("platform file, git, and voice handlers are required")
	case services.StructuredView == nil:
		return errors.New("platform structured view is required")
	case services.Updater == nil:
		return errors.New("platform updater is required")
	case services.AIHandler == nil:
		return errors.New("platform AI handler is required")
	case services.Notifications == nil:
		return errors.New("platform notification service is required")
	case services.WebPush == nil:
		return errors.New("platform web push service is required")
	case services.ReinitializeAI == nil:
		return errors.New("platform AI reinitializer is required")
	default:
		return nil
	}
}

func validatePlatformCapabilityInventory(registry *automation.PlatformCapabilityRegistry) error {
	descriptors := registry.ListForActor(automation.Actor{})
	reads := 0
	names := make(map[application.CapabilityName]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if _, duplicate := names[descriptor.Name]; duplicate {
			return fmt.Errorf("duplicate platform capability %s", descriptor.Name)
		}
		names[descriptor.Name] = struct{}{}
		if !descriptor.Mutation {
			reads++
		}
	}
	mutations := len(descriptors) - reads
	if len(descriptors) != expectedPlatformCapabilities || reads != expectedPlatformReads || mutations != expectedPlatformMutations {
		return fmt.Errorf("platform capability inventory mismatch: total=%d reads=%d mutations=%d", len(descriptors), reads, mutations)
	}
	return nil
}

type platformAIReinitializer struct{ callback func() }

func (r platformAIReinitializer) ReinitializeAI(context.Context) {
	if r.callback != nil {
		r.callback()
	}
}
