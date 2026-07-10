package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/llm"
	"openpoet/internal/notifications"
)

type platformMemoryMirror struct{ api *API }

func (m platformMemoryMirror) MirrorMemoryDocument(_ context.Context, project *database.Project, content string) {
	if m.api != nil && project != nil {
		m.api.writeClaudeMD(project, content)
	}
}

type platformProposalBackend struct{ api *API }

func (b platformProposalBackend) CreateMemoryProposalAtomic(ctx context.Context, proposal application.MemoryProposal) (*application.ProposalRecord, error) {
	if b.api == nil || b.api.db == nil {
		return nil, errors.New("proposal backend unavailable")
	}
	if _, err := b.api.db.GetProject(ctx, proposal.ProjectID); err != nil {
		return nil, err
	}
	id := uuid.NewString()[:8]
	doc := &database.TempDocument{ID: id, Title: "Memory document proposal", Content: proposal.Content, Summary: proposal.Summary, Status: "pending"}
	if proposal.ConversationID != nil {
		doc.ConversationID = sql.NullInt64{Int64: *proposal.ConversationID, Valid: true}
	}
	if err := b.api.db.CreateTempDocument(ctx, doc); err != nil {
		return nil, err
	}
	b.api.storePendingMemoryDoc(id, proposal.ProjectID, proposal.Content, proposal.Summary)
	return &application.ProposalRecord{ID: id, Kind: application.ProposalMemory, Risk: application.ProposalRiskR2, Status: "pending", Title: doc.Title, Summary: proposal.Summary, ProjectID: proposal.ProjectID, ConversationID: proposal.ConversationID}, nil
}

func (b platformProposalBackend) GetPendingProposal(ctx context.Context, id string, kind application.ProposalKind) (*application.ProposalRecord, error) {
	if b.api == nil || b.api.db == nil {
		return nil, errors.New("proposal backend unavailable")
	}
	doc, err := b.api.db.GetTempDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	record := &application.ProposalRecord{ID: id, Kind: kind, Status: doc.Status, Title: doc.Title, Summary: doc.Summary}
	if doc.ConversationID.Valid {
		value := doc.ConversationID.Int64
		record.ConversationID = &value
	}
	switch kind {
	case application.ProposalMemory:
		b.api.pendingMemoryDocsMu.Lock()
		pending := b.api.pendingMemoryDocs[id]
		b.api.pendingMemoryDocsMu.Unlock()
		if pending == nil {
			return nil, sql.ErrNoRows
		}
		record.ProjectID, record.Risk = pending.ProjectID, application.ProposalRiskR2
	case application.ProposalTask:
		b.api.pendingTaskProposalsMu.Lock()
		pending := b.api.pendingTaskProposals[id]
		b.api.pendingTaskProposalsMu.Unlock()
		if pending == nil {
			return nil, sql.ErrNoRows
		}
		if len(pending.Actions) > 0 {
			record.ProjectID = pending.Actions[0].ProjectID
		}
		record.Risk = application.ProposalRiskR3
	case application.ProposalSkill:
		b.api.pendingSkillProposalsMu.Lock()
		pending := b.api.pendingSkillProposals[id]
		b.api.pendingSkillProposalsMu.Unlock()
		if pending == nil {
			return nil, sql.ErrNoRows
		}
		record.ProjectID, record.Risk = pending.ProjectID, application.ProposalRiskR2
	case application.ProposalTool:
		b.api.pendingToolProposalsMu.Lock()
		pending := b.api.pendingToolProposals[id]
		b.api.pendingToolProposalsMu.Unlock()
		if pending == nil {
			return nil, sql.ErrNoRows
		}
		record.ProjectID, record.Risk = pending.Action.ProjectID, application.ProposalRiskR4
	default:
		return nil, errors.New("unsupported proposal kind")
	}
	return record, nil
}

func (b platformProposalBackend) AcceptProposalAtomic(ctx context.Context, id string, kind application.ProposalKind, authorization application.ActionAuthorization) (application.ProposalAcceptance, error) {
	switch kind {
	case application.ProposalMemory:
		return b.acceptMemory(ctx, id, authorization)
	case application.ProposalTask:
		return b.acceptTasks(ctx, id, authorization)
	case application.ProposalSkill:
		return b.acceptSkill(ctx, id)
	case application.ProposalTool:
		return b.acceptTool(ctx, id)
	default:
		return application.ProposalAcceptance{}, errors.New("unsupported proposal kind")
	}
}

