package automation

import (
	"context"

	"openpoet/internal/application"
)

// Artifact capabilities share the documents:read / documents:write scopes: an
// HTML artifact is a document an agent publishes, so a client that may publish
// Docs may publish artifacts, with no extra grant.

type artifactCollaborationExecutor struct{ service *application.ArtifactService }

// maxArtifactAutomationPayloadBytes is the automation plane's request body
// limit (DefaultBodyLimit). One automation command therefore carries a smaller
// HTML than application.MaxArtifactHTMLBytes; REST and MCP callers get the full
// per-version limit.
const maxArtifactAutomationPayloadBytes = int(DefaultBodyLimit)

type artifactCreatePayload struct {
	Title          string `json:"title,omitempty"`
	HTML           string `json:"html"`
	Summary        string `json:"summary,omitempty"`
	ProjectID      *int64 `json:"project_id,omitempty"`
	TaskID         *int64 `json:"task_id,omitempty"`
	ConversationID *int64 `json:"conversation_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
}

type artifactUpdatePayload struct {
	HTML        string `json:"html"`
	ChangeNote  string `json:"change_note,omitempty"`
	BaseVersion int    `json:"base_version,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
}

type artifactUpdateMetaPayload struct {
	Title   *string `json:"title,omitempty"`
	Summary *string `json:"summary,omitempty"`
	Status  *string `json:"status,omitempty"`
}

type artifactListPayload struct {
	ProjectID       int64  `json:"project_id,omitempty"`
	TaskID          int64  `json:"task_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	IncludeArchived bool   `json:"include_archived,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}

type artifactGetVersionPayload struct {
	Version int `json:"version,omitempty"`
}

type artifactVersionAutomationView struct {
	application.ArtifactVersionView
	WasTruncated bool `json:"was_truncated,omitempty"`
}

func artifactCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationReadCapability("artifacts.list", "artifacts", "documents:read"),
		collaborationReadCapability("artifacts.get", "artifacts", "documents:read"),
		collaborationReadCapability("artifacts.get_version", "artifacts", "documents:read"),
		collaborationPayloadLimit(collaborationWriteCapability("artifacts.create", "artifacts", "documents:write"), maxArtifactAutomationPayloadBytes),
		collaborationPayloadLimit(collaborationWriteCapability("artifacts.update", "artifacts", "documents:write"), maxArtifactAutomationPayloadBytes),
		collaborationWriteCapability("artifacts.update_meta", "artifacts", "documents:write"),
	}
}

