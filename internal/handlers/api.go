package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/configsync"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/mcp"
	"openpoet/internal/notifications"
	"openpoet/internal/providerbridge"
	"openpoet/internal/security"
	"openpoet/internal/session"
	"openpoet/internal/tunnel"
	"openpoet/internal/updater"
	"openpoet/internal/websocket"

	"github.com/go-chi/chi/v5"
)

// pendingMemoryDoc holds a proposed memory doc edit awaiting user approval.
type pendingMemoryDoc struct {
	ProjectID int64
	Content   string
	Summary   string
}

// PlanningTaskAction represents a single task action in a planning proposal.
type PlanningTaskAction struct {
	Action      string                 `json:"action"` // "create", "update", "delete"
	ProjectID   int64                  `json:"project_id"`
	TaskID      int64                  `json:"task_id,omitempty"`
	Title       string                 `json:"title"`
	Description string                 `json:"description,omitempty"`
	Status      string                 `json:"status,omitempty"`
	Priority    string                 `json:"priority,omitempty"`
	DueDate     string                 `json:"due_date,omitempty"`
	ParentID    int64                  `json:"parent_id,omitempty"`
	ParentRef   int                    `json:"parent_ref,omitempty"` // 1-based index referencing another task in the same batch
	SortOrder   int                    `json:"sort_order,omitempty"`
	Extra       map[string]interface{} `json:"extra,omitempty"` // for update fields
}

// sensitiveSettingKeys lists setting keys that must be stored encrypted.
var sensitiveSettingKeys = map[string]bool{
	"anthropic_api_key":  true,
	"openai_api_key":     true,
	"groq_api_key":       true,
	"ollama_api_key":     true,
	"tunnel_relay_token": true,
	"tunnel_jwt_secret":  true,
}

// aiProviderSettingKeys lists settings that require AI provider reinitialization when changed.
var aiProviderSettingKeys = map[string]bool{
	"ai_provider":       true,
	"anthropic_api_key": true,
	"ollama_api_key":    true,
	"ollama_base_url":   true,
	"ollama_model":      true,
	"ai_model":          true,
}

// apiKeyPreview returns the first N visible characters of a key + "..."
func apiKeyPreview(key string) string {
	if len(key) < 6 {
		return "***"
	}
	n := 8
	if len(key) < n {
		n = len(key) / 2
	}
	return key[:n] + "..."
}

// pendingTaskProposal holds a task proposal awaiting user approval.
type pendingTaskProposal struct {
	Actions []PlanningTaskAction
	Summary string
}

// pendingToolProposal holds a custom tool execution proposal awaiting user approval.
type pendingToolProposal struct {
	Action         PlanningTaskAction
	ConversationID int64
}

// pendingSkillProposal holds a skill proposal awaiting user approval.
type pendingSkillProposal struct {
	ProjectID int64  `json:"project_id"`
	SkillName string `json:"skill_name"`
	Content   string `json:"content"`
	Category  string `json:"category"`
	Action    string `json:"action"`             // "create" or "update"
	SkillID   int64  `json:"skill_id,omitempty"` // for updates
}

type API struct {
	db                   *database.DB
	hub                  *websocket.Hub
	sessionMgr           *session.Manager
	configSync           *configsync.ConfigSyncer
	encryptor            *security.Encryptor
	notifService         *notifications.Service
	hookHandler          *HookHandler
	aiHandler            *AIHandler
	otelHandler          *OTELHandler
	taskService          *application.ProjectTaskService
	workRunService       *application.WorkRunService
	capabilities         *application.CapabilityRegistry
	platformMu           sync.RWMutex
	platformCapabilities *automation.PlatformCapabilityRegistry
	workspaceService     *application.WorkspaceService
	platformServices     *PlatformApplicationServices
	providerBridge       *providerbridge.Manager

	// ReinitAIProvider is called when legacy AI settings change (kept for backward compat).
	ReinitAIProvider func()
	// ReinitAIProviders is called when AI configs/assignments change to reinitialize all providers.
	ReinitAIProviders func()

	pendingMemoryDocsMu sync.Mutex
	pendingMemoryDocs   map[string]*pendingMemoryDoc // docID -> pending edit

	pendingTaskProposalsMu sync.Mutex
	pendingTaskProposals   map[string]*pendingTaskProposal // docID -> pending task

	pendingSkillProposalsMu sync.Mutex
	pendingSkillProposals   map[string]*pendingSkillProposal // docID -> pending skill

	pendingToolProposalsMu sync.Mutex
	pendingToolProposals   map[string]*pendingToolProposal // docID -> pending tool execution

	// Binary auto-updater
	updater *updater.Updater

	// Structured view (JSONL event browser)
	structuredView *StructuredViewHandler

	// Tunnel client for remote access (dynamically created/destroyed)
	tunnelMu     sync.Mutex
	tunnelClient *tunnel.Client
	tunnelDeps   *TunnelDeps
	pairingMgr   *tunnel.PairingManager
}

type startSessionInput struct {
	ProjectID                  int64
	TaskID                     *int64
	EnvVars                    map[string]string
	DangerouslySkipPermissions bool
	AutoStartTaskPrompt        bool
	PlanningMode               bool
	CustomPrompt               string
}

type startSessionError struct {
	status  int
	message string
}

func (e *startSessionError) Error() string {
	return e.message
}

func newStartSessionError(status int, message string) error {
	return &startSessionError{status: status, message: message}
}

type sessionHistoryRequest struct {
	Mode          string
	Query         string
	Regex         bool
	CaseSensitive bool
	Lines         int
	Offset        int
	Limit         int
	ContextLines  int
	MaxChars      int
}

type sessionHistoryResponse struct {
	SessionID     string `json:"session_id"`
	Source        string `json:"source"`
	Mode          string `json:"mode"`
	TotalLines    int    `json:"total_lines"`
	ReturnedLines int    `json:"returned_lines"`
	Offset        int    `json:"offset"`
	Truncated     bool   `json:"truncated"`
	Content       string `json:"content"`
}

var sessionHistoryANSIRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// DefaultRelayURL is the pre-configured relay URL injected at build time.
var DefaultRelayURL string

// TunnelDeps holds dependencies for dynamic tunnel client creation.
type TunnelDeps struct {
	DB         *database.DB
	Encryptor  *security.Encryptor
	Hub        *websocket.Hub
	Port       int
	PairingMgr *tunnel.PairingManager
}

// SetAIHandler sets the AI handler for AI chat functionality.
func (a *API) SetAIHandler(h *AIHandler) {
	a.aiHandler = h
}

// SetProviderBridge installs the process-owned OpenAI-to-Anthropic bridge.
// The bridge uses its own encrypted provider profiles and never reads Codex CLI auth.
func (a *API) SetProviderBridge(bridge *providerbridge.Manager) {
	a.providerBridge = bridge
}

// GetDecryptedSetting reads an encrypted setting and returns the plaintext.
// If the key has no _iv companion (legacy plaintext), it encrypts in place (lazy migration).
func (a *API) GetDecryptedSetting(ctx context.Context, key string) (string, error) {
	value, err := a.db.GetSetting(ctx, key)
	if err != nil || value == "" {
		return "", err
	}

	iv, ivErr := a.db.GetSetting(ctx, key+"_iv")
	if ivErr != nil || iv == "" {
		// No IV — legacy plaintext value. Migrate it.
		encrypted, newIV, encErr := a.encryptor.Encrypt(value)
		if encErr != nil {
			return value, nil // degraded: return plaintext
		}
		_ = a.db.SetSetting(ctx, key, encrypted)
		_ = a.db.SetSetting(ctx, key+"_iv", newIV)
		_ = a.db.SetSetting(ctx, key+"_preview", apiKeyPreview(value))
		return value, nil
	}

	plaintext, err := a.encryptor.Decrypt(value, iv)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt setting %s: %w", key, err)
	}
	return plaintext, nil
}

// SetOTELHandler sets the OTEL handler for reading live session metrics.
func (a *API) SetOTELHandler(h *OTELHandler) {
	a.otelHandler = h
}

// writeClaudeMD writes content to the project's CLAUDE.md file.
func (a *API) writeClaudeMD(project *database.Project, content string) {
	if project.Type == "local" {
		fm := files.NewLocalFileManager(project.Path)
		if err := fm.Write("CLAUDE.md", []byte(content)); err != nil {
			log.Printf("[MemoryDoc] Failed to write CLAUDE.md for project %d: %v", project.ID, err)
		}
	}
}

// storePendingMemoryDoc stores a proposed memory doc edit for later approval.
func (a *API) storePendingMemoryDoc(docID string, projectID int64, content, summary string) {
	a.pendingMemoryDocsMu.Lock()
	defer a.pendingMemoryDocsMu.Unlock()
	if a.pendingMemoryDocs == nil {
		a.pendingMemoryDocs = make(map[string]*pendingMemoryDoc)
	}
	a.pendingMemoryDocs[docID] = &pendingMemoryDoc{
		ProjectID: projectID,
		Content:   content,
		Summary:   summary,
	}
}

