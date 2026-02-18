package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"devmanager/internal/database"
	"devmanager/internal/files"
	"devmanager/internal/configsync"
	"devmanager/internal/llm"
	"devmanager/internal/mcp"
	"devmanager/internal/notifications"
	"devmanager/internal/security"
	"devmanager/internal/session"
	"devmanager/internal/websocket"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var (
	cryptoRandRead = rand.Read
	hexEncode      = hex.EncodeToString
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
	"anthropic_api_key": true,
	"openai_api_key":    true,
	"groq_api_key":      true,
	"ollama_api_key":    true,
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

// pendingSkillProposal holds a skill proposal awaiting user approval.
type pendingSkillProposal struct {
	ProjectID int64  `json:"project_id"`
	SkillName string `json:"skill_name"`
	Content   string `json:"content"`
	Category  string `json:"category"`
	Action    string `json:"action"` // "create" or "update"
	SkillID   int64  `json:"skill_id,omitempty"` // for updates
}

type API struct {
	db           *database.DB
	hub          *websocket.Hub
	sessionMgr   *session.Manager
	configSync   *configsync.ConfigSyncer
	encryptor    *security.Encryptor
	notifService *notifications.Service
	hookHandler  *HookHandler
	aiHandler    *AIHandler
	otelHandler  *OTELHandler

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
}

// SetAIHandler sets the AI handler for AI chat functionality.
func (a *API) SetAIHandler(h *AIHandler) {
	a.aiHandler = h
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
	docID := chi.URLParam(r, "docId")

	a.pendingMemoryDocsMu.Lock()
	pending, ok := a.pendingMemoryDocs[docID]
	if ok {
		delete(a.pendingMemoryDocs, docID)
	}
	a.pendingMemoryDocsMu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending memory doc edit found for this document")
		return
	}

	a.db.UpdateTempDocumentStatus(r.Context(), docID, "approved")

	project, err := a.db.GetProject(r.Context(), pending.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	doc, err := a.db.UpsertMemoryDoc(r.Context(), pending.ProjectID, pending.Content, "ai", pending.Summary)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Write back to project's CLAUDE.md
	a.writeClaudeMD(project, pending.Content)

	a.hub.BroadcastStateUpdate("memory_doc", map[string]interface{}{
		"action":     "updated",
		"project_id": pending.ProjectID,
		"version":    doc.Version,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "approved",
		"project_id": pending.ProjectID,
		"version":    doc.Version,
	})
}

// RejectMemoryDoc discards a pending memory doc edit.
func (a *API) RejectMemoryDoc(w http.ResponseWriter, r *http.Request) {
	docID := chi.URLParam(r, "docId")

	a.pendingMemoryDocsMu.Lock()
	_, ok := a.pendingMemoryDocs[docID]
	if ok {
		delete(a.pendingMemoryDocs, docID)
	}
	a.pendingMemoryDocsMu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending memory doc edit found for this document")
		return
	}

	a.db.UpdateTempDocumentStatus(r.Context(), docID, "rejected")

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
	docID := chi.URLParam(r, "docId")

	a.pendingTaskProposalsMu.Lock()
	pending, ok := a.pendingTaskProposals[docID]
	if ok {
		delete(a.pendingTaskProposals, docID)
	}
	a.pendingTaskProposalsMu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending task proposal found for this document")
		return
	}

	a.db.UpdateTempDocumentStatus(r.Context(), docID, "approved")

	created := 0
	updated := 0
	deleted := 0

	// Map action index (1-based) -> created task ID, for resolving parent_ref
	createdTaskIDs := make(map[int]int64)

	for i, action := range pending.Actions {
		switch action.Action {
		case "create":
			task := &database.ProjectTask{
				ProjectID:   action.ProjectID,
				Title:       action.Title,
				Description: action.Description,
				Status:      action.Status,
				Priority:    action.Priority,
				SortOrder:   action.SortOrder,
			}
			if task.Status == "" {
				task.Status = "todo"
			}
			if task.Priority == "" {
				task.Priority = "medium"
			}
			if action.DueDate != "" {
				t, err := parseFlexibleTime(action.DueDate)
				if err == nil {
					task.DueDate = sql.NullTime{Time: t, Valid: true}
				}
			}
			// Resolve parent: parent_ref (batch reference) takes priority over parent_id
			if action.ParentRef > 0 {
				if realID, ok := createdTaskIDs[action.ParentRef]; ok {
					task.ParentID = sql.NullInt64{Int64: realID, Valid: true}
				}
			} else if action.ParentID > 0 {
				task.ParentID = sql.NullInt64{Int64: action.ParentID, Valid: true}
			}
			if err := a.db.CreateTask(r.Context(), task); err != nil {
				log.Printf("[TaskProposal] Failed to create task '%s': %v", action.Title, err)
				continue
			}
			createdTaskIDs[i+1] = task.ID // Store with 1-based index
			a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "created", "project_id": action.ProjectID, "task": task})
			created++

		case "update":
			task, err := a.db.GetTask(r.Context(), action.TaskID)
			if err != nil {
				log.Printf("[TaskProposal] Task %d not found for update: %v", action.TaskID, err)
				continue
			}
			if action.Title != "" {
				task.Title = action.Title
			}
			if action.Description != "" {
				task.Description = action.Description
			}
			if action.Status != "" {
				task.Status = action.Status
			}
			if action.Priority != "" {
				task.Priority = action.Priority
			}
			if action.DueDate != "" {
				t, err := parseFlexibleTime(action.DueDate)
				if err == nil {
					task.DueDate = sql.NullTime{Time: t, Valid: true}
				}
			}
			if err := a.db.UpdateTask(r.Context(), task); err != nil {
				log.Printf("[TaskProposal] Failed to update task %d: %v", action.TaskID, err)
				continue
			}
			a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": action.ProjectID, "task": task})
			updated++

		case "delete":
			if err := a.db.DeleteTask(r.Context(), action.TaskID); err != nil {
				log.Printf("[TaskProposal] Failed to delete task %d: %v", action.TaskID, err)
				continue
			}
			a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "deleted", "project_id": action.ProjectID, "id": action.TaskID})
			deleted++
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "approved",
		"created": created,
		"updated": updated,
		"deleted": deleted,
	})
}

