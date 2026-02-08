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
	"devmanager/internal/macro"
	"devmanager/internal/notifications"
	"devmanager/internal/security"
	"devmanager/internal/session"
	"devmanager/internal/voice"
	"devmanager/internal/websocket"
	"devmanager/web"
)

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
	// Parse command line flags
	bind := flag.String("bind", "", "Address to bind (default: 0.0.0.0)")
	port := flag.Int("port", 0, "Port to listen on (default: 8080)")
	dbPath := flag.String("db", "", "Database path (default: devmanager.db)")
	openaiKey := flag.String("openai-key", "", "OpenAI API key for voice transcription")
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

	// Initialize database
	db, err := database.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
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

	// Initialize macro executor
	macroExec := macro.NewExecutor(db, hub, encryptor.Decrypt)

	// Initialize config syncer
	configSync := macro.NewConfigSyncer(db, encryptor.Decrypt)

	// Initialize built-in macros
	if err := macro.InitBuiltinMacros(context.Background(), db); err != nil {
		log.Printf("Warning: Failed to initialize built-in macros: %v", err)
	}

	// Initialize web push service
	webpush, err := notifications.NewWebPushService(db, cfg.VAPIDEmail)
	if err != nil {
		log.Printf("Warning: Failed to initialize web push service: %v", err)
	}

	// Initialize notification service
	notifService := notifications.NewService(db, hub, webpush)

	// Initialize API handlers
	api := handlers.NewAPI(db, hub, sessionMgr, macroExec, configSync, encryptor, notifService)

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
	hookHandler := handlers.NewHookHandler(hub, notifService, sessionMgr)
	wsHandler := handlers.NewWebSocketHandler(hub, api, webpush)

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
		// Projects
		r.Get("/projects", api.ListProjects)
		r.Post("/projects", api.CreateProject)
		r.Get("/projects/{id}", api.GetProject)
		r.Put("/projects/{id}", api.UpdateProject)
		r.Delete("/projects/{id}", api.DeleteProject)
		r.Post("/projects/{id}/validate", api.ValidateProject)
		r.Post("/projects/{id}/sync-config", api.SyncProjectConfig)

		// Sessions
		r.Get("/sessions", api.ListSessions)
		r.Post("/sessions", api.CreateSession)
		r.Get("/sessions/{id}", api.GetSession)
		r.Get("/sessions/{id}/output", api.GetSessionOutput)
		r.Delete("/sessions/{id}", api.DeleteSession)

		// Session files
		r.Get("/sessions/{id}/files", fileHandler.ListFiles)
		r.Get("/sessions/{id}/files/view/*", fileHandler.ViewFile)
		r.Get("/sessions/{id}/files/*", fileHandler.DownloadFile)
		r.Post("/sessions/{id}/files", fileHandler.UploadFiles)
		r.Post("/sessions/{id}/files/paste", fileHandler.PasteImage)


		// Macros
		r.Get("/macros", api.ListMacros)
		r.Post("/macros", api.CreateMacro)
		r.Get("/macros/{id}", api.GetMacro)
		r.Put("/macros/{id}", api.UpdateMacro)
		r.Delete("/macros/{id}", api.DeleteMacro)
		r.Post("/macros/{id}/run", api.RunMacro)

		// Config - Skills
		r.Get("/config/skills", api.ListSkills)
		r.Post("/config/skills", api.CreateSkill)
		r.Get("/config/skills/{id}", api.GetSkill)
		r.Put("/config/skills/{id}", api.UpdateSkill)
		r.Delete("/config/skills/{id}", api.DeleteSkill)

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

		// Voice
		r.Post("/voice/transcribe", voiceHandler.Transcribe)

		// Notifications
		r.Get("/notifications", api.GetNotifications)
		r.Put("/notifications/{id}/read", api.MarkNotificationRead)
		r.Post("/notifications/subscribe", wsHandler.HandlePushSubscribe)
		r.Delete("/notifications/subscribe", wsHandler.HandlePushUnsubscribe)
		r.Get("/notifications/vapid", wsHandler.HandleVAPIDPublicKey)

		// Hooks
		r.Post("/hooks/permission", hookHandler.HandlePermission)
		r.Post("/hooks/permission/{sessionId}/respond", hookHandler.HandlePermissionRespond)
		r.Post("/hooks/event", hookHandler.HandleEvent)
	})

	// WebSocket routes
	r.Get("/ws/session/{id}", wsHandler.HandleSessionWS)
	r.Get("/ws/macro/{execution_id}", wsHandler.HandleMacroWS)
	r.Get("/ws/events", wsHandler.HandleEventsWS)

	// Static files and SPA - use web.FS from embed
	webFS := web.FS

	// Serve static files
	staticFS, _ := fs.Sub(webFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Serve service worker and manifest
	r.Get("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		data, _ := fs.ReadFile(webFS, "sw.js")
		w.Write(data)
	})

	r.Get("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := fs.ReadFile(webFS, "manifest.json")
		w.Write(data)
	})

	// Serve index.html for all other routes (SPA)
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		data, err := fs.ReadFile(webFS, "templates/index.html")
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		w.Write(data)
	})

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