// ApproveMemoryDoc applies a pending memory doc edit.
func (a *API) ApproveMemoryDoc(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	a.pendingMemoryDocsMu.Lock()
	pending := a.pendingMemoryDocs[docID]
	a.pendingMemoryDocsMu.Unlock()
	result, err := services.Collaboration.Proposals.ApproveMemory(platformUIContext(r), docID, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	response := map[string]interface{}{"status": result.Status}
	if pending != nil {
		response["project_id"] = pending.ProjectID
		if doc, readErr := a.db.GetMemoryDoc(r.Context(), pending.ProjectID); readErr == nil {
			response["version"] = doc.Version
		}
	}
	respondJSON(w, http.StatusOK, response)
}

// RejectMemoryDoc discards a pending memory doc edit.
func (a *API) RejectMemoryDoc(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	if err := services.Collaboration.Proposals.RejectMemory(platformUIContext(r), docID, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// storePendingTaskProposal stores a proposed task action set for later approval.
func (a *API) storePendingTaskProposal(docID string, actions []PlanningTaskAction, summary string) {
	a.pendingTaskProposalsMu.Lock()
	defer a.pendingTaskProposalsMu.Unlock()
	if a.pendingTaskProposals == nil {
		a.pendingTaskProposals = make(map[string]*pendingTaskProposal)
	}
	a.pendingTaskProposals[docID] = &pendingTaskProposal{Actions: actions, Summary: summary}
}

// ApproveTaskProposal applies all task actions from a pending task proposal.
func (a *API) ApproveTaskProposal(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	result, err := services.Collaboration.Proposals.ApproveTask(platformUIContext(r), docID, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": result.Status, "created": result.CreatedCount,
		"updated": result.UpdatedCount, "deleted": result.DeletedCount,
	})
}

// RejectTaskProposal discards a pending task proposal.
func (a *API) RejectTaskProposal(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	if err := services.Collaboration.Proposals.RejectTask(platformUIContext(r), docID, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// storePendingSkillProposal stores a proposed skill action for later approval.
func (a *API) storePendingSkillProposal(docID string, proposal *pendingSkillProposal) {
	a.pendingSkillProposalsMu.Lock()
	defer a.pendingSkillProposalsMu.Unlock()
	if a.pendingSkillProposals == nil {
		a.pendingSkillProposals = make(map[string]*pendingSkillProposal)
	}
	a.pendingSkillProposals[docID] = proposal
}

// ApproveSkillProposal applies a pending skill proposal (create or update project skill).
func (a *API) ApproveSkillProposal(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	a.pendingSkillProposalsMu.Lock()
	pending := a.pendingSkillProposals[docID]
	a.pendingSkillProposalsMu.Unlock()
	result, err := services.Collaboration.Proposals.ApproveSkill(platformUIContext(r), docID, platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	response := map[string]interface{}{"status": result.Status, "action": result.Action}
	if pending != nil {
		if result.Action == "update" && pending.SkillID > 0 {
			if skill, readErr := a.db.GetProjectSkill(r.Context(), pending.SkillID); readErr == nil {
				response["skill"] = skill
			}
		} else if skills, readErr := a.db.ListProjectSkills(r.Context(), pending.ProjectID); readErr == nil {
			for i := range skills {
				if skills[i].Name == pending.SkillName {
					response["skill"] = skills[i]
					break
				}
			}
		}
	}
	respondJSON(w, http.StatusOK, response)
}

// RejectSkillProposal discards a pending skill proposal.
func (a *API) RejectSkillProposal(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	if err := services.Collaboration.Proposals.RejectSkill(platformUIContext(r), docID, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// storePendingToolProposal stores a proposed tool execution for later approval.
func (a *API) storePendingToolProposal(docID string, proposal *pendingToolProposal) {
	a.pendingToolProposalsMu.Lock()
	defer a.pendingToolProposalsMu.Unlock()
	if a.pendingToolProposals == nil {
		a.pendingToolProposals = make(map[string]*pendingToolProposal)
	}
	a.pendingToolProposals[docID] = proposal
}

// ApproveToolProposal executes a pending custom tool and streams the output via SSE.
func (a *API) ApproveToolProposal(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	sendSSE := func(event string, data interface{}) {
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, jsonData)
		flusher.Flush()
	}

	sendSSE("started", map[string]string{"proposal_id": docID})
	result, err := services.Collaboration.Proposals.ApproveTool(platformUIContext(r), docID, platformUIAuthorization(r))
	if err != nil {
		sendSSE("error", map[string]string{"message": err.Error()})
		return
	}
	for _, line := range strings.Split(strings.TrimSuffix(result.Output, "\n"), "\n") {
		if line == "" {
			continue
		}
		sendSSE("output", map[string]string{"line": line})
	}
	sendSSE("done", map[string]interface{}{"exit_code": 0, "status": result.Status})
}

// RejectToolProposal discards a pending tool execution proposal.
func (a *API) RejectToolProposal(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	docID := chi.URLParam(r, "docId")
	if err := services.Collaboration.Proposals.RejectTool(platformUIContext(r), docID, platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// ProposeMemoryDoc creates a pending memory doc proposal for user approval.
// Called by the MCP tool instead of saving directly.
func (a *API) ProposeMemoryDoc(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input struct {
		ProjectID      int64  `json:"project_id"`
		Content        string `json:"content"`
		Summary        string `json:"summary"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if input.Content == "" {
		respondError(w, http.StatusBadRequest, "content is required")
		return
	}

	ctx := platformUIContext(r)
	project, err := services.Configuration.Projects.Get(ctx, input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	convID, _ := strconv.ParseInt(input.ConversationID, 10, 64)
	var conversationID *int64
	if convID > 0 {
		conversationID = &convID
	}
	proposal, err := services.Collaboration.Proposals.ProposeMemory(ctx, application.MemoryProposal{
		ProjectID: input.ProjectID, Content: input.Content, Summary: input.Summary,
		ConversationID: conversationID, Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	// Notify the chat panel via WebSocket to inject an inline doc card
	// Include conversation_id so the frontend can match the correct chat session
	a.hub.BroadcastChatDocCard(map[string]interface{}{
		"doc_id":          proposal.ID,
		"type":            "proposal",
		"title":           fmt.Sprintf("Change proposal — %s", project.Name),
		"summary":         input.Summary,
		"conversation_id": input.ConversationID,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "pending_approval",
		"doc_id":  proposal.ID,
		"message": fmt.Sprintf("Proposal created for project %s. The user must approve before the change is applied.", project.Name),
	})
}

func NewAPI(
	db *database.DB,
	hub *websocket.Hub,
	sessionMgr *session.Manager,
	configSync *configsync.ConfigSyncer,
	encryptor *security.Encryptor,
	notifService *notifications.Service,
	hookHandler *HookHandler,
) *API {
	api := &API{
		db:           db,
		hub:          hub,
		sessionMgr:   sessionMgr,
		configSync:   configSync,
		encryptor:    encryptor,
		notifService: notifService,
		hookHandler:  hookHandler,
	}
	api.taskService = application.NewProjectTaskService(db, api)
	api.workRunService = application.NewWorkRunService(db)
	api.capabilities, _ = application.NewProjectTaskCapabilityRegistry(api.taskService)
	_ = application.RegisterWorkRunCapabilities(api.capabilities, api.workRunService)
	return api
}

// ProjectTaskService exposes the shared application service to future
// Automation, MCP and AI adapters without exposing handler internals.
func (a *API) ProjectTaskService() *application.ProjectTaskService {
	return a.taskService
}

func (a *API) WorkRunService() *application.WorkRunService {
	return a.workRunService
}

func (a *API) CapabilityRegistry() *application.CapabilityRegistry {
	return a.capabilities
}

// WorkspaceService exposes the workspace application service (merge
// prediction for the coordinator tier, Phase 7.5).
func (a *API) WorkspaceService() *application.WorkspaceService {
	if a == nil {
		return nil
	}
	a.platformMu.RLock()
	defer a.platformMu.RUnlock()
	return a.workspaceService
}

func (a *API) PlatformCapabilityRegistry() *automation.PlatformCapabilityRegistry {
	if a == nil {
		return nil
	}
	a.platformMu.RLock()
	defer a.platformMu.RUnlock()
	return a.platformCapabilities
}

// SetUpdater configures the binary auto-updater for the API.
func (a *API) SetUpdater(u *updater.Updater) {
	a.updater = u
}

// SetStructuredView configures the structured view handler for the API.
func (a *API) SetStructuredView(sv *StructuredViewHandler) {
	a.structuredView = sv
}

// GetSessionEvents delegates to the structured view handler.
func (a *API) GetSessionEvents(w http.ResponseWriter, r *http.Request) {
	if a.structuredView == nil {
		respondJSON(w, http.StatusOK, []any{})
		return
	}
	a.structuredView.GetSessionEvents(w, r)
}

// StartWatchingSessionEvents delegates to the structured view handler.
func (a *API) StartWatchingSessionEvents(w http.ResponseWriter, r *http.Request) {
	if a.structuredView == nil {
		respondJSON(w, http.StatusOK, map[string]string{"status": "unavailable"})
		return
	}
	a.structuredView.StartWatching(w, r)
}

// StopWatchingSessionEvents delegates to the structured view handler.
func (a *API) StopWatchingSessionEvents(w http.ResponseWriter, r *http.Request) {
	if a.structuredView == nil {
		respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	a.structuredView.StopWatching(w, r)
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

// validateProjectPath checks that a project path is non-empty and, for local
// projects, that the directory exists on the filesystem.
func validateProjectPath(path, projectType string) string {
	if strings.TrimSpace(path) == "" {
		return "Path is required"
	}
	if projectType != "remote" {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Sprintf("Invalid path: %s", err.Error())
		}
		info, err := os.Stat(absPath)
		if os.IsNotExist(err) {
			return fmt.Sprintf("Directory does not exist: %s", absPath)
		}
		if err != nil {
			return fmt.Sprintf("Cannot access path: %s", err.Error())
		}
		if !info.IsDir() {
			return fmt.Sprintf("Path is not a directory: %s", absPath)
		}
	}
	return ""
}

type platformProjectPathValidator struct{}

func (platformProjectPathValidator) ValidateProjectPath(_ context.Context, path, projectType string) error {
	if message := validateProjectPath(path, projectType); message != "" {
		return errors.New(message)
	}
	return nil
}

func validateProjectBackend(projectType, backend string) string {
	if backend == "" {
		backend = string(session.BackendClaudeCode)
	}

	switch backend {
	case string(session.BackendClaudeCode), string(session.BackendCopilot), string(session.BackendACP), string(session.BackendCodex), string(session.BackendOpenCode):
	default:
		return fmt.Sprintf("Unsupported project backend: %s", backend)
	}

	return ""
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

	// Enrich with tags
	allPT, _ := a.db.ListAllProjectTagDetails(r.Context())
	tagMap := make(map[int64][]tagInfo)
	for _, t := range allPT {
		tagMap[t.ProjectID] = append(tagMap[t.ProjectID], tagInfo{ID: t.TagID, Name: t.Name, Color: t.Color})
	}

	type projectWithTags struct {
		database.Project
		Tags []tagInfo `json:"tags"`
	}
	result := make([]projectWithTags, len(projects))
	for i, p := range projects {
		tags := tagMap[p.ID]
		if tags == nil {
			tags = []tagInfo{}
		}
		result[i] = projectWithTags{Project: p, Tags: tags}
	}
	respondJSON(w, http.StatusOK, result)
}

func (a *API) CreateProject(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input database.ProjectInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	project, err := services.Configuration.Projects.Create(platformUIContext(r), input)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
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
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input database.ProjectInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	project, err := services.Configuration.Projects.Update(platformUIContext(r), id, input)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, project)
}

func (a *API) DuplicateProject(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	// Decode optional overrides from request body
	var overrides database.ProjectInput
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&overrides)
	}
	clone, err := services.Configuration.Projects.Duplicate(platformUIContext(r), application.DuplicateProjectCommand{ProjectID: id, Overrides: overrides})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, clone)
}

// BrowseDirectory lists directories at a given local filesystem path.
// Used by the project creation form to pick a working directory.
func (a *API) BrowseDirectory(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			path = "/"
		} else {
			path = home
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid path: %s", err.Error()))
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Cannot read directory: %s", err.Error()))
		return
	}

	var dirs []files.FileInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Skip hidden directories
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, files.FileInfo{
			Name:    entry.Name(),
			Path:    filepath.Join(absPath, entry.Name()),
			Size:    info.Size(),
			IsDir:   true,
			ModTime: info.ModTime(),
			Mode:    info.Mode().String(),
		})
	}

	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"current": absPath,
		"entries": dirs,
	})
}

// BrowseRemoteDirectory lists directories on a remote server via SSH/SFTP.
// Used by the project creation form to pick a working directory on a remote host.
func (a *API) BrowseRemoteDirectory(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input struct {
		SSHHost       string `json:"ssh_host"`
		SSHPort       int    `json:"ssh_port"`
		SSHUser       string `json:"ssh_user"`
		SSHAuthType   string `json:"ssh_auth_type"`
		SSHCredential string `json:"ssh_credential"`
		Path          string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	result, err := services.Configuration.ProjectOperations.BrowseRemote(platformUIContext(r), application.BrowseRemoteProjectCommand{
		Connection:    application.RemoteBrowseConnection{Host: input.SSHHost, Port: input.SSHPort, User: input.SSHUser, AuthType: input.SSHAuthType, Credential: input.SSHCredential, Path: input.Path},
		Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	entries := make([]map[string]interface{}, len(result.Entries))
	for i, entry := range result.Entries {
		entries[i] = map[string]interface{}{"name": entry.Name, "path": entry.Path, "is_dir": entry.IsDir}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"current": result.Current,
		"entries": entries,
	})
}

// looksLikeSFTPWindowsPath reports whether p is in the form Windows OpenSSH
// SFTP uses — "/C:/Users/foo" — so we can decide whether to treat the host as
// Windows for path-format purposes.
func looksLikeSFTPWindowsPath(p string) bool {
	return len(p) >= 4 && p[0] == '/' && isASCIILetter(p[1]) && p[2] == ':' && (p[3] == '/' || p[3] == '\\')
}

// windowsPathToSFTP converts a native Windows path ("C:\Users\foo" or
// "C:/Users/foo") back to the SFTP-style POSIX form ("/C:/Users/foo") that the
// Windows OpenSSH SFTP server expects.
func windowsPathToSFTP(p string) string {
	if p == "" {
		return p
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && p[2] == '/' {
		return "/" + p
	}
	return p
}

// sftpPathToWindows is the inverse of windowsPathToSFTP. "/C:/Users/foo" ->
// "C:\Users\foo".
func sftpPathToWindows(p string) string {
	if looksLikeSFTPWindowsPath(p) {
		p = p[1:]
	}
	return strings.ReplaceAll(p, "/", `\`)
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func (a *API) DeleteProject(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	if err := services.Configuration.Projects.Delete(platformUIContext(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ValidateProject(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	result, err := services.Configuration.ProjectOperations.Validate(platformUIContext(r), application.ValidateProjectCommand{ProjectID: id, Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": result.Status})
}

func (a *API) SyncProjectConfig(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	if err := services.Configuration.Configuration.SyncProject(platformUIContext(r), platformUIAuthorization(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
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

// ActiveSessionDetail is the enriched response for a single active session.
type ActiveSessionDetail struct {
	database.Session
	ProjectName          string        `json:"project_name"`
	ProjectType          string        `json:"project_type"`
	ProjectSSHHost       string        `json:"project_ssh_host,omitempty"`
	TaskTitle            string        `json:"task_title,omitempty"`
	TotalInputTokens     int64         `json:"total_input_tokens"`
	TotalOutputTokens    int64         `json:"total_output_tokens"`
	TotalCost            float64       `json:"total_cost"`
	HasPendingPermission bool          `json:"has_pending_permission"`
	ExecutionMode        string        `json:"execution_mode"`
	CodexRuntime         string        `json:"codex_runtime,omitempty"`
	ACPUsage             *ACPUsageInfo `json:"acp_usage,omitempty"`
}

func (a *API) GetActiveSessionDetails(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.db.ListActiveSessions(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if len(sessions) == 0 {
		respondJSON(w, http.StatusOK, []ActiveSessionDetail{})
		return
	}

	// Fetch projects in batch
	projects := make(map[int64]*database.Project)
	for _, s := range sessions {
		if _, ok := projects[s.ProjectID]; !ok {
			if p, err := a.db.GetProject(r.Context(), s.ProjectID); err == nil {
				projects[s.ProjectID] = p
			}
		}
	}

	// Build enriched response
	details := make([]ActiveSessionDetail, 0, len(sessions))
	for _, s := range sessions {
		d := ActiveSessionDetail{Session: s}

		if p, ok := projects[s.ProjectID]; ok {
			d.ProjectName = p.Name
			d.ProjectType = p.Type
			if p.SSHHost.Valid {
				d.ProjectSSHHost = p.SSHHost.String
			}
			if s.Backend == string(session.BackendCodex) {
				d.CodexRuntime = "app-server"
				var cfg struct {
					Runtime string `json:"runtime"`
				}
				if err := json.Unmarshal([]byte(p.BackendConfig), &cfg); err == nil && strings.TrimSpace(cfg.Runtime) != "" {
					d.CodexRuntime = strings.TrimSpace(cfg.Runtime)
				}
			}
		}

		if s.TaskID.Valid {
			if task, err := a.db.GetTask(r.Context(), s.TaskID.Int64); err == nil {
				d.TaskTitle = task.Title
			}
		}

		if a.otelHandler != nil {
			if metrics := a.otelHandler.GetLiveMetrics(s.ID); metrics != nil {
				d.TotalInputTokens = metrics.TotalInputTokens
				d.TotalOutputTokens = metrics.TotalOutputTokens
				d.TotalCost = metrics.TotalCost
			}
		}

		if a.hookHandler != nil {
			d.HasPendingPermission = a.hookHandler.HasPendingPermission(s.ID)
			d.ExecutionMode = a.hookHandler.GetSessionMode(s.ID)
			d.ACPUsage = a.hookHandler.GetACPUsage(s.ID)
		}

		details = append(details, d)
	}

	respondJSON(w, http.StatusOK, details)
}

func (a *API) CreateSession(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input struct {
		ProjectID                  int64             `json:"project_id"`
		TaskID                     *int64            `json:"task_id,omitempty"`
		EnvVars                    map[string]string `json:"env_vars,omitempty"`
		DangerouslySkipPermissions bool              `json:"dangerously_skip_permissions"`
		AutoStartTaskPrompt        bool              `json:"auto_start_task_prompt"`
		PlanningMode               bool              `json:"planning_mode"`
		CustomPrompt               string            `json:"custom_prompt"`
		WorkspaceID                string            `json:"workspace_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	authorization := platformUIAuthorization(r)
	authorization.AllowEnvironment = len(input.EnvVars) > 0
	authorization.AllowUnsafePermissions = input.DangerouslySkipPermissions
	sess, err := services.Execution.Sessions.Create(platformUIContext(r), application.CreateSessionCommand{
		ProjectID: input.ProjectID, TaskID: input.TaskID, Environment: input.EnvVars,
		DangerouslySkipPermissions: input.DangerouslySkipPermissions,
		AutoStartTaskPrompt:        input.AutoStartTaskPrompt,
		PlanningMode:               input.PlanningMode,
		CustomPrompt:               input.CustomPrompt,
		WorkspaceID:                input.WorkspaceID,
		Authorization:              authorization,
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusCreated, sess)
}

func (a *API) startManagedSession(ctx context.Context, input startSessionInput) (*database.Session, error) {
	services, ok := a.platformApplicationServices()
	if !ok {
		return nil, newStartSessionError(http.StatusServiceUnavailable, "OpenPoet platform services are unavailable")
	}
	authorization := application.ActionAuthorization{
		Actor:      application.Actor{Type: "agent", ID: "openpoet-ai"},
		Reason:     "OpenPoet AI session request",
		Approved:   true,
		ApprovedBy: "user:local-ui",
	}
	authorization.AllowEnvironment = len(input.EnvVars) > 0
	authorization.AllowUnsafePermissions = input.DangerouslySkipPermissions
	return services.Execution.Sessions.Create(ctx, application.CreateSessionCommand{
		ProjectID: input.ProjectID, TaskID: input.TaskID, Environment: input.EnvVars,
		DangerouslySkipPermissions: input.DangerouslySkipPermissions,
		AutoStartTaskPrompt:        input.AutoStartTaskPrompt,
		PlanningMode:               input.PlanningMode,
		CustomPrompt:               input.CustomPrompt,
		Authorization:              authorization,
	})
}

const defaultTaskStartPrompt = application.DefaultTaskStartPrompt

const (
	localSessionLineSubmitDelay   = 1500 * time.Millisecond
	remoteSessionLineSubmitDelay  = 2 * time.Second
	taskSessionInitialDelay       = 500 * time.Millisecond
	taskSessionReadyPollInterval  = 50 * time.Millisecond
	taskSessionReadyQuietPeriod   = 250 * time.Millisecond
	taskSessionReadyTimeout       = 30 * time.Second
	claudeAlternateScreenSequence = "\x1b[?1049h"
)

func (a *API) autoStartTaskSession(sessionID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), taskSessionReadyTimeout)
		defer cancel()
		if err := a.waitForTaskSessionReady(ctx, sessionID); err != nil {
			log.Printf("[Session] task prompt readiness wait ended for %s: %v", sessionID, err)
		}
		if err := a.submitSessionLine(sessionID, defaultTaskStartPrompt); err != nil {
			log.Printf("[Session] failed to auto-submit task start prompt for %s: %v", sessionID, err)
		}
	}()
}

// waitForTaskSessionReady prevents the automatic task prompt from reaching
// Claude Code while its terminal is still starting. During that window Claude
// preserves typed text but can discard Enter, leaving the prompt stranded in
// the input. The alternate-screen sequence marks TUI startup; a short quiet
// period lets its initial render finish before input is sent.
func (a *API) waitForTaskSessionReady(ctx context.Context, sessionID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil || a.db == nil || a.sessionMgr == nil {
		return errors.New("session readiness dependencies are unavailable")
	}

	sess, err := a.db.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.Backend != string(session.BackendClaudeCode) {
		timer := time.NewTimer(taskSessionInitialDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}

	ticker := time.NewTicker(taskSessionReadyPollInterval)
	defer ticker.Stop()
	lastOutputSize := -1
	var quietSince time.Time

	for {
		output, outputErr := a.sessionMgr.GetSessionOutput(sessionID)
		if outputErr != nil {
			return outputErr
		}
		if strings.Contains(string(output), claudeAlternateScreenSequence) {
			if len(output) != lastOutputSize {
				lastOutputSize = len(output)
				quietSince = time.Now()
			} else if !quietSince.IsZero() && time.Since(quietSince) >= taskSessionReadyQuietPeriod {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *API) submitSessionLine(sessionID, text string) error {
	delay := a.sessionLineSubmitDelay(context.Background(), sessionID)
	return a.sessionMgr.SubmitLineToSession(sessionID, text, delay)
}

func (a *API) sessionLineSubmitDelay(ctx context.Context, sessionID string) time.Duration {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil || a.db == nil {
		return localSessionLineSubmitDelay
	}

	sess, err := a.db.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return localSessionLineSubmitDelay
	}

	project, err := a.db.GetProject(ctx, sess.ProjectID)
	if err != nil || project == nil {
		return localSessionLineSubmitDelay
	}
	if project.Type == "remote" {
		return remoteSessionLineSubmitDelay
	}
	return localSessionLineSubmitDelay
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

func (a *API) GetSessionHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	q := r.URL.Query()

	req := sessionHistoryRequest{
		Mode:          q.Get("mode"),
		Query:         q.Get("query"),
		Regex:         parseBoolQuery(q.Get("regex")),
		CaseSensitive: parseBoolQuery(q.Get("case_sensitive")),
		Lines:         parseIntQuery(q.Get("lines"), 80),
		Offset:        parseIntQuery(q.Get("offset"), 1),
		Limit:         parseIntQuery(q.Get("limit"), 0),
		ContextLines:  parseIntQuery(q.Get("context"), 2),
		MaxChars:      parseIntQuery(q.Get("max_chars"), 12000),
	}

	result, err := a.readSessionHistory(r.Context(), id, req)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, result)
}

func parseBoolQuery(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func parseIntQuery(v string, fallback int) int {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func (a *API) readSessionHistory(ctx context.Context, sessionID string, req sessionHistoryRequest) (*sessionHistoryResponse, error) {
	sess, err := a.db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found")
	}

	source := "terminal_buffer"
	content := ""

	if sess.Backend == string(session.BackendCodex) {
		if events, err := a.db.ListCodexTranscriptEvents(ctx, sessionID, 4000); err == nil && len(events) > 0 {
			content = renderCodexTranscriptEvents(events)
			source = "codex_transcript"
		}
	}

	if content == "" {
		if output, err := a.sessionMgr.GetSessionOutput(sessionID); err == nil && len(output) > 0 {
			content = cleanSessionHistoryOutput(string(output))
		}
	}

	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("session has no readable history; terminal buffers are only available for active sessions unless a durable transcript exists")
	}

	return sliceSessionHistory(sessionID, source, content, req), nil
}

func renderCodexTranscriptEvents(events []database.CodexTranscriptEvent) string {
	var sb strings.Builder
	for _, e := range events {
		label := e.Kind
		if e.Title != "" {
			label += ":" + e.Title
		}
		text := strings.TrimSpace(e.Text)
		if e.Command != "" {
			text = "$ " + e.Command
			if strings.TrimSpace(e.Text) != "" {
				text += "\n" + strings.TrimSpace(e.Text)
			}
		}
		if text == "" {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimRight(line, " \t")
			if line != "" {
				sb.WriteString(fmt.Sprintf("[%s] %s\n", label, line))
			}
		}
	}
	return sb.String()
}

func cleanSessionHistoryOutput(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = sessionHistoryANSIRe.ReplaceAllString(content, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, content)
}

func sliceSessionHistory(sessionID, source, content string, req sessionHistoryRequest) *sessionHistoryResponse {
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Mode == "" {
		req.Mode = "tail"
	}
	if strings.TrimSpace(req.Query) != "" {
		req.Mode = "search"
	}
	if req.Lines <= 0 {
		req.Lines = 80
	}
	if req.Limit <= 0 {
		req.Limit = req.Lines
	}
	if req.ContextLines < 0 {
		req.ContextLines = 0
	}
	if req.MaxChars <= 0 {
		req.MaxChars = 12000
	}
	if req.MaxChars > 50000 {
		req.MaxChars = 50000
	}

	content = strings.TrimRight(content, "\n")
	lines := []string{}
	if content != "" {
		lines = strings.Split(content, "\n")
	}

	start, end := 0, len(lines)
	selected := []string{}
	switch req.Mode {
	case "head":
		end = minInt(len(lines), req.Lines)
		selected = lines[:end]
	case "window":
		if req.Offset <= 0 {
			req.Offset = 1
		}
		start = minInt(maxInt(req.Offset-1, 0), len(lines))
		end = minInt(start+req.Limit, len(lines))
		selected = lines[start:end]
	case "search":
		selected, start = searchSessionHistoryLines(lines, req)
	default:
		req.Mode = "tail"
		start = maxInt(len(lines)-req.Lines, 0)
		selected = lines[start:]
	}

	out := strings.Join(selected, "\n")
	truncated := false
	if len(out) > req.MaxChars {
		truncated = true
		if req.Mode == "tail" {
			out = out[len(out)-req.MaxChars:]
		} else {
			out = out[:req.MaxChars]
		}
	}

	return &sessionHistoryResponse{
		SessionID:     sessionID,
		Source:        source,
		Mode:          req.Mode,
		TotalLines:    len(lines),
		ReturnedLines: len(selected),
		Offset:        start + 1,
		Truncated:     truncated,
		Content:       out,
	}
}

func searchSessionHistoryLines(lines []string, req sessionHistoryRequest) ([]string, int) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return []string{}, 0
	}

	matches := make([]int, 0)
	if req.Regex {
		pattern := query
		if !req.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return []string{fmt.Sprintf("Invalid regex: %v", err)}, 0
		}
		for i, line := range lines {
			if re.MatchString(line) {
				matches = append(matches, i)
			}
		}
	} else {
		needle := query
		if !req.CaseSensitive {
			needle = strings.ToLower(needle)
		}
		for i, line := range lines {
			haystack := line
			if !req.CaseSensitive {
				haystack = strings.ToLower(haystack)
			}
			if strings.Contains(haystack, needle) {
				matches = append(matches, i)
			}
		}
	}

	if len(matches) == 0 {
		return []string{fmt.Sprintf("No matches for %q.", query)}, 0
	}
	if req.Limit > 0 && len(matches) > req.Limit {
		matches = matches[:req.Limit]
	}

	selected := make([]string, 0)
	firstStart := 0
	lastEnd := -1
	for idx, match := range matches {
		start := maxInt(match-req.ContextLines, 0)
		end := minInt(match+req.ContextLines+1, len(lines))
		if idx == 0 {
			firstStart = start
		}
		if start <= lastEnd {
			start = lastEnd
		} else if len(selected) > 0 {
			selected = append(selected, "...")
		}
		for i := start; i < end; i++ {
			prefix := "  "
			if i == match {
				prefix = "> "
			}
			selected = append(selected, fmt.Sprintf("%s%d | %s", prefix, i+1, lines[i]))
		}
		lastEnd = end
	}
	return selected, firstStart
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a *API) GetSessionPlan(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	planContent, updatedAt, err := a.db.GetSessionPlan(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	result := map[string]interface{}{
		"plan_content": planContent,
	}
	if updatedAt != nil {
		result["plan_updated_at"] = updatedAt
	}
	respondJSON(w, http.StatusOK, result)
}

// ListSessionDocuments returns all documents associated with a session (temp_documents + session plan).
func (a *API) ListSessionDocuments(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	type docEntry struct {
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		Type      string    `json:"type"` // "document" or "plan"
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}

	var results []docEntry

	// 1. Documents created in this session
	docs, _ := a.db.ListDocumentsBySession(r.Context(), sessionID)
	for _, d := range docs {
		results = append(results, docEntry{
			ID:        d.ID,
			Title:     d.Title,
			Type:      "document",
			Status:    d.Status,
			CreatedAt: d.CreatedAt,
		})
	}

	// 2. Session plan (if any)
	planContent, _, err := a.db.GetSessionPlan(r.Context(), sessionID)
	if err == nil && planContent != "" {
		session, _ := a.db.GetSession(r.Context(), sessionID)
		planTitle := "Plan"
		if session != nil && session.Name != "" {
			planTitle = "Plan: " + session.Name
		}
		results = append(results, docEntry{
			ID:    "plan:" + sessionID,
			Title: planTitle,
			Type:  "plan",
		})
	}

	if results == nil {
		results = []docEntry{}
	}
	respondJSON(w, http.StatusOK, results)
}

func (a *API) DeleteSession(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if a.hookHandler != nil {
		a.hookHandler.MarkUserStopped(id)
	}
	if _, err := services.Execution.Sessions.Stop(platformUIContext(r), application.StopSessionCommand{SessionID: id, Authorization: platformUIAuthorization(r)}); err != nil {
		respondApplicationError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *API) stopManagedSession(ctx context.Context, id string) error {
	services, ok := a.platformApplicationServices()
	if !ok {
		return newStartSessionError(http.StatusServiceUnavailable, "OpenPoet platform services are unavailable")
	}
	if a.hookHandler != nil {
		a.hookHandler.MarkUserStopped(id)
	}
	_, err := services.Execution.Sessions.Stop(ctx, application.StopSessionCommand{
		SessionID: id,
		Authorization: application.ActionAuthorization{
			Actor: application.Actor{Type: "agent", ID: "openpoet-ai"}, Reason: "OpenPoet AI session stop",
			Approved: true, ApprovedBy: "user:local-ui",
		},
	})
	return err
}

func (a *API) ReopenSession(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "id")

	// Parse optional request body for dangerously_skip_permissions flag
	var reopenInput struct {
		DangerouslySkipPermissions bool `json:"dangerously_skip_permissions"`
	}
	// Body is optional — ignore decode errors (empty body is fine)
	_ = json.NewDecoder(r.Body).Decode(&reopenInput)

	authorization := platformUIAuthorization(r)
	authorization.AllowUnsafePermissions = reopenInput.DangerouslySkipPermissions
	sess, err := services.Execution.Sessions.Reopen(platformUIContext(r), application.ReopenSessionCommand{SessionID: sessionID, DangerouslySkipPermissions: reopenInput.DangerouslySkipPermissions, Authorization: authorization})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, sess)
}

// AutoRestoreSession restores a previously-active session after a service restart.
// It rebuilds the full session context (task, config, env vars) and calls ReopenSession.
// Returns nil on success, or an error (session is marked "stopped" on failure).
func (a *API) AutoRestoreSession(ctx context.Context, sess *database.Session) error {
	sessionID := sess.ID

	// Get the project — skip if deleted
	project, err := a.db.GetProject(ctx, sess.ProjectID)
	if err != nil {
		a.db.EndSession(ctx, sessionID, "stopped")
		_ = a.db.ReleaseWorkspaceLeaseBySession(ctx, sessionID)
		return fmt.Errorf("project not found (may have been deleted): %w", err)
	}

	// Check backend supports resume
	backend := session.GetBackend(sess.Backend)
	if !backend.SupportsResume() {
		a.db.EndSession(ctx, sessionID, "stopped")
		_ = a.db.ReleaseWorkspaceLeaseBySession(ctx, sessionID)
		return fmt.Errorf("backend %q does not support resume", sess.Backend)
	}

	// Workspace sessions restore back into their lane — never silently into
	// the main checkout. A vanished lane fails the restore loudly, and every
	// failure path releases the lease so the lane can't stay stranded
	// 'leased' by a session that will never run again.
	laneRestore := false
	if sess.WorkDir != "" && sess.WorkDir != project.Path {
		// The local Stat only proves anything for local lanes; a remote lane's
		// path lives on the SSH host and is validated by the reopen itself.
		if project.Type == "local" {
			if _, statErr := os.Stat(sess.WorkDir); statErr != nil {
				a.db.EndSession(ctx, sessionID, "stopped")
				_ = a.db.ReleaseWorkspaceLeaseBySession(ctx, sessionID)
				return fmt.Errorf("workspace directory gone, not restoring session %s: %w", sessionID, statErr)
			}
		}
		if sess.WorkspaceID.Valid && sess.WorkspaceID.String != "" {
			if leaseErr := a.db.LeaseWorkspaceForSession(ctx, sess.WorkspaceID.String, sessionID); leaseErr != nil {
				a.db.EndSession(ctx, sessionID, "stopped")
				return fmt.Errorf("workspace lease unavailable, not restoring session %s: %w", sessionID, leaseErr)
			}
		}
		laneProject := *project
		laneProject.Path = sess.WorkDir
		project = &laneProject
		laneRestore = true
	}

	// Sync config to project before restoring (lanes are materialize-only).
	// DETACHED context with a hard timeout: the restore loop's ctx must never
	// cancel an in-flight SQLite query mid-SSH (the documented poisoning), and
	// a hung remote host must not stall the whole restore pass.
	syncCtx, cancelSync := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancelSync()
	if a.configSync != nil {
		var syncErr error
		if laneRestore {
			syncErr = a.configSync.MaterializeToWorkspace(syncCtx, project)
		} else {
			syncErr = a.configSync.SyncToProject(syncCtx, project)
		}
		if syncErr != nil {
			log.Printf("[AutoRestore] Warning: config sync failed for session %s: %v", sessionID, syncErr)
		}
	}

	// Build env vars with task context if linked
	envVars := make(map[string]string)
	linkedTask, _ := a.db.GetTaskForSession(ctx, sessionID)
	if linkedTask != nil {
		envVars["OPENPOET_TASK_ID"] = fmt.Sprintf("%d", linkedTask.ID)
		envVars["OPENPOET_TASK_TITLE"] = linkedTask.Title

		// See CreateSession comment: keep system prompt single-line so
		// Windows cmd.exe quoting doesn't corrupt the rest of the args.
		envVars["OPENPOET_APPEND_SYSTEM_PROMPT"] = fmt.Sprintf(
			"This is an AUTO-RESTORED OpenPoet session for task #%d titled %q. Call the openpoet_get_my_task MCP tool now to read its current description and priority. Communicate with the user in the same language as the task; call openpoet_request_task_evaluation when you believe significant work is complete.",
			linkedTask.ID, linkedTask.Title,
		)
	}

	// Resolve the same trusted provider environment used for newly-created sessions.
	if project.Backend == "claude_code" && a.aiHandler != nil {
		providerEnv, providerErr := (platformSessionEnvironmentProvider{handler: a.aiHandler}).SessionEnvironment(ctx, project)
		if providerErr != nil {
			return fmt.Errorf("resolve session provider environment: %w", providerErr)
		}
		for key, value := range providerEnv {
			envVars[key] = value
		}
	}

	// Restore skip_permissions flag (persisted in DB, gated by project setting)
	if sess.SkipPermissions && project.DangerouslySkipPermissions {
		envVars["OPENPOET_DANGEROUSLY_SKIP_PERMISSIONS"] = "true"
	}

	// Mark session as "stopped" first so ReopenSession SQL works
	// (it requires status IN ('stopped', 'completed'))
	a.db.EndSession(ctx, sessionID, "stopped")

	// Reopen the session
	if err := a.sessionMgr.ReopenSession(ctx, sess, project, envVars, a.encryptor.Decrypt); err != nil {
		// The runner never came back: free any lane lease this session held
		// so the workspace isn't stranded 'leased' by a dead session.
		_ = a.db.ReleaseWorkspaceLeaseBySession(ctx, sessionID)
		return fmt.Errorf("failed to restore session %s: %w", sessionID, err)
	}

	return nil
}

// ============ Skills ============

// validateSkillName checks for empty, path traversal, and invalid characters.
func validateSkillName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Name is required"
	}
	if len(name) > 200 {
		return "Name must be 200 characters or less"
	}
	if strings.ContainsAny(name, "/\\") {
		return "Name cannot contain / or \\"
	}
	if strings.Contains(name, "..") {
		return "Name cannot contain .."
	}
	if filepath.Base(name) != name {
		return "Invalid skill name"
	}
	return ""
}

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
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var s database.Skill
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	created, err := services.Configuration.Skills.Create(platformUIContext(r), application.SkillInput{Name: s.Name, Content: s.Content, Enabled: s.Enabled, Category: s.Category, SortOrder: s.SortOrder})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, created)
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
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
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

	updated, err := services.Configuration.Skills.Update(platformUIContext(r), application.UpdateSkillCommand{ID: id, Name: &s.Name, Content: &s.Content, Enabled: &s.Enabled, Category: &s.Category, SortOrder: &s.SortOrder})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, updated)
}

func (a *API) DeleteSkill(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	if err := services.Configuration.Skills.Delete(platformUIContext(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) DuplicateSkill(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	dup, err := services.Configuration.Skills.Duplicate(platformUIContext(r), id, "")
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, dup)
}

func (a *API) ExportSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := a.db.ListSkills(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skills == nil {
		skills = []database.Skill{}
	}

	type exportSkill struct {
		Name     string `json:"name"`
		Content  string `json:"content"`
		Enabled  bool   `json:"enabled"`
		Category string `json:"category"`
	}
	exported := make([]exportSkill, len(skills))
	for i, s := range skills {
		exported[i] = exportSkill{Name: s.Name, Content: s.Content, Enabled: s.Enabled, Category: s.Category}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=openpoet-skills.json")
	json.NewEncoder(w).Encode(exported)
}

func (a *API) ImportSkills(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	type importSkill struct {
		Name     string `json:"name"`
		Content  string `json:"content"`
		Enabled  bool   `json:"enabled"`
		Category string `json:"category"`
	}

	var skills []importSkill
	if err := json.NewDecoder(r.Body).Decode(&skills); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON: expected array of skills")
		return
	}

	inputs := make([]application.SkillInput, len(skills))
	for i, s := range skills {
		inputs[i] = application.SkillInput{Name: s.Name, Content: s.Content, Enabled: s.Enabled, Category: s.Category}
	}
	result, err := services.Configuration.Skills.Import(platformUIContext(r), inputs)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"imported": len(result.Imported),
		"skipped":  len(result.Skipped),
	})
}