func (b platformProposalBackend) RejectProposalAtomic(ctx context.Context, id string, kind application.ProposalKind, _ application.ActionAuthorization) error {
	if _, err := b.GetPendingProposal(ctx, id, kind); err != nil {
		return err
	}
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = claimProposalInTx(ctx, tx, id, "rejected"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	b.deletePending(id, kind)
	return nil
}

func (b platformProposalBackend) acceptMemory(ctx context.Context, id string, authorization application.ActionAuthorization) (application.ProposalAcceptance, error) {
	b.api.pendingMemoryDocsMu.Lock()
	pending := b.api.pendingMemoryDocs[id]
	b.api.pendingMemoryDocsMu.Unlock()
	if pending == nil {
		return application.ProposalAcceptance{}, sql.ErrNoRows
	}
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return application.ProposalAcceptance{}, err
	}
	defer tx.Rollback()
	if err = claimProposalInTx(ctx, tx, id, "approved"); err != nil {
		return application.ProposalAcceptance{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_docs (project_id, content, version, last_updated_by, summary)
		VALUES (?, ?, 1, ?, ?) ON CONFLICT(project_id) DO UPDATE SET content=excluded.content,
		version=version+1, last_updated_by=excluded.last_updated_by, summary=excluded.summary,
		updated_at=CURRENT_TIMESTAMP`, pending.ProjectID, pending.Content, application.EventActorValue(authorization.Actor), sql.NullString{String: pending.Summary, Valid: pending.Summary != ""}); err != nil {
		return application.ProposalAcceptance{}, err
	}
	var version int
	if err = tx.GetContext(ctx, &version, `SELECT version FROM memory_docs WHERE project_id=?`, pending.ProjectID); err != nil {
		return application.ProposalAcceptance{}, err
	}
	if err = tx.Commit(); err != nil {
		return application.ProposalAcceptance{}, err
	}
	if project, getErr := b.api.db.GetProject(ctx, pending.ProjectID); getErr == nil {
		b.api.writeClaudeMD(project, pending.Content)
	}
	b.deletePending(id, application.ProposalMemory)
	return application.ProposalAcceptance{Status: "approved", Action: "updated", Message: fmt.Sprintf("Memory document version %d approved", version), UpdatedCount: 1}, nil
}

func (b platformProposalBackend) acceptTasks(ctx context.Context, id string, authorization application.ActionAuthorization) (application.ProposalAcceptance, error) {
	b.api.pendingTaskProposalsMu.Lock()
	pending := b.api.pendingTaskProposals[id]
	b.api.pendingTaskProposalsMu.Unlock()
	if pending == nil {
		return application.ProposalAcceptance{}, sql.ErrNoRows
	}
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return application.ProposalAcceptance{}, err
	}
	defer tx.Rollback()
	if err = claimProposalInTx(ctx, tx, id, "approved"); err != nil {
		return application.ProposalAcceptance{}, err
	}
	created, updated, deleted := 0, 0, 0
	createdTaskIDs := map[int]int64{}
	for i, action := range pending.Actions {
		switch action.Action {
		case "create":
			var parentID sql.NullInt64
			if action.ParentRef > 0 {
				if realID := createdTaskIDs[action.ParentRef]; realID > 0 {
					parentID = sql.NullInt64{Int64: realID, Valid: true}
				}
			} else if action.ParentID > 0 {
				parentID = sql.NullInt64{Int64: action.ParentID, Valid: true}
			}
			status, priority := action.Status, action.Priority
			if status == "" {
				status = "todo"
			}
			if priority == "" {
				priority = "medium"
			}
			sortOrder := action.SortOrder
			if sortOrder == 0 && status != "done" {
				if parentID.Valid {
					_ = tx.GetContext(ctx, &sortOrder, `SELECT COALESCE(MAX(sort_order),0)+1 FROM project_tasks WHERE project_id=? AND parent_id=?`, action.ProjectID, parentID.Int64)
				} else {
					_ = tx.GetContext(ctx, &sortOrder, `SELECT COALESCE(MAX(sort_order),0)+1 FROM project_tasks WHERE project_id=? AND parent_id IS NULL`, action.ProjectID)
				}
			}
			globalOrder := 0
			if status != "done" {
				_ = tx.GetContext(ctx, &globalOrder, `SELECT COALESCE(MAX(global_sort_order),0)+1 FROM project_tasks`)
			}
			var dueDate sql.NullTime
			if action.DueDate != "" {
				if parsed, parseErr := parseFlexibleTime(action.DueDate); parseErr == nil {
					dueDate = sql.NullTime{Time: parsed, Valid: true}
				}
			}
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO project_tasks (project_id,parent_id,title,description,status,priority,due_date,sort_order,global_sort_order) VALUES (?,?,?,?,?,?,?,?,?)`, action.ProjectID, parentID, action.Title, action.Description, status, priority, dueDate, sortOrder, globalOrder)
			if insertErr != nil {
				return application.ProposalAcceptance{}, insertErr
			}
			taskID, _ := result.LastInsertId()
			createdTaskIDs[i+1], created = taskID, created+1
			if err = appendProposalTaskEvent(ctx, tx, application.ProjectTaskEventCreated, taskID, action.ProjectID, id, authorization.Actor); err != nil {
				return application.ProposalAcceptance{}, err
			}
		case "update":
			var task database.ProjectTask
			if err = tx.GetContext(ctx, &task, `SELECT * FROM project_tasks WHERE id=? AND project_id=?`, action.TaskID, action.ProjectID); err != nil {
				return application.ProposalAcceptance{}, err
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
				if parsed, parseErr := parseFlexibleTime(action.DueDate); parseErr == nil {
					task.DueDate = sql.NullTime{Time: parsed, Valid: true}
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE project_tasks SET title=?,description=?,status=?,priority=?,due_date=?,updated_at=? WHERE id=? AND project_id=?`, task.Title, task.Description, task.Status, task.Priority, task.DueDate, time.Now(), task.ID, action.ProjectID); err != nil {
				return application.ProposalAcceptance{}, err
			}
			if err = appendProposalTaskEvent(ctx, tx, application.ProjectTaskEventUpdated, task.ID, task.ProjectID, id, authorization.Actor); err != nil {
				return application.ProposalAcceptance{}, err
			}
			updated++
		case "delete":
			result, deleteErr := tx.ExecContext(ctx, `DELETE FROM project_tasks WHERE id=? AND project_id=?`, action.TaskID, action.ProjectID)
			if deleteErr != nil {
				return application.ProposalAcceptance{}, deleteErr
			}
			if affected, _ := result.RowsAffected(); affected == 0 {
				return application.ProposalAcceptance{}, sql.ErrNoRows
			}
			if err = appendProposalTaskEvent(ctx, tx, application.ProjectTaskEventDeleted, action.TaskID, action.ProjectID, id, authorization.Actor); err != nil {
				return application.ProposalAcceptance{}, err
			}
			deleted++
		default:
			return application.ProposalAcceptance{}, errors.New("unsupported task proposal action")
		}
	}
	if err = tx.Commit(); err != nil {
		return application.ProposalAcceptance{}, err
	}
	b.deletePending(id, application.ProposalTask)
	if b.api.hub != nil {
		b.api.hub.BroadcastStateUpdate("task", map[string]any{"action": "proposal_applied", "created": created, "updated": updated, "deleted": deleted})
	}
	return application.ProposalAcceptance{Status: "approved", CreatedCount: created, UpdatedCount: updated, DeletedCount: deleted}, nil
}

func (b platformProposalBackend) acceptSkill(ctx context.Context, id string) (application.ProposalAcceptance, error) {
	b.api.pendingSkillProposalsMu.Lock()
	pending := b.api.pendingSkillProposals[id]
	b.api.pendingSkillProposalsMu.Unlock()
	if pending == nil {
		return application.ProposalAcceptance{}, sql.ErrNoRows
	}
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return application.ProposalAcceptance{}, err
	}
	defer tx.Rollback()
	if err = claimProposalInTx(ctx, tx, id, "approved"); err != nil {
		return application.ProposalAcceptance{}, err
	}
	action := pending.Action
	switch action {
	case "create":
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_skills (project_id,name,content,enabled,category,sort_order) VALUES (?,?,?,?,?,0)`, pending.ProjectID, pending.SkillName, pending.Content, true, pending.Category); err != nil {
			return application.ProposalAcceptance{}, err
		}
	case "update":
		var skill database.ProjectSkill
		err = tx.GetContext(ctx, &skill, `SELECT * FROM project_skills WHERE id=? AND project_id=?`, pending.SkillID, pending.ProjectID)
		if err != nil {
			return application.ProposalAcceptance{}, err
		}
		if pending.SkillName != "" {
			skill.Name = pending.SkillName
		}
		if pending.Content != "" {
			skill.Content = pending.Content
		}
		if pending.Category != "" {
			skill.Category = pending.Category
		}
		if _, err = tx.ExecContext(ctx, `UPDATE project_skills SET name=?,content=?,category=?,updated_at=? WHERE id=? AND project_id=?`, skill.Name, skill.Content, skill.Category, time.Now(), skill.ID, pending.ProjectID); err != nil {
			return application.ProposalAcceptance{}, err
		}
	default:
		return application.ProposalAcceptance{}, errors.New("unsupported skill proposal action")
	}
	if err = tx.Commit(); err != nil {
		return application.ProposalAcceptance{}, err
	}
	b.deletePending(id, application.ProposalSkill)
	result := application.ProposalAcceptance{Status: "approved", Action: action}
	if action == "create" {
		result.CreatedCount = 1
	} else {
		result.UpdatedCount = 1
	}
	return result, nil
}

func (b platformProposalBackend) acceptTool(ctx context.Context, id string) (application.ProposalAcceptance, error) {
	b.api.pendingToolProposalsMu.Lock()
	defer b.api.pendingToolProposalsMu.Unlock()
	pending := b.api.pendingToolProposals[id]
	if pending == nil {
		return application.ProposalAcceptance{}, sql.ErrNoRows
	}
	toolID, _ := pending.Action.Extra["tool_id"].(int64)
	projectID, _ := pending.Action.Extra["project_id"].(int64)
	toolName, _ := pending.Action.Extra["tool_name"].(string)
	input, _ := pending.Action.Extra["input"].(map[string]any)
	if toolID <= 0 || projectID <= 0 {
		return application.ProposalAcceptance{}, errors.New("tool proposal is invalid")
	}
	output, err := b.api.aiHandler.executeCustomProjectToolByID(ctx, projectID, toolID, input)
	if err != nil {
		return application.ProposalAcceptance{}, err
	}
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return application.ProposalAcceptance{}, err
	}
	defer tx.Rollback()
	if err = claimProposalInTx(ctx, tx, id, "approved"); err != nil {
		return application.ProposalAcceptance{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE temp_documents SET content=? WHERE id=?`, "# Tool Executed — "+toolName, id); err != nil {
		return application.ProposalAcceptance{}, err
	}
	if err = tx.Commit(); err != nil {
		return application.ProposalAcceptance{}, err
	}
	delete(b.api.pendingToolProposals, id)
	return application.ProposalAcceptance{Status: "approved", Action: "executed", Output: output}, nil
}

func (b platformProposalBackend) deletePending(id string, kind application.ProposalKind) {
	switch kind {
	case application.ProposalMemory:
		b.api.pendingMemoryDocsMu.Lock()
		delete(b.api.pendingMemoryDocs, id)
		b.api.pendingMemoryDocsMu.Unlock()
	case application.ProposalTask:
		b.api.pendingTaskProposalsMu.Lock()
		delete(b.api.pendingTaskProposals, id)
		b.api.pendingTaskProposalsMu.Unlock()
	case application.ProposalSkill:
		b.api.pendingSkillProposalsMu.Lock()
		delete(b.api.pendingSkillProposals, id)
		b.api.pendingSkillProposalsMu.Unlock()
	case application.ProposalTool:
		b.api.pendingToolProposalsMu.Lock()
		delete(b.api.pendingToolProposals, id)
		b.api.pendingToolProposalsMu.Unlock()
	}
}

func appendProposalTaskEvent(ctx context.Context, tx *sqlx.Tx, eventType string, taskID, projectID int64, proposalID string, actor application.Actor) error {
	metadata := application.EventMetadataFromContext(ctx)
	payload, _ := json.Marshal(map[string]any{"project_id": projectID, "proposal_id": proposalID})
	_, err := database.AppendEventOutbox(ctx, tx, database.EventOutboxAppend{
		EventID: uuid.NewString(), EventType: eventType, AggregateType: "project_task",
		AggregateID: fmt.Sprintf("%d", taskID), Actor: application.EventActorValue(actor),
		CorrelationID: metadata.CorrelationID, SchemaVersion: 1, PayloadJSON: string(payload), OccurredAt: time.Now().UTC(),
	})
	return err
}

func claimProposalInTx(ctx context.Context, tx *sqlx.Tx, id, status string) error {
	result, err := tx.ExecContext(ctx, `UPDATE temp_documents SET status=? WHERE id=? AND status='pending'`, status, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

type platformAIProvider struct{ handler *AIHandler }

func (p platformAIProvider) TestConnection(ctx context.Context, request application.AIConnectionTestRequest) (application.AIConnectionTestResult, error) {
	if p.handler == nil {
		return application.AIConnectionTestResult{}, errors.New("AI provider unavailable")
	}
	return p.handler.probeAIConnection(ctx, request)
}

func (p platformAIProvider) Chat(ctx context.Context, request application.AIProviderChatRequest) (application.AIProviderTextResult, error) {
	provider := p.handler.getProviderForSlot(llm.SlotChat)
	if provider == nil {
		return application.AIProviderTextResult{}, errors.New("AI provider unavailable")
	}
	conversation, err := p.handler.api.db.GetAIConversation(ctx, request.ConversationID)
	if err != nil {
		return application.AIProviderTextResult{}, err
	}
	messages := []llm.Message{}
	if sessionProvider, ok := provider.(llm.SessionProvider); ok && sessionProvider.HasActiveSession(request.ConversationID) {
		messages = append(messages, llm.NewTextMessage("user", request.Prompt))
	} else {
		history, _ := p.handler.api.db.ListAIMessages(ctx, request.ConversationID)
		for _, message := range history {
			if message.Status == "completed" {
				messages = append(messages, llm.NewTextMessage(message.Role, message.Content))
			}
		}
	}
	if request.Feedback != "" && len(messages) > 0 {
		last := &messages[len(messages)-1]
		if last.Role == "user" && len(last.Content) > 0 {
			last.Content[0].Text = request.Feedback + last.Content[0].Text
		}
	}
	_, sessionProvider := provider.(llm.SessionProvider)
	system, _ := p.handler.buildChatRuntime(ctx, conversation, sessionProvider)
	model := p.handler.getSlotModel(llm.SlotChat)
	text, response, err := streamProviderText(ctx, provider, &llm.Request{System: system, Messages: messages, MaxTokens: 8192, Model: model, ConversationID: request.ConversationID, SessionID: conversation.SessionID})
	if err != nil {
		return application.AIProviderTextResult{}, err
	}
	p.recordUsage(ctx, "chat", request.ProjectID, request.ConversationID, response)
	if response != nil && response.Model != "" {
		model = response.Model
	}
	return application.AIProviderTextResult{Text: text, Model: model}, nil
}

func (p platformAIProvider) GenerateSkill(ctx context.Context, description string) (application.AIProviderTextResult, error) {
	return p.GenerateSkillStream(ctx, description, nil)
}

func (p platformAIProvider) GenerateSkillStream(ctx context.Context, description string, onDelta func(string) error) (application.AIProviderTextResult, error) {
	return p.generate(ctx, "skill_generate", llm.SkillGenerationPrompt, description, 4096, onDelta)
}

func (p platformAIProvider) ValidateSkill(ctx context.Context, content string) (application.AISkillValidationResult, error) {
	result, err := p.generate(ctx, "skill_validate", llm.SkillValidationPrompt, content, 1024, nil)
	if err != nil {
		return application.AISkillValidationResult{}, err
	}
	raw := strings.TrimSpace(result.Text)
	if start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); start >= 0 && end >= start {
		raw = raw[start : end+1]
	}
	var parsed application.AISkillValidationResult
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return application.AISkillValidationResult{}, errors.New("AI validation response is invalid")
	}
	return parsed, nil
}

func (p platformAIProvider) generate(ctx context.Context, subcategory, system, prompt string, maxTokens int, onDelta func(string) error) (application.AIProviderTextResult, error) {
	provider := p.handler.getProviderForSlot(llm.SlotBackground)
	if provider == nil {
		return application.AIProviderTextResult{}, errors.New("AI provider unavailable")
	}
	model := p.handler.getSlotModel(llm.SlotBackground)
	text, response, err := streamProviderTextWithDelta(ctx, provider, &llm.Request{System: system, Messages: []llm.Message{llm.NewTextMessage("user", prompt)}, MaxTokens: maxTokens, Model: model}, onDelta)
	if err != nil {
		return application.AIProviderTextResult{}, err
	}
	p.recordUsage(ctx, subcategory, 0, 0, response)
	if response != nil && response.Model != "" {
		model = response.Model
	}
	return application.AIProviderTextResult{Text: text, Model: model}, nil
}

func streamProviderText(ctx context.Context, provider llm.Provider, request *llm.Request) (string, *llm.Response, error) {
	return streamProviderTextWithDelta(ctx, provider, request, nil)
}

func streamProviderTextWithDelta(ctx context.Context, provider llm.Provider, request *llm.Request, onDelta func(string) error) (string, *llm.Response, error) {
	var output strings.Builder
	response, err := provider.StreamMessage(ctx, request, func(event llm.StreamEvent) error {
		if event.Delta != nil && event.Delta.Type == "text_delta" {
			output.WriteString(event.Delta.Text)
			if onDelta != nil {
				return onDelta(event.Delta.Text)
			}
		}
		return nil
	})
	if err != nil {
		return "", response, err
	}
	if output.Len() == 0 && response != nil {
		for _, block := range response.Content {
			if block.Type == "text" {
				output.WriteString(block.Text)
			}
		}
	}
	return output.String(), response, nil
}

func (p platformAIProvider) recordUsage(ctx context.Context, subcategory string, projectID, conversationID int64, response *llm.Response) {
	if response == nil {
		return
	}
	_ = p.handler.api.db.CreateTokenUsage(ctx, &database.TokenUsage{Source: "ai_assistant", Subcategory: subcategory, ProjectID: sql.NullInt64{Int64: projectID, Valid: projectID > 0}, ConversationID: sql.NullInt64{Int64: conversationID, Valid: conversationID > 0}, Model: response.Model, InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens, CacheReadTokens: response.Usage.CacheReadTokens, CacheCreationTokens: response.Usage.CacheCreationTokens, CostUSD: response.CostUSD})
}

type platformAIConversationBackend struct{ api *API }

func (b platformAIConversationBackend) PrepareChatAtomic(ctx context.Context, request application.AIChatPreparationRequest) (*application.AIChatPreparation, error) {
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	conversation := &database.AIConversation{}
	if request.ConversationID > 0 {
		if err = tx.GetContext(ctx, conversation, `SELECT * FROM ai_conversations WHERE id=?`, request.ConversationID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, &application.Error{Kind: application.ErrorNotFound, Code: "ai_conversation_not_found", Message: "Conversation not found", Cause: err}
			}
			return nil, err
		}
		if request.ProjectID > 0 && (conversation.ProactiveContext == "" || conversation.ProactiveContext == "{}") {
			conversation.ProactiveContext = fmt.Sprintf(`{"project_id":%d}`, request.ProjectID)
			if _, err = tx.ExecContext(ctx, `UPDATE ai_conversations SET proactive_context=? WHERE id=? AND (proactive_context='' OR proactive_context='{}')`, conversation.ProactiveContext, conversation.ID); err != nil {
				return nil, err
			}
		}
	} else {
		conversation.Title = request.Title
		conversation.ProactiveContext = "{}"
		if request.ProjectID > 0 {
			conversation.ProactiveContext = fmt.Sprintf(`{"project_id":%d}`, request.ProjectID)
		}
		if request.AgentID != nil {
			conversation.AgentID = sql.NullInt64{Int64: *request.AgentID, Valid: true}
		}
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO ai_conversations (title,proactive_context,agent_id) VALUES (?,?,?)`, conversation.Title, conversation.ProactiveContext, conversation.AgentID)
		if insertErr != nil {
			return nil, insertErr
		}
		conversation.ID, err = result.LastInsertId()
		if err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ai_messages (conversation_id,role,content,tool_calls,status) VALUES (?,'user',?,'[]','completed')`, conversation.ID, request.Prompt); err != nil {
		return nil, err
	}
	var feedbackDocuments []database.TempDocument
	if err = tx.SelectContext(ctx, &feedbackDocuments, `SELECT * FROM temp_documents WHERE conversation_id=? AND status IN ('approved','rejected') AND feedback_ack=0 ORDER BY created_at ASC`, conversation.ID); err != nil {
		return nil, err
	}
	var feedback strings.Builder
	if len(feedbackDocuments) > 0 {
		feedback.WriteString("[System notification — Proposal feedback]\n")
		for _, document := range feedbackDocuments {
			status := "APPROVED"
			if document.Status == "rejected" {
				status = "REJECTED"
			}
			feedback.WriteString(fmt.Sprintf("- %s: %s — %s\n", document.Title, document.Summary, status))
			if _, err = tx.ExecContext(ctx, `UPDATE temp_documents SET feedback_ack=1 WHERE id=? AND feedback_ack=0`, document.ID); err != nil {
				return nil, err
			}
		}
		feedback.WriteString("\n---\n")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_conversations SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, conversation.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &application.AIChatPreparation{Conversation: *aiReference(conversation), Prompt: request.Prompt, Feedback: feedback.String()}, nil
}

func (b platformAIConversationBackend) BeginAssistantMessage(ctx context.Context, conversationID int64) (int64, error) {
	result, err := b.api.db.ExecContext(ctx, `INSERT INTO ai_messages (conversation_id,role,content,tool_calls,status) VALUES (?,'assistant','','[]','streaming')`, conversationID)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (b platformAIConversationBackend) UpdateAssistantMessageProgress(ctx context.Context, progress application.AIChatProgress) error {
	result, err := b.api.db.ExecContext(ctx, `UPDATE ai_messages SET content=?,tool_calls=? WHERE id=? AND conversation_id=? AND status='streaming'`, progress.Content, progress.ToolCallsJSON, progress.MessageID, progress.ConversationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (b platformAIConversationBackend) CompleteAssistantMessageAtomic(ctx context.Context, completion application.AIChatCompletion) error {
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE ai_messages SET status=?,content=?,tool_calls=?,error_info=? WHERE id=? AND conversation_id=?`, completion.Status, completion.Content, completion.ToolCallsJSON, completion.Error, completion.MessageID, completion.ConversationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_conversations SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, completion.ConversationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (b platformAIConversationBackend) DeleteConversationAtomic(ctx context.Context, id int64, _ application.ActionAuthorization) error {
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM temp_documents WHERE conversation_id=?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM ai_conversations WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (b platformAIConversationBackend) DeleteAllConversationsAtomic(ctx context.Context, _ application.ActionAuthorization) (int64, error) {
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM temp_documents WHERE conversation_id IS NOT NULL`); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM ai_conversations`)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (b platformAIConversationBackend) StopConversation(_ context.Context, id int64) error {
	b.api.aiHandler.activeStreamsMu.Lock()
	stream := b.api.aiHandler.activeStreams[id]
	b.api.aiHandler.activeStreamsMu.Unlock()
	if stream != nil {
		stream.cancel()
	}
	b.api.aiHandler.providerMgr.DisconnectSession(id)
	return nil
}

func (b platformAIConversationBackend) InitiateConversationAtomic(ctx context.Context, request application.AIInitiationRequest) (*application.AIConversationReference, error) {
	if b.api == nil || b.api.db == nil {
		return nil, errors.New("AI conversation backend unavailable")
	}
	var (
		conversation *database.AIConversation
		err          error
	)
	switch request.Kind {
	case application.AIInitiateMemoryDoc:
		conversation, err = b.initiateMemoryDocEdit(ctx, request.ProjectID)
	case application.AIInitiateTaskCreation:
		conversation, err = b.initiateTaskCreation(ctx, request)
	case application.AIInitiateTaskDiscussion:
		conversation, err = b.initiateTaskDiscussion(ctx, request.ProjectID, request.TaskID)
	case application.AIInitiateSkillCustomize:
		conversation, err = b.initiateSkillCustomization(ctx, request.ProjectID, request.SkillID)
	case application.AIInitiateProactiveTesting:
		conversation, err = b.initiateProactiveTest(ctx, request.ProactiveLevel)
	default:
		return nil, &application.Error{Kind: application.ErrorValidation, Code: "invalid_ai_initiation_kind", Message: "Unsupported AI conversation initiation"}
	}
	if err != nil {
		return nil, err
	}
	return aiReference(conversation), nil
}

func (b platformAIConversationBackend) initiateMemoryDocEdit(ctx context.Context, projectID int64) (*database.AIConversation, error) {
	project, err := b.api.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, &application.Error{Kind: application.ErrorNotFound, Code: "project_not_found", Message: "Project not found", Cause: err}
	}
	document, docErr := b.api.db.GetMemoryDoc(ctx, projectID)
	documentContent := ""
	if docErr == nil && document != nil {
		documentContent = document.Content
	}
	contextJSON, _ := json.Marshal(map[string]any{
		"project_id": projectID, "project_name": project.Name,
		"memory_doc": documentContent, "intent": "edit_memory_doc",
	})
	var message string
	if documentContent == "" {
		message = fmt.Sprintf("Project **%s** doesn't have a memory doc yet. Would you like me to create one?", project.Name)
	} else {
		preview := truncateAIText(documentContent, 300, "...")
		message = fmt.Sprintf("I loaded the memory doc for project **%s**.\n\nCurrent content preview:\n> %s\n\nWhat would you like to change?", project.Name, preview)
	}
	return b.createProactiveAtomic(ctx, "Edit Memory Doc: "+project.Name, "standard", string(application.AIInitiateMemoryDoc), string(contextJSON), message)
}

func (b platformAIConversationBackend) initiateTaskCreation(ctx context.Context, request application.AIInitiationRequest) (*database.AIConversation, error) {
	project, err := b.api.db.GetProject(ctx, request.ProjectID)
	if err != nil {
		return nil, &application.Error{Kind: application.ErrorNotFound, Code: "project_not_found", Message: "Project not found", Cause: err}
	}
	contextData := map[string]any{
		"intent": "task_creation", "project_id": request.ProjectID, "project_name": project.Name,
		"title": request.Title, "description": request.Description, "status": request.Status,
		"priority": request.Priority, "due_date": request.DueDate,
	}
	if request.ParentID > 0 {
		contextData["parent_id"] = request.ParentID
	}
	contextJSON, _ := json.Marshal(contextData)
	details := ""
	for _, field := range []struct{ label, value string }{
		{"Title", request.Title}, {"Description", request.Description}, {"Priority", request.Priority}, {"Due date", request.DueDate},
	} {
		if field.value != "" {
			details += fmt.Sprintf("- **%s:** %s\n", field.label, field.value)
		}
	}
	message := fmt.Sprintf("AI-assisted task creation for project **%s**.\n\n", project.Name)
	if details != "" {
		message += fmt.Sprintf("Pre-filled data:\n%s\n", details)
	}
	message += "Describe what you need in more detail and I'll help refine the task, break it into subtasks if needed, and create it in the project."
	return b.createProactiveAtomic(ctx, "New Task: "+project.Name, "standard", string(application.AIInitiateTaskCreation), string(contextJSON), message)
}

func (b platformAIConversationBackend) initiateTaskDiscussion(ctx context.Context, projectID, taskID int64) (*database.AIConversation, error) {
	project, err := b.api.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, &application.Error{Kind: application.ErrorNotFound, Code: "project_not_found", Message: "Project not found", Cause: err}
	}
	task, err := b.api.db.GetTask(ctx, taskID)
	if err != nil {
		return nil, &application.Error{Kind: application.ErrorNotFound, Code: "task_not_found", Message: "Task not found", Cause: err}
	}
	if task.ProjectID != projectID {
		return nil, &application.Error{Kind: application.ErrorValidation, Code: "task_project_mismatch", Message: "Task does not belong to this project"}
	}
	var subtasks []database.ProjectTask
	if err = b.api.db.SelectContext(ctx, &subtasks, "SELECT * FROM project_tasks WHERE parent_id = ? ORDER BY sort_order, created_at", taskID); err != nil {
		return nil, err
	}
	contextData := map[string]any{
		"intent": "task_discussion", "project_id": projectID, "project_name": project.Name,
		"task_id": task.ID, "task_title": task.Title, "task_description": task.Description,
		"task_status": task.Status, "task_priority": task.Priority,
	}
	if task.DueDate.Valid {
		contextData["task_due_date"] = task.DueDate.Time.Format("2006-01-02 15:04")
	}
	if len(subtasks) > 0 {
		items := make([]map[string]any, len(subtasks))
		for index, subtask := range subtasks {
			items[index] = map[string]any{"id": subtask.ID, "title": subtask.Title, "status": subtask.Status, "priority": subtask.Priority}
		}
		contextData["existing_subtasks"] = items
	}
	contextJSON, _ := json.Marshal(contextData)
	message := fmt.Sprintf("Discussion about task **%s** of project **%s**.\n\n**Status:** %s | **Priority:** %s\n\n", task.Title, project.Name, task.Status, task.Priority)
	if task.DueDate.Valid {
		message += fmt.Sprintf("**Due date:** %s\n\n", task.DueDate.Time.Format("2006-01-02 15:04"))
	}
	if task.Description != "" {
		message += fmt.Sprintf("**Description:**\n%s\n\n", truncateAIText(task.Description, 500, "..."))
	}
	if len(subtasks) > 0 {
		message += fmt.Sprintf("**Existing subtasks (%d):**\n", len(subtasks))
		for _, subtask := range subtasks {
			message += fmt.Sprintf("- %s (%s)\n", subtask.Title, subtask.Status)
		}
		message += "\n"
	}
	message += "How can I help? I can:\n- **Refine** the task title or description\n- **Break** into detailed subtasks\n- **Adjust** status, priority, or due date\n\nWhat would you like to do?"
	title := "Discussion: " + truncateAIText(task.Title, 60, "...")
	return b.createProactiveAtomic(ctx, title, "standard", string(application.AIInitiateTaskDiscussion), string(contextJSON), message)
}

func (b platformAIConversationBackend) initiateSkillCustomization(ctx context.Context, projectID, skillID int64) (*database.AIConversation, error) {
	project, err := b.api.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, &application.Error{Kind: application.ErrorNotFound, Code: "project_not_found", Message: "Project not found", Cause: err}
	}
	skill, err := b.api.db.GetSkill(ctx, skillID)
	if err != nil {
		return nil, &application.Error{Kind: application.ErrorNotFound, Code: "skill_not_found", Message: "Skill not found", Cause: err}
	}
	memoryContent := ""
	if memory, memoryErr := b.api.db.GetMemoryDoc(ctx, projectID); memoryErr == nil && memory != nil {
		memoryContent = truncateAIText(memory.Content, 3000, "\n...(truncated)")
	}
	instructions := `IMPORTANT INSTRUCTIONS FOR THIS CONVERSATION:
- Your goal is to help the user customize the skill content for their specific project.
- You already have all the context you need: the skill content and the project memory doc are provided below.
- DO NOT use tools like openpoet_list_project_files or openpoet_read_project_file to explore the project. You already have the project context.
- When the user asks you to adapt/customize/create the skill, use the openpoet_create_skill tool to propose the adapted skill content.
- The skill will NOT be created immediately — it requires user approval via a review card.
- Call openpoet_create_skill with the adapted name, content, and category.
- Keep the same markdown structure as the original skill but adapt the content for the specific project.
- Be concise and direct. Propose the adapted content immediately when asked.
- Respond in the same language the user uses (Portuguese or English).`
	contextData := map[string]any{
		"intent": "skill_customization", "instructions": instructions,
		"project_id": projectID, "project_name": project.Name, "project_path": project.Path, "project_type": project.Type,
		"skill_id": skill.ID, "skill_name": skill.Name, "skill_category": skill.Category, "skill_content": skill.Content,
	}
	if memoryContent != "" {
		contextData["project_memory_doc"] = memoryContent
	}
	contextJSON, _ := json.Marshal(contextData)
	content := truncateAIText(skill.Content, 2000, "\n...(truncated)")
	message := fmt.Sprintf("Let's customize skill **%s** for project **%s**.\n\n**Category:** %s\n\n**Current global skill content:**\n```markdown\n%s\n```\n\n", skill.Name, project.Name, skill.Category, content)
	if memoryContent != "" {
		message += "I already have the project context (memory doc) loaded. "
	}
	message += "How can I help adapt this skill for this project? I can:\n- **Adapt** the content to the project's specific needs\n- **Add** specific rules or instructions\n- **Remove** sections that don't apply\n- **Rewrite** completely with focus on this project\n\nWhat would you like to modify?"
	title := truncateAIText(fmt.Sprintf("Customize: %s → %s", skill.Name, project.Name), 80, "")
	return b.createProactiveAtomic(ctx, title, "standard", string(application.AIInitiateSkillCustomize), string(contextJSON), message)
}