// RejectTaskProposal discards a pending task proposal.
func (a *API) RejectTaskProposal(w http.ResponseWriter, r *http.Request) {
	docID := chi.URLParam(r, "docId")

	a.pendingTaskProposalsMu.Lock()
	_, ok := a.pendingTaskProposals[docID]
	if ok {
		delete(a.pendingTaskProposals, docID)
	}
	a.pendingTaskProposalsMu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending task proposal found for this document")
		return
	}

	a.db.UpdateTempDocumentStatus(r.Context(), docID, "rejected")

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
	docID := chi.URLParam(r, "docId")

	a.pendingSkillProposalsMu.Lock()
	pending, ok := a.pendingSkillProposals[docID]
	if ok {
		delete(a.pendingSkillProposals, docID)
	}
	a.pendingSkillProposalsMu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending skill proposal found for this document")
		return
	}

	a.db.UpdateTempDocumentStatus(r.Context(), docID, "approved")

	switch pending.Action {
	case "create":
		ps := &database.ProjectSkill{
			ProjectID: pending.ProjectID,
			Name:      pending.SkillName,
			Content:   pending.Content,
			Enabled:   true,
			Category:  pending.Category,
		}
		if err := a.db.CreateProjectSkill(r.Context(), ps); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to create project skill: %v", err))
			return
		}
		a.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{"action": "created", "project_id": pending.ProjectID, "skill": ps})
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "approved",
			"action":  "created",
			"skill":   ps,
		})

	case "update":
		ps, err := a.db.GetProjectSkill(r.Context(), pending.SkillID)
		if err != nil {
			respondError(w, http.StatusNotFound, "Project skill not found")
			return
		}
		if pending.SkillName != "" {
			ps.Name = pending.SkillName
		}
		if pending.Content != "" {
			ps.Content = pending.Content
		}
		if pending.Category != "" {
			ps.Category = pending.Category
		}
		if err := a.db.UpdateProjectSkill(r.Context(), ps); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to update project skill: %v", err))
			return
		}
		a.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{"action": "updated", "project_id": pending.ProjectID, "skill": ps})
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status": "approved",
			"action": "updated",
			"skill":  ps,
		})

	default:
		respondError(w, http.StatusBadRequest, "Unknown skill action: "+pending.Action)
	}
}

// RejectSkillProposal discards a pending skill proposal.
func (a *API) RejectSkillProposal(w http.ResponseWriter, r *http.Request) {
	docID := chi.URLParam(r, "docId")

	a.pendingSkillProposalsMu.Lock()
	_, ok := a.pendingSkillProposals[docID]
	if ok {
		delete(a.pendingSkillProposals, docID)
	}
	a.pendingSkillProposalsMu.Unlock()

	if !ok {
		respondError(w, http.StatusNotFound, "No pending skill proposal found for this document")
		return
	}

	a.db.UpdateTempDocumentStatus(r.Context(), docID, "rejected")

	respondJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// ProposeMemoryDoc creates a pending memory doc proposal for user approval.
// Called by the MCP tool instead of saving directly.
func (a *API) ProposeMemoryDoc(w http.ResponseWriter, r *http.Request) {
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

	project, err := a.db.GetProject(r.Context(), input.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	docID := uuid.New().String()[:8]
	convID, _ := strconv.ParseInt(input.ConversationID, 10, 64)
	tempDoc := &database.TempDocument{
		ID:             docID,
		Title:          fmt.Sprintf("Memory Doc: %s", project.Name),
		Content:        input.Content,
		ConversationID: sql.NullInt64{Int64: convID, Valid: convID > 0},
		Summary:        input.Summary,
	}
	if err := a.db.CreateTempDocument(r.Context(), tempDoc); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.storePendingMemoryDoc(docID, input.ProjectID, input.Content, input.Summary)

	// Notify the chat panel via WebSocket to inject an inline doc card
	// Include conversation_id so the frontend can match the correct chat session
	a.hub.BroadcastChatDocCard(map[string]interface{}{
		"doc_id":          docID,
		"type":            "proposal",
		"title":           fmt.Sprintf("Change proposal — %s", project.Name),
		"summary":         input.Summary,
		"conversation_id": input.ConversationID,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "pending_approval",
		"doc_id":  docID,
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
	return &API{
		db:           db,
		hub:          hub,
		sessionMgr:   sessionMgr,
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
		Name:       input.Name,
		Path:       input.Path,
		Type:       input.Type,
		ToolPolicy: input.ToolPolicy,
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
	project.ToolPolicy = input.ToolPolicy

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

func (a *API) DuplicateProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	original, err := a.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	// Decode optional overrides from request body
	var overrides database.ProjectInput
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&overrides)
	}

	newName := original.Name + " (Copy)"
	if overrides.Name != "" {
		newName = overrides.Name
	}

	clone := &database.Project{
		Name:                   newName,
		Path:                   original.Path,
		Type:                   original.Type,
		SSHHost:                original.SSHHost,
		SSHPort:                original.SSHPort,
		SSHUser:                original.SSHUser,
		SSHAuthType:            original.SSHAuthType,
		SSHCredentialEncrypted: original.SSHCredentialEncrypted,
		SSHCredentialIV:        original.SSHCredentialIV,
	}

	// Apply overrides
	if overrides.Path != "" {
		clone.Path = overrides.Path
	}
	if overrides.Type != "" {
		clone.Type = overrides.Type
	}
	if overrides.SSHHost != "" {
		clone.SSHHost = sql.NullString{String: overrides.SSHHost, Valid: true}
	}
	if overrides.SSHPort > 0 {
		clone.SSHPort = sql.NullInt64{Int64: int64(overrides.SSHPort), Valid: true}
	}
	if overrides.SSHUser != "" {
		clone.SSHUser = sql.NullString{String: overrides.SSHUser, Valid: true}
	}
	if overrides.SSHAuthType != "" {
		clone.SSHAuthType = sql.NullString{String: overrides.SSHAuthType, Valid: true}
	}
	// If a new credential is provided, encrypt it; otherwise keep the original
	if overrides.SSHCredential != "" {
		encrypted, iv, err := a.encryptor.Encrypt(overrides.SSHCredential)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to encrypt credentials")
			return
		}
		clone.SSHCredentialEncrypted = sql.NullString{String: encrypted, Valid: true}
		clone.SSHCredentialIV = sql.NullString{String: iv, Valid: true}
	}

	if err := a.db.CreateProject(r.Context(), clone); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project", map[string]interface{}{"action": "created", "project": clone})
	respondJSON(w, http.StatusCreated, clone)
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
		a.hub.BroadcastSyncProgress(id, "sync", "error", err.Error())
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.db.UpdateProjectConfigSyncedAt(r.Context(), id)
	a.hub.BroadcastSyncProgress(id, "sync", "done", "Configuration synced successfully")

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
	ProjectName          string  `json:"project_name"`
	ProjectType          string  `json:"project_type"`
	ProjectSSHHost       string  `json:"project_ssh_host,omitempty"`
	TaskTitle            string  `json:"task_title,omitempty"`
	TotalInputTokens     int64   `json:"total_input_tokens"`
	TotalOutputTokens    int64   `json:"total_output_tokens"`
	TotalCost            float64 `json:"total_cost"`
	HasPendingPermission bool    `json:"has_pending_permission"`
	ExecutionMode        string  `json:"execution_mode"`
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
		}

		details = append(details, d)
	}

	respondJSON(w, http.StatusOK, details)
}