func (a *API) ListSkillVersions(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	versions, err := a.db.ListSkillVersions(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if versions == nil {
		versions = []database.SkillVersion{}
	}
	respondJSON(w, http.StatusOK, versions)
}

func (a *API) RestoreSkillVersion(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	skillID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}
	versionID, err := strconv.ParseInt(chi.URLParam(r, "versionId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid version ID")
		return
	}

	skill, err := services.Configuration.Skills.Restore(platformUIContext(r), skillID, versionID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, skill)
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
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var m database.MCPServer
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	created, err := services.Configuration.MCP.CreateGlobal(platformUIContext(r), platformUIAuthorization(r), application.MCPServerInput{Name: m.Name, Command: m.Command, Args: m.Args, Env: m.Env, Enabled: m.Enabled})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, created)
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
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
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

	updated, err := services.Configuration.MCP.UpdateGlobal(platformUIContext(r), platformUIAuthorization(r), application.UpdateMCPServerCommand{ID: id, Name: &m.Name, Command: &m.Command, Args: &m.Args, Env: &m.Env, Enabled: &m.Enabled})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, updated)
}

func (a *API) DeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	if err := services.Configuration.MCP.DeleteGlobal(platformUIContext(r), platformUIAuthorization(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============ AI Agents ============

func (a *API) ListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := a.db.ListAIAgents(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agents == nil {
		agents = []database.AIAgent{}
	}
	respondJSON(w, http.StatusOK, agents)
}

func (a *API) CreateAgent(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var m database.AIAgent
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	created, err := services.Configuration.Agents.Create(platformUIContext(r), application.AIAgentInput{Name: m.Name, SystemPrompt: m.SystemPrompt, ToolPolicy: m.ToolPolicy, ProjectFilter: m.ProjectFilter, Enabled: m.Enabled})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, created)
}

func (a *API) GetAgent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid agent ID")
		return
	}
	m, err := a.db.GetAIAgent(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Agent not found")
		return
	}
	respondJSON(w, http.StatusOK, m)
}

