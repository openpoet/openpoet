package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"devmanager/internal/config"
	"devmanager/internal/database"
	"devmanager/internal/handlers"
	"devmanager/internal/llm"
	"devmanager/internal/configsync"
	"devmanager/internal/mcp"
	"devmanager/internal/notifications"
	"devmanager/internal/security"
	"devmanager/internal/session"
	"devmanager/internal/voice"
	"devmanager/internal/websocket"
	"devmanager/web"
)

// BuildVersion is injected at build time via -ldflags
var BuildVersion = "dev"

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
	// as "nested sessions" when DevManager itself was launched from Claude Code.
	os.Unsetenv("CLAUDECODE")
	os.Unsetenv("CLAUDE_CODE_ENTRYPOINT")

	// Handle mcp-serve subcommand before flag parsing
	if len(os.Args) > 1 && os.Args[1] == "mcp-serve" {
		apiURL := os.Getenv("DEVMANAGER_API_URL")
		if apiURL == "" {
			apiURL = "http://localhost:8080"
		}
		mcp.Serve(apiURL)
		return
	}

	// Parse command line flags
	bind := flag.String("bind", "", "Address to bind (default: 0.0.0.0)")
	port := flag.Int("port", 0, "Port to listen on (default: 8080)")
	dbPath := flag.String("db", "", "Database path (default: devmanager.db)")
	openaiKey := flag.String("openai-key", "", "OpenAI API key for voice transcription")
	mcpHTTP := flag.Bool("mcp-http", false, "Enable MCP HTTP endpoint at /mcp")
	flag.Parse()

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

	// Clean up stale sessions (server restart means all in-memory sessions are lost)
	ctx := context.Background()
	sessions, err := db.ListSessions(ctx)
	if err == nil {
		for _, sess := range sessions {
			if sess.Status == "running" || sess.Status == "starting" {
				log.Printf("Cleaning up stale session: %s (status: %s)", sess.ID, sess.Status)
				db.EndSession(ctx, sess.ID, "stopped")
			}
		}
	}

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

	// Initialize other handlers
	fileHandler := handlers.NewFileHandler(api)
	voiceHandler := handlers.NewVoiceHandler(api, func() (voice.ProviderType, string) {
		// Get provider type from settings, default to openai
		providerSetting, _ := db.GetSetting(context.Background(), "whisper_provider")
		if providerSetting == "" {
			providerSetting = "openai"
		}
		provider := voice.ProviderType(providerSetting)

		// Get API key based on provider
		var key string
		switch provider {
		case voice.ProviderOpenAI:
			if k, err := db.GetSetting(context.Background(), "openai_api_key"); err == nil && k != "" {
				key = k
			} else {
				key = cfg.OpenAIKey
			}
		case voice.ProviderGroq:
			if k, err := db.GetSetting(context.Background(), "groq_api_key"); err == nil && k != "" {
				key = k
			} else {
				key = cfg.GroqKey
			}
		}

		return provider, key
	})
	wsHandler := handlers.NewWebSocketHandler(hub, api, webpush)

	// Wire hub into config syncer for live progress
	configSync.SetHub(hub)

	// Initialize AI provider
	aiProvider := initAIProvider(db, fmt.Sprintf("http://localhost:%d", cfg.Port))
	aiHandler := handlers.NewAIHandler(api, aiProvider)

	// Wire AI handler into API for AI chat functionality
	api.SetAIHandler(aiHandler)

	// Wire tool executor into SDK providers (lazy binding — provider created before handler)
	if goSDK, ok := aiProvider.(*llm.GoSDKProvider); ok {
		goSDK.SetToolExecutor(aiHandler)
	}

	// Initialize OTEL handler for Claude Code token tracking
	otelHandler := handlers.NewOTELHandler(db)

	// Wire AI evaluation callbacks into session manager
	sessionMgr.OnSessionStart = func(sessionID string) {
		log.Printf("[AI-Session] >>> OnSessionStart callback fired for session %s", sessionID[:8])
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
		aiHandler.EvaluateSession(context.Background(), sessionID, "session_end", output)
	}

	// Wire AI evaluation callback into hook handler for mid-session triggers
	hookHandler.OnEvaluateSession = func(sessionID string, trigger string, outputSnapshot []byte) {
		aiHandler.EvaluateSession(context.Background(), sessionID, trigger, outputSnapshot)
	}

	// Wire activity tracking callback into hook handler
	hookHandler.OnActivityTouch = func(sessionID string) {
		db.TouchSessionActivity(context.Background(), sessionID)
	}

	// Wire OTEL handler into API for live session metrics
	api.SetOTELHandler(otelHandler)

	// Determine if MCP HTTP endpoint should be enabled
	mcpHTTPEnabled := *mcpHTTP
	if !mcpHTTPEnabled {
		if v, _ := db.GetSetting(ctx, "mcp_http_enabled"); v == "true" {
			mcpHTTPEnabled = true
		}
	}

	// Initialize MCP HTTP handler if enabled
	var mcpHandler *mcp.HTTPHandler
	if mcpHTTPEnabled {
		mcpHandler = mcp.NewHTTPHandler(
			fmt.Sprintf("http://localhost:%d", cfg.Port),
			func() string { key, _ := db.GetSetting(context.Background(), "mcp_api_key"); return key },
			func() mcp.ToolPolicy {
				policyStr, _ := db.GetSetting(context.Background(), "mcp_tool_policy_http")
				return mcp.ParsePolicy(policyStr)
			},
		)
		log.Printf("MCP HTTP endpoint enabled at /mcp")
	}

	// Set up router
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(middleware.RealIP)

	// DEBUG: Log static file requests with Content-Type and User-Agent
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

	// MCP HTTP endpoint (if enabled)
	if mcpHandler != nil {
		r.Handle("/mcp", mcpHandler)
	}

	// API routes
	// DEBUG: Client error reporting endpoint
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

	r.Route("/api", func(r chi.Router) {
		// Version
		r.Get("/version", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-cache")
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

		// Global Tasks (cross-project)
		r.Get("/tasks/session-summary", api.GetAllTaskSessionSummary)
		r.Get("/tasks", api.ListAllTasks)
		r.Put("/tasks/reorder", api.ReorderAllTasks)

		r.Get("/projects/{id}/files", fileHandler.ListProjectFiles)
		r.Get("/projects/{id}/files/view/*", fileHandler.ViewProjectFile)

		// Sessions
		r.Get("/sessions", api.ListSessions)
		r.Get("/sessions/active-details", api.GetActiveSessionDetails)
		r.Post("/sessions", api.CreateSession)
		r.Get("/sessions/{id}", api.GetSession)
		r.Get("/sessions/{id}/output", api.GetSessionOutput)
		r.Delete("/sessions/{id}", api.DeleteSession)
		r.Post("/sessions/{id}/reopen", api.ReopenSession)

		// Session input
		r.Post("/sessions/{id}/input", api.SendSessionInput)
		r.Get("/sessions/{id}/tools", api.GetResolvedSessionTools)

		// Session-Task integration
		r.Get("/sessions/{id}/task", api.GetSessionLinkedTask)
		r.Post("/sessions/{id}/link-task", api.LinkSessionTask)
		r.Post("/sessions/{id}/evaluate", api.TriggerSessionEvaluation)
		r.Post("/sessions/{id}/suggest-task-data", aiHandler.HandleSuggestTaskData)

		// Session files
		r.Get("/sessions/{id}/files", fileHandler.ListFiles)
		r.Get("/sessions/{id}/files/view/*", fileHandler.ViewFile)
		r.Get("/sessions/{id}/files/*", fileHandler.DownloadFile)
		r.Post("/sessions/{id}/files", fileHandler.UploadFiles)
		r.Post("/sessions/{id}/files/paste", fileHandler.PasteImage)


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
	})

	// Test-only endpoints (only available when DEVMANAGER_TEST_MODE=1)
	if os.Getenv("DEVMANAGER_TEST_MODE") == "1" {
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

	// Static files and SPA - use web.FS from embed
	webFS := web.FS

	// Serve static files
	staticFS, _ := fs.Sub(webFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Serve service worker with version injection
	r.Get("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-cache")
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
		w.Header().Set("Cache-Control", "no-cache")
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

	// Create server
	// WriteTimeout is 600s to support long-polling hook permission requests (up to 590s)
	server := &http.Server{
		Addr:         cfg.Address(),
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("DevManager starting on http://%s", cfg.Address())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")

	// Close AI provider sessions (SDK providers may have persistent subprocesses)
	if sp, ok := aiProvider.(llm.SessionProvider); ok {
		sp.CloseAllSessions()
	}

	// Stop all sessions
	sessionMgr.StopAll()

	// Stop WebSocket hub
	hub.Stop()

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	fmt.Println("DevManager stopped")
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

	go func() {
		log.Printf("[DB] Serving error page on http://%s", cfg.Address())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[DB] Error server failed: %v", err)
		}
	}()

	<-ready

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

// newGoSDKWithBudget creates a GoSDKProvider with optional budget from settings.
func newGoSDKWithBudget(db *database.DB, apiURL string) *llm.GoSDKProvider {
	p := llm.NewGoSDKProvider(apiURL)
	if budgetStr, _ := db.GetSetting(context.Background(), "ai_budget_usd"); budgetStr != "" {
		var budget float64
		if _, err := fmt.Sscanf(budgetStr, "%f", &budget); err == nil && budget > 0 {
			p.SetBudgetUSD(budget)
			log.Printf("[AI] Budget limit: $%.2f per query", budget)
		}
	}
	return p
}

// initAIProvider creates the appropriate LLM provider based on settings.
func initAIProvider(db *database.DB, apiURL string) llm.Provider {
	ctx := context.Background()

	// Check provider setting
	providerType, _ := db.GetSetting(ctx, "ai_provider")

	switch providerType {
	case "apikey":
		apiKey, _ := db.GetSetting(ctx, "anthropic_api_key")
		if apiKey != "" {
			log.Printf("[AI] Using Anthropic API key provider")
			return llm.NewAnthropicProvider(apiKey)
		}
		log.Printf("[AI] Provider set to apikey but no API key configured")
		return nil

	case "gosdk":
		if llm.IsClaudeCLIAvailable() {
			log.Printf("[AI] Using Go Agent SDK provider (streaming, session-based)")
			return newGoSDKWithBudget(db, apiURL)
		}
		log.Printf("[AI] Provider set to gosdk but Claude CLI not available")
		return nil

	case "ollama":
		ollamaURL, _ := db.GetSetting(ctx, "ollama_base_url")
		ollamaKey, _ := db.GetSetting(ctx, "ollama_api_key")
		ollamaModel, _ := db.GetSetting(ctx, "ollama_model")
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}
		if ollamaModel == "" {
			ollamaModel = "qwen3-coder"
		}
		log.Printf("[AI] Using Ollama provider at %s with model %s", ollamaURL, ollamaModel)
		return llm.NewOllamaProvider(ollamaURL, ollamaKey, ollamaModel)

	case "nodesdk":
		log.Printf("[AI] Using Node.js Agent SDK sidecar provider")
		return llm.NewNodeSDKProvider(apiURL)

	case "claudecode":
		// Legacy: redirect to gosdk
		if llm.IsClaudeCLIAvailable() {
			log.Printf("[AI] Legacy claudecode provider redirected to Go Agent SDK")
			return newGoSDKWithBudget(db, apiURL)
		}
		log.Printf("[AI] Provider set to claudecode but CLI not available")
		return nil

	default:
		// Auto-detect: try Go SDK (Claude CLI) first, then API key
		if llm.IsClaudeCLIAvailable() {
			log.Printf("[AI] Auto-detected Go Agent SDK provider (streaming, session-based)")
			return newGoSDKWithBudget(db, apiURL)
		}
		apiKey, _ := db.GetSetting(ctx, "anthropic_api_key")
		if apiKey != "" {
			log.Printf("[AI] Auto-detected Anthropic API key provider")
			return llm.NewAnthropicProvider(apiKey)
		}
		log.Printf("[AI] No AI provider available (no Claude CLI, no API key)")
		return nil
	}
}

// recoverDataFromBackup copies data from an old DB backup into the fresh DB.
func recoverDataFromBackup(db *database.DB, backupPath string) {
	tables := []string{
		"projects", "sessions", "skills",
		"mcp_servers", "settings", "push_subscriptions", "notifications",
		"ai_conversations", "ai_messages", "memory_docs",
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
			notifService.Send(ctx, "", "warning", title, body)
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
			notifService.Send(ctx, "", "info", title, body)
			db.MarkTaskDueNotified(ctx, task.ID)
		}
	}
}

const migrationErrorPageHTML = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>DevManager</title>
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
  <h1>A aplicação não pôde iniciar</h1>
  <p>Ocorreu um problema interno. Verifique os logs para mais detalhes.</p>
  <button onclick="doRetry(this)">Tentar Novamente</button>
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