func (a *API) CreateSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID int64             `json:"project_id"`
		TaskID    *int64            `json:"task_id,omitempty"`
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

	// Validate task if provided
	var linkedTask *database.ProjectTask
	if input.TaskID != nil && *input.TaskID > 0 {
		linkedTask, err = a.db.GetTask(r.Context(), *input.TaskID)
		if err != nil || linkedTask.ProjectID != project.ID {
			respondError(w, http.StatusBadRequest, "Task not found or belongs to different project")
			return
		}
	}

	// Auto-sync config (hooks, skills, mcp) to project before starting session
	if a.configSync != nil {
		if syncErr := a.configSync.SyncToProject(r.Context(), project); syncErr != nil {
			log.Printf("Warning: config sync failed before session start: %v", syncErr)
		}
	}

	// Inject task context into env vars
	if input.EnvVars == nil {
		input.EnvVars = make(map[string]string)
	}

	if linkedTask != nil {
		input.EnvVars["DEVMANAGER_TASK_ID"] = fmt.Sprintf("%d", linkedTask.ID)
		input.EnvVars["DEVMANAGER_TASK_TITLE"] = linkedTask.Title

		// Build system prompt so Claude Code starts with full task context
		description := linkedTask.Description
		if description == "" {
			description = "(no description provided)"
		}
		input.EnvVars["DEVMANAGER_APPEND_SYSTEM_PROMPT"] = fmt.Sprintf(
			"You have been assigned the following task by DevManager:\n\n"+
				"Title: %s\n\n"+
				"Description:\n%s\n\n"+
				"IMPORTANT: Communicate with the user in the same language used in the task title and description above. "+
				"If the task is written in Portuguese, respond in Portuguese. If in English, respond in English. Match the language naturally.\n\n"+
				"You can use the devmanager_get_my_task MCP tool to fetch updated task details or the devmanager_request_task_evaluation tool when you believe you have completed significant work.",
			linkedTask.Title, description,
		)
	}

	// Inject env vars based on the claude_session slot config
	if a.aiHandler != nil && a.aiHandler.providerMgr != nil {
		sessionConfig := a.aiHandler.providerMgr.GetSlotConfig(llm.SlotSession)
		if sessionConfig != nil {
			switch sessionConfig.ProviderType {
			case "ollama", "ollama-sdk":
				baseURL := sessionConfig.BaseURL
				if baseURL == "" {
					baseURL = "http://localhost:11434"
				}
				model := sessionConfig.Model
				if model == "" {
					model = "qwen3-coder"
				}
				log.Printf("[Session] Injecting Ollama env vars: url=%s, model=%s", baseURL, model)
				input.EnvVars["ANTHROPIC_BASE_URL"] = baseURL
				input.EnvVars["ANTHROPIC_API_KEY"] = ""
				input.EnvVars["ANTHROPIC_DEFAULT_OPUS_MODEL"] = model
				input.EnvVars["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
				input.EnvVars["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model
				input.EnvVars["CLAUDE_CODE_SUBAGENT_MODEL"] = model
				if sessionConfig.APIKey != "" {
					input.EnvVars["ANTHROPIC_AUTH_TOKEN"] = sessionConfig.APIKey
				} else {
					input.EnvVars["ANTHROPIC_AUTH_TOKEN"] = "ollama"
				}
			case "apikey":
				if sessionConfig.APIKey != "" {
					input.EnvVars["ANTHROPIC_API_KEY"] = sessionConfig.APIKey
					log.Printf("[Session] Injecting Anthropic API key from session config")
				}
			}
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

	// Auto-generate session name
	if linkedTask != nil {
		// Task-linked session: use task title
		autoName := "Task: " + linkedTask.Title
		a.db.ExecContext(r.Context(), "UPDATE sessions SET name = ? WHERE id = ?", autoName, sess.ID)
		sess.Name = autoName

		a.db.LinkSessionToTask(r.Context(), sess.ID, linkedTask.ID)

		// Auto-set task status to in_progress if it's currently todo
		if linkedTask.Status == "todo" {
			a.db.UpdateTaskStatus(r.Context(), linkedTask.ID, "in_progress")
			linkedTask.Status = "in_progress"
			a.hub.BroadcastStateUpdate("task", map[string]interface{}{
				"action":     "updated",
				"project_id": linkedTask.ProjectID,
				"task":       linkedTask,
			})
		}

		// Broadcast task-loaded notification so the frontend shows a decision dialog
		if a.hookHandler != nil {
			a.hookHandler.SetTaskNotification(sess.ID, linkedTask.ID, linkedTask.Title, linkedTask.Description)
		}
	} else {
		// Regular session: use project name + timestamp
		autoName := project.Name + " (" + time.Now().Format("15:04:05") + ")"
		a.db.ExecContext(r.Context(), "UPDATE sessions SET name = ? WHERE id = ?", autoName, sess.ID)
		sess.Name = autoName
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

func (a *API) ReopenSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	sess, err := a.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	if sess.Status != "stopped" && sess.Status != "completed" {
		respondError(w, http.StatusConflict, fmt.Sprintf("Session cannot be reopened: current status is '%s'", sess.Status))
		return
	}

	if a.sessionMgr.IsSessionRunning(sessionID) {
		respondError(w, http.StatusConflict, "Session is already running")
		return
	}

	project, err := a.db.GetProject(r.Context(), sess.ProjectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	// Auto-sync config before starting
	if a.configSync != nil {
		if syncErr := a.configSync.SyncToProject(r.Context(), project); syncErr != nil {
			log.Printf("Warning: config sync failed before session reopen: %v", syncErr)
		}
	}

	// Build env vars with task context if linked
	envVars := make(map[string]string)
	linkedTask, _ := a.db.GetTaskForSession(r.Context(), sessionID)
	if linkedTask != nil {
		envVars["DEVMANAGER_TASK_ID"] = fmt.Sprintf("%d", linkedTask.ID)
		envVars["DEVMANAGER_TASK_TITLE"] = linkedTask.Title

		description := linkedTask.Description
		if description == "" {
			description = "(no description provided)"
		}
		envVars["DEVMANAGER_APPEND_SYSTEM_PROMPT"] = fmt.Sprintf(
			"You have been assigned the following task by DevManager:\n\n"+
				"Title: %s\n\n"+
				"Description:\n%s\n\n"+
				"IMPORTANT: This is a RESUMED session. You are continuing work from a previous conversation.\n\n"+
				"IMPORTANT: Communicate with the user in the same language used in the task title and description above. "+
				"If the task is written in Portuguese, respond in Portuguese. If in English, respond in English. Match the language naturally.\n\n"+
				"You can use the devmanager_get_my_task MCP tool to fetch updated task details or the devmanager_request_task_evaluation tool when you believe you have completed significant work.",
			linkedTask.Title, description,
		)
	}

	if err := a.sessionMgr.ReopenSession(r.Context(), sess, project, envVars, a.encryptor.Decrypt); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Refresh session from DB for updated fields
	sess, _ = a.db.GetSession(r.Context(), sessionID)
	respondJSON(w, http.StatusOK, sess)
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
	var s database.Skill
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if msg := validateSkillName(s.Name); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	if strings.TrimSpace(s.Content) == "" {
		respondError(w, http.StatusBadRequest, "Content is required")
		return
	}

	if err := a.db.CreateSkill(r.Context(), &s); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Save initial version
	nextVer, _ := a.db.GetNextSkillVersion(r.Context(), s.ID)
	a.db.CreateSkillVersion(r.Context(), &database.SkillVersion{
		SkillID: s.ID, Name: s.Name, Content: s.Content, Version: nextVer,
	})

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

	if msg := validateSkillName(s.Name); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	if strings.TrimSpace(s.Content) == "" {
		respondError(w, http.StatusBadRequest, "Content is required")
		return
	}

	// Get old skill to check if content changed
	old, err := a.db.GetSkill(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Skill not found")
		return
	}

	s.ID = id
	if err := a.db.UpdateSkill(r.Context(), &s); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Save version if content or name changed
	if old.Content != s.Content || old.Name != s.Name {
		nextVer, _ := a.db.GetNextSkillVersion(r.Context(), s.ID)
		a.db.CreateSkillVersion(r.Context(), &database.SkillVersion{
			SkillID: s.ID, Name: s.Name, Content: s.Content, Version: nextVer,
		})
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

func (a *API) DuplicateSkill(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid skill ID")
		return
	}

	original, err := a.db.GetSkill(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Skill not found")
		return
	}

	// Find a unique name
	newName := original.Name + " (copy)"
	for i := 2; ; i++ {
		var existing database.Skill
		err := a.db.GetContext(r.Context(), &existing, "SELECT id FROM skills WHERE name = ?", newName)
		if err != nil {
			break // name available
		}
		newName = original.Name + fmt.Sprintf(" (copy %d)", i)
	}

	dup := database.Skill{
		Name:      newName,
		Content:   original.Content,
		Enabled:   original.Enabled,
		Category:  original.Category,
		SortOrder: original.SortOrder,
	}

	if err := a.db.CreateSkill(r.Context(), &dup); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "created", "skill": dup})
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
	w.Header().Set("Content-Disposition", "attachment; filename=devmanager-skills.json")
	json.NewEncoder(w).Encode(exported)
}

func (a *API) ImportSkills(w http.ResponseWriter, r *http.Request) {
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

	imported := 0
	skipped := 0
	for _, s := range skills {
		if msg := validateSkillName(s.Name); msg != "" {
			skipped++
			continue
		}
		if strings.TrimSpace(s.Content) == "" {
			skipped++
			continue
		}

		skill := database.Skill{
			Name:     s.Name,
			Content:  s.Content,
			Enabled:  s.Enabled,
			Category: s.Category,
		}
		if err := a.db.CreateSkill(r.Context(), &skill); err != nil {
			skipped++ // likely duplicate name
			continue
		}
		imported++
	}

	a.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "imported"})
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"imported": imported,
		"skipped":  skipped,
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

	ver, err := a.db.GetSkillVersion(r.Context(), versionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Version not found")
		return
	}

	skill, err := a.db.GetSkill(r.Context(), skillID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Skill not found")
		return
	}

	skill.Name = ver.Name
	skill.Content = ver.Content
	if err := a.db.UpdateSkill(r.Context(), skill); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Save as new version
	nextVer, _ := a.db.GetNextSkillVersion(r.Context(), skillID)
	a.db.CreateSkillVersion(r.Context(), &database.SkillVersion{
		SkillID: skillID, Name: ver.Name, Content: ver.Content, Version: nextVer,
	})

	a.hub.BroadcastStateUpdate("skill", map[string]interface{}{"action": "updated", "skill": skill})
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
	if req.Name == "" || req.ProviderType == "" {
		respondError(w, http.StatusBadRequest, "Name and provider_type are required")
		return
	}
	if req.ExtraJSON == "" {
		req.ExtraJSON = "{}"
	}

	c := database.AIConfig{
		Name:         req.Name,
		ProviderType: req.ProviderType,
		Model:        req.Model,
		BaseURL:      req.BaseURL,
		ExtraJSON:    req.ExtraJSON,
	}

	// Encrypt API key if provided
	if req.APIKey != "" {
		encrypted, iv, err := a.encryptor.Encrypt(req.APIKey)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to encrypt API key")
			return
		}
		c.APIKeyEncrypted = encrypted
		c.APIKeyIV = iv
		c.APIKeyPreview = apiKeyPreview(req.APIKey)
	}

	if err := a.db.CreateAIConfig(r.Context(), &c); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if a.ReinitAIProviders != nil {
		a.ReinitAIProviders()
	}
	a.hub.BroadcastStateUpdate("ai_config", map[string]interface{}{"action": "created", "config": c})
	respondJSON(w, http.StatusCreated, c)
}

func (a *API) UpdateAIConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}

	existing, err := a.db.GetAIConfig(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "AI config not found")
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

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.ProviderType != "" {
		existing.ProviderType = req.ProviderType
	}
	existing.Model = req.Model
	existing.BaseURL = req.BaseURL
	if req.ExtraJSON != "" {
		existing.ExtraJSON = req.ExtraJSON
	}

	// Encrypt API key if provided (empty = keep existing)
	if req.APIKey != "" {
		encrypted, iv, encErr := a.encryptor.Encrypt(req.APIKey)
		if encErr != nil {
			respondError(w, http.StatusInternalServerError, "Failed to encrypt API key")
			return
		}
		existing.APIKeyEncrypted = encrypted
		existing.APIKeyIV = iv
		existing.APIKeyPreview = apiKeyPreview(req.APIKey)
	}

	if err := a.db.UpdateAIConfig(r.Context(), existing); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if a.ReinitAIProviders != nil {
		a.ReinitAIProviders()
	}
	a.hub.BroadcastStateUpdate("ai_config", map[string]interface{}{"action": "updated", "config": existing})
	respondJSON(w, http.StatusOK, existing)
}