func (a *API) UpdateAgent(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid agent ID")
		return
	}
	var m database.AIAgent
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	updated, err := services.Configuration.Agents.Update(platformUIContext(r), application.UpdateAIAgentCommand{ID: id, Name: &m.Name, SystemPrompt: &m.SystemPrompt, ToolPolicy: &m.ToolPolicy, ProjectFilter: &m.ProjectFilter, Enabled: &m.Enabled})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, updated)
}

func (a *API) DeleteAgent(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid agent ID")
		return
	}
	if err := services.Configuration.Agents.Delete(platformUIContext(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============ AI Configs ============

func (a *API) ListAIConfigs(w http.ResponseWriter, r *http.Request) {
	configs, err := a.db.ListAIConfigs(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if configs == nil {
		configs = []database.AIConfig{}
	}
	respondJSON(w, http.StatusOK, configs)
}

func (a *API) CreateAIConfig(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var req struct {
		Name         string `json:"name"`
		ProviderType string `json:"provider_type"`
		APIKey       string `json:"api_key"`
		Model        string `json:"model"`
		BaseURL      string `json:"base_url"`
		ExtraJSON    string `json:"extra_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	created, err := services.Configuration.AIConfigs.Create(platformUIContext(r), platformUIAuthorization(r), application.AIConfigInput{Name: req.Name, ProviderType: req.ProviderType, APIKey: req.APIKey, Model: req.Model, BaseURL: req.BaseURL, ExtraJSON: req.ExtraJSON})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, created)
}

func (a *API) UpdateAIConfig(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}

	var req struct {
		Name         string `json:"name"`
		ProviderType string `json:"provider_type"`
		APIKey       string `json:"api_key"`
		Model        string `json:"model"`
		BaseURL      string `json:"base_url"`
		ExtraJSON    string `json:"extra_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	command := application.UpdateAIConfigCommand{ID: id, APIKey: &req.APIKey, Model: &req.Model, BaseURL: &req.BaseURL}
	if req.Name != "" {
		command.Name = &req.Name
	}
	if req.ProviderType != "" {
		command.ProviderType = &req.ProviderType
	}
	if req.ExtraJSON != "" {
		command.ExtraJSON = &req.ExtraJSON
	}
	updated, err := services.Configuration.AIConfigs.Update(platformUIContext(r), platformUIAuthorization(r), command)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, updated)
}

func (a *API) DeleteAIConfig(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}

	if a.providerBridge != nil {
		a.providerBridge.StopProxy(id)
	}
	if err := services.Configuration.AIConfigs.Delete(platformUIContext(r), platformUIAuthorization(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) GetOpenAIOAuthStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}
	if a.providerBridge == nil {
		respondError(w, http.StatusServiceUnavailable, "OpenAI provider bridge is unavailable")
		return
	}
	connected, err := a.providerBridge.Connected(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, helperErr := a.providerBridge.BinaryPath()
	result := map[string]interface{}{
		"connected":        connected,
		"helper_available": helperErr == nil,
	}
	if helperErr != nil {
		result["helper_error"] = helperErr.Error()
	}
	respondJSON(w, http.StatusOK, result)
}

func (a *API) StartOpenAIOAuth(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}
	if a.providerBridge == nil {
		respondError(w, http.StatusServiceUnavailable, "OpenAI provider bridge is unavailable")
		return
	}
	login, err := a.providerBridge.StartDeviceLogin(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusAccepted, login)
}

func (a *API) GetOpenAIOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if a.providerBridge == nil {
		respondError(w, http.StatusServiceUnavailable, "OpenAI provider bridge is unavailable")
		return
	}
	login, err := a.providerBridge.DeviceLoginStatus(chi.URLParam(r, "loginID"))
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, login)
}

