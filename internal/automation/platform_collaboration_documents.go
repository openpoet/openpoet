package automation

import (
	"context"

	"openpoet/internal/application"
)

type documentCollaborationExecutor struct{ service *application.DocumentService }

type documentUpdateMemoryPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Content   string `json:"content"`
	Summary   string `json:"summary,omitempty"`
}

type documentCreateTempPayload struct {
	Title          string `json:"title,omitempty"`
	Content        string `json:"content"`
	Summary        string `json:"summary,omitempty"`
	ConversationID *int64 `json:"conversation_id,omitempty"`
	TaskID         *int64 `json:"task_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
}

type documentAutomationView struct {
	ID             string `json:"id,omitempty"`
	ProjectID      int64  `json:"project_id,omitempty"`
	Title          string `json:"title,omitempty"`
	Content        string `json:"content,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Status         string `json:"status,omitempty"`
	Version        int    `json:"version,omitempty"`
	Exists         bool   `json:"exists,omitempty"`
	ConversationID *int64 `json:"conversation_id,omitempty"`
	TaskID         *int64 `json:"task_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	WasTruncated   bool   `json:"was_truncated,omitempty"`
}

func documentCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationReadCapability("documents.get_memory", "documents", "documents:read"),
		collaborationReadCapability("documents.get_temp", "documents", "documents:read"),
		collaborationPayloadLimit(collaborationWriteCapability("documents.update_memory", "documents", "documents:write"), 512<<10),
		collaborationPayloadLimit(collaborationWriteCapability("documents.create_temp", "documents", "documents:write"), 512<<10),
	}
}

func (e *documentCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeCollaborationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "documents.get_memory":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		projectID, err := collaborationProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"project_id": projectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			view, err := e.service.GetMemory(ctx, projectID)
			if err != nil {
				return nil, err
			}
			return memoryDocumentAutomationView(view), nil
		}}, nil
	case "documents.get_temp":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "document id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"document_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			view, err := e.service.GetTemp(ctx, id)
			if err != nil {
				return nil, err
			}
			return tempDocumentAutomationView(view), nil
		}}, nil
	case "documents.update_memory":
		var payload documentUpdateMemoryPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := collaborationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		if payload.Content, err = boundedCollaborationInput(payload.Content, "document content", maxCollaborationInputRunes, true); err != nil {
			return nil, err
		}
		if payload.Summary, err = boundedCollaborationInput(payload.Summary, "document summary", 16<<10, false); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"project_id": projectID, "content_bytes": len(payload.Content), "has_summary": payload.Summary != "",
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.UpdateMemory(ctx, application.UpdateMemoryDocumentCommand{
				ProjectID: projectID, Content: payload.Content, Summary: payload.Summary, Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return memoryDocumentAutomationView(view), nil
		}}, nil
	case "documents.create_temp":
		var payload documentCreateTempPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.Title, err = boundedCollaborationInput(payload.Title, "document title", 1000, false); err != nil {
			return nil, err
		}
		if payload.Content, err = boundedCollaborationInput(payload.Content, "document content", maxCollaborationInputRunes, true); err != nil {
			return nil, err
		}
		if payload.Summary, err = boundedCollaborationInput(payload.Summary, "document summary", 16<<10, false); err != nil {
			return nil, err
		}
		if payload.SessionID, err = boundedCollaborationInput(payload.SessionID, "session id", 200, false); err != nil {
			return nil, err
		}
		if payload.ConversationID != nil && *payload.ConversationID <= 0 || payload.TaskID != nil && *payload.TaskID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "document references must be positive", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"content_bytes": len(payload.Content), "has_conversation": payload.ConversationID != nil,
			"has_task": payload.TaskID != nil, "has_session": payload.SessionID != "",
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.CreateTemp(ctx, application.CreateTempDocumentCommand{
				Title: payload.Title, Content: payload.Content, Summary: payload.Summary,
				ConversationID: payload.ConversationID, TaskID: payload.TaskID, SessionID: payload.SessionID,
				Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return tempDocumentAutomationView(view), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the document capability handler is unsupported", false)
	}
}

func memoryDocumentAutomationView(view application.MemoryDocumentView) documentAutomationView {
	content, contentTruncated := boundedCollaborationOutput(view.Content, maxCollaborationOutputBytes)
	summary, summaryTruncated := boundedCollaborationOutput(view.Summary, maxCollaborationSummaryBytes)
	return documentAutomationView{
		ProjectID: view.ProjectID, Content: content, Summary: summary, Version: view.Version,
		Exists: view.Exists, WasTruncated: view.WasTruncated || contentTruncated || summaryTruncated,
	}
}

func tempDocumentAutomationView(view application.TempDocumentView) documentAutomationView {
	title, titleTruncated := boundedCollaborationOutput(view.Title, 1000)
	content, contentTruncated := boundedCollaborationOutput(view.Content, maxCollaborationOutputBytes)
	summary, summaryTruncated := boundedCollaborationOutput(view.Summary, maxCollaborationSummaryBytes)
	status, statusTruncated := boundedCollaborationOutput(view.Status, 100)
	sessionID, sessionTruncated := boundedCollaborationOutput(view.SessionID, 200)
	return documentAutomationView{
		ID: view.ID, Title: title, Content: content, Summary: summary, Status: status,
		ConversationID: view.ConversationID, TaskID: view.TaskID, SessionID: sessionID,
		WasTruncated: view.WasTruncated || titleTruncated || contentTruncated || summaryTruncated || statusTruncated || sessionTruncated,
	}
}

type proposalCollaborationExecutor struct{ service *application.ProposalService }

type memoryProposalPayload struct {
	ProjectID      int64  `json:"project_id,omitempty"`
	Content        string `json:"content"`
	Summary        string `json:"summary,omitempty"`
	ConversationID *int64 `json:"conversation_id,omitempty"`
}

type proposalAutomationView struct {
	ID             string                   `json:"id,omitempty"`
	Kind           application.ProposalKind `json:"kind,omitempty"`
	Risk           application.ProposalRisk `json:"risk,omitempty"`
	Status         string                   `json:"status"`
	Title          string                   `json:"title,omitempty"`
	Summary        string                   `json:"summary,omitempty"`
	Action         string                   `json:"action,omitempty"`
	Message        string                   `json:"message,omitempty"`
	Output         string                   `json:"output,omitempty"`
	ProjectID      int64                    `json:"project_id,omitempty"`
	ConversationID *int64                   `json:"conversation_id,omitempty"`
	CreatedCount   int                      `json:"created,omitempty"`
	UpdatedCount   int                      `json:"updated,omitempty"`
	DeletedCount   int                      `json:"deleted,omitempty"`
	WasTruncated   bool                     `json:"was_truncated,omitempty"`
}

func proposalCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationPayloadLimit(collaborationWriteCapability("proposals.memory.create", "proposals", "documents:write"), 512<<10),
		collaborationWriteCapability("proposals.memory.approve", "proposals", "documents:write"),
		platformMutation(collaborationReadCapability("proposals.memory.reject", "proposals", "documents:write")),
		collaborationDestructiveCapability("proposals.task.approve", "proposals", "tasks:write"),
		platformMutation(collaborationReadCapability("proposals.task.reject", "proposals", "tasks:write")),
		collaborationWriteCapability("proposals.skill.approve", "proposals", "skills:write"),
		platformMutation(collaborationReadCapability("proposals.skill.reject", "proposals", "skills:write")),
		collaborationUnsafeCapability("proposals.tool.approve", "proposals", "tools:execute"),
		platformMutation(collaborationReadCapability("proposals.tool.reject", "proposals", "tools:execute")),
	}
}

func (e *proposalCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeCollaborationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	if input.Handler == "proposals.memory.create" {
		var payload memoryProposalPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := collaborationProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		if payload.Content, err = boundedCollaborationInput(payload.Content, "proposal content", maxCollaborationInputRunes, true); err != nil {
			return nil, err
		}
		if payload.Summary, err = boundedCollaborationInput(payload.Summary, "proposal summary", 16<<10, false); err != nil {
			return nil, err
		}
		if payload.ConversationID != nil && *payload.ConversationID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "conversation id must be positive", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"project_id": projectID, "content_bytes": len(payload.Content), "has_conversation": payload.ConversationID != nil,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.ProposeMemory(ctx, application.MemoryProposal{
				ProjectID: projectID, Content: payload.Content, Summary: payload.Summary,
				ConversationID: payload.ConversationID, Authorization: authorization,
			})
			if err != nil {
				return nil, err
			}
			return proposalViewForAutomation(view), nil
		}}, nil
	}
	if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
		return nil, err
	}
	id, err := collaborationStringID(target, "", "proposal id")
	if err != nil {
		return nil, err
	}
	preview := collaborationPreview(input.Handler, map[string]any{"proposal_id": id})
	switch input.Handler {
	case "proposals.memory.approve", "proposals.task.approve", "proposals.skill.approve", "proposals.tool.approve":
		return &collaborationValidatedCommand{preview: preview, execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			var view application.ProposalAcceptanceView
			var err error
			switch input.Handler {
			case "proposals.memory.approve":
				view, err = e.service.ApproveMemory(ctx, id, authorization)
			case "proposals.task.approve":
				view, err = e.service.ApproveTask(ctx, id, authorization)
			case "proposals.skill.approve":
				view, err = e.service.ApproveSkill(ctx, id, authorization)
			case "proposals.tool.approve":
				view, err = e.service.ApproveTool(ctx, id, authorization)
			}
			if err != nil {
				return nil, err
			}
			return proposalAcceptanceForAutomation(view), nil
		}}, nil
	case "proposals.memory.reject", "proposals.task.reject", "proposals.skill.reject", "proposals.tool.reject":
		return &collaborationValidatedCommand{preview: preview, execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			var err error
			switch input.Handler {
			case "proposals.memory.reject":
				err = e.service.RejectMemory(ctx, id, authorization)
			case "proposals.task.reject":
				err = e.service.RejectTask(ctx, id, authorization)
			case "proposals.skill.reject":
				err = e.service.RejectSkill(ctx, id, authorization)
			case "proposals.tool.reject":
				err = e.service.RejectTool(ctx, id, authorization)
			}
			if err != nil {
				return nil, err
			}
			return proposalAutomationView{ID: id, Status: "rejected"}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the proposal capability handler is unsupported", false)
	}
}

func proposalViewForAutomation(view application.ProposalView) proposalAutomationView {
	title, titleTruncated := boundedCollaborationOutput(view.Title, 1000)
	summary, summaryTruncated := boundedCollaborationOutput(view.Summary, maxCollaborationSummaryBytes)
	status, statusTruncated := boundedCollaborationOutput(view.Status, 100)
	return proposalAutomationView{
		ID: view.ID, Kind: view.Kind, Risk: view.Risk, Status: status, Title: title, Summary: summary,
		ProjectID: view.ProjectID, ConversationID: view.ConversationID,
		WasTruncated: titleTruncated || summaryTruncated || statusTruncated,
	}
}

func proposalAcceptanceForAutomation(view application.ProposalAcceptanceView) proposalAutomationView {
	status, statusTruncated := boundedCollaborationOutput(view.Status, 100)
	action, actionTruncated := boundedCollaborationOutput(view.Action, 100)
	message, messageTruncated := boundedCollaborationOutput(view.Message, maxCollaborationSummaryBytes)
	output, outputTruncated := boundedCollaborationOutput(view.Output, maxCollaborationOutputBytes)
	return proposalAutomationView{
		Status: status, Action: action, Message: message, Output: output,
		CreatedCount: view.CreatedCount, UpdatedCount: view.UpdatedCount, DeletedCount: view.DeletedCount,
		WasTruncated: view.WasTruncated || statusTruncated || actionTruncated || messageTruncated || outputTruncated,
	}
}