func (a *API) DeleteAIConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid AI config ID")
		return
	}

	if err := a.db.DeleteAIConfig(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if a.ReinitAIProviders != nil {
		a.ReinitAIProviders()
	}
	a.hub.BroadcastStateUpdate("ai_config", map[string]interface{}{"action": "deleted", "id": id})
	w.WriteHeader(http.StatusNoContent)
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
	var req map[string]*int64 // slot -> config_id (null = unassign)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	validSlots := map[string]bool{"ai_chat": true, "ai_background": true, "claude_session": true}
	for slot, configID := range req {
		if !validSlots[slot] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid slot: %s", slot))
			return
		}
		if err := a.db.SetAIConfigAssignment(r.Context(), slot, configID); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if a.ReinitAIProviders != nil {
		a.ReinitAIProviders()
	}
	a.hub.BroadcastStateUpdate("ai_config", map[string]interface{}{"action": "assignments_updated"})
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ============ Settings ============

func (a *API) GetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := a.db.GetAllSettings(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Remove always-hidden keys
	delete(settings, "vapid_private_key")

	// For sensitive keys: remove encrypted blob and IV, keep only preview
	for key := range sensitiveSettingKeys {
		delete(settings, key)
		delete(settings, key+"_iv")
		// _preview keys remain in the map if they exist
	}

	respondJSON(w, http.StatusOK, settings)
}

func (a *API) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var settings map[string]string
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	for key, value := range settings {
		if sensitiveSettingKeys[key] {
			if value == "" {
				continue // empty means unchanged
			}
			encrypted, iv, err := a.encryptor.Encrypt(value)
			if err != nil {
				respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to encrypt %s", key))
				return
			}
			if err := a.db.SetSetting(r.Context(), key, encrypted); err != nil {
				respondError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := a.db.SetSetting(r.Context(), key+"_iv", iv); err != nil {
				respondError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := a.db.SetSetting(r.Context(), key+"_preview", apiKeyPreview(value)); err != nil {
				respondError(w, http.StatusInternalServerError, err.Error())
				return
			}
			continue
		}
		if err := a.db.SetSetting(r.Context(), key, value); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// Reinitialize AI provider if any AI-related setting changed
	if a.ReinitAIProvider != nil {
		for key := range settings {
			if aiProviderSettingKeys[key] {
				a.ReinitAIProvider()
				break
			}
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

func (a *API) GetActiveNotifications(w http.ResponseWriter, r *http.Request) {
	notifications, err := a.notifService.GetActive(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, notifications)
}

func (a *API) GetNotificationUnreadCount(w http.ResponseWriter, r *http.Request) {
	count, err := a.notifService.GetUnreadCount(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]int{"unread_count": count})
}

func (a *API) MarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if err := a.notifService.MarkAllRead(r.Context()); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============ Memory Docs ============

func (a *API) GetMemoryDoc(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	doc, err := a.db.GetMemoryDoc(r.Context(), id)
	if err != nil {
		// Return empty placeholder if none exists
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"project_id": id,
			"content":    "",
			"version":    0,
			"exists":     false,
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"project_id":      doc.ProjectID,
		"content":         doc.Content,
		"version":         doc.Version,
		"last_updated_by": doc.LastUpdatedBy,
		"summary":         doc.Summary.String,
		"updated_at":      doc.UpdatedAt,
		"exists":          true,
	})
}

func (a *API) UpdateMemoryDoc(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	// Verify project exists
	project, err := a.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
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

	if input.UpdatedBy == "" {
		input.UpdatedBy = "user"
	}

	doc, err := a.db.UpsertMemoryDoc(r.Context(), id, input.Content, input.UpdatedBy, input.Summary)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Write back to project's CLAUDE.md
	a.writeClaudeMD(project, input.Content)

	a.hub.BroadcastStateUpdate("memory_doc", map[string]interface{}{
		"action":     "updated",
		"project_id": id,
		"version":    doc.Version,
	})

	respondJSON(w, http.StatusOK, doc)
}

// ============ Temp Documents ============

func (a *API) CreateTempDocument(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title          string `json:"title"`
		Content        string `json:"content"`
		ConversationID string `json:"conversation_id"`
		TaskID         string `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if strings.TrimSpace(input.Content) == "" {
		respondError(w, http.StatusBadRequest, "Content is required")
		return
	}

	title := input.Title
	if title == "" {
		title = "Documento"
	}

	convID, _ := strconv.ParseInt(input.ConversationID, 10, 64)
	taskID, _ := strconv.ParseInt(input.TaskID, 10, 64)
	doc := &database.TempDocument{
		ID:             uuid.New().String()[:8],
		Title:          title,
		Content:        input.Content,
		ConversationID: sql.NullInt64{Int64: convID, Valid: convID > 0},
		TaskID:         sql.NullInt64{Int64: taskID, Valid: taskID > 0},
	}
	if err := a.db.CreateTempDocument(r.Context(), doc); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Notify the chat panel via WebSocket to inject an inline doc card
	a.hub.BroadcastChatDocCard(map[string]interface{}{
		"doc_id":          doc.ID,
		"type":            "document",
		"title":           title,
		"conversation_id": input.ConversationID,
	})

	respondJSON(w, http.StatusCreated, map[string]string{
		"id":   doc.ID,
		"link": fmt.Sprintf("/app/doc/%s", doc.ID),
	})
}

func (a *API) GetTempDocument(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	doc, err := a.db.GetTempDocument(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Document not found")
		return
	}
	respondJSON(w, http.StatusOK, doc)
}

// ============ Project Tasks ============

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

	// Preserve the original status filter, then clear it for the DB query
	// so umbrella tasks are fetched regardless of their stored status.
	statusFilter := f.Status
	if statusFilter != "" {
		f.Status = ""
	}

	tasks, err := a.db.ListAllTasks(r.Context(), f)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []database.ProjectTask{}
	}
	database.ApplyUmbrellaStatus(tasks)

	// Re-apply status filter after umbrella status computation
	if statusFilter != "" {
		filtered := tasks[:0]
		for _, t := range tasks {
			if t.Status == statusFilter {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	summary, _ := a.db.GetAllTasksSummary(r.Context())

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"tasks":   tasks,
		"summary": summary,
	})
}

func (a *API) ListProjectTasks(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	tasks, err := a.db.ListTasksByProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []database.ProjectTask{}
	}
	database.ApplyUmbrellaStatus(tasks)
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

	// Verify project exists
	if _, err := a.db.GetProject(r.Context(), projectID); err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
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

	if strings.TrimSpace(input.Title) == "" {
		respondError(w, http.StatusBadRequest, "Title is required")
		return
	}

	// Validate status
	if input.Status == "" {
		input.Status = "todo"
	}
	validStatuses := map[string]bool{"todo": true, "in_progress": true, "done": true, "awaiting_approval": true}
	if !validStatuses[input.Status] {
		respondError(w, http.StatusBadRequest, "Invalid status. Must be: todo, in_progress, awaiting_approval, done")
		return
	}

	// Validate priority
	if input.Priority == "" {
		input.Priority = "medium"
	}
	validPriorities := map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}
	if !validPriorities[input.Priority] {
		respondError(w, http.StatusBadRequest, "Invalid priority. Must be: low, medium, high, urgent")
		return
	}

	task := &database.ProjectTask{
		ProjectID:   projectID,
		Title:       strings.TrimSpace(input.Title),
		Description: input.Description,
		Status:      input.Status,
		Priority:    input.Priority,
		SortOrder:   input.SortOrder,
	}

	if input.ParentID != nil {
		task.ParentID = sql.NullInt64{Int64: *input.ParentID, Valid: true}
	}

	if input.DueDate != "" {
		t, err := parseFlexibleTime(input.DueDate)
		if err != nil {
			respondError(w, http.StatusBadRequest, "Invalid due_date format")
			return
		}
		task.DueDate = sql.NullTime{Time: t, Valid: true}
	}

	if err := a.db.CreateTask(r.Context(), task); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "created", "project_id": projectID, "task": task})
	a.recordTaskHistory(r.Context(), task.ID, projectID, "task_created", map[string]interface{}{
		"title": task.Title, "status": task.Status, "priority": task.Priority,
	}, "user", "")
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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Compute umbrella status dynamically from children
	allTasks, err := a.db.ListTasksByProject(r.Context(), projectID)
	if err == nil {
		database.ApplyUmbrellaStatus(allTasks)
		for _, t := range allTasks {
			if t.ID == taskID {
				task.Status = t.Status
				break
			}
		}
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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Capture old values for history tracking
	oldStatus := task.Status
	oldPriority := task.Priority
	oldTitle := task.Title
	oldDescription := task.Description

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

	if input.Title != nil {
		if strings.TrimSpace(*input.Title) == "" {
			respondError(w, http.StatusBadRequest, "Title cannot be empty")
			return
		}
		task.Title = strings.TrimSpace(*input.Title)
	}
	if input.Description != nil {
		task.Description = *input.Description
	}
	if input.Status != nil {
		validStatuses := map[string]bool{"todo": true, "in_progress": true, "done": true, "awaiting_approval": true}
		if !validStatuses[*input.Status] {
			respondError(w, http.StatusBadRequest, "Invalid status")
			return
		}
		task.Status = *input.Status
	}
	if input.Priority != nil {
		validPriorities := map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}
		if !validPriorities[*input.Priority] {
			respondError(w, http.StatusBadRequest, "Invalid priority")
			return
		}
		task.Priority = *input.Priority
	}
	if input.DueDate != nil {
		if *input.DueDate == "" {
			task.DueDate = sql.NullTime{}
		} else {
			t, err := parseFlexibleTime(*input.DueDate)
			if err != nil {
				respondError(w, http.StatusBadRequest, "Invalid due_date format")
				return
			}
			task.DueDate = sql.NullTime{Time: t, Valid: true}
			task.DueNotified = false // Reset notification on date change
		}
	}
	if input.ParentID != nil {
		task.ParentID = sql.NullInt64{Int64: *input.ParentID, Valid: *input.ParentID > 0}
	}
	if input.SortOrder != nil {
		task.SortOrder = *input.SortOrder
	}

	if err := a.db.UpdateTask(r.Context(), task); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": projectID, "task": task})

	// Record history for each changed field
	if task.Status != oldStatus {
		a.recordTaskHistory(r.Context(), taskID, projectID, "status_change", map[string]interface{}{
			"old": oldStatus, "new": task.Status,
		}, "user", "")
	}
	if task.Priority != oldPriority {
		a.recordTaskHistory(r.Context(), taskID, projectID, "priority_change", map[string]interface{}{
			"old": oldPriority, "new": task.Priority,
		}, "user", "")
	}
	if task.Title != oldTitle {
		a.recordTaskHistory(r.Context(), taskID, projectID, "title_updated", map[string]interface{}{
			"old": oldTitle, "new": task.Title,
		}, "user", "")
	}
	if task.Description != oldDescription {
		a.recordTaskHistory(r.Context(), taskID, projectID, "description_updated", map[string]interface{}{}, "user", "")
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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}
	oldStatus := task.Status

	var input struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	validStatuses := map[string]bool{"todo": true, "in_progress": true, "done": true, "awaiting_approval": true}
	if !validStatuses[input.Status] {
		respondError(w, http.StatusBadRequest, "Invalid status")
		return
	}

	if err := a.db.UpdateTaskStatus(r.Context(), taskID, input.Status); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Re-fetch to get updated sort_order
	task, err = a.db.GetTask(r.Context(), taskID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": projectID, "task": task})
	if input.Status != oldStatus {
		a.recordTaskHistory(r.Context(), taskID, projectID, "status_change", map[string]interface{}{
			"old": oldStatus, "new": input.Status,
		}, "user", "")
	}

	// Trigger async verification document generation when transitioning to awaiting_approval
	if input.Status == "awaiting_approval" && a.aiHandler != nil {
		go a.aiHandler.GenerateVerificationDoc(context.Background(), task)
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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}
	if task.Status != "awaiting_approval" {
		respondError(w, http.StatusBadRequest, "Task is not awaiting approval")
		return
	}

	// Update verification document status if present
	if task.VerificationDocID != "" {
		a.db.UpdateTempDocumentStatus(r.Context(), task.VerificationDocID, "approved")
	}

	// Transition task to done
	if err := a.db.UpdateTaskStatus(r.Context(), taskID, "done"); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	task, _ = a.db.GetTask(r.Context(), taskID)
	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": projectID, "task": task})
	a.recordTaskHistory(r.Context(), taskID, projectID, "verification_approved", map[string]interface{}{
		"old": "awaiting_approval", "new": "done",
	}, "user", "")

	respondJSON(w, http.StatusOK, task)
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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}
	if task.Status != "awaiting_approval" {
		respondError(w, http.StatusBadRequest, "Task is not awaiting approval")
		return
	}

	// Update verification document status if present
	if task.VerificationDocID != "" {
		a.db.UpdateTempDocumentStatus(r.Context(), task.VerificationDocID, "rejected")
	}

	// Clear verification doc and return to in_progress
	task.VerificationDocID = ""
	a.db.SetTaskVerificationDoc(r.Context(), taskID, "")

	if err := a.db.UpdateTaskStatus(r.Context(), taskID, "in_progress"); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	task, _ = a.db.GetTask(r.Context(), taskID)
	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "updated", "project_id": projectID, "task": task})
	a.recordTaskHistory(r.Context(), taskID, projectID, "verification_rejected", map[string]interface{}{
		"old": "awaiting_approval", "new": "in_progress",
	}, "user", "")

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

	if len(items) == 0 {
		respondError(w, http.StatusBadRequest, "Empty reorder list")
		return
	}
	if len(items) > 100 {
		respondError(w, http.StatusBadRequest, "Too many items (max 100)")
		return
	}

	if err := a.db.ReorderTasks(r.Context(), projectID, items); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("task", map[string]interface{}{
		"action":     "reordered",
		"project_id": projectID,
	})

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ReorderAllTasks batch-updates global_sort_order for cross-project reordering.
func (a *API) ReorderAllTasks(w http.ResponseWriter, r *http.Request) {
	var items []database.GlobalReorderItem
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if len(items) == 0 {
		respondError(w, http.StatusBadRequest, "Empty reorder list")
		return
	}
	if len(items) > 200 {
		respondError(w, http.StatusBadRequest, "Too many items (max 200)")
		return
	}

	if err := a.db.ReorderTasksGlobal(r.Context(), items); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("task", map[string]interface{}{
		"action": "reordered",
	})

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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}

	if err := a.db.DeleteTask(r.Context(), taskID); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "deleted", "project_id": projectID, "id": taskID})
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

	task, err := a.db.GetTask(r.Context(), taskID)
	if err != nil || task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Task not found")
		return
	}

	clone, err := a.db.DuplicateTask(r.Context(), taskID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("task", map[string]interface{}{"action": "created", "project_id": projectID, "task": clone})
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

	task, err := a.db.GetTaskForSession(r.Context(), sessionID)
	if err != nil || task == nil {
		respondError(w, http.StatusNotFound, "No task linked to this session")
		return
	}

	respondJSON(w, http.StatusOK, task)
}

// LinkSessionTask links an active session to an existing task or creates a new task and links it
func (a *API) LinkSessionTask(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	sess, err := a.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	// If session already has a linked task, unlink it first (allows re-linking)
	existingTask, _ := a.db.GetTaskForSession(r.Context(), sessionID)
	if existingTask != nil {
		if _, err := a.db.UnlinkSessionFromTask(r.Context(), sessionID); err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to unlink existing task")
			return
		}
		a.recordTaskHistory(r.Context(), existingTask.ID, existingTask.ProjectID, "session_unlinked", map[string]interface{}{
			"session_id": sessionID, "session_name": sess.Name,
		}, "user", sessionID)
	}

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

	var linkedTask *database.ProjectTask

	if input.TaskID != nil && *input.TaskID > 0 {
		// Link to existing task
		linkedTask, err = a.db.GetTask(r.Context(), *input.TaskID)
		if err != nil {
			respondError(w, http.StatusNotFound, "Task not found")
			return
		}
		if linkedTask.ProjectID != sess.ProjectID {
			respondError(w, http.StatusBadRequest, "Task belongs to a different project")
			return
		}
	} else if input.TaskData != nil && strings.TrimSpace(input.TaskData.Title) != "" {
		// Create new task and link
		priority := input.TaskData.Priority
		if priority == "" {
			priority = "medium"
		}
		linkedTask = &database.ProjectTask{
			ProjectID:   sess.ProjectID,
			Title:       strings.TrimSpace(input.TaskData.Title),
			Description: input.TaskData.Description,
			Status:      "in_progress",
			Priority:    priority,
		}
		if err := a.db.CreateTask(r.Context(), linkedTask); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.hub.BroadcastStateUpdate("task", map[string]interface{}{
			"action":     "created",
			"project_id": sess.ProjectID,
			"task":       linkedTask,
		})
	} else {
		respondError(w, http.StatusBadRequest, "Either task_id or task_data with title is required")
		return
	}

	// Link session to task and rename
	newName := "Task: " + linkedTask.Title
	if err := a.db.LinkSessionToTask(r.Context(), sessionID, linkedTask.ID); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.db.ExecContext(r.Context(), "UPDATE sessions SET name = ? WHERE id = ?", newName, sessionID)

	// Broadcast session rename
	a.hub.BroadcastStateUpdate("session", map[string]interface{}{
		"action":     "renamed",
		"session_id": sessionID,
		"name":       newName,
	})

	// Record session linked and task assigned history
	a.recordTaskHistory(r.Context(), linkedTask.ID, linkedTask.ProjectID, "session_linked", map[string]interface{}{
		"session_id": sessionID, "session_name": newName,
	}, "user", sessionID)
	a.recordTaskHistory(r.Context(), linkedTask.ID, linkedTask.ProjectID, "task_assigned", map[string]interface{}{
		"session_id": sessionID, "session_name": newName,
	}, "system", sessionID)

	// Auto-set task to in_progress if currently todo
	if linkedTask.Status == "todo" {
		oldStatus := linkedTask.Status
		a.db.UpdateTaskStatus(r.Context(), linkedTask.ID, "in_progress")
		linkedTask.Status = "in_progress"
		a.hub.BroadcastStateUpdate("task", map[string]interface{}{
			"action":     "updated",
			"project_id": linkedTask.ProjectID,
			"task":       linkedTask,
		})
		a.recordTaskHistory(r.Context(), linkedTask.ID, linkedTask.ProjectID, "status_change", map[string]interface{}{
			"old": oldStatus, "new": "in_progress", "reason": "auto_on_link",
		}, "system", sessionID)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"task":         linkedTask,
		"session_name": newName,
	})
}

// UnlinkSessionTask removes the task link from a session.
func (a *API) UnlinkSessionTask(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	sess, err := a.db.GetSession(r.Context(), sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	task, _ := a.db.GetTaskForSession(r.Context(), sessionID)
	if task == nil {
		respondError(w, http.StatusConflict, "Session has no linked task")
		return
	}

	if _, err := a.db.UnlinkSessionFromTask(r.Context(), sessionID); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reset session name (remove "Task: " prefix)
	newName := sess.Name
	if strings.HasPrefix(newName, "Task: ") {
		newName = "Session " + sessionID[:8]
	}
	a.db.ExecContext(r.Context(), "UPDATE sessions SET name = ? WHERE id = ?", newName, sessionID)

	a.hub.BroadcastStateUpdate("session", map[string]interface{}{
		"action":     "renamed",
		"session_id": sessionID,
		"name":       newName,
	})

	a.recordTaskHistory(r.Context(), task.ID, task.ProjectID, "session_unlinked", map[string]interface{}{
		"session_id": sessionID, "session_name": sess.Name,
	}, "user", sessionID)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"unlinked_task_id": task.ID,
		"session_name":     newName,
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
	history, err := a.db.ListTaskHistory(r.Context(), taskID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if history == nil {
		history = []database.TaskHistory{}
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
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.Comment) == "" {
		respondError(w, http.StatusBadRequest, "Comment is required")
		return
	}
	a.recordTaskHistory(r.Context(), taskID, projectID, "comment_added", map[string]interface{}{
		"comment": strings.TrimSpace(input.Comment),
	}, "user", "")
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
		Type      string    `json:"type"`   // "document" or "plan"
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
	sessionID := chi.URLParam(r, "id")

	if !a.sessionMgr.IsSessionRunning(sessionID) {
		respondError(w, http.StatusNotFound, "Session not running")
		return
	}

	output, err := a.sessionMgr.GetSessionOutput(sessionID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to read session output")
		return
	}

	if a.aiHandler != nil {
		go a.aiHandler.EvaluateSession(context.Background(), sessionID, "session_request", output)
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

	// Append newline to simulate Enter
	data := []byte(input.Text + "\n")
	if err := a.sessionMgr.WriteToSession(id, data); err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// GenerateMCPAPIKey generates a new random API key for MCP HTTP access.
func (a *API) GenerateMCPAPIKey(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	if _, err := cryptoRandRead(b); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to generate key")
		return
	}
	key := fmt.Sprintf("dm_%s", hexEncode(b))

	if err := a.db.SetSetting(r.Context(), "mcp_api_key", key); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"key":    key,
		"status": "generated",
	})
}

// GetMCPAPIKeyStatus returns whether an API key exists and its prefix.
func (a *API) GetMCPAPIKeyStatus(w http.ResponseWriter, r *http.Request) {
	key, _ := a.db.GetSetting(r.Context(), "mcp_api_key")
	if key == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"exists": false,
		})
		return
	}
	prefix := key
	if len(prefix) > 10 {
		prefix = prefix[:10] + "..."
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"exists": true,
		"prefix": prefix,
	})
}

// RevokeMCPAPIKey removes the MCP API key.
func (a *API) RevokeMCPAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := a.db.SetSetting(r.Context(), "mcp_api_key", ""); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// GetToolPolicies returns the global tool policies for each context.
func (a *API) GetToolPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session, _ := a.db.GetSetting(ctx, "mcp_tool_policy_session")
	chat, _ := a.db.GetSetting(ctx, "mcp_tool_policy_chat")
	httpPolicy, _ := a.db.GetSetting(ctx, "mcp_tool_policy_http")

	respondJSON(w, http.StatusOK, map[string]string{
		"session": session,
		"chat":    chat,
		"http":    httpPolicy,
	})
}

// UpdateToolPolicies updates the global tool policies.
func (a *API) UpdateToolPolicies(w http.ResponseWriter, r *http.Request) {
	var input map[string]string
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	ctx := r.Context()
	validKeys := map[string]bool{
		"session": true,
		"chat":    true,
		"http":    true,
	}
	for key, value := range input {
		if !validKeys[key] {
			continue
		}
		if err := a.db.SetSetting(ctx, "mcp_tool_policy_"+key, value); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// GetProjectToolPolicy returns the tool policy for a specific project.
func (a *API) GetProjectToolPolicy(w http.ResponseWriter, r *http.Request) {
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

	respondJSON(w, http.StatusOK, map[string]string{
		"project_id":  fmt.Sprintf("%d", project.ID),
		"tool_policy": project.ToolPolicy,
	})
}

// UpdateProjectToolPolicy updates the tool policy for a specific project.
func (a *API) UpdateProjectToolPolicy(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		ToolPolicy string `json:"tool_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	project.ToolPolicy = input.ToolPolicy
	if err := a.db.UpdateProject(r.Context(), project); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// --- Project Skills ---

// GetProjectSkills returns global skills with per-project config + project-specific skills.
func (a *API) GetProjectSkills(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := r.Context()
	project, err := a.db.GetProject(ctx, projectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	// Get all global skills
	allSkills, err := a.db.ListSkills(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Get per-project overrides
	configs, err := a.db.ListProjectSkillConfigs(ctx, projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
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
	projectSkills, err := a.db.ListProjectSkills(ctx, projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
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
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := r.Context()
	project, err := a.db.GetProject(ctx, projectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
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

	if input.Inherit {
		project.SkillPolicy = ""
	} else {
		project.SkillPolicy = "custom"
		// Upsert all overrides
		for _, o := range input.Overrides {
			if err := a.db.UpsertProjectSkillConfig(ctx, projectID, o.SkillID, o.Enabled); err != nil {
				respondError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}

	if err := a.db.UpdateProject(ctx, project); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project", map[string]interface{}{
		"action":  "updated",
		"project": project,
	})

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// CreateProjectSkillHandler creates a project-specific skill.
func (a *API) CreateProjectSkillHandler(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := r.Context()
	if _, err := a.db.GetProject(ctx, projectID); err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
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

	if msg := validateSkillName(input.Name); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}

	ps := &database.ProjectSkill{
		ProjectID: projectID,
		Name:      input.Name,
		Content:   input.Content,
		Enabled:   true,
		Category:  input.Category,
	}

	if err := a.db.CreateProjectSkill(ctx, ps); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			respondError(w, http.StatusConflict, "A project skill with this name already exists")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{
		"action":     "created",
		"project_id": projectID,
		"skill":      ps,
	})

	respondJSON(w, http.StatusCreated, ps)
}

// UpdateProjectSkillHandler updates a project-specific skill.
func (a *API) UpdateProjectSkillHandler(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()
	ps, err := a.db.GetProjectSkill(ctx, skillID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project skill not found")
		return
	}
	if ps.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Project skill not found")
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

	if input.Name != nil {
		if msg := validateSkillName(*input.Name); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		ps.Name = *input.Name
	}
	if input.Content != nil {
		ps.Content = *input.Content
	}
	if input.Enabled != nil {
		ps.Enabled = *input.Enabled
	}
	if input.Category != nil {
		ps.Category = *input.Category
	}

	if err := a.db.UpdateProjectSkill(ctx, ps); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			respondError(w, http.StatusConflict, "A project skill with this name already exists")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{
		"action":     "updated",
		"project_id": projectID,
		"skill":      ps,
	})

	respondJSON(w, http.StatusOK, ps)
}

// DeleteProjectSkillHandler deletes a project-specific skill.
func (a *API) DeleteProjectSkillHandler(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()
	ps, err := a.db.GetProjectSkill(ctx, skillID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project skill not found")
		return
	}
	if ps.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "Project skill not found")
		return
	}

	if err := a.db.DeleteProjectSkill(ctx, skillID); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.hub.BroadcastStateUpdate("project_skill", map[string]interface{}{
		"action":     "deleted",
		"project_id": projectID,
		"id":         skillID,
	})

	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetResolvedTools returns the effective list of tools available for a given context.
// For sessions: GET /api/sessions/{id}/tools
// For projects: GET /api/projects/{id}/tools
func (a *API) GetResolvedProjectTools(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return
	}

	ctx := r.Context()
	allTools := mcp.AllTools()

	// Global session policy (sessions default to deny_all)
	globalPolicy := mcp.ToolPolicy{Mode: "deny_all"}
	if policyStr, _ := a.db.GetSetting(ctx, "mcp_tool_policy_session"); policyStr != "" {
		globalPolicy = mcp.ParsePolicy(policyStr)
	}

	// Project policy
	project, err := a.db.GetProject(ctx, projectID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return
	}

	effectivePolicy := mcp.ResolveProjectPolicy(globalPolicy, project.ToolPolicy)

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
	sessionID := chi.URLParam(r, "id")

	ctx := r.Context()
	sess, err := a.db.GetSession(ctx, sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "Session not found")
		return
	}

	allTools := mcp.AllTools()

	// Global session policy (sessions default to deny_all)
	globalPolicy := mcp.ToolPolicy{Mode: "deny_all"}
	if policyStr, _ := a.db.GetSetting(ctx, "mcp_tool_policy_session"); policyStr != "" {
		globalPolicy = mcp.ParsePolicy(policyStr)
	}

	// Resolve: project policy overrides global if set, otherwise inherit global
	var projectPolicyJSON string
	if sess.ProjectID > 0 {
		project, err := a.db.GetProject(ctx, sess.ProjectID)
		if err == nil {
			projectPolicyJSON = project.ToolPolicy
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
	ctx := r.Context()

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
	summary, err := a.db.GetTokenUsageSummary(ctx, since, projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get token usage summary")
		return
	}

	bySource, err := a.db.GetTokenUsageBySource(ctx, since, projectID)
	if err != nil {
		log.Printf("[API] GetTokenUsageBySource error: %v", err)
	}
	byModel, err := a.db.GetTokenUsageByModel(ctx, since, projectID)
	if err != nil {
		log.Printf("[API] GetTokenUsageByModel error: %v", err)
	}
	daily, err := a.db.GetTokenUsageDaily(ctx, since, projectID)
	if err != nil {
		log.Printf("[API] GetTokenUsageDaily error: %v", err)
	}

	// Build per-project and per-AI-subcategory breakdowns (only when no project filter)
	var byProject []database.TokenUsageByProject
	var byProjectModel []database.TokenUsageByProjectModel
	var byAISubcategory []database.TokenUsageBySubcategory
	if projectID == nil {
		byProject, err = a.db.GetTokenUsageByProject(ctx, since)
		if err != nil {
			log.Printf("[API] GetTokenUsageByProject error: %v", err)
		}
		byProjectModel, err = a.db.GetTokenUsageByProjectModel(ctx, since)
		if err != nil {
			log.Printf("[API] GetTokenUsageByProjectModel error: %v", err)
		}
		byAISubcategory, err = a.db.GetTokenUsageByAISubcategory(ctx, since)
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
	deleted, err := a.db.ClearTokenUsage(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to clear token usage data")
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