func (a *API) CancelOpenAIOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if a.providerBridge == nil {
		respondError(w, http.StatusServiceUnavailable, "OpenAI provider bridge is unavailable")
		return
	}
	if err := a.providerBridge.CancelDeviceLogin(chi.URLParam(r, "loginID")); err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (a *API) DisconnectOpenAIOAuth(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}
	if a.providerBridge == nil {
		respondError(w, http.StatusServiceUnavailable, "OpenAI provider bridge is unavailable")
		return
	}
	if err := a.providerBridge.Disconnect(r.Context(), id); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

func (a *API) GetAIConfigAssignments(w http.ResponseWriter, r *http.Request) {
	assignments, err := a.db.GetAIConfigAssignments(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, assignments)
}

func (a *API) UpdateAIConfigAssignments(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var req map[string]*int64 // slot -> config_id (null = unassign)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := services.Configuration.AIConfigs.Assign(platformUIContext(r), platformUIAuthorization(r), req); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ============ Settings ============

func (a *API) GetSettings(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	settings, err := services.Configuration.Configuration.Settings(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, settings)
}

func (a *API) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var settings map[string]string
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := services.Configuration.Configuration.UpdateSettings(platformUIContext(r), platformUIAuthorization(r), settings); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) SyncAllConfig(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	if err := services.Configuration.Configuration.SyncAll(platformUIContext(r), platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ============ Notifications ============

func (a *API) GetNotifications(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			limit = parsed
		}
	}

	notifications, err := services.Collaboration.Notifications.List(platformUIContext(r), limit)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, notifications)
}

func (a *API) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid notification ID")
		return
	}

	if err := services.Collaboration.Notifications.MarkRead(platformUIContext(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *API) GetActiveNotifications(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	notifications, err := services.Collaboration.Notifications.ListActive(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, notifications)
}

func (a *API) GetNotificationUnreadCount(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	count, err := services.Collaboration.Notifications.UnreadCount(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]int{"unread_count": count})
}

func (a *API) MarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	if err := services.Collaboration.Notifications.MarkAllRead(platformUIContext(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============ Memory Docs ============

func (a *API) GetMemoryDoc(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	doc, err := services.Collaboration.Documents.GetMemory(platformUIContext(r), id)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, doc)
}

func (a *API) UpdateMemoryDoc(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		Content   string `json:"content"`
		UpdatedBy string `json:"updated_by"`
		Summary   string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	doc, err := services.Collaboration.Documents.UpdateMemory(platformUIContext(r), application.UpdateMemoryDocumentCommand{ProjectID: id, Content: input.Content, Summary: input.Summary, Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, doc)
}

// ============ Temp Documents ============

func (a *API) CreateTempDocument(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input struct {
		Title          string `json:"title"`
		Content        string `json:"content"`
		ConversationID string `json:"conversation_id"`
		TaskID         string `json:"task_id"`
		MissionID      string `json:"mission_id"`
		SessionID      string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	convID, _ := strconv.ParseInt(input.ConversationID, 10, 64)
	taskID, _ := strconv.ParseInt(input.TaskID, 10, 64)
	missionID, _ := strconv.ParseInt(input.MissionID, 10, 64)
	var conversationID, linkedTaskID, linkedMissionID *int64
	if convID > 0 {
		conversationID = &convID
	}
	if taskID > 0 {
		linkedTaskID = &taskID
	}
	if missionID > 0 {
		linkedMissionID = &missionID
	}
	doc, err := services.Collaboration.Documents.CreateTemp(platformUIContext(r), application.CreateTempDocumentCommand{Title: input.Title, Content: input.Content, ConversationID: conversationID, TaskID: linkedTaskID, MissionID: linkedMissionID, SessionID: input.SessionID, Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	// Notify the frontend via WebSocket to inject an inline doc card
	a.hub.BroadcastChatDocCard(map[string]interface{}{
		"doc_id":          doc.ID,
		"type":            "document",
		"title":           doc.Title,
		"conversation_id": input.ConversationID,
		"session_id":      input.SessionID,
	})

	respondJSON(w, http.StatusCreated, map[string]string{
		"id":   doc.ID,
		"link": fmt.Sprintf("/app/doc/%s", doc.ID),
	})
}

func (a *API) GetTempDocument(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := services.Collaboration.Documents.GetTemp(platformUIContext(r), id)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, doc)
}

// ============ Project Tasks ============

func (a *API) PublishTaskChange(change application.TaskChange) {
	payload := map[string]interface{}{"action": change.Action}
	if change.ProjectID > 0 {
		payload["project_id"] = change.ProjectID
	}
	if change.Task != nil {
		payload["task"] = change.Task
	}
	if change.TaskID > 0 {
		payload["id"] = change.TaskID
	}
	a.hub.BroadcastStateUpdate("task", payload)
}

func (a *API) PublishTaskHistory(history *database.TaskHistory) {
	if history == nil {
		return
	}
	a.hub.BroadcastStateUpdate("task_history", map[string]interface{}{
		"action":     "created",
		"task_id":    history.TaskID,
		"project_id": history.ProjectID,
		"entry":      history,
	})
}

func (a *API) PublishSessionRename(rename application.SessionRename) {
	a.hub.BroadcastStateUpdate("session", map[string]interface{}{
		"action":     "renamed",
		"session_id": rename.SessionID,
		"name":       rename.Name,
	})
}

func (a *API) RequestTaskVerification(task *database.ProjectTask) {
	if task != nil && a.aiHandler != nil {
		go a.aiHandler.GenerateVerificationDoc(context.Background(), task)
	}
}

func respondApplicationError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case application.ErrorIsKind(err, application.ErrorValidation):
		status = http.StatusBadRequest
	case application.ErrorIsKind(err, application.ErrorNotFound):
		status = http.StatusNotFound
	case application.ErrorIsKind(err, application.ErrorConflict):
		status = http.StatusConflict
	}
	if code := application.ErrorCode(err); code != "" {
		respondJSON(w, status, map[string]string{"error": err.Error(), "code": code})
		return
	}
	respondError(w, status, err.Error())
}

func (a *API) ListAllTasks(w http.ResponseWriter, r *http.Request) {
	f := database.TaskFilter{}

	if s := r.URL.Query().Get("status"); s != "" {
		f.Status = s
	}
	if p := r.URL.Query().Get("priority"); p != "" {
		f.Priority = p
	}
	if pid := r.URL.Query().Get("project_id"); pid != "" {
		if n, err := strconv.ParseInt(pid, 10, 64); err == nil {
			f.ProjectID = &n
		}
	}
	if db := r.URL.Query().Get("due_before"); db != "" {
		if t, err := parseFlexibleTime(db); err == nil {
			f.DueBefore = &t
		}
	}
	if da := r.URL.Query().Get("due_after"); da != "" {
		if t, err := parseFlexibleTime(da); err == nil {
			f.DueAfter = &t
		}
	}
	if s := r.URL.Query().Get("search"); s != "" {
		f.Search = s
	}

	result, err := a.taskService.ListAll(r.Context(), f)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"tasks":   result.Tasks,
		"summary": result.Summary,
	})
}

func (a *API) ListProjectTasks(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	tasks, err := a.taskService.ListByProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, tasks)
}

// recordTaskHistory creates a task history entry and broadcasts it via WebSocket.
func (a *API) recordTaskHistory(ctx context.Context, taskID, projectID int64, eventType string, details map[string]interface{}, actor string, sessionID string) {
	detailsJSON, _ := json.Marshal(details)
	h := &database.TaskHistory{
		TaskID:    taskID,
		ProjectID: projectID,
		EventType: eventType,
		Details:   string(detailsJSON),
		Actor:     actor,
	}
	if sessionID != "" {
		h.SessionID = sql.NullString{String: sessionID, Valid: true}
	}
	if err := a.db.CreateTaskHistory(ctx, h); err != nil {
		log.Printf("[TaskHistory] Failed to record %s for task %d: %v", eventType, taskID, err)
		return
	}
	a.hub.BroadcastStateUpdate("task_history", map[string]interface{}{
		"action":     "created",
		"task_id":    taskID,
		"project_id": projectID,
		"entry":      h,
	})
}

// RecordTaskHistory is the public version of recordTaskHistory for use from external packages.
func (a *API) RecordTaskHistory(ctx context.Context, taskID, projectID int64, eventType string, details map[string]interface{}, actor string, sessionID string) {
	a.recordTaskHistory(ctx, taskID, projectID, eventType, details, actor, sessionID)
}

func (a *API) CreateProjectTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Priority    string `json:"priority"`
		DueDate     string `json:"due_date"`
		ParentID    *int64 `json:"parent_id"`
		SortOrder   int    `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	task, err := a.taskService.Create(platformUIContext(r), application.CreateProjectTaskCommand{
		ProjectID: projectID, Title: input.Title, Description: input.Description,
		Status: input.Status, Priority: input.Priority, DueDate: input.DueDate,
		ParentID: input.ParentID, SortOrder: input.SortOrder, Actor: requestActor(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, task)
}

func (a *API) GetProjectTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := a.taskService.Get(r.Context(), projectID, taskID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, task)
}

func (a *API) UpdateProjectTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	var input struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Status      *string `json:"status"`
		Priority    *string `json:"priority"`
		DueDate     *string `json:"due_date"`
		ParentID    *int64  `json:"parent_id"`
		SortOrder   *int    `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	task, err := a.taskService.Update(r.Context(), application.UpdateProjectTaskCommand{
		ProjectID: projectID, TaskID: taskID, Title: input.Title, Description: input.Description,
		Status: input.Status, Priority: input.Priority, DueDate: input.DueDate,
		ParentID: input.ParentID, SortOrder: input.SortOrder, Actor: application.UserActor(),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, task)
}

func (a *API) UpdateTaskStatus(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	var input struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	task, err := a.taskService.ChangeStatus(r.Context(), application.ChangeTaskStatusCommand{
		ProjectID: projectID, TaskID: taskID, Status: input.Status, Actor: application.UserActor(),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, task)
}

// ApproveTaskVerification approves an awaiting_approval task, transitioning it to done.
func (a *API) ApproveTaskVerification(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := a.taskService.ApproveVerification(r.Context(), projectID, taskID, application.UserActor())
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, task)
}

