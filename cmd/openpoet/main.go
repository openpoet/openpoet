package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"openpoet/internal/benchmark"
	"openpoet/internal/config"
	"openpoet/internal/configsync"
	"openpoet/internal/database"
	"openpoet/internal/handlers"
	"openpoet/internal/llm"
	"openpoet/internal/mcp"
	"openpoet/internal/notifications"
	"openpoet/internal/security"
	"openpoet/internal/session"
	"openpoet/internal/tunnel"
	"openpoet/internal/updater"
	"openpoet/internal/voice"
	"openpoet/internal/websocket"
	"openpoet/web"
)

// Build-time variables injected via -ldflags
var BuildVersion = "dev"
var DefaultRelayURL = ""   // e.g., "wss://tunnel-connect.openpoet.ai/"
var DebugDefault = "false" // Overridden to "true" via ldflags in dev builds

// debugResponseWriter wraps http.ResponseWriter to capture status and size for logging
type debugResponseWriter struct {
	http.ResponseWriter
	path         string
	statusCode   int
	bytesWritten int
	wroteHeader  bool
}

func (dw *debugResponseWriter) WriteHeader(code int) {
	dw.statusCode = code
	dw.wroteHeader = true
	dw.ResponseWriter.WriteHeader(code)
}

func (dw *debugResponseWriter) Write(b []byte) (int, error) {
	if !dw.wroteHeader {
		dw.statusCode = 200
		dw.wroteHeader = true
	}
	// Log first 100 bytes of response for JS/CSS files to detect if HTML is being served
	if dw.bytesWritten == 0 && (strings.HasSuffix(dw.path, ".js") || strings.HasSuffix(dw.path, ".css") || strings.Contains(dw.path, ".js?") || strings.Contains(dw.path, ".css?")) {
		snippet := string(b)
		if len(snippet) > 100 {
			snippet = snippet[:100]
		}
		log.Printf("[DEBUG-BODY] %s first100=%q", dw.path, snippet)
	}
	n, err := dw.ResponseWriter.Write(b)
	dw.bytesWritten += n
	return n, err
}

