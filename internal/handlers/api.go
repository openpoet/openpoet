package handlers

import (
	"context"
	"database/sql"
	"devmanager/internal/database"
	"devmanager/internal/macro"
	"devmanager/internal/notifications"
	"devmanager/internal/security"
	"devmanager/internal/session"
	"devmanager/internal/websocket"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

type API struct {
	db           *database.DB
	hub          *websocket.Hub
	sessionMgr   *session.Manager
	macroExec    *macro.Executor
	configSync   *macro.ConfigSyncer
	encryptor    *security.Encryptor
	notifService *notifications.Service
	hookHandler  *HookHandler
}

func NewAPI(
	db *database.DB,
	hub *websocket.Hub,
	sessionMgr *session.Manager,
	macroExec *macro.Executor,
	configSync *macro.ConfigSyncer,
	encryptor *security.Encryptor,
	notifService *notifications.Service,
	hookHandler *HookHandler,
) *API {
	return &API{
		db:           db,
		hub:          hub,
		sessionMgr:   sessionMgr,
		macroExec:    macroExec,
		configSync:   configSync,
		encryptor:    encryptor,
		notifService: notifService,
		hookHandler:  hookHandler,
	}
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

// ============ Projects ============

func (a *API) ListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := a.db.ListProjects(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if projects == nil {
		projects = []database.Project{}
	}
	respondJSON(w, http.StatusOK, projects)
}

func (a *API) CreateProject(w http.ResponseWriter, r *http.Request) {
	var input database.ProjectInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	project := &database.Project{
		Name: input.Name,
		Path: input.Path,
		Type: input.Type,
	}

	if input.Type == "remote" {
		project.SSHHost = sql.NullString{String: input.SSHHost, Valid: input.SSHHost != ""}
		project.SSHPort = sql.NullInt64{Int64: int64(input.SSHPort), Valid: input.SSHPort > 0}
		project.SSHUser = sql.NullString{String: input.SSHUser, Valid: input.SSHUser != ""}
		project.SSHAuthType = sql.NullString{String: input.SSHAuthType, Valid: input.SSHAuthType != ""}

		if input.SSHCredential != "" {
			encrypted, iv, err := a.encryptor.Encrypt(input.SSHCredential)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "Failed to encrypt credentials")
				return
			}
			project.SSHCredentialEncrypted = sql.NullString{String: encrypted, Valid: true}
			project.SSHCredentialIV = sql.NullString{String: iv, Valid: true}
		}
	}

	if err := a.db.CreateProject(r.Context(), project); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project", map[string]interface{}{"action": "created", "project": project})
	respondJSON(w, http.StatusCreated, project)
}

func (a *API) GetProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	project, err := a.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	respondJSON(w, http.StatusOK, project)
}

func (a *API) UpdateProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	project, err := a.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	var input database.ProjectInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	project.Name = input.Name
	project.Path = input.Path
	project.Type = input.Type

	if input.Type == "remote" {
		project.SSHHost = sql.NullString{String: input.SSHHost, Valid: input.SSHHost != ""}
		project.SSHPort = sql.NullInt64{Int64: int64(input.SSHPort), Valid: input.SSHPort > 0}
		project.SSHUser = sql.NullString{String: input.SSHUser, Valid: input.SSHUser != ""}
		project.SSHAuthType = sql.NullString{String: input.SSHAuthType, Valid: input.SSHAuthType != ""}

		if input.SSHCredential != "" {
			encrypted, iv, err := a.encryptor.Encrypt(input.SSHCredential)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "Failed to encrypt credentials")
				return
			}
			project.SSHCredentialEncrypted = sql.NullString{String: encrypted, Valid: true}
			project.SSHCredentialIV = sql.NullString{String: iv, Valid: true}
		}
	}

	if err := a.db.UpdateProject(r.Context(), project); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project", map[string]interface{}{"action": "updated", "project": project})
	respondJSON(w, http.StatusOK, project)
}

func (a *API) DeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	if err := a.db.DeleteProject(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project", map[string]interface{}{"action": "deleted", "id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ValidateProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	project, err := a.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	if project.Type == "remote" {
		if err := session.ValidateConnection(project, a.encryptor.Decrypt); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) SyncProjectConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	project, err := a.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	if err := a.configSync.SyncToProject(r.Context(), project); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.db.UpdateProjectConfigSyncedAt(r.Context(), id)
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ============ Sessions ============

func (a *API) ListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.db.ListSessions(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []database.Session{}
	}
	respondJSON(w, http.StatusOK, sessions)
}