// ApproveTaskVerificationBulk approves an explicit task list or every pending
// task in an optional project scope. Per-item failures do not roll back the
// approvals that succeeded.
func (a *API) ApproveTaskVerificationBulk(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TaskIDs    []int64 `json:"task_ids"`
		AllPending bool    `json:"all_pending"`
		ProjectID  *int64  `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	result, err := a.taskService.ApproveVerificationBulk(r.Context(), application.BulkApproveVerificationCommand{
		TaskIDs: input.TaskIDs, AllPending: input.AllPending, ProjectID: input.ProjectID,
		Actor: application.UserActor(), BulkID: fmt.Sprintf("ui:%x", time.Now().UTC().UnixNano()),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// RejectTaskVerification rejects an awaiting_approval task, returning it to in_progress.
func (a *API) RejectTaskVerification(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := a.taskService.RejectVerification(r.Context(), projectID, taskID, application.UserActor())
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, task)
}

// ReorderProjectTasks batch-updates sort_order and parent_id for multiple tasks.
func (a *API) ReorderProjectTasks(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var items []database.ReorderItem
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := a.taskService.ReorderProject(r.Context(), projectID, items); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ReorderAllTasks batch-updates global_sort_order for cross-project reordering.
func (a *API) ReorderAllTasks(w http.ResponseWriter, r *http.Request) {
	var items []database.GlobalReorderItem
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := a.taskService.ReorderGlobal(r.Context(), items); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) DeleteProjectTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	if err := a.taskService.Delete(r.Context(), projectID, taskID); err != nil {
		respondApplicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) DuplicateProjectTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	clone, err := a.taskService.Duplicate(r.Context(), projectID, taskID, application.UserActor())
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, clone)
}

// parseFlexibleTime parses time strings in multiple formats.
func parseFlexibleTime(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %s", s)
}

// GetSessionLinkedTask returns the primary task linked to a session
func (a *API) GetSessionLinkedTask(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	task, err := a.taskService.GetSessionTask(r.Context(), sessionID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, task)
}

// LinkSessionTask links an active session to an existing task or creates a new task and links it
func (a *API) LinkSessionTask(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	var input struct {
		TaskID   *int64 `json:"task_id,omitempty"`
		TaskData *struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Priority    string `json:"priority"`
		} `json:"task_data,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	command := application.LinkSessionTaskCommand{SessionID: sessionID, Actor: application.UserActor()}
	if input.TaskID != nil {
		command.TaskID = input.TaskID
	}
	if input.TaskData != nil {
		command.Title = input.TaskData.Title
		command.Description = input.TaskData.Description
		command.Priority = input.TaskData.Priority
	}
	result, err := a.taskService.LinkSession(r.Context(), command)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"task":         result.Task,
		"session_name": result.SessionName,
	})
}

// UnlinkSessionTask removes the task link from a session.
func (a *API) UnlinkSessionTask(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	result, err := a.taskService.UnlinkSession(r.Context(), sessionID, application.UserActor())
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"unlinked_task_id": result.TaskID,
		"session_name":     result.SessionName,
	})
}

// ListTaskHistory returns activity history for a task.
func (a *API) ListTaskHistory(w http.ResponseWriter, r *http.Request) {
	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	history, err := a.taskService.ListHistory(r.Context(), taskID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, history)
}

// AddTaskComment adds a user comment to a task's history.
func (a *API) AddTaskComment(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}
	var input struct {
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Comment is required")
		return
	}
	if err := a.taskService.AddComment(r.Context(), projectID, taskID, input.Comment, application.UserActor()); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

// ListTaskDocuments returns all documents associated with a task (temp_documents + session plans).
func (a *API) ListTaskDocuments(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}
	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}

	type docEntry struct {
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		Type      string    `json:"type"` // "document" or "plan"
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}

	var results []docEntry

	// 1. Temp documents linked to this task
	docs, _ := a.db.ListDocumentsByTask(r.Context(), taskID)
	for _, d := range docs {
		results = append(results, docEntry{
			ID:        d.ID,
			Title:     d.Title,
			Type:      "document",
			Status:    d.Status,
			CreatedAt: d.CreatedAt,
		})
	}

	// 2. Session plans from linked sessions
	sessions, _ := a.db.GetSessionsForTask(r.Context(), taskID)
	for _, s := range sessions {
		if s.PlanContent != "" {
			results = append(results, docEntry{
				ID:        "plan:" + s.ID,
				Title:     "Plano: " + s.Name,
				Type:      "plan",
				Status:    s.Status,
				CreatedAt: s.StartTime,
			})
		}
	}

	if results == nil {
		results = []docEntry{}
	}
	respondJSON(w, http.StatusOK, results)
}

// TriggerSessionEvaluation triggers an AI evaluation for the current session
func (a *API) TriggerSessionEvaluation(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "id")
	if _, err := services.Execution.Sessions.Evaluate(platformUIContext(r), application.EvaluateSessionCommand{SessionID: sessionID, Authorization: platformUIAuthorization(r)}); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "AI evaluation triggered",
	})
}

// ListTaskSessions returns all sessions linked to a specific task
func (a *API) ListTaskSessions(w http.ResponseWriter, r *http.Request) {
	taskID, err := strconv.ParseInt(chi.URLParam(r, "taskId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	sessions, err := a.db.GetSessionsForTask(r.Context(), taskID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to fetch sessions")
		return
	}

	respondJSON(w, http.StatusOK, sessions)
}

// GetTaskSessionSummary returns session counts per task for a project (batch)
func (a *API) GetTaskSessionSummary(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	summary, err := a.db.GetTaskSessionSummary(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to fetch session summary")
		return
	}

	respondJSON(w, http.StatusOK, summary)
}

func (a *API) GetAllTaskSessionSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := a.db.GetAllTaskSessionSummary(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to fetch session summary")
		return
	}

	respondJSON(w, http.StatusOK, summary)
}