func main() {
	// Strip Claude Code marker env vars so Go SDK subprocesses are not rejected
	// as "nested sessions" when OpenPoet itself was launched from Claude Code.
	os.Unsetenv("CLAUDECODE")
	os.Unsetenv("CLAUDE_CODE_ENTRYPOINT")

	// Propagate build version to the llm package for sidecar version tracking
	llm.BuildVersion = BuildVersion

	// Handle version subcommand before flag parsing
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("openpoet %s\n", BuildVersion)
		os.Exit(0)
	}

	// Handle mcp-serve subcommand before flag parsing
	if len(os.Args) > 1 && os.Args[1] == "mcp-serve" {
		// Parse CLI args for session-id and api-url.
		// CLI args are more reliable than env vars because not all MCP clients
		// pass the "env" field from the MCP config to subprocesses.
		for i := 2; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "--session-id":
				if i+1 < len(os.Args) {
					os.Setenv("OPENPOET_SESSION_ID", os.Args[i+1])
					i++
				}
			case "--api-url":
				if i+1 < len(os.Args) {
					os.Setenv("OPENPOET_API_URL", os.Args[i+1])
					i++
				}
			}
		}
		apiURL := os.Getenv("OPENPOET_API_URL")
		if apiURL == "" {
			apiURL = "http://localhost:8080"
		}
		mcp.Serve(apiURL)
		return
	}

	// Handle cli subcommand before flag parsing — exposes MCP tools via bash
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		mcp.RunCLI(os.Args[2:])
		return
	}

	// Handle benchmark subcommand before flag parsing
	if len(os.Args) > 1 && os.Args[1] == "benchmark" {
		benchmark.RunCLI(os.Args[2:])
		return
	}

	// Parse command line flags
	bind := flag.String("bind", "", "Address to bind (default: localhost, use 0.0.0.0 for all interfaces)")
	port := flag.Int("port", 0, "Port to listen on (default: 8080)")
	dbPath := flag.String("db", "", "Database path (default: openpoet.db)")
	openaiKey := flag.String("openai-key", "", "OpenAI API key for voice transcription")
	mcpHTTP := flag.Bool("mcp-http", false, "Enable MCP HTTP endpoint at /mcp")
	debugFlag := flag.Bool("debug", DebugDefault == "true", "Enable debug logging")
	flag.Parse()

	if *debugFlag {
		log.Printf("Debug mode enabled (pass -debug=false to disable)")
	}

	// Load configuration
	cfg := config.Load()

	// Override with command line flags
	if *bind != "" {
		cfg.Bind = *bind
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	if *openaiKey != "" {
		cfg.OpenAIKey = *openaiKey
	}

	// Initialize database (with silent recovery on migration failure)
	db, err := initDatabase(cfg.DBPath)
	if err != nil {
		// Unrecoverable: serve error page and wait for user action
		log.Printf("[DB] Unrecoverable error: %v", err)
		for {
			serveMigrationError(cfg)
			log.Printf("[DB] User requested retry...")
			db, err = initDatabase(cfg.DBPath)
			if err == nil {
				break
			}
			log.Printf("[DB] Retry failed: %v", err)
		}
	}
	defer db.Close()

	// Collect active sessions for auto-restore after server is fully initialized
	ctx := context.Background()
	sessionsToRestore, _ := db.ListActiveSessions(ctx)

	// Clean up stale streaming AI messages (server restart means in-flight streams are lost)
	if err := db.FixStaleStreamingMessages(ctx); err != nil {
		log.Printf("Warning: failed to fix stale streaming messages: %v", err)
	}

	// Initialize encryptor
	encryptor, err := security.NewEncryptor(cfg.EncryptKey)
	if err != nil {
		log.Fatalf("Failed to initialize encryptor: %v", err)
	}

	// Initialize WebSocket hub
	hub := websocket.NewHub()
	go hub.Run()

	// Initialize session manager
	sessionMgr := session.NewManager(db, hub, cfg.Address())

	// Initialize config syncer
	configSync := configsync.NewConfigSyncer(db, encryptor.Decrypt, cfg.Address())

	// Initialize web push service
	webpush, err := notifications.NewWebPushService(db, cfg.VAPIDEmail)
	if err != nil {
		log.Printf("Warning: Failed to initialize web push service: %v", err)
	}

	// Initialize notification service
	notifService := notifications.NewService(db, hub, webpush)

	// Initialize hook handler (before API so it can be passed as dependency)
	hookHandler := handlers.NewHookHandler(hub, notifService, sessionMgr)

	// Initialize API handlers
	api := handlers.NewAPI(db, hub, sessionMgr, configSync, encryptor, notifService, hookHandler)

	// Initialize binary auto-updater
	appUpdater := updater.New(BuildVersion)
	api.SetUpdater(appUpdater)

	// Initialize structured view handler (JSONL event browser)
	svHandler := handlers.NewStructuredViewHandler(db, hub, api.DecryptFunc())
	api.SetStructuredView(svHandler)

	// Initialize other handlers
	fileHandler := handlers.NewFileHandler(api)
	voiceHandler := handlers.NewVoiceHandler(api, func() (voice.ProviderType, string, string) {
		// Get provider type from settings, default to openai
		providerSetting, _ := db.GetSetting(context.Background(), "whisper_provider")
		if providerSetting == "" {
			providerSetting = "openai"
		}
		provider := voice.ProviderType(providerSetting)

		// Get model from settings (empty = use default for provider)
		model, _ := db.GetSetting(context.Background(), "whisper_model")

		// Get API key based on provider (decrypt from DB)
		var key string
		switch provider {
		case voice.ProviderOpenAI:
			key = decryptSetting(db, encryptor, "openai_api_key")
			if key == "" {
				key = cfg.OpenAIKey
			}
		case voice.ProviderGroq:
			key = decryptSetting(db, encryptor, "groq_api_key")
			if key == "" {
				key = cfg.GroqKey
			}
		}

		return provider, key, model
	})
	wsHandler := handlers.NewWebSocketHandler(hub, api, webpush)

	// Wire hub into config syncer for live progress
	configSync.SetHub(hub)

	// Initialize AI provider manager (per-slot providers)
	apiURL := fmt.Sprintf("http://localhost:%d", cfg.Port)
	providerMgr := initProviderManager(db, encryptor, apiURL)
	aiHandler := handlers.NewAIHandler(api, providerMgr)

	// Wire AI handler into API for AI chat functionality
	api.SetAIHandler(aiHandler)

	// Wire tool executor into SDK providers (lazy binding — provider created before handler)
	providerMgr.SetToolExecutor(aiHandler)

	// Wire AI provider reinitialization callbacks
	api.ReinitAIProviders = func() {
		newMgr := initProviderManager(db, encryptor, apiURL)
		newMgr.SetToolExecutor(aiHandler)
		aiHandler.SetProviderManager(newMgr)
		providerMgr = newMgr
		log.Printf("[AI] All providers reinitialized after config change")
	}
	// Legacy callback — also reinit providers when flat settings change
	api.ReinitAIProvider = api.ReinitAIProviders

	// Initialize tunnel remote access
	jwtSecret := loadOrGenerateJWTSecret(db)
	pairingMgr := tunnel.NewPairingManager(db, encryptor, []byte(jwtSecret))
	api.SetPairingManager(pairingMgr)

	// Wire build-time relay URL into handlers
	handlers.DefaultRelayURL = DefaultRelayURL

	api.SetTunnelDeps(&handlers.TunnelDeps{
		DB:         db,
		Encryptor:  encryptor,
		Hub:        hub,
		Port:       cfg.Port,
		PairingMgr: pairingMgr,
	})

	// Initialize OTEL handler for Claude Code token tracking
	otelHandler := handlers.NewOTELHandler(db)

	// Wire AI evaluation callbacks into session manager
	sessionMgr.OnSessionStart = func(sessionID string) {
		log.Printf("[AI-Session] >>> OnSessionStart callback fired for session %s", sessionID[:8])
		// Record task history if session is linked to a task
		if task, err := db.GetTaskForSession(context.Background(), sessionID); err == nil && task != nil {
			api.RecordTaskHistory(context.Background(), task.ID, task.ProjectID, "session_started", map[string]interface{}{
				"session_id": sessionID,
			}, "system", sessionID)
		}
		aiHandler.EvaluateSession(context.Background(), sessionID, "session_start", nil)
	}
	sessionMgr.OnSessionFlush = func(sessionID string) {
		log.Printf("[OTEL] >>> OnSessionFlush callback fired for session %s", sessionID[:8])
		otelHandler.FlushSession(sessionID)
		// Expire all notifications for this session and clean up hook state
		go notifService.MarkSessionRead(context.Background(), sessionID)
		hookHandler.ClearSession(sessionID)
	}
	sessionMgr.OnSessionEnd = func(sessionID string, output []byte) {
		log.Printf("[AI-Session] >>> OnSessionEnd callback fired for session %s (outputLen=%d)", sessionID[:8], len(output))
		// Stop structured view watcher for this session
		svHandler.StopSessionWatcher(sessionID)
		// Record basic session_ended history (AI may enrich with summary later)
		if task, err := db.GetTaskForSession(context.Background(), sessionID); err == nil && task != nil {
			api.RecordTaskHistory(context.Background(), task.ID, task.ProjectID, "session_ended", map[string]interface{}{
				"session_id": sessionID,
			}, "system", sessionID)
		}
		aiHandler.EvaluateSession(context.Background(), sessionID, "session_end", output)
	}

	// Wire AI evaluation callback into hook handler for mid-session triggers
	hookHandler.OnEvaluateSession = func(sessionID string, trigger string, outputSnapshot []byte) bool {
		return aiHandler.EvaluateSession(context.Background(), sessionID, trigger, outputSnapshot)
	}

	// Wire task/suggestion guard callbacks for debounced evaluation
	hookHandler.HasLinkedTask = func(sessionID string) bool {
		task, err := db.GetTaskForSession(context.Background(), sessionID)
		return err == nil && task != nil
	}
	hookHandler.HasRecentSuggestions = func(sessionID string) bool {
		since := time.Now().Add(-3 * time.Minute)
		has, err := db.HasRecentAISuggestions(context.Background(), sessionID, since)
		return err == nil && has
	}

	// Wire activity tracking callback into hook handler
	hookHandler.OnActivityTouch = func(sessionID string) {
		db.TouchSessionActivity(context.Background(), sessionID)
	}

	// Wire plan persistence callback into hook handler
	hookHandler.OnPlanUpdated = func(sessionID string, planContent string) {
		ctx := context.Background()

		// Read old plan before overwriting (for history tracking)
		oldPlan, _, _ := db.GetSessionPlan(ctx, sessionID)
		oldPlanLen := len(oldPlan)

		if err := db.UpdateSessionPlan(ctx, sessionID, planContent); err != nil {
			log.Printf("[hooks] Failed to save plan for session %s: %v", sessionID, err)
			return
		}
		log.Printf("[hooks] Plan saved for session %s (%d bytes)", sessionID[:8], len(planContent))
		hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeSessionPlanUpdated,
			Data: map[string]interface{}{"session_id": sessionID},
		})

		// Record plan_updated event in task history if session is linked to a task
		if task, err := db.GetTaskForSession(ctx, sessionID); err == nil && task != nil {
			api.RecordTaskHistory(ctx, task.ID, task.ProjectID, "plan_updated", map[string]interface{}{
				"plan_length":     len(planContent),
				"old_plan_length": oldPlanLen,
				"is_rewrite":      oldPlanLen > 0,
			}, "system", sessionID)
		}
	}

	// Wire OTEL handler into API for live session metrics
	api.SetOTELHandler(otelHandler)

	// Initialize MCP HTTP handler (always enabled — auth middleware protects it).
	// The --mcp-http flag and mcp_http_enabled setting are kept for backward compatibility.
	mcpHandler := mcp.NewHTTPHandler(
		fmt.Sprintf("http://localhost:%d", cfg.Port),
		func() string { key, _ := db.GetSetting(context.Background(), "mcp_api_key"); return key },
		func() mcp.ToolPolicy {
			policyStr, _ := db.GetSetting(context.Background(), "mcp_tool_policy_http")
			return mcp.ParsePolicy(policyStr)
		},
	)
	_ = *mcpHTTP // kept for backward compatibility

	// Set up router
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(middleware.RealIP)

	// DEBUG: Log static file requests with Content-Type and User-Agent
	if *debugFlag {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.RequestURI()
				if strings.HasPrefix(path, "/static/") || path == "/sw.js" || path == "/" || path == "/manifest.json" {
					ua := r.Header.Get("User-Agent")
					log.Printf("[DEBUG-REQ] %s %s UA=%s", r.Method, path, ua)

					// Wrap response writer to capture Content-Type
					dw := &debugResponseWriter{ResponseWriter: w, path: path}
					next.ServeHTTP(dw, r)

					ct := w.Header().Get("Content-Type")
					log.Printf("[DEBUG-RES] %s Content-Type=%s Status=%d Size=%d", path, ct, dw.statusCode, dw.bytesWritten)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
	}

	// CORS for development
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	// Tunnel auth middleware (only activates for tunnel-originated requests)
	r.Use(tunnel.AuthMiddleware(db, []byte(jwtSecret)))

	// MCP HTTP endpoint (always registered, auth-protected)
	r.Handle("/mcp", mcpHandler)

	// API routes
	// DEBUG: Client error reporting endpoint
	if *debugFlag {
		r.Post("/api/debug/client-error", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Error string `json:"error"`
				UA    string `json:"ua"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", 400)
				return
			}
			log.Printf("[CLIENT-ERROR] UA=%s\n%s", body.UA, body.Error)
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		})
	}

	r.Route("/api", func(r chi.Router) {
		// Version
		r.Get("/version", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(map[string]string{"version": BuildVersion})
		})

		// Projects
		r.Get("/projects", api.ListProjects)
		r.Post("/projects", api.CreateProject)
		r.Get("/projects/{id}", api.GetProject)
		r.Put("/projects/{id}", api.UpdateProject)
		r.Delete("/projects/{id}", api.DeleteProject)
		r.Post("/projects/{id}/duplicate", api.DuplicateProject)
		r.Post("/projects/{id}/validate", api.ValidateProject)
		r.Post("/projects/{id}/sync-config", api.SyncProjectConfig)
		r.Get("/projects/{id}/memory-doc", api.GetMemoryDoc)
		r.Put("/projects/{id}/memory-doc", api.UpdateMemoryDoc)
		// Memory Doc approval (AI-proposed edits)
		r.Post("/memory-doc/propose", api.ProposeMemoryDoc)
		r.Post("/memory-doc/approve/{docId}", api.ApproveMemoryDoc)
		r.Post("/memory-doc/reject/{docId}", api.RejectMemoryDoc)
		// Task proposal approval (single task create/update)
		r.Post("/task-proposal/approve/{docId}", api.ApproveTaskProposal)
		r.Post("/task-proposal/reject/{docId}", api.RejectTaskProposal)
		// Skill proposal approval (create/update project skill)
		r.Post("/skill-proposal/approve/{docId}", api.ApproveSkillProposal)
		r.Post("/skill-proposal/reject/{docId}", api.RejectSkillProposal)
		// Project Tasks
		r.Get("/projects/{id}/tasks", api.ListProjectTasks)
		r.Post("/projects/{id}/tasks", api.CreateProjectTask)
		r.Put("/projects/{id}/tasks/reorder", api.ReorderProjectTasks)
		r.Get("/projects/{id}/tasks/session-summary", api.GetTaskSessionSummary)
		r.Get("/projects/{id}/tasks/{taskId}", api.GetProjectTask)
		r.Put("/projects/{id}/tasks/{taskId}", api.UpdateProjectTask)
		r.Patch("/projects/{id}/tasks/{taskId}/status", api.UpdateTaskStatus)
		r.Delete("/projects/{id}/tasks/{taskId}", api.DeleteProjectTask)
		r.Post("/projects/{id}/tasks/{taskId}/duplicate", api.DuplicateProjectTask)
		r.Get("/projects/{id}/tasks/{taskId}/sessions", api.ListTaskSessions)
		r.Get("/projects/{id}/tasks/{taskId}/history", api.ListTaskHistory)
		r.Post("/projects/{id}/tasks/{taskId}/history", api.AddTaskComment)
		r.Post("/projects/{id}/tasks/{taskId}/approve", api.ApproveTaskVerification)
		r.Post("/projects/{id}/tasks/{taskId}/reject", api.RejectTaskVerification)
		r.Get("/projects/{id}/tasks/{taskId}/documents", api.ListTaskDocuments)

		// Global Tasks (cross-project)
		r.Get("/tasks/session-summary", api.GetAllTaskSessionSummary)
		r.Get("/tasks", api.ListAllTasks)
		r.Put("/tasks/reorder", api.ReorderAllTasks)

		// Directory browser (for project creation/editing)
		r.Get("/browse", api.BrowseDirectory)
		r.Post("/browse/remote", api.BrowseRemoteDirectory)

		r.Get("/projects/{id}/files", fileHandler.ListProjectFiles)
		r.Get("/projects/{id}/files/view/*", fileHandler.ViewProjectFile)
		r.Post("/projects/{id}/files/write", fileHandler.WriteProjectFile)
		r.Get("/projects/{id}/files/raw/*", fileHandler.DownloadProjectFile)
		r.Post("/projects/{id}/files/raw", fileHandler.UploadProjectFile)

		// Sessions
		r.Get("/sessions", api.ListSessions)
		r.Get("/sessions/active-details", api.GetActiveSessionDetails)
		r.Post("/sessions", api.CreateSession)
		r.Get("/sessions/{id}", api.GetSession)
		r.Get("/sessions/{id}/output", api.GetSessionOutput)
		r.Get("/sessions/{id}/events", api.GetSessionEvents)
		r.Post("/sessions/{id}/events/watch", api.StartWatchingSessionEvents)
		r.Delete("/sessions/{id}/events/watch", api.StopWatchingSessionEvents)
		r.Get("/sessions/{id}/plan", api.GetSessionPlan)
		r.Get("/sessions/{id}/documents", api.ListSessionDocuments)
		r.Delete("/sessions/{id}", api.DeleteSession)
		r.Post("/sessions/{id}/reopen", api.ReopenSession)

		// Session input
		r.Post("/sessions/{id}/input", api.SendSessionInput)
		r.Get("/sessions/{id}/tools", api.GetResolvedSessionTools)

		// Session-Task integration
		r.Get("/sessions/{id}/task", api.GetSessionLinkedTask)
		r.Post("/sessions/{id}/link-task", api.LinkSessionTask)
		r.Post("/sessions/{id}/unlink-task", api.UnlinkSessionTask)
		r.Post("/sessions/{id}/evaluate", api.TriggerSessionEvaluation)
		r.Post("/sessions/{id}/suggest-task-data", aiHandler.HandleSuggestTaskData)

		// Session files
		r.Get("/sessions/{id}/files", fileHandler.ListFiles)
		r.Get("/sessions/{id}/files/view/*", fileHandler.ViewFile)
		r.Get("/sessions/{id}/files/*", fileHandler.DownloadFile)
		r.Post("/sessions/{id}/files", fileHandler.UploadFiles)
		r.Post("/sessions/{id}/files/paste", fileHandler.PasteImage)
		r.Post("/sessions/{id}/image-prompt-hint", hookHandler.HandleImagePromptHint)

		// Config - Skills
		r.Get("/config/skills", api.ListSkills)
		r.Post("/config/skills", api.CreateSkill)
		r.Get("/config/skills/export", api.ExportSkills)
		r.Post("/config/skills/import", api.ImportSkills)
		r.Get("/config/skills/{id}", api.GetSkill)
		r.Put("/config/skills/{id}", api.UpdateSkill)
		r.Delete("/config/skills/{id}", api.DeleteSkill)
		r.Post("/config/skills/{id}/duplicate", api.DuplicateSkill)
		r.Get("/config/skills/{id}/versions", api.ListSkillVersions)
		r.Post("/config/skills/{id}/versions/{versionId}/restore", api.RestoreSkillVersion)

		// Config - MCP Servers
		r.Get("/config/mcps", api.ListMCPServers)
		r.Post("/config/mcps", api.CreateMCPServer)
		r.Get("/config/mcps/{id}", api.GetMCPServer)
		r.Put("/config/mcps/{id}", api.UpdateMCPServer)
		r.Delete("/config/mcps/{id}", api.DeleteMCPServer)

		// Config - AI Configs
		r.Get("/config/ai-configs", api.ListAIConfigs)
		r.Post("/config/ai-configs", api.CreateAIConfig)
		r.Put("/config/ai-configs/{id}", api.UpdateAIConfig)
		r.Delete("/config/ai-configs/{id}", api.DeleteAIConfig)
		r.Get("/config/ai-config-assignments", api.GetAIConfigAssignments)
		r.Put("/config/ai-config-assignments", api.UpdateAIConfigAssignments)

		// Config - Settings
		r.Get("/config/settings", api.GetSettings)
		r.Put("/config/settings", api.UpdateSettings)
		r.Post("/config/sync-all", api.SyncAllConfig)

		// Config - MCP Tools list (all available tool names per context)
		r.Get("/config/mcp-tools", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.URL.Query().Get("context")
			type toolInfo struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			var result []toolInfo
			if ctx == "chat" {
				for _, t := range llm.ChatTools() {
					result = append(result, toolInfo{Name: t.Name, Description: t.Description})
				}
			} else {
				for _, t := range mcp.AllTools() {
					result = append(result, toolInfo{Name: t.Name, Description: t.Description})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
		})

		// Config - MCP API Key
		r.Post("/config/mcp-api-key/generate", api.GenerateMCPAPIKey)
		r.Get("/config/mcp-api-key/status", api.GetMCPAPIKeyStatus)
		r.Delete("/config/mcp-api-key", api.RevokeMCPAPIKey)

		// Config - Tool Policies
		r.Get("/config/tool-policies", api.GetToolPolicies)
		r.Put("/config/tool-policies", api.UpdateToolPolicies)

		// Project Tool Policy
		r.Get("/projects/{id}/tool-policy", api.GetProjectToolPolicy)
		r.Put("/projects/{id}/tool-policy", api.UpdateProjectToolPolicy)
		r.Get("/projects/{id}/tools", api.GetResolvedProjectTools)

		// Project Shares
		r.Get("/projects/{id}/shares", api.GetProjectShares)
		r.Put("/projects/{id}/shares", api.UpdateProjectShares)

		// Project Skills
		r.Get("/projects/{id}/skills", api.GetProjectSkills)
		r.Put("/projects/{id}/skill-config", api.SaveProjectSkillConfig)
		r.Post("/projects/{id}/skills", api.CreateProjectSkillHandler)
		r.Put("/projects/{id}/skills/{skillId}", api.UpdateProjectSkillHandler)
		r.Delete("/projects/{id}/skills/{skillId}", api.DeleteProjectSkillHandler)

		// Project MCP Servers
		r.Get("/projects/{id}/mcp-servers", api.GetProjectMCPServers)
		r.Get("/projects/{id}/mcp-servers/{mcpId}", api.GetProjectMCPServerHandler)
		r.Post("/projects/{id}/mcp-servers", api.CreateProjectMCPServerHandler)
		r.Put("/projects/{id}/mcp-servers/{mcpId}", api.UpdateProjectMCPServerHandler)
		r.Delete("/projects/{id}/mcp-servers/{mcpId}", api.DeleteProjectMCPServerHandler)

		// Voice
		r.Post("/voice/transcribe", voiceHandler.Transcribe)

		// Notifications
		r.Get("/notifications", api.GetNotifications)
		r.Get("/notifications/active", api.GetActiveNotifications)
		r.Get("/notifications/unread-count", api.GetNotificationUnreadCount)
		r.Put("/notifications/{id}/read", api.MarkNotificationRead)
		r.Put("/notifications/read-all", api.MarkAllNotificationsRead)
		r.Post("/notifications/subscribe", wsHandler.HandlePushSubscribe)
		r.Delete("/notifications/subscribe", wsHandler.HandlePushUnsubscribe)
		r.Get("/notifications/vapid", wsHandler.HandleVAPIDPublicKey)
		r.Post("/notifications/test-push", wsHandler.HandleTestPush)
		r.Get("/notifications/preference", wsHandler.HandleNotificationPreference)
		r.Put("/notifications/preference", wsHandler.HandleSetNotificationPreference)

		// Hooks
		r.Post("/hooks/permission", hookHandler.HandlePermission)
		r.Post("/hooks/permission/{sessionId}/respond", hookHandler.HandlePermissionRespond)
		r.Post("/hooks/event", hookHandler.HandleEvent)
		r.Get("/hooks/pending/{sessionId}", hookHandler.HandleGetPending)
		r.Post("/hooks/task-notification/{sessionId}/respond", hookHandler.HandleTaskNotificationRespond)

		// Temp Documents
		r.Post("/documents", api.CreateTempDocument)
		r.Get("/documents/{id}", api.GetTempDocument)

		// AI
		r.Get("/ai/status", aiHandler.HandleStatus)
		r.Post("/ai/test-connection", aiHandler.HandleTestConnection)
		r.Post("/ai/chat", aiHandler.HandleChat)
		r.Get("/ai/conversations", aiHandler.HandleListConversations)
		r.Delete("/ai/conversations", aiHandler.HandleDeleteAllConversations)
		r.Get("/ai/conversations/{id}", aiHandler.HandleGetConversation)
		r.Delete("/ai/conversations/{id}", aiHandler.HandleDeleteConversation)
		r.Post("/ai/conversations/{id}/stop", aiHandler.HandleStopChat)
		r.Get("/ai/active-stream", aiHandler.HandleActiveStream)
		r.Get("/ai/conversations/{id}/stream", aiHandler.HandleStreamReconnect)
		r.Post("/ai/initiate-memory-doc-edit", aiHandler.HandleInitiateMemoryDocEdit)
		r.Post("/ai/initiate-task-creation", aiHandler.HandleInitiateTaskCreation)
		r.Post("/ai/initiate-task-discussion", aiHandler.HandleInitiateTaskDiscussion)
		r.Post("/ai/initiate-skill-customization", aiHandler.HandleInitiateSkillCustomization)
		r.Post("/ai/generate-skill", aiHandler.HandleGenerateSkill)
		r.Post("/ai/validate-skill", aiHandler.HandleValidateSkill)
		r.Post("/ai/execute-tool", aiHandler.HandleExecuteTool)
		r.Get("/ai/tool-definitions", aiHandler.HandleToolDefinitions)

		// AI Suggestions
		r.Get("/ai/suggestions", aiHandler.ListSuggestions)
		r.Post("/ai/suggestions/{id}/accept", aiHandler.AcceptSuggestion)
		r.Post("/ai/suggestions/{id}/dismiss", aiHandler.DismissSuggestion)
		r.Post("/ai/suggestions/{id}/discuss", aiHandler.HandleDiscussSuggestion)

		// AI Proactive
		r.Post("/ai/conversations/{id}/read", aiHandler.HandleMarkRead)
		r.Get("/ai/unread-count", aiHandler.HandleUnreadCount)
		r.Post("/ai/test-proactive", aiHandler.HandleTestProactive)

		// Token usage
		r.Get("/token-usage/summary", api.GetTokenUsageSummary)
		r.Delete("/token-usage", api.ClearTokenUsage)

		// Binary Auto-Update
		r.Get("/update/check", api.CheckUpdate)
		r.Post("/update/apply", api.ApplyUpdate)

		// Remote Access / Tunnel
		r.Get("/tunnel/status", api.GetTunnelStatus)
		r.Post("/tunnel/enable", api.EnableTunnel)
		r.Post("/tunnel/disable", api.DisableTunnel)
		r.Get("/tunnel/devices", api.ListPairedDevices)
		r.Delete("/tunnel/devices/{id}", api.RevokePairedDevice)
		r.Delete("/tunnel/devices/{id}/permanent", api.DeletePairedDevice)
		r.Post("/tunnel/pair-confirm", api.ConfirmPairing)
	})

	// Test-only endpoints (only available when OPENPOET_TEST_MODE=1)
	if os.Getenv("OPENPOET_TEST_MODE") == "1" {
		log.Printf("[TEST] Test mode enabled — test endpoints available")
		r.Post("/api/test/seed-token-usage", api.SeedTokenUsage)
	}

	// OTLP endpoints (standard paths for OpenTelemetry HTTP/JSON)
	r.Post("/v1/metrics", otelHandler.HandleMetrics)
	r.Post("/v1/traces", otelHandler.HandleTraces)
	r.Post("/v1/logs", otelHandler.HandleLogs)

	// WebSocket routes
	r.Get("/ws/session/{id}", wsHandler.HandleSessionWS)
	r.Get("/ws/events", wsHandler.HandleEventsWS)

	// Tunnel pairing routes (outside /api, exempt from auth)
	r.Get("/pair", pairingMgr.HandlePairingPage)
	r.Get("/pair/status", pairingMgr.HandlePairingStatus)

	// Static files and SPA - use web.FS from embed
	webFS := web.FS

	// Serve static files with proper cache headers
	staticFS, _ := fs.Sub(webFS, "static")
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))
	r.Handle("/static/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			// Versioned assets (?v=hash) — content-addressed, cache indefinitely
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Non-versioned (vendor libs, images) — short cache with revalidation
			w.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate")
		}
		staticHandler.ServeHTTP(w, r)
	}))

	// Serve service worker with version injection
	r.Get("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-store")
		data, _ := fs.ReadFile(webFS, "sw.js")
		content := strings.ReplaceAll(string(data), "__BUILD_VERSION__", BuildVersion)
		w.Write([]byte(content))
	})

	r.Get("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := fs.ReadFile(webFS, "manifest.json")
		w.Write(data)
	})

	// Serve index.html with version injection for all SPA routes
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "no-store")
		data, err := fs.ReadFile(webFS, "templates/index.html")
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		content := strings.ReplaceAll(string(data), "__BUILD_VERSION__", BuildVersion)
		w.Write([]byte(content))
	})

	// Start task due date checker goroutine
	go runTaskDueChecker(db, notifService, hub)

	// Start binary update checker goroutine
	go runUpdateChecker(db, appUpdater, hub)

	// Create server
	// WriteTimeout is 600s to support long-polling hook permission requests (up to 590s)
	server := &http.Server{
		Addr:         cfg.Address(),
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Check for port conflicts before binding.
	// macOS allows 0.0.0.0 and 127.0.0.1 to coexist on the same port,
	// which silently shadows traffic. Probe the port to detect this.
	probeAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	if conn, err := net.DialTimeout("tcp", probeAddr, 200*time.Millisecond); err == nil {
		conn.Close()
		log.Fatalf("Port %d is already in use by another process", cfg.Port)
	}

	// Bind listener on the main goroutine so port conflicts fail immediately
	ln, err := net.Listen("tcp", cfg.Address())
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", cfg.Address(), err)
	}
	log.Printf("OpenPoet starting on http://%s", cfg.Address())

	// Serve in background
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Auto-connect tunnel if previously enabled (non-blocking)
	go func() {
		time.Sleep(2 * time.Second) // let server start first
		if enabled, _ := db.GetSetting(context.Background(), "tunnel_enabled"); enabled == "true" {
			log.Printf("[TUNNEL] Auto-connecting (previously enabled)")
			api.AutoConnectTunnel()
		}
	}()

	// Auto-restore previously active sessions (non-blocking)
	if len(sessionsToRestore) > 0 {
		go func() {
			time.Sleep(2 * time.Second) // let server start first
			log.Printf("[AutoRestore] Restoring %d active session(s) from before restart...", len(sessionsToRestore))
			restoreCtx := context.Background()
			restored := 0
			for _, sess := range sessionsToRestore {
				sess := sess // capture loop var
				if err := api.AutoRestoreSession(restoreCtx, &sess); err != nil {
					log.Printf("[AutoRestore] Failed to restore session %s: %v", sess.ID, err)
				} else {
					restored++
					log.Printf("[AutoRestore] Session %s restored successfully", sess.ID)
				}
			}
			log.Printf("[AutoRestore] Done: %d/%d sessions restored", restored, len(sessionsToRestore))
		}()
	}

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")

	// Disconnect tunnel
	api.DisconnectTunnel()

	// Close AI provider sessions (SDK providers may have persistent subprocesses)
	providerMgr.CloseAll()

	// Stop structured view watchers
	svHandler.StopAllWatchers()

	// Stop all sessions (preserve DB state for auto-restore on next startup)
	sessionMgr.StopAllForRestart()

	// Stop WebSocket hub
	hub.Stop()

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	fmt.Println("OpenPoet stopped")
}

// initDatabase tries to open/migrate the database. On failure, it silently
// attempts recovery: backs up the old DB, creates a fresh one, and copies data over.
func initDatabase(dbPath string) (*database.DB, error) {
	db, err := database.New(dbPath)
	if err == nil {
		return db, nil
	}

	log.Printf("[DB] Migration failed: %v — attempting silent recovery...", err)

	// Silent recovery: backup old DB, create fresh, recover data
	backupPath := dbPath + ".bak." + time.Now().Format("20060102-150405")
	if renameErr := os.Rename(dbPath, backupPath); renameErr != nil {
		log.Printf("[DB] Recovery: could not backup DB: %v", renameErr)
		// Try removing instead
		if rmErr := os.Remove(dbPath); rmErr != nil {
			return nil, fmt.Errorf("migration failed and recovery impossible: %w", err)
		}
		backupPath = ""
	}

	// Create fresh DB
	db, freshErr := database.New(dbPath)
	if freshErr != nil {
		// Restore backup if possible
		if backupPath != "" {
			os.Rename(backupPath, dbPath)
		}
		return nil, fmt.Errorf("migration failed and fresh install also failed: %w", freshErr)
	}

	// Recover data from backup
	if backupPath != "" {
		recoverDataFromBackup(db, backupPath)
	}

	log.Printf("[DB] Silent recovery successful")
	return db, nil
}

// serveMigrationError starts a minimal HTTP server showing a generic error page.
// Blocks until the user clicks "Tentar Novamente".
func serveMigrationError(cfg *config.Config) {
	ready := make(chan struct{}, 1)

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(migrationErrorPageHTML))
	})

	mux.HandleFunc("/api/migration/retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		select {
		case ready <- struct{}{}:
		default:
		}
	})

	server := &http.Server{
		Addr:    cfg.Address(),
		Handler: mux,
	}

	errLn, listenErr := net.Listen("tcp", cfg.Address())
	if listenErr != nil {
		log.Fatalf("[DB] Failed to listen on %s: %v", cfg.Address(), listenErr)
	}
	log.Printf("[DB] Serving error page on http://%s", cfg.Address())
	go func() {
		if err := server.Serve(errLn); err != nil && err != http.ErrServerClosed {
			log.Printf("[DB] Error server failed: %v", err)
		}
	}()

	<-ready

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

// loadOrGenerateJWTSecret loads the tunnel JWT secret from DB or generates a new one.
func loadOrGenerateJWTSecret(db *database.DB) string {
	jwtSecret, _ := db.GetSetting(context.Background(), "tunnel_jwt_secret")
	if jwtSecret == "" {
		generatedKey, err := security.GenerateKey()
		if err != nil {
			log.Printf("[TUNNEL] Failed to generate JWT secret: %v", err)
			return ""
		}
		jwtSecret = generatedKey
		db.SetSetting(context.Background(), "tunnel_jwt_secret", jwtSecret)
	}
	return jwtSecret
}

// decryptSetting reads an encrypted setting, auto-migrating plaintext values.
func decryptSetting(db *database.DB, enc *security.Encryptor, key string) string {
	ctx := context.Background()
	value, err := db.GetSetting(ctx, key)
	if err != nil || value == "" {
		return ""
	}

	iv, ivErr := db.GetSetting(ctx, key+"_iv")
	if ivErr != nil || iv == "" {
		// Legacy plaintext — migrate it
		encrypted, newIV, encErr := enc.Encrypt(value)
		if encErr != nil {
			return value // degraded: return plaintext
		}
		_ = db.SetSetting(ctx, key, encrypted)
		_ = db.SetSetting(ctx, key+"_iv", newIV)
		n := 8
		if len(value) < 6 {
			_ = db.SetSetting(ctx, key+"_preview", "***")
		} else {
			if len(value) < n {
				n = len(value) / 2
			}
			_ = db.SetSetting(ctx, key+"_preview", value[:n]+"...")
		}
		return value
	}

	plaintext, err := enc.Decrypt(value, iv)
	if err != nil {
		log.Printf("[Settings] Failed to decrypt %s: %v", key, err)
		return ""
	}
	return plaintext
}

// initProviderManager creates a ProviderManager with per-slot configs from the database.
func initProviderManager(db *database.DB, enc *security.Encryptor, apiURL string) *llm.ProviderManager {
	pm := llm.NewProviderManager(apiURL)
	ctx := context.Background()

	for _, slot := range []llm.Slot{llm.SlotChat, llm.SlotBackground, llm.SlotSession} {
		cfg, err := db.GetAIConfigForSlot(ctx, string(slot))
		if err != nil || cfg == nil {
			log.Printf("[AI] Slot %s: no config assigned (auto-detect)", slot)
			continue
		}

		// Decrypt the API key
		apiKey := ""
		if cfg.APIKeyEncrypted != "" && cfg.APIKeyIV != "" {
			decrypted, decErr := enc.Decrypt(cfg.APIKeyEncrypted, cfg.APIKeyIV)
			if decErr != nil {
				log.Printf("[AI] Slot %s: failed to decrypt API key for config %q: %v", slot, cfg.Name, decErr)
			} else {
				apiKey = decrypted
			}
		}

		pm.SetSlotConfig(slot, &llm.ProviderConfig{
			ProviderType: cfg.ProviderType,
			APIKey:       apiKey,
			Model:        cfg.Model,
			BaseURL:      cfg.BaseURL,
			ExtraJSON:    cfg.ExtraJSON,
		})
		log.Printf("[AI] Slot %s: config=%q type=%s model=%s", slot, cfg.Name, cfg.ProviderType, cfg.Model)
	}

	return pm
}

// recoverDataFromBackup copies data from an old DB backup into the fresh DB.
func recoverDataFromBackup(db *database.DB, backupPath string) {
	tables := []string{
		"projects", "sessions", "skills",
		"mcp_servers", "settings", "push_subscriptions", "notifications",
		"ai_conversations", "ai_messages", "memory_docs", "ai_configs", "ai_config_assignments",
		"temp_documents", "project_tasks",
	}

	_, err := db.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS old_db", backupPath))
	if err != nil {
		log.Printf("[DB] Recovery: could not attach backup: %v", err)
		return
	}
	defer db.Exec("DETACH DATABASE old_db")

	for _, table := range tables {
		result, err := db.Exec(fmt.Sprintf("INSERT OR IGNORE INTO main.%s SELECT * FROM old_db.%s", table, table))
		if err != nil {
			log.Printf("[DB] Recovery: table %s skipped (%v)", table, err)
			continue
		}
		if rows, _ := result.RowsAffected(); rows > 0 {
			log.Printf("[DB] Recovery: table %s — %d rows recovered", table, rows)
		}
	}
}

// runTaskDueChecker runs every 60s checking for overdue and upcoming tasks.
func runTaskDueChecker(db *database.DB, notifService *notifications.Service, hub *websocket.Hub) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ctx := context.Background()

		// Check overdue tasks
		overdue, err := db.ListOverdueTasks(ctx)
		if err != nil {
			continue
		}
		for _, task := range overdue {
			// Get project name for notification
			project, err := db.GetProject(ctx, task.ProjectID)
			projectName := fmt.Sprintf("project %d", task.ProjectID)
			if err == nil {
				projectName = project.Name
			}

			title := fmt.Sprintf("Task Overdue: %s", task.Title)
			body := fmt.Sprintf("Task in %s is past due", projectName)
			notifService.Send(ctx, "", "warning", title, body, fmt.Sprintf("/app/project/%d", task.ProjectID))
			db.MarkTaskDueNotified(ctx, task.ID)
		}

		// Check upcoming tasks (due within 30 minutes)
		upcoming, err := db.ListUpcomingTasks(ctx, 30*time.Minute)
		if err != nil {
			continue
		}
		for _, task := range upcoming {
			project, err := db.GetProject(ctx, task.ProjectID)
			projectName := fmt.Sprintf("project %d", task.ProjectID)
			if err == nil {
				projectName = project.Name
			}

			title := fmt.Sprintf("Task Due Soon: %s", task.Title)
			body := fmt.Sprintf("Task in %s is due within 30 minutes", projectName)
			notifService.Send(ctx, "", "info", title, body, fmt.Sprintf("/app/project/%d", task.ProjectID))
			db.MarkTaskDueNotified(ctx, task.ID)
		}
	}
}

// runUpdateChecker periodically checks for binary updates via GitHub Releases.
// Always runs — not configurable. Notifies once per release version.
func runUpdateChecker(db *database.DB, u *updater.Updater, hub *websocket.Hub) {
	// Initial delay to let server start and avoid contention at boot
	time.Sleep(30 * time.Second)

	checkOnce := func() {
		if updater.IsDevBuild(u.CurrentVersion) {
			return
		}

		ctx := context.Background()
		status, err := u.CheckForUpdate(ctx)
		if err != nil {
			log.Printf("[Updater] Check failed: %v", err)
			return
		}

		db.SetSetting(ctx, "auto_update_last_check", time.Now().Format(time.RFC3339))

		if status.Available {
			// Only notify once per release version
			notifiedVersion, _ := db.GetSetting(ctx, "auto_update_notified_version")
			if notifiedVersion == status.LatestVersion {
				return // already notified for this version
			}

			db.SetSetting(ctx, "auto_update_notified_version", status.LatestVersion)
			hub.BroadcastStateUpdate("update", map[string]interface{}{
				"action":  "available",
				"version": status.LatestVersion,
			})
			log.Printf("[Updater] New version available: v%s", status.LatestVersion)
		}
	}

	checkOnce()

	ticker := time.NewTicker(updater.CheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		checkOnce()
	}
}

const migrationErrorPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OpenPoet</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    background: #1a1a2e; color: #e0e0e0;
    display: flex; justify-content: center; align-items: center;
    min-height: 100vh; padding: 20px;
  }
  .container {
    max-width: 420px; width: 100%%;
    background: #16213e; border-radius: 12px;
    padding: 32px; box-shadow: 0 4px 24px rgba(0,0,0,0.4);
    text-align: center;
  }
  .icon { font-size: 48px; margin-bottom: 16px; }
  h1 { font-size: 20px; margin-bottom: 12px; color: #e0e0e0; }
  p { color: #a0a0a0; font-size: 14px; margin-bottom: 24px; line-height: 1.5; }
  button {
    width: 100%%; padding: 14px; border: none; border-radius: 8px;
    font-size: 16px; font-weight: 600; cursor: pointer;
    background: #4361ee; color: white; transition: opacity 0.2s;
  }
  button:hover { opacity: 0.9; }
  button:disabled { opacity: 0.5; cursor: not-allowed; }
  .status { margin-top: 16px; font-size: 14px; display: none; }
  .spinner { display: inline-block; width: 16px; height: 16px;
    border: 2px solid #666; border-top-color: #fff; border-radius: 50%%;
    animation: spin 0.8s linear infinite; vertical-align: middle; margin-right: 8px;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
</style>
</head>
<body>
<div class="container">
  <div class="icon">&#9888;&#65039;</div>
  <h1>The application could not start</h1>
  <p>An internal problem occurred. Check the logs for more details.</p>
  <button onclick="doRetry(this)">Try Again</button>
  <div class="status" id="status"><span class="spinner"></span> Reiniciando...</div>
</div>
<script>
function doRetry(btn) {
  btn.disabled = true;
  document.getElementById('status').style.display = 'block';
  fetch('/api/migration/retry', { method: 'POST' })
    .then(function() { setTimeout(function() { window.location.reload(); }, 3000); })
    .catch(function() { btn.disabled = false; document.getElementById('status').style.display = 'none'; });
}
</script>
</body>
</html>`