func (b platformAIConversationBackend) initiateProactiveTest(ctx context.Context, level string) (*database.AIConversation, error) {
	var title, body, proactiveType string
	var actions []ProactiveAction
	switch level {
	case "critical":
		proactiveType = "alert"
		title = "Critical error detected"
		body = "The system detected a critical error that requires immediate attention. The last process failed with a permission error and data may be in an inconsistent state."
		actions = []ProactiveAction{{Label: "View details", Action: "discuss", Style: "primary"}, {Label: "Ignore", Action: "dismiss", Style: "secondary"}}
	case "subtle":
		proactiveType = "memory_doc_update"
		title = "Memory Doc updated"
		body = "The project memory doc was updated with the CLAUDE.md content."
		actions = []ProactiveAction{{Label: "View", Action: "open", Style: "outline"}}
	default:
		level = "standard"
		proactiveType = "task_suggestion"
		title = "New task suggested"
		body = "While analyzing the recent session, I identified that it would be useful to create a task to review the changes made to the deploy pipeline."
		actions = []ProactiveAction{{Label: "Accept", Action: "accept", Style: "primary"}, {Label: "Discuss", Action: "discuss", Style: "outline"}, {Label: "Ignore", Action: "dismiss", Style: "secondary"}}
	}
	conversation, err := b.createProactiveAtomic(ctx, title, level, proactiveType, "{}", body)
	if err != nil {
		return nil, err
	}
	if b.api.hub != nil {
		b.api.hub.BroadcastAIProactive(map[string]any{
			"level": level, "proactive_type": proactiveType, "title": title, "body": body,
			"conversation_id": conversation.ID, "actions": actions,
		})
	}
	return conversation, nil
}