// SendSessionInput sends text to a running session's terminal PTY.
func (a *API) SendSessionInput(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	var input struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if input.Text == "" {
		respondError(w, http.StatusBadRequest, "text is required")
		return
	}

	if err := services.Execution.Sessions.SendInput(platformUIContext(r), application.SendSessionInputCommand{SessionID: id, Text: input.Text, Authorization: platformUIAuthorization(r)}); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// SetSessionModel changes the model used by future turns of an active session.
func (a *API) SetSessionModel(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var input struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	updated, err := services.Execution.Sessions.SetModel(platformUIContext(r), application.SetSessionModelCommand{
		SessionID: id, Model: input.Model, Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{
		"session_id":      updated.ID,
		"model":           updated.Model,
		"requested_model": updated.RequestedModel,
		"effort":          updated.Effort,
		"harness":         updated.Harness,
	})
}

// SetSessionEffort changes the reasoning/thinking level used by future turns.
func (a *API) SetSessionEffort(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var input struct {
		Effort string `json:"effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	updated, err := services.Execution.Sessions.SetEffort(platformUIContext(r), application.SetSessionEffortCommand{
		SessionID: id, Effort: input.Effort, Authorization: platformUIAuthorization(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{
		"session_id":      updated.ID,
		"model":           updated.Model,
		"requested_model": updated.RequestedModel,
		"effort":          updated.Effort,
		"harness":         updated.Harness,
	})
}

func respondSessionSettingError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, session.ErrInvalidSessionSetting):
		status = http.StatusBadRequest
	case errors.Is(err, session.ErrSessionNotRunning):
		status = http.StatusConflict
	case errors.Is(err, session.ErrSessionSettingUnsupported):
		status = http.StatusUnprocessableEntity
	}
	respondError(w, status, err.Error())
}

// GenerateMCPAPIKey generates a new random API key for MCP HTTP access.
func (a *API) GenerateMCPAPIKey(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	status, err := services.Configuration.Configuration.GenerateMCPAPIKey(platformUIContext(r), platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{
		"key":    status.Secret,
		"status": "generated",
	})
}

// GetMCPAPIKeyStatus returns whether an API key exists and its prefix.
func (a *API) GetMCPAPIKeyStatus(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	status, err := services.Configuration.Configuration.MCPAPIKeyStatus(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"exists": status.Exists,
		"prefix": status.Preview,
	})
}

// RevokeMCPAPIKey removes the MCP API key.
func (a *API) RevokeMCPAPIKey(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	if err := services.Configuration.Configuration.RevokeMCPAPIKey(platformUIContext(r), platformUIAuthorization(r)); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// GetToolPolicies returns the global tool policies for each context.
func (a *API) GetToolPolicies(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	policies, err := services.Configuration.Configuration.ToolPolicies(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, policies)
}

// UpdateToolPolicies updates the global tool policies.
func (a *API) UpdateToolPolicies(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input map[string]string
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := services.Configuration.Configuration.UpdateToolPolicies(platformUIContext(r), platformUIAuthorization(r), input); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// GetProjectToolPolicy returns the tool policy for a specific project.
func (a *API) GetProjectToolPolicy(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	policy, err := services.Configuration.Configuration.ProjectToolPolicy(platformUIContext(r), id)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"project_id":  fmt.Sprintf("%d", id),
		"tool_policy": policy,
	})
}

// UpdateProjectToolPolicy updates the tool policy for a specific project.
func (a *API) UpdateProjectToolPolicy(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		ToolPolicy string `json:"tool_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := services.Configuration.Configuration.UpdateProjectToolPolicy(platformUIContext(r), platformUIAuthorization(r), id, input.ToolPolicy); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// --- Project Shares ---

// GetProjectShares returns the list of projects that a given project has read access to.
func (a *API) GetProjectShares(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	shares, err := services.Configuration.Configuration.ProjectShares(platformUIContext(r), id)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, shares)
}

// UpdateProjectShares replaces the sharing relationships for a project.
func (a *API) UpdateProjectShares(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		SharedProjectIDs []int64 `json:"shared_project_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := services.Configuration.Configuration.ReplaceProjectShares(platformUIContext(r), id, input.SharedProjectIDs); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Tags ---

type tagInfo struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Tag CRUD

func (a *API) ListAllTags(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	tags, err := services.Configuration.Tags.List(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	result := make([]tagInfo, len(tags))
	for i, t := range tags {
		result[i] = tagInfo{ID: t.ID, Name: t.Name, Color: t.Color}
	}
	respondJSON(w, http.StatusOK, result)
}

func (a *API) CreateTag(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	var input struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	tag, err := services.Configuration.Tags.Create(platformUIContext(r), input.Name, input.Color)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, tagInfo{ID: tag.ID, Name: tag.Name, Color: tag.Color})
}

func (a *API) UpdateTag(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid tag ID")
		return
	}

	var input struct {
		Name  *string `json:"name"`
		Color *string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	tag, err := services.Configuration.Tags.Update(platformUIContext(r), application.UpdateTagCommand{ID: id, Name: input.Name, Color: input.Color})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, tagInfo{ID: tag.ID, Name: tag.Name, Color: tag.Color})
}

func (a *API) DeleteTagHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid tag ID")
		return
	}
	if err := services.Configuration.Tags.Delete(platformUIContext(r), id); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Project tag assignments

func (a *API) GetProjectTags(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	tags, err := services.Configuration.Tags.ListProject(platformUIContext(r), id)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	result := make([]tagInfo, len(tags))
	for i, t := range tags {
		result[i] = tagInfo{ID: t.TagID, Name: t.Name, Color: t.Color}
	}
	respondJSON(w, http.StatusOK, result)
}

func (a *API) UpdateProjectTags(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		TagIDs []int64 `json:"tag_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := services.Configuration.Tags.ReplaceProject(platformUIContext(r), id, input.TagIDs); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Project Skills ---

// GetProjectSkills returns global skills with per-project config + project-specific skills.
func (a *API) GetProjectSkills(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := platformUIContext(r)
	project, err := services.Configuration.Projects.Get(ctx, projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	// Get all global skills
	allSkills, err := services.Configuration.Skills.List(ctx)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	// Get per-project overrides
	configs, err := services.Configuration.Skills.ListProjectConfig(ctx, projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	configMap := make(map[int64]bool) // skillID -> enabled
	for _, c := range configs {
		configMap[c.SkillID] = c.Enabled
	}

	// Build global skills with project-specific enabled state
	type globalSkillInfo struct {
		database.Skill
		ProjectEnabled bool `json:"project_enabled"` // effective enabled state for this project
	}
	globalSkills := make([]globalSkillInfo, 0, len(allSkills))
	for _, s := range allSkills {
		projectEnabled := s.Enabled // default: inherit global
		if project.SkillPolicy == "custom" {
			if override, ok := configMap[s.ID]; ok {
				projectEnabled = override
			} else {
				projectEnabled = false // custom mode: not configured = disabled
			}
		}
		globalSkills = append(globalSkills, globalSkillInfo{
			Skill:          s,
			ProjectEnabled: projectEnabled,
		})
	}

	// Get project-specific skills
	projectSkills, err := services.Configuration.Skills.ListProjectSkills(ctx, projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"skill_policy":   project.SkillPolicy,
		"global_skills":  globalSkills,
		"project_skills": projectSkills,
	})
}

// SaveProjectSkillConfig saves per-project skill overrides (inherit or custom).
func (a *API) SaveProjectSkillConfig(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		Inherit   bool `json:"inherit"`
		Overrides []struct {
			SkillID int64 `json:"skill_id"`
			Enabled bool  `json:"enabled"`
		} `json:"overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	configs := make(map[int64]bool, len(input.Overrides))
	for _, override := range input.Overrides {
		configs[override.SkillID] = override.Enabled
	}
	if err := services.Configuration.Skills.SetProjectConfig(platformUIContext(r), projectID, input.Inherit, configs); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// CreateProjectSkillHandler creates a project-specific skill.
func (a *API) CreateProjectSkillHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		Name     string `json:"name"`
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	ps, err := services.Configuration.Skills.CreateProjectSkill(platformUIContext(r), projectID, application.SkillInput{Name: input.Name, Content: input.Content, Enabled: true, Category: input.Category})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusCreated, ps)
}

// UpdateProjectSkillHandler updates a project-specific skill.
func (a *API) UpdateProjectSkillHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	skillID, err := strconv.ParseInt(chi.URLParam(r, "skillId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	var input struct {
		Name     *string `json:"name"`
		Content  *string `json:"content"`
		Enabled  *bool   `json:"enabled"`
		Category *string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	ps, err := services.Configuration.Skills.UpdateProjectSkill(platformUIContext(r), projectID, application.UpdateSkillCommand{ID: skillID, Name: input.Name, Content: input.Content, Enabled: input.Enabled, Category: input.Category})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, ps)
}

// DeleteProjectSkillHandler deletes a project-specific skill.
func (a *API) DeleteProjectSkillHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	skillID, err := strconv.ParseInt(chi.URLParam(r, "skillId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	if err := services.Configuration.Skills.DeleteProjectSkill(platformUIContext(r), projectID, skillID); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ============ Project MCP Servers ============

// GetProjectMCPServers returns global MCP servers + project-specific MCP servers.
func (a *API) GetProjectMCPServers(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := platformUIContext(r)
	globalServers, err := services.Configuration.MCP.ListGlobal(ctx)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	projectServers, err := services.Configuration.MCP.ListProject(ctx, projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"global_mcp_servers":  globalServers,
		"project_mcp_servers": projectServers,
	})
}

// CreateProjectMCPServerHandler creates a project-specific MCP server.
func (a *API) CreateProjectMCPServerHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	var input struct {
		Name    string `json:"name"`
		Command string `json:"command"`
		Args    string `json:"args"`
		Env     string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	m, err := services.Configuration.MCP.CreateProject(platformUIContext(r), platformUIAuthorization(r), projectID, application.MCPServerInput{Name: input.Name, Command: input.Command, Args: input.Args, Env: input.Env, Enabled: true})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusCreated, m)
}

// GetProjectMCPServerHandler returns a single project-specific MCP server.
func (a *API) GetProjectMCPServerHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	mcpID, err := strconv.ParseInt(chi.URLParam(r, "mcpId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	servers, err := services.Configuration.MCP.ListProject(platformUIContext(r), projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	for i := range servers {
		if servers[i].ID == mcpID {
			respondJSON(w, http.StatusOK, servers[i])
			return
		}
	}
	respondError(w, http.StatusNotFound, "Project MCP server not found")
}

// UpdateProjectMCPServerHandler updates a project-specific MCP server.
func (a *API) UpdateProjectMCPServerHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	mcpID, err := strconv.ParseInt(chi.URLParam(r, "mcpId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	var input struct {
		Name    *string `json:"name"`
		Command *string `json:"command"`
		Args    *string `json:"args"`
		Env     *string `json:"env"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	m, err := services.Configuration.MCP.UpdateProject(platformUIContext(r), platformUIAuthorization(r), projectID, application.UpdateMCPServerCommand{ID: mcpID, Name: input.Name, Command: input.Command, Args: input.Args, Env: input.Env, Enabled: input.Enabled})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, m)
}

// DeleteProjectMCPServerHandler deletes a project-specific MCP server.
func (a *API) DeleteProjectMCPServerHandler(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	mcpID, err := strconv.ParseInt(chi.URLParam(r, "mcpId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid MCP server ID")
		return
	}

	if err := services.Configuration.MCP.DeleteProject(platformUIContext(r), platformUIAuthorization(r), projectID, mcpID); err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Project Custom Tools ---

func (a *API) ListProjectTools(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	tools, err := services.Configuration.CustomTools.List(platformUIContext(r), projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, tools)
}

func (a *API) CreateProjectTool(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Command     string `json:"command"`
		Parameters  string `json:"parameters"`
		Confirm     bool   `json:"confirm"`
		WorkingDir  string `json:"working_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	t, err := services.Configuration.CustomTools.Create(platformUIContext(r), platformUIAuthorization(r), projectID, application.CustomToolInput{Name: input.Name, Description: input.Description, Command: input.Command, Parameters: input.Parameters, Confirm: input.Confirm, WorkingDir: input.WorkingDir, Enabled: true})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, t)
}

func (a *API) UpdateProjectTool(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	toolID, err := strconv.ParseInt(chi.URLParam(r, "toolId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid tool ID")
		return
	}

	var input map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	command := application.UpdateCustomToolCommand{ID: toolID}
	if value, exists := input["name"].(string); exists {
		command.Name = &value
	}
	if value, exists := input["description"].(string); exists {
		command.Description = &value
	}
	if value, exists := input["command"].(string); exists {
		command.Command = &value
	}
	if value, exists := input["parameters"].(string); exists {
		command.Parameters = &value
	}
	if value, exists := input["confirm"].(bool); exists {
		command.Confirm = &value
	}
	if value, exists := input["working_dir"].(string); exists {
		command.WorkingDir = &value
	}
	if value, exists := input["enabled"].(bool); exists {
		command.Enabled = &value
	}
	t, err := services.Configuration.CustomTools.Update(platformUIContext(r), platformUIAuthorization(r), projectID, command)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, t)
}

func (a *API) DeleteProjectTool(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}
	toolID, err := strconv.ParseInt(chi.URLParam(r, "toolId"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid tool ID")
		return
	}

	if err := services.Configuration.CustomTools.Delete(platformUIContext(r), platformUIAuthorization(r), projectID, toolID); err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetResolvedTools returns the effective list of tools available for a given context.
// For sessions: GET /api/sessions/{id}/tools
// For projects: GET /api/projects/{id}/tools
func (a *API) GetResolvedProjectTools(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := platformUIContext(r)
	allTools := mcp.AllTools()

	// Global session policy (sessions default to deny_all)
	globalPolicy := mcp.ToolPolicy{Mode: "deny_all"}
	policies, err := services.Configuration.Configuration.ToolPolicies(ctx)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	if policyStr := policies["session"]; policyStr != "" {
		globalPolicy = mcp.ParsePolicy(policyStr)
	}

	// Project policy
	projectPolicy, err := services.Configuration.Configuration.ProjectToolPolicy(ctx, projectID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	effectivePolicy := mcp.ResolveProjectPolicy(globalPolicy, projectPolicy)

	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	result := make([]toolInfo, 0, len(allTools))
	for _, t := range allTools {
		result = append(result, toolInfo{
			Name:        t.Name,
			Description: t.Description,
			Enabled:     effectivePolicy.IsAllowed(t.Name),
		})
	}
	respondJSON(w, http.StatusOK, result)
}

func (a *API) GetResolvedSessionTools(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "id")

	ctx := platformUIContext(r)
	sess, err := services.Execution.SessionQueries.GetSession(ctx, sessionID)
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	allTools := mcp.AllTools()

	// Global session policy (sessions default to deny_all)
	globalPolicy := mcp.ToolPolicy{Mode: "deny_all"}
	policies, err := services.Configuration.Configuration.ToolPolicies(ctx)
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	if policyStr := policies["session"]; policyStr != "" {
		globalPolicy = mcp.ParsePolicy(policyStr)
	}

	// Resolve: project policy overrides global if set, otherwise inherit global
	var projectPolicyJSON string
	if sess.ProjectID > 0 {
		projectPolicy, projectErr := services.Configuration.Configuration.ProjectToolPolicy(ctx, sess.ProjectID)
		if projectErr == nil {
			projectPolicyJSON = projectPolicy
		}
	}
	effectivePolicy := mcp.ResolveProjectPolicy(globalPolicy, projectPolicyJSON)

	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	result := make([]toolInfo, 0, len(allTools))
	for _, t := range allTools {
		result = append(result, toolInfo{
			Name:        t.Name,
			Description: t.Description,
			Enabled:     effectivePolicy.IsAllowed(t.Name),
		})
	}
	respondJSON(w, http.StatusOK, result)
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

// GetTokenUsageSummary handles GET /api/token-usage/summary
// Query params: since (ISO date), project_id (optional), days (7|30|90, default 30)
func (a *API) GetTokenUsageSummary(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	ctx := platformUIContext(r)
	queries, ok := services.Collaboration.TokenUsageQueries.(interface {
		GetTokenUsageSummary(context.Context, time.Time, *int64) (*database.TokenUsageSummary, error)
		GetTokenUsageBySource(context.Context, time.Time, *int64) ([]database.TokenUsageBySource, error)
		GetTokenUsageByModel(context.Context, time.Time, *int64) ([]database.TokenUsageByModel, error)
		GetTokenUsageDaily(context.Context, time.Time, *int64) ([]database.TokenUsageDaily, error)
		GetTokenUsageByProject(context.Context, time.Time) ([]database.TokenUsageByProject, error)
		GetTokenUsageByProjectModel(context.Context, time.Time) ([]database.TokenUsageByProjectModel, error)
		GetTokenUsageByAISubcategory(context.Context, time.Time) ([]database.TokenUsageBySubcategory, error)
	})
	if !ok {
		respondError(w, http.StatusServiceUnavailable, "Application services unavailable")
		return
	}

	// Parse time range
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	since := time.Now().AddDate(0, 0, -days)
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		}
	}

	// Parse optional project_id
	var projectID *int64
	if pid := r.URL.Query().Get("project_id"); pid != "" {
		if n, err := strconv.ParseInt(pid, 10, 64); err == nil {
			projectID = &n
		}
	}

	// Fetch all data in parallel-ish (sequential but fast on SQLite)
	summary, err := queries.GetTokenUsageSummary(ctx, since, projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get token usage summary")
		return
	}

	bySource, err := queries.GetTokenUsageBySource(ctx, since, projectID)
	if err != nil {
		log.Printf("[API] GetTokenUsageBySource error: %v", err)
	}
	byModel, err := queries.GetTokenUsageByModel(ctx, since, projectID)
	if err != nil {
		log.Printf("[API] GetTokenUsageByModel error: %v", err)
	}
	daily, err := queries.GetTokenUsageDaily(ctx, since, projectID)
	if err != nil {
		log.Printf("[API] GetTokenUsageDaily error: %v", err)
	}

	// Build per-project and per-AI-subcategory breakdowns (only when no project filter)
	var byProject []database.TokenUsageByProject
	var byProjectModel []database.TokenUsageByProjectModel
	var byAISubcategory []database.TokenUsageBySubcategory
	if projectID == nil {
		byProject, err = queries.GetTokenUsageByProject(ctx, since)
		if err != nil {
			log.Printf("[API] GetTokenUsageByProject error: %v", err)
		}
		byProjectModel, err = queries.GetTokenUsageByProjectModel(ctx, since)
		if err != nil {
			log.Printf("[API] GetTokenUsageByProjectModel error: %v", err)
		}
		byAISubcategory, err = queries.GetTokenUsageByAISubcategory(ctx, since)
		if err != nil {
			log.Printf("[API] GetTokenUsageByAISubcategory error: %v", err)
		}
	}

	result := map[string]interface{}{
		"period_start": since.Format(time.RFC3339),
		"period_days":  days,
		"summary":      summary,
		"by_source":    bySource,
		"by_model":     byModel,
		"daily":        daily,
	}
	if byProject != nil {
		result["by_project"] = byProject
	}
	if byProjectModel != nil {
		result["by_project_model"] = byProjectModel
	}
	if byAISubcategory != nil {
		result["by_ai_subcategory"] = byAISubcategory
	}

	respondJSON(w, http.StatusOK, result)
}

// ClearTokenUsage handles DELETE /api/token-usage
func (a *API) ClearTokenUsage(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	deleted, err := services.Collaboration.TokenUsage.Clear(platformUIContext(r), platformUIAuthorization(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deleted": deleted,
	})
}

// SeedTokenUsage handles POST /api/test/seed-token-usage (test mode only).
// Inserts a token_usage record directly into the database for testing purposes.
func (a *API) SeedTokenUsage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source              string  `json:"source"`
		Subcategory         string  `json:"subcategory"`
		ProjectID           *int64  `json:"project_id"`
		SessionID           *string `json:"session_id"`
		ConversationID      *int64  `json:"conversation_id"`
		Model               string  `json:"model"`
		InputTokens         int     `json:"input_tokens"`
		OutputTokens        int     `json:"output_tokens"`
		CacheReadTokens     int     `json:"cache_read_tokens"`
		CacheCreationTokens int     `json:"cache_creation_tokens"`
		CostUSD             float64 `json:"cost_usd"`
		CreatedAt           string  `json:"created_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if req.Source == "" {
		req.Source = "ai_assistant"
	}
	if req.Model == "" {
		req.Model = "test-model"
	}

	tu := &database.TokenUsage{
		Source:              req.Source,
		Subcategory:         req.Subcategory,
		Model:               req.Model,
		InputTokens:         req.InputTokens,
		OutputTokens:        req.OutputTokens,
		CacheReadTokens:     req.CacheReadTokens,
		CacheCreationTokens: req.CacheCreationTokens,
		CostUSD:             req.CostUSD,
	}
	if req.ProjectID != nil {
		tu.ProjectID = sql.NullInt64{Int64: *req.ProjectID, Valid: true}
	}
	if req.SessionID != nil {
		tu.SessionID = sql.NullString{String: *req.SessionID, Valid: true}
	}
	if req.ConversationID != nil {
		tu.ConversationID = sql.NullInt64{Int64: *req.ConversationID, Valid: true}
	}

	if err := a.db.CreateTokenUsage(r.Context(), tu); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to seed token usage: "+err.Error())
		return
	}

	// If created_at was specified, update it (SQLite DEFAULT CURRENT_TIMESTAMP was used)
	if req.CreatedAt != "" {
		_, err := a.db.ExecContext(r.Context(), `UPDATE token_usage SET created_at = ? WHERE id = ?`, req.CreatedAt, tu.ID)
		if err != nil {
			log.Printf("[TEST] Failed to set created_at for seeded record: %v", err)
		}
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":      tu.ID,
		"message": "Token usage record seeded",
	})
}

// SetTunnelDeps sets the dependencies for dynamic tunnel client creation.
func (a *API) SetTunnelDeps(deps *TunnelDeps) {
	a.tunnelDeps = deps
}

// SetPairingManager sets the pairing manager reference.
func (a *API) SetPairingManager(pm *tunnel.PairingManager) {
	a.pairingMgr = pm
}

// GetTunnelStatus returns the current tunnel status.
func (a *API) GetTunnelStatus(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	status, err := services.Execution.Tunnel.OperationalTunnelStatus(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":       status.Status,
		"url":          status.PublicURL,
		"subdomain":    "",
		"device_count": status.DeviceCount,
		"relay_url":    DefaultRelayURL,
		"has_token":    status.HasToken,
	})
}

// EnableTunnel creates a tunnel client dynamically and connects it.
func (a *API) EnableTunnel(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	result, err := services.Execution.TunnelMutations.Enable(platformUIContext(r), application.EnableTunnelCommand{Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": result.Status, "url": result.PublicURL})
}

// DisableTunnel stops the tunnel connection.
func (a *API) DisableTunnel(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	result, err := services.Execution.TunnelMutations.Disable(platformUIContext(r), application.DisableTunnelCommand{Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": result.Status})
}

// AutoConnectTunnel is called on startup if tunnel_enabled=true.
func (a *API) AutoConnectTunnel() {
	a.tunnelMu.Lock()
	defer a.tunnelMu.Unlock()

	if a.tunnelClient != nil {
		return
	}

	client, err := a.createAndConnectTunnel(context.Background())
	if err != nil {
		log.Printf("[TUNNEL] Auto-connect failed: %v", err)
		return
	}
	a.tunnelClient = client
}

// DisconnectTunnel is called on shutdown.
func (a *API) DisconnectTunnel() {
	a.tunnelMu.Lock()
	defer a.tunnelMu.Unlock()

	if a.tunnelClient != nil {
		a.tunnelClient.Disconnect()
		a.tunnelClient = nil
	}
}

// createAndConnectTunnel creates a new tunnel client, auto-registers if needed, and connects.
// Must be called with a.tunnelMu held.
func (a *API) createAndConnectTunnel(ctx context.Context) (*tunnel.Client, error) {
	if a.tunnelDeps == nil {
		return nil, fmt.Errorf("tunnel dependencies not configured")
	}

	// Relay URL: build-time default only (injected via -ldflags)
	relayURL := DefaultRelayURL
	if relayURL == "" {
		return nil, fmt.Errorf("no relay URL configured (build with -ldflags)")
	}

	// Read token from DB (decrypted)
	relayToken, _ := a.GetDecryptedSetting(ctx, "tunnel_relay_token")

	// Auto-register if no token
	if relayToken == "" {
		token, err := a.autoRegisterTunnel(ctx, relayURL)
		if err != nil {
			return nil, fmt.Errorf("auto-registration failed: %w", err)
		}
		relayToken = token
	}

	// Create client
	client := tunnel.NewClient(relayURL, relayToken, fmt.Sprintf("localhost:%d", a.tunnelDeps.Port))
	client.OnStatus = func(status, url string) {
		a.tunnelDeps.Hub.BroadcastStateUpdate("tunnel", map[string]string{"status": status, "url": url})
		log.Printf("[TUNNEL] Status: %s, URL: %s", status, url)
	}

	// On auth failure, clear old token and re-register automatically
	client.OnAuthFailed = func() (string, error) {
		ctx := context.Background()
		// Clear old token
		a.db.SetSetting(ctx, "tunnel_relay_token", "")
		a.db.SetSetting(ctx, "tunnel_relay_token_iv", "")
		a.db.SetSetting(ctx, "tunnel_relay_token_preview", "")
		// Register new token
		return a.autoRegisterTunnel(ctx, relayURL)
	}

	// Set encryption key
	if a.tunnelDeps.PairingMgr != nil {
		if encKey, err := a.tunnelDeps.PairingMgr.GetEncryptionKeyRaw(); err == nil {
			client.SetEncryptionKey(encKey)
		}
	}

	if err := client.Connect(context.Background()); err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}

	return client, nil
}

// autoRegisterTunnel calls the relay's open registration endpoint to get a per-user token.
func (a *API) autoRegisterTunnel(ctx context.Context, relayURL string) (string, error) {
	token, err := tunnel.RegisterWithRelay(relayURL)
	if err != nil {
		return "", err
	}

	// Store token encrypted in DB
	cipher, iv, encErr := a.tunnelDeps.Encryptor.Encrypt(token)
	if encErr != nil {
		return "", fmt.Errorf("encrypt token: %w", encErr)
	}
	a.db.SetSetting(ctx, "tunnel_relay_token", cipher)
	a.db.SetSetting(ctx, "tunnel_relay_token_iv", iv)
	preview := token[:8] + "..."
	a.db.SetSetting(ctx, "tunnel_relay_token_preview", preview)

	log.Printf("[TUNNEL] Auto-registered with relay, token=%s", preview)
	return token, nil
}

// ListPairedDevices returns all paired devices.
func (a *API) ListPairedDevices(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	devices, err := services.Execution.Tunnel.ListPairedDevices(platformUIContext(r))
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, devices)
}

// RevokePairedDevice revokes a paired device by ID.
func (a *API) RevokePairedDevice(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "Missing device ID")
		return
	}

	result, err := services.Execution.TunnelMutations.RevokeDevice(platformUIContext(r), application.RevokeTunnelDeviceCommand{DeviceID: id, Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": result.Status})
}

// DeletePairedDevice permanently deletes a paired device by ID.
func (a *API) DeletePairedDevice(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "Missing device ID")
		return
	}

	result, err := services.Execution.TunnelMutations.DeleteDevice(platformUIContext(r), application.DeleteTunnelDeviceCommand{DeviceID: id, Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": result.Status})
}

// ConfirmPairing validates a 6-digit code entered by the local admin to pair a remote device.
func (a *API) ConfirmPairing(w http.ResponseWriter, r *http.Request) {
	services, ok := requirePlatformApplicationServices(a, w)
	if !ok {
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	result, err := services.Execution.TunnelMutations.ConfirmPairing(platformUIContext(r), application.ConfirmTunnelPairingCommand{Code: body.Code, Authorization: platformUIAuthorization(r)})
	if err != nil {
		respondApplicationError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"device_id": result.DeviceID,
	})
}

// CreateMissionGrant (Phase 7.5): the human pre-authorizes a destructive
// capability for a mission with bounded uses and TTL — the coordinator's merge
// gate spends these instead of per-command approval tokens. UI-credentialed
// mutation (same trust boundary as project edits).
func (a *API) CreateMissionGrant(w http.ResponseWriter, r *http.Request) {
	missionID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || missionID <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid mission id")
		return
	}
	var input struct {
		Capability       string `json:"capability"`
		MaxUses          int    `json:"max_uses"`
		ExpiresInMinutes int    `json:"expires_in_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	input.Capability = strings.TrimSpace(input.Capability)
	if input.Capability != "workspaces.merge" {
		respondError(w, http.StatusBadRequest, "Only workspaces.merge grants are supported")
		return
	}
	if input.MaxUses <= 0 || input.MaxUses > 100 {
		respondError(w, http.StatusBadRequest, "max_uses must be between 1 and 100")
		return
	}
	if input.ExpiresInMinutes <= 0 || input.ExpiresInMinutes > 24*60 {
		respondError(w, http.StatusBadRequest, "expires_in_minutes must be between 1 and 1440")
		return
	}
	// HUMAN authority only: mission grants exist precisely so a human stays in
	// the loop. A session bearer (the coordinator itself, or any worker) must
	// never mint its own merge authority — that is the deny_self_grant stance.
	authorization := platformUIAuthorization(r)
	if !authorization.Approved || authorization.Actor.Type == "session" {
		respondError(w, http.StatusForbidden, "Mission grants require an approved user credential (a session cannot grant itself authority)")
		return
	}
	mission, err := a.db.GetMission(r.Context(), missionID)
	if err != nil || mission == nil {
		respondError(w, http.StatusNotFound, "Mission not found")
		return
	}
	if mission.Status != "active" {
		respondError(w, http.StatusConflict, "Mission is not active — grants attach to running missions only")
		return
	}
	grant := &database.MissionGrant{
		MissionID: missionID, Capability: input.Capability,
		UsesRemaining: input.MaxUses,
		ExpiresAt:     time.Now().UTC().Add(time.Duration(input.ExpiresInMinutes) * time.Minute),
		GrantedBy:     application.EventActorValue(authorization.Actor),
	}
	if err := a.db.CreateMissionGrant(r.Context(), grant); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create mission grant")
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{
		"grant_id": grant.ID, "mission_id": missionID, "capability": grant.Capability,
		"uses_remaining": grant.UsesRemaining, "expires_at": grant.ExpiresAt,
	})
}

// ListMissions (Phase 7.6): the mission panel's index.
func (a *API) ListMissions(w http.ResponseWriter, r *http.Request) {
	missions, err := a.db.ListMissions(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to list missions")
		return
	}
	respondJSON(w, http.StatusOK, missions)
}

// GetMissionPanel (Phase 7.6): the single aggregated mission view — mission,
// worker roster with live session status, occupied worktrees, mission-linked
// documents, and the mission.* event timeline. Read-only, durable-backed.
func (a *API) GetMissionPanel(w http.ResponseWriter, r *http.Request) {
	missionID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || missionID <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid mission id")
		return
	}
	panel, err := a.db.GetMissionPanel(r.Context(), missionID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to load mission panel")
		return
	}
	if panel == nil {
		respondError(w, http.StatusNotFound, "Mission not found")
		return
	}
	respondJSON(w, http.StatusOK, panel)
}

// UpdateMissionStatusUI (Phase 7.6): end/transition a mission from the panel.
// Non-destructive — it only changes the mission's status. Sessions and
// worktrees are untouched; the productive cleanup (merging lanes, discarding
// worktrees, archiving) stays a coordinator-driven act. Approved user
// credential only (a session cannot drive its own mission's lifecycle).
func (a *API) UpdateMissionStatusUI(w http.ResponseWriter, r *http.Request) {
	missionID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || missionID <= 0 {
		respondError(w, http.StatusBadRequest, "Invalid mission id")
		return
	}
	var input struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	status := strings.TrimSpace(input.Status)
	switch status {
	case "active", "paused", "completed", "failed", "archived":
	default:
		respondError(w, http.StatusBadRequest, "status must be active, paused, completed, failed or archived")
		return
	}
	authorization := platformUIAuthorization(r)
	if !authorization.Approved || authorization.Actor.Type == "session" {
		respondError(w, http.StatusForbidden, "Ending a mission requires an approved user credential")
		return
	}
	mission, err := a.db.GetMission(r.Context(), missionID)
	if err != nil || mission == nil {
		respondError(w, http.StatusNotFound, "Mission not found")
		return
	}
	if err := a.db.UpdateMissionStatus(r.Context(), missionID, status); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to update mission status")
		return
	}
	_ = a.db.AppendMissionEvent(r.Context(), "mission.status_changed", missionID, map[string]any{
		"mission_id": missionID, "status": status, "by_actor": application.EventActorValue(authorization.Actor),
	})
	respondJSON(w, http.StatusOK, map[string]any{"mission_id": missionID, "status": status})
}