func (e *artifactCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeCollaborationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "artifacts.list":
		var payload artifactListPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.ProjectID == 0 {
			payload.ProjectID = target.ProjectID
		}
		if payload.Limit == 0 {
			payload.Limit = maxCollaborationListItems
		}
		if payload.ProjectID < 0 || payload.TaskID < 0 || payload.Limit < 1 || payload.Limit > maxCollaborationListItems {
			return nil, platformFailure("platform_payload_invalid", "artifact list filters are invalid", false)
		}
		if payload.SessionID, err = boundedCollaborationInput(payload.SessionID, "session id", 200, false); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"project_id": payload.ProjectID, "task_id": payload.TaskID, "has_session": payload.SessionID != "", "limit": payload.Limit,
		}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.List(ctx, application.ArtifactListQuery{
				ProjectID: payload.ProjectID, TaskID: payload.TaskID, SessionID: payload.SessionID,
				IncludeArchived: payload.IncludeArchived, Limit: payload.Limit,
			})
		}}, nil
	case "artifacts.get":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"artifact_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.Get(ctx, id)
		}}, nil
	case "artifacts.get_version":
		var payload artifactGetVersionPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.Version < 0 {
			return nil, platformFailure("platform_payload_invalid", "artifact version cannot be negative", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"artifact_id": id, "version": payload.Version}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			view, err := e.service.GetVersion(ctx, id, payload.Version)
			if err != nil {
				return nil, err
			}
			content, truncated := boundedCollaborationOutput(view.Content, application.MaxArtifactHTMLBytes)
			view.Content = content
			return artifactVersionAutomationView{ArtifactVersionView: view, WasTruncated: truncated}, nil
		}}, nil
	case "artifacts.create":
		var payload artifactCreatePayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.Title, err = boundedCollaborationInput(payload.Title, "artifact title", 1000, false); err != nil {
			return nil, err
		}
		if payload.HTML, err = boundedCollaborationInput(payload.HTML, "artifact html", application.MaxArtifactHTMLBytes, true); err != nil {
			return nil, err
		}
		if payload.Summary, err = boundedCollaborationInput(payload.Summary, "artifact summary", 16<<10, false); err != nil {
			return nil, err
		}
		if payload.SessionID, err = boundedCollaborationInput(payload.SessionID, "session id", 200, false); err != nil {
			return nil, err
		}
		if payload.ProjectID != nil && *payload.ProjectID <= 0 || payload.TaskID != nil && *payload.TaskID <= 0 ||
			payload.ConversationID != nil && *payload.ConversationID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "artifact references must be positive", false)
		}
		if payload.ProjectID == nil && target.ProjectID > 0 {
			projectID := target.ProjectID
			payload.ProjectID = &projectID
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"html_bytes": len(payload.HTML), "has_project": payload.ProjectID != nil, "has_task": payload.TaskID != nil,
			"has_conversation": payload.ConversationID != nil, "has_session": payload.SessionID != "",
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.Create(ctx, application.CreateArtifactCommand{
				Title: payload.Title, Summary: payload.Summary, Content: payload.HTML,
				ProjectID: payload.ProjectID, TaskID: payload.TaskID, ConversationID: payload.ConversationID,
				SessionID: payload.SessionID, Authorization: authorization,
			})
		}}, nil
	case "artifacts.update":
		var payload artifactUpdatePayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.HTML, err = boundedCollaborationInput(payload.HTML, "artifact html", application.MaxArtifactHTMLBytes, true); err != nil {
			return nil, err
		}
		if payload.ChangeNote, err = boundedCollaborationInput(payload.ChangeNote, "artifact change note", 1000, false); err != nil {
			return nil, err
		}
		if payload.SessionID, err = boundedCollaborationInput(payload.SessionID, "session id", 200, false); err != nil {
			return nil, err
		}
		if payload.BaseVersion < 0 {
			return nil, platformFailure("platform_payload_invalid", "base_version cannot be negative", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "html_bytes": len(payload.HTML), "base_version": payload.BaseVersion, "has_session": payload.SessionID != "",
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.Update(ctx, application.UpdateArtifactCommand{
				ArtifactID: id, Content: payload.HTML, ChangeNote: payload.ChangeNote, BaseVersion: payload.BaseVersion,
				SessionID: payload.SessionID, Authorization: authorization,
			})
		}}, nil
	case "artifacts.update_meta":
		var payload artifactUpdateMetaPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.Title == nil && payload.Summary == nil && payload.Status == nil {
			return nil, platformFailure("platform_payload_invalid", "at least one of title, summary or status is required", false)
		}
		for label, field := range map[string]*string{"artifact title": payload.Title, "artifact summary": payload.Summary, "artifact status": payload.Status} {
			if field == nil {
				continue
			}
			if *field, err = boundedCollaborationInput(*field, label, 16<<10, false); err != nil {
				return nil, err
			}
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "sets_title": payload.Title != nil, "sets_summary": payload.Summary != nil, "sets_status": payload.Status != nil,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.UpdateMeta(ctx, application.UpdateArtifactMetaCommand{
				ArtifactID: id, Title: payload.Title, Summary: payload.Summary, Status: payload.Status, Authorization: authorization,
			})
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the artifact capability handler is unsupported", false)
	}
}