func (a *API) CreateSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID int64             `json:"project_id"`
		EnvVars   map[string]string `json:"env_vars,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	project, err := a.db.GetProject(r.Context(), input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	// Auto-sync config (hooks, skills, mcp) to project before starting session
	if a.configSync != nil {
		if syncErr := a.configSync.SyncToProject(r.Context(), project); syncErr != nil {
			log.Printf("Warning: config sync failed before session start: %v", syncErr)
		}
	}

	var sess *database.Session
	if project.Type == "local" {
		sess, err = a.sessionMgr.StartSession(r.Context(), project, input.EnvVars)
	} else {
		sess, err = a.sessionMgr.StartRemoteSession(r.Context(), project, input.EnvVars, a.encryptor.Decrypt)
	}

	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusCreated, sess)
}

func (a *API) GetSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	sess, err := a.db.GetSession(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	respondJSON(w, http.StatusOK, sess)
}

// GetSessionOutput returns the buffered terminal output for a session
func (a *API) GetSessionOutput(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	output, err := a.sessionMgr.GetSessionOutput(id)
	if err != nil {
		// Session might not be running (already ended) - return empty
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(output)
}

func (a *API) DeleteSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Mark as user-initiated stop so the Stop hook doesn't send a push notification
	if a.hookHandler != nil {
		a.hookHandler.MarkUserStopped(id)
	}

	// Try to stop the running session
	if err := a.sessionMgr.StopSession(r.Context(), id); err != nil {
		// Session might not be running in memory, that's OK
	}

	// Always update DB to mark session as stopped
	if err := a.db.EndSession(r.Context(), id, "stopped"); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastSessionStatus(id, "stopped")
	w.WriteHeader(http.StatusNoContent)
}

// ============ Macros ============

func (a *API) ListMacros(w http.ResponseWriter, r *http.Request) {
	macros, err := a.db.ListMacros(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if macros == nil {
		macros = []database.Macro{}
	}
	respondJSON(w, http.StatusOK, macros)
}

func (a *API) CreateMacro(w http.ResponseWriter, r *http.Request) {
	var m database.Macro
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	m.IsBuiltin = false
	if err := a.db.CreateMacro(r.Context(), &m); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("macro", map[string]interface{}{"action": "created", "macro": m})
	respondJSON(w, http.StatusCreated, m)
}

func (a *API) GetMacro(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid macro ID")
		return
	}

	m, err := a.db.GetMacro(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Macro not found")
		return
	}

	respondJSON(w, http.StatusOK, m)
}

func (a *API) UpdateMacro(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid macro ID")
		return
	}

	existing, err := a.db.GetMacro(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Macro not found")
		return
	}

	if existing.IsBuiltin {
		respondError(w, http.StatusForbidden, "Cannot modify built-in macros")
		return
	}

	var m database.Macro
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	m.ID = id
	m.IsBuiltin = false
	if err := a.db.UpdateMacro(r.Context(), &m); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("macro", map[string]interface{}{"action": "updated", "macro": m})
	respondJSON(w, http.StatusOK, m)
}

func (a *API) DeleteMacro(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid macro ID")
		return
	}

	if err := a.db.DeleteMacro(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("macro", map[string]interface{}{"action": "deleted", "id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) RunMacro(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid macro ID")
		return
	}

	var input struct {
		ProjectID int64 `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	m, err := a.db.GetMacro(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Macro not found")
		return
	}

	project, err := a.db.GetProject(r.Context(), input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	execID, err := a.macroExec.Run(r.Context(), m, project)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"execution_id": execID})
}

// ============ Skills ============

func (a *API) ListSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := a.db.ListSkills(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skills == nil {
		skills = []database.Skill{}
	}
	respondJSON(w, http.StatusOK, skills)
}

func (a *API) CreateSkill(w http.ResponseWriter, r *http.Request) {
	var s database.Skill
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := a.db.CreateSkill(r.Context(), &s); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "created", "skill": s})
	respondJSON(w, http.StatusCreated, s)
}

func (a *API) GetSkill(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	s, err := a.db.GetSkill(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Skill not found")
		return
	}

	respondJSON(w, http.StatusOK, s)
}

func (a *API) UpdateSkill(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	var s database.Skill
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	s.ID = id
	if err := a.db.UpdateSkill(r.Context(), &s); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "updated", "skill": s})
	respondJSON(w, http.StatusOK, s)
}

func (a *API) DeleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	if err := a.db.DeleteSkill(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "deleted", "id": id})
	w.WriteHeader(http.StatusNoContent)
}

// ============ MCP Servers ============

func (a *API) ListMCPServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.db.ListMCPServers(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if servers == nil {
		servers = []database.MCPServer{}
	}
	respondJSON(w, http.StatusOK, servers)
}

func (a *API) CreateMCPServer(w http.ResponseWriter, r *http.Request) {
	var m database.MCPServer
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := a.db.CreateMCPServer(r.Context(), &m); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("mcp", map[string]interface{}{"action": "created", "mcp": m})
	respondJSON(w, http.StatusCreated, m)
}

func (a *API) GetMCPServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	m, err := a.db.GetMCPServer(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "MCP server not found")
		return
	}

	respondJSON(w, http.StatusOK, m)
}

func (a *API) UpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	var m database.MCPServer
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	m.ID = id
	if err := a.db.UpdateMCPServer(r.Context(), &m); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("mcp", map[string]interface{}{"action": "updated", "mcp": m})
	respondJSON(w, http.StatusOK, m)
}

func (a *API) DeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	if err := a.db.DeleteMCPServer(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("mcp", map[string]interface{}{"action": "deleted", "id": id})
	w.WriteHeader(http.StatusNoContent)
}

// ============ Settings ============

func (a *API) GetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := a.db.GetAllSettings(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Remove sensitive settings
	delete(settings, "vapid_private_key")
	delete(settings, "openai_api_key")
	delete(settings, "groq_api_key")

	respondJSON(w, http.StatusOK, settings)
}

func (a *API) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var settings map[string]string
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	for key, value := range settings {
		if err := a.db.SetSetting(r.Context(), key, value); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	a.hub.BroadcastStateUpdate("settings", map[string]interface{}{"action": "updated"})
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) SyncAllConfig(w http.ResponseWriter, r *http.Request) {
	if err := a.configSync.SyncAllProjects(r.Context()); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ============ Notifications ============

func (a *API) GetNotifications(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			limit = parsed
		}
	}

	notifications, err := a.notifService.GetRecent(r.Context(), limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, notifications)
}

func (a *API) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid notification ID")
		return
	}

	if err := a.notifService.MarkRead(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// DecryptFunc returns the decrypt function for use by other components
func (a *API) DecryptFunc() func(string, string) (string, error) {
	return a.encryptor.Decrypt
}

// GetDB returns the database for use by other components
func (a *API) GetDB() *database.DB {
	return a.db
}

// Context helper for API
func (a *API) Context() context.Context {
	return context.Background()
}