func truncateAIText(value string, maximum int, suffix string) string {
	if maximum <= 0 || len(value) <= maximum {
		return value
	}
	return value[:maximum] + suffix
}

func (b platformAIConversationBackend) MarkConversationRead(ctx context.Context, id int64) error {
	return b.api.db.MarkConversationRead(ctx, id)
}

func (b platformAIConversationBackend) GetSuggestion(ctx context.Context, id int64) (*application.AISuggestionRecord, error) {
	suggestion, err := b.api.db.GetAISuggestion(ctx, id)
	if err != nil {
		return nil, err
	}
	return &application.AISuggestionRecord{ID: suggestion.ID, Status: suggestion.Status, Type: suggestion.Type, Risk: application.ProposalRiskR3}, nil
}

func (b platformAIConversationBackend) AcceptSuggestionAtomic(ctx context.Context, id int64, authorization application.ActionAuthorization) (application.AISuggestionAcceptance, error) {
	suggestion, err := b.api.db.GetAISuggestion(ctx, id)
	if err != nil {
		return application.AISuggestionAcceptance{}, err
	}
	var payload struct {
		TaskID   *int64         `json:"task_id"`
		TaskData map[string]any `json:"task_data"`
	}
	_ = json.Unmarshal([]byte(suggestion.ContextJSON), &payload)
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return application.AISuggestionAcceptance{}, err
	}
	defer tx.Rollback()
	resultMessage := "Suggestion accepted"
	var (
		taskForBroadcast    *database.ProjectTask
		taskBroadcastAction string
		sessionRename       string
		histories           []*database.TaskHistory
		historyTaskID       int64
	)
	switch suggestion.Type {
	case "link_task":
		if payload.TaskID == nil || suggestion.SessionID == "" {
			return application.AISuggestionAcceptance{}, errors.New("suggestion context is invalid")
		}
		var taskTitle string
		if err = tx.GetContext(ctx, &taskTitle, `SELECT title FROM project_tasks WHERE id=?`, *payload.TaskID); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		sessionRename = "Task: " + taskTitle
		linkResult, updateErr := tx.ExecContext(ctx, `UPDATE sessions SET task_id=?, name=?, last_activity_at=CURRENT_TIMESTAMP WHERE id=?`, *payload.TaskID, sessionRename, suggestion.SessionID)
		if updateErr != nil {
			return application.AISuggestionAcceptance{}, updateErr
		}
		if affected, _ := linkResult.RowsAffected(); affected != 1 {
			return application.AISuggestionAcceptance{}, sql.ErrNoRows
		}
		historyTaskID = *payload.TaskID
		resultMessage = fmt.Sprintf("Session linked to task %d", *payload.TaskID)
	case "create_task":
		title, _ := payload.TaskData["title"].(string)
		if title == "" {
			title = suggestion.Title
		}
		description, _ := payload.TaskData["description"].(string)
		priority, _ := payload.TaskData["priority"].(string)
		if priority == "" {
			priority = "medium"
		}
		taskForBroadcast, err = createAISuggestionTaskInTx(ctx, tx, suggestion.ProjectID, title, description, priority)
		if err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		taskBroadcastAction = "created"
		historyTaskID = taskForBroadcast.ID
		if suggestion.SessionID != "" {
			sessionRename = "Task: " + title
			if _, err = tx.ExecContext(ctx, `UPDATE sessions SET task_id=?, name=?, last_activity_at=CURRENT_TIMESTAMP WHERE id=?`, taskForBroadcast.ID, sessionRename, suggestion.SessionID); err != nil {
				return application.AISuggestionAcceptance{}, err
			}
		}
		resultMessage = fmt.Sprintf("Task created: [%d] %s", taskForBroadcast.ID, title)
	case "update_task":
		if payload.TaskID == nil {
			return application.AISuggestionAcceptance{}, errors.New("suggestion context is invalid")
		}
		taskForBroadcast, err = getAITaskInTx(ctx, tx, *payload.TaskID)
		if err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		if status, ok := payload.TaskData["status"].(string); ok && status != "" {
			taskForBroadcast.Status = status
		}
		if description, ok := payload.TaskData["description"].(string); ok {
			taskForBroadcast.Description = description
		}
		if _, err = tx.ExecContext(ctx, `UPDATE project_tasks SET status=?,description=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, taskForBroadcast.Status, taskForBroadcast.Description, taskForBroadcast.ID); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		taskBroadcastAction = "updated"
		historyTaskID = taskForBroadcast.ID
		resultMessage = fmt.Sprintf("Task [%d] updated", taskForBroadcast.ID)
	case "complete_task":
		if payload.TaskID == nil {
			return application.AISuggestionAcceptance{}, errors.New("suggestion context is invalid")
		}
		taskForBroadcast, err = getAITaskInTx(ctx, tx, *payload.TaskID)
		if err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		taskForBroadcast.Status = "done"
		if _, err = tx.ExecContext(ctx, `UPDATE project_tasks SET status='done',updated_at=CURRENT_TIMESTAMP WHERE id=?`, taskForBroadcast.ID); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		taskBroadcastAction = "updated"
		historyTaskID = taskForBroadcast.ID
		resultMessage = fmt.Sprintf("Task [%d] marked as done", taskForBroadcast.ID)
	case "unlink_task":
		if suggestion.SessionID == "" {
			return application.AISuggestionAcceptance{}, errors.New("suggestion context is invalid")
		}
		var oldTaskID sql.NullInt64
		var oldSessionName string
		if err = tx.QueryRowxContext(ctx, `SELECT task_id,name FROM sessions WHERE id=?`, suggestion.SessionID).Scan(&oldTaskID, &oldSessionName); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET task_id=NULL WHERE id=?`, suggestion.SessionID); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		title, _ := payload.TaskData["title"].(string)
		if title == "" {
			title = suggestion.Title
		}
		description, _ := payload.TaskData["description"].(string)
		priority, _ := payload.TaskData["priority"].(string)
		if priority == "" {
			priority = "medium"
		}
		taskForBroadcast, err = createAISuggestionTaskInTx(ctx, tx, suggestion.ProjectID, title, description, priority)
		if err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		taskBroadcastAction = "created"
		historyTaskID = taskForBroadcast.ID
		sessionRename = "Task: " + taskForBroadcast.Title
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET task_id=?,name=?,last_activity_at=CURRENT_TIMESTAMP WHERE id=?`, taskForBroadcast.ID, sessionRename, suggestion.SessionID); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		if oldTaskID.Valid && oldTaskID.Int64 > 0 {
			history, historyErr := createAITaskHistoryInTx(ctx, tx, oldTaskID.Int64, suggestion.ProjectID, "session_unlinked", map[string]any{
				"session_id": suggestion.SessionID, "session_name": oldSessionName, "reason": "scope_drift",
			}, "system", suggestion.SessionID)
			if historyErr != nil {
				return application.AISuggestionAcceptance{}, historyErr
			}
			histories = append(histories, history)
		}
		history, historyErr := createAITaskHistoryInTx(ctx, tx, taskForBroadcast.ID, taskForBroadcast.ProjectID, "session_linked", map[string]any{
			"session_id": suggestion.SessionID, "session_name": sessionRename,
		}, "system", suggestion.SessionID)
		if historyErr != nil {
			return application.AISuggestionAcceptance{}, historyErr
		}
		histories = append(histories, history)
		resultMessage = fmt.Sprintf("Unlinked from task %d, created and linked to new task [%d] %s", oldTaskID.Int64, taskForBroadcast.ID, title)
	default:
		return application.AISuggestionAcceptance{}, errors.New("unsupported suggestion type")
	}
	suggestionResult, err := tx.ExecContext(ctx, `UPDATE ai_suggestions SET status='accepted' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return application.AISuggestionAcceptance{}, err
	}
	if affected, _ := suggestionResult.RowsAffected(); affected != 1 {
		return application.AISuggestionAcceptance{}, sql.ErrNoRows
	}
	if suggestion.ConversationID.Valid {
		if _, err = tx.ExecContext(ctx, `INSERT INTO ai_messages (conversation_id,role,content,tool_calls,status) VALUES (?,'user',?,'[]','completed')`, suggestion.ConversationID.Int64, "Aceito. "+resultMessage); err != nil {
			return application.AISuggestionAcceptance{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE ai_conversations SET is_read=1, updated_at=CURRENT_TIMESTAMP WHERE id=?`, suggestion.ConversationID.Int64)
		if err != nil {
			return application.AISuggestionAcceptance{}, err
		}
	}
	if historyTaskID > 0 {
		history, historyErr := createAITaskHistoryInTx(ctx, tx, historyTaskID, suggestion.ProjectID, "suggestion_accepted", map[string]any{
			"suggestion_type": suggestion.Type, "title": suggestion.Title,
		}, "user", suggestion.SessionID)
		if historyErr != nil {
			return application.AISuggestionAcceptance{}, historyErr
		}
		histories = append(histories, history)
	}
	if err = tx.Commit(); err != nil {
		return application.AISuggestionAcceptance{}, err
	}
	if b.api.hub != nil {
		if taskForBroadcast != nil {
			b.api.hub.BroadcastStateUpdate("task", map[string]any{
				"action": taskBroadcastAction, "project_id": taskForBroadcast.ProjectID, "task": taskForBroadcast,
			})
		}
		if sessionRename != "" {
			b.api.hub.BroadcastStateUpdate("session", map[string]any{
				"action": "renamed", "session_id": suggestion.SessionID, "name": sessionRename,
			})
		}
		for _, history := range histories {
			broadcastAITaskHistory(b.api, history)
		}
	}
	_ = authorization
	return application.AISuggestionAcceptance{Status: "accepted", Message: resultMessage}, nil
}

func createAISuggestionTaskInTx(ctx context.Context, tx *sqlx.Tx, projectID int64, title, description, priority string) (*database.ProjectTask, error) {
	var sortOrder, globalOrder int
	if err := tx.GetContext(ctx, &sortOrder, `SELECT COALESCE(MAX(sort_order),0)+1 FROM project_tasks WHERE project_id=? AND parent_id IS NULL`, projectID); err != nil {
		return nil, err
	}
	if err := tx.GetContext(ctx, &globalOrder, `SELECT COALESCE(MAX(global_sort_order),0)+1 FROM project_tasks`); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO project_tasks (project_id,title,description,status,priority,sort_order,global_sort_order) VALUES (?,?,?,?,?,?,?)`, projectID, title, description, "in_progress", priority, sortOrder, globalOrder)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return getAITaskInTx(ctx, tx, id)
}

func getAITaskInTx(ctx context.Context, tx *sqlx.Tx, id int64) (*database.ProjectTask, error) {
	var task database.ProjectTask
	if err := tx.GetContext(ctx, &task, `SELECT * FROM project_tasks WHERE id=?`, id); err != nil {
		return nil, err
	}
	return &task, nil
}

func createAITaskHistoryInTx(ctx context.Context, tx *sqlx.Tx, taskID, projectID int64, eventType string, details map[string]any, actor, sessionID string) (*database.TaskHistory, error) {
	detailsJSON, _ := json.Marshal(details)
	history := &database.TaskHistory{TaskID: taskID, ProjectID: projectID, EventType: eventType, Details: string(detailsJSON), Actor: actor}
	if sessionID != "" {
		history.SessionID = sql.NullString{String: sessionID, Valid: true}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO task_history (task_id,project_id,event_type,details,actor,session_id) VALUES (?,?,?,?,?,?)`, taskID, projectID, eventType, history.Details, actor, history.SessionID)
	if err != nil {
		return nil, err
	}
	history.ID, err = result.LastInsertId()
	if err != nil {
		return nil, err
	}
	history.CreatedAt = time.Now()
	return history, nil
}

func broadcastAITaskHistory(api *API, history *database.TaskHistory) {
	if api == nil || api.hub == nil || history == nil {
		return
	}
	api.hub.BroadcastStateUpdate("task_history", map[string]any{
		"action": "created", "task_id": history.TaskID, "project_id": history.ProjectID, "entry": history,
	})
}

func (b platformAIConversationBackend) DismissSuggestionAtomic(ctx context.Context, id int64, _ application.ActionAuthorization) error {
	suggestion, err := b.api.db.GetAISuggestion(ctx, id)
	if err != nil {
		return err
	}
	var payload struct {
		TaskID *int64 `json:"task_id"`
	}
	_ = json.Unmarshal([]byte(suggestion.ContextJSON), &payload)
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE ai_suggestions SET status='dismissed' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return sql.ErrNoRows
	}
	if suggestion.ConversationID.Valid {
		if _, err = tx.ExecContext(ctx, `INSERT INTO ai_messages (conversation_id,role,content,tool_calls,status) VALUES (?,'user','Ignorado.','[]','completed')`, suggestion.ConversationID.Int64); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ai_conversations SET is_read=1,updated_at=CURRENT_TIMESTAMP WHERE id=?`, suggestion.ConversationID.Int64); err != nil {
			return err
		}
	}
	var history *database.TaskHistory
	if payload.TaskID != nil && *payload.TaskID > 0 {
		history, err = createAITaskHistoryInTx(ctx, tx, *payload.TaskID, suggestion.ProjectID, "suggestion_dismissed", map[string]any{
			"suggestion_type": suggestion.Type, "title": suggestion.Title,
		}, "user", suggestion.SessionID)
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	broadcastAITaskHistory(b.api, history)
	return nil
}

func (b platformAIConversationBackend) DiscussSuggestionAtomic(ctx context.Context, id int64, _ application.ActionAuthorization) (*application.AIConversationReference, error) {
	suggestion, err := b.api.db.GetAISuggestion(ctx, id)
	if err != nil {
		return nil, err
	}
	if suggestion.ConversationID.Valid {
		conversation, err := b.api.db.GetAIConversation(ctx, suggestion.ConversationID.Int64)
		if err != nil {
			return nil, err
		}
		return aiReference(conversation), nil
	}
	typeLabel := map[string]string{
		"link_task": "Link Task", "create_task": "New Task", "update_task": "Update Task", "complete_task": "Complete Task", "unlink_task": "Change Task",
	}[suggestion.Type]
	if typeLabel == "" {
		typeLabel = suggestion.Type
	}
	message := fmt.Sprintf("I identified an opportunity and would like to suggest an action:\n\n**Type:** %s\n**Title:** %s\n", typeLabel, suggestion.Title)
	if suggestion.Description != "" {
		message += fmt.Sprintf("**Description:** %s\n", suggestion.Description)
	}
	message += "\nI can help refine this suggestion. What would you like to do?"
	contextJSON, _ := json.Marshal(map[string]any{"suggestion_id": id, "session_id": suggestion.SessionID, "project_id": suggestion.ProjectID})
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO ai_conversations (title,source,proactive_level,proactive_type,proactive_context,is_read) VALUES (?,'ai',?,'task_suggestion',?,0)`, suggestion.Title, suggestion.Level, string(contextJSON))
	if err != nil {
		return nil, err
	}
	conversationID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ai_messages (conversation_id,role,content,tool_calls,status) VALUES (?,'assistant',?,'[]','completed')`, conversationID, message); err != nil {
		return nil, err
	}
	update, err := tx.ExecContext(ctx, `UPDATE ai_suggestions SET conversation_id=? WHERE id=? AND status='pending'`, conversationID, id)
	if err != nil {
		return nil, err
	}
	if affected, _ := update.RowsAffected(); affected != 1 {
		return nil, sql.ErrNoRows
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	conversation, err := b.api.db.GetAIConversation(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if b.api.hub != nil {
		b.api.hub.BroadcastAIProactive(map[string]any{
			"level": suggestion.Level, "proactive_type": "task_suggestion", "title": suggestion.Title,
			"body": message, "conversation_id": conversation.ID, "actions": []ProactiveAction(nil),
			"suggestion_id": suggestion.ID, "session_id": suggestion.SessionID, "project_id": suggestion.ProjectID,
		})
	}
	return aiReference(conversation), nil
}

func (b platformAIConversationBackend) createProactiveAtomic(ctx context.Context, title, level, kind, contextJSON, message string) (*database.AIConversation, error) {
	tx, err := b.api.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO ai_conversations (title,source,proactive_level,proactive_type,proactive_context,is_read) VALUES (?,'ai',?,?,?,0)`, title, level, kind, contextJSON)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	if _, err = tx.ExecContext(ctx, `INSERT INTO ai_messages (conversation_id,role,content,tool_calls,status) VALUES (?,'assistant',?,'[]','completed')`, id, message); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return b.api.db.GetAIConversation(ctx, id)
}

func aiReference(conversation *database.AIConversation) *application.AIConversationReference {
	return &application.AIConversationReference{ID: conversation.ID, Title: conversation.Title, Source: conversation.Source, ProactiveType: conversation.ProactiveType, IsRead: conversation.IsRead}
}

type platformAIToolExecutor struct{ handler *AIHandler }

func (e platformAIToolExecutor) ExecuteAITool(ctx context.Context, request application.AIToolExecutionRequest) (application.AIToolExecutionResult, error) {
	if e.handler == nil {
		return application.AIToolExecutionResult{}, errors.New("AI tool executor unavailable")
	}
	output, err := e.handler.ExecuteToolAuthorized(ctx, request.Name, request.Arguments, request.ConversationID, request.Authorization)
	return application.AIToolExecutionResult{Output: output, ExitCode: 0}, err
}

type platformNotificationDeliveryBackend struct {
	db   *database.DB
	push *notifications.WebPushService
}

func (b platformNotificationDeliveryBackend) SubscribePush(ctx context.Context, input application.PushSubscriptionInput) error {
	return b.push.Subscribe(ctx, input.Endpoint, input.P256dh, input.Auth)
}
func (b platformNotificationDeliveryBackend) UnsubscribePush(ctx context.Context, endpoint string) error {
	return b.push.Unsubscribe(ctx, endpoint)
}
func (b platformNotificationDeliveryBackend) GetPushDisabled(ctx context.Context) (bool, error) {
	value, err := b.db.GetSetting(ctx, "push_notifications_disabled")
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value == "true", err
}
func (b platformNotificationDeliveryBackend) SetPushDisabled(ctx context.Context, disabled bool) error {
	if disabled {
		return b.db.SetSetting(ctx, "push_notifications_disabled", "true")
	}
	return b.db.DeleteSetting(ctx, "push_notifications_disabled")
}
func (b platformNotificationDeliveryBackend) SendTestPush(ctx context.Context) (application.PushTestResult, error) {
	if err := b.push.SendToAll(ctx, "OpenPoet Test", "Push notifications are working!", map[string]string{"type": "test"}); err != nil {
		return application.PushTestResult{}, err
	}
	return application.PushTestResult{Status: "sent", Message: "Test notification sent"}, nil
}
