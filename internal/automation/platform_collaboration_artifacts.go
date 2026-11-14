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

type artifactPatchPayload struct {
	Edits       []application.ArtifactEdit `json:"edits"`
	ChangeNote  string                     `json:"change_note,omitempty"`
	BaseVersion int                        `json:"base_version"`
	SessionID   string                     `json:"session_id,omitempty"`
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

type artifactAnnotationListPayload struct {
	Status string `json:"status,omitempty"`
}

type artifactAnswersSavePayload struct {
	Version int                               `json:"version,omitempty"`
	Answers []application.ArtifactAnswerInput `json:"answers"`
}

type artifactAnswerWithdrawPayload struct {
	Question string `json:"question"`
}

type artifactAnnotationCreatePayload struct {
	Version int                        `json:"version,omitempty"`
	Anchor  application.ArtifactAnchor `json:"anchor"`
	Body    string                     `json:"body"`
}

// artifactAnnotationChangePayload addresses one annotation of the artifact in
// the command target.
type artifactAnnotationChangePayload struct {
	AnnotationID   int64  `json:"annotation_id"`
	ResolutionNote string `json:"resolution_note,omitempty"`
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
		collaborationPayloadLimit(collaborationWriteCapability("artifacts.patch", "artifacts", "documents:write"), maxArtifactAutomationPayloadBytes),
		collaborationWriteCapability("artifacts.update_meta", "artifacts", "documents:write"),
		collaborationReadCapability("artifacts.annotations.list", "artifacts", "documents:read"),
		collaborationWriteCapability("artifacts.annotations.create", "artifacts", "documents:write"),
		collaborationWriteCapability("artifacts.annotations.resolve", "artifacts", "documents:write"),
		collaborationWriteCapability("artifacts.annotations.reopen", "artifacts", "documents:write"),
		collaborationWriteCapability("artifacts.annotations.send", "artifacts", "documents:write"),
		collaborationReadCapability("artifacts.questions.list", "artifacts", "documents:read"),
		collaborationWriteCapability("artifacts.answers.save", "artifacts", "documents:write"),
		collaborationDestructiveCapability("artifacts.answers.withdraw", "artifacts", "documents:write"),
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
	case "artifacts.patch":
		var payload artifactPatchPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.ChangeNote, err = boundedCollaborationInput(payload.ChangeNote, "artifact change note", 1000, false); err != nil {
			return nil, err
		}
		if payload.SessionID, err = boundedCollaborationInput(payload.SessionID, "session id", 200, false); err != nil {
			return nil, err
		}
		if payload.BaseVersion <= 0 {
			return nil, platformFailure("platform_payload_invalid", "base_version is required", false)
		}
		if len(payload.Edits) == 0 {
			return nil, platformFailure("platform_payload_invalid", "edits is required", false)
		}
		editBytes := 0
		for _, edit := range payload.Edits {
			editBytes += len(edit.Old) + len(edit.New)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "edits": len(payload.Edits), "edit_bytes": editBytes, "base_version": payload.BaseVersion, "has_session": payload.SessionID != "",
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.Patch(ctx, application.PatchArtifactCommand{
				ArtifactID: id, BaseVersion: payload.BaseVersion, Edits: payload.Edits, ChangeNote: payload.ChangeNote,
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
	case "artifacts.annotations.list":
		var payload artifactAnnotationListPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.Status, err = boundedCollaborationInput(payload.Status, "annotation status", 20, false); err != nil {
			return nil, err
		}
		if payload.Status == "" {
			payload.Status = "open"
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"artifact_id": id, "status": payload.Status}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			return e.service.ListAnnotations(ctx, id, payload.Status)
		}}, nil
	case "artifacts.annotations.create":
		var payload artifactAnnotationCreatePayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.Body, err = boundedCollaborationInput(payload.Body, "annotation text", 16<<10, true); err != nil {
			return nil, err
		}
		if payload.Version < 0 {
			return nil, platformFailure("platform_payload_invalid", "artifact version cannot be negative", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "version": payload.Version, "anchor_type": payload.Anchor.Type, "body_bytes": len(payload.Body),
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.CreateAnnotation(ctx, application.CreateArtifactAnnotationCommand{
				ArtifactID: id, Version: payload.Version, Anchor: payload.Anchor, Body: payload.Body, Authorization: authorization,
			})
		}}, nil
	case "artifacts.annotations.resolve", "artifacts.annotations.reopen":
		var payload artifactAnnotationChangePayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.AnnotationID <= 0 {
			return nil, platformFailure("platform_payload_invalid", "annotation_id must be a positive integer", false)
		}
		if payload.ResolutionNote, err = boundedCollaborationInput(payload.ResolutionNote, "resolution note", 16<<10, false); err != nil {
			return nil, err
		}
		resolve := input.Handler == "artifacts.annotations.resolve"
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "annotation_id": payload.AnnotationID, "has_note": payload.ResolutionNote != "",
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			command := application.ResolveArtifactAnnotationCommand{
				ArtifactID: id, AnnotationID: payload.AnnotationID, ResolutionNote: payload.ResolutionNote, Authorization: authorization,
			}
			if resolve {
				return e.service.ResolveAnnotation(ctx, command)
			}
			return e.service.ReopenAnnotation(ctx, command)
		}}, nil
	case "artifacts.annotations.send":
		var payload struct{}
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"artifact_id": id}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.SendAnnotationsToAgent(ctx, application.SendArtifactAnnotationsCommand{ArtifactID: id, Authorization: authorization})
		}}, nil
	case "artifacts.questions.list":
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
			return e.service.Questions(ctx, id, payload.Version)
		}}, nil
	case "artifacts.answers.save":
		var payload artifactAnswersSavePayload
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
		if len(payload.Answers) == 0 {
			return nil, platformFailure("platform_payload_invalid", "answers must name at least one question", false)
		}
		for i := range payload.Answers {
			answer := &payload.Answers[i]
			if answer.Question, err = boundedCollaborationInput(answer.Question, "question id", 64, true); err != nil {
				return nil, err
			}
			if answer.Text, err = boundedCollaborationInput(answer.Text, "answer text", 16<<10, false); err != nil {
				return nil, err
			}
			if answer.Comment, err = boundedCollaborationInput(answer.Comment, "answer comment", 16<<10, false); err != nil {
				return nil, err
			}
		}
		// The preview names the questions, never what was answered.
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "version": payload.Version, "answers": len(payload.Answers),
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.AnswerQuestions(ctx, application.AnswerArtifactQuestionsCommand{
				ArtifactID: id, Version: payload.Version, Answers: payload.Answers, Authorization: authorization,
			})
		}}, nil
	case "artifacts.answers.withdraw":
		var payload artifactAnswerWithdrawPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		id, err := collaborationStringID(target, "", "artifact id")
		if err != nil {
			return nil, err
		}
		if payload.Question, err = boundedCollaborationInput(payload.Question, "question id", 64, true); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{
			"artifact_id": id, "question": payload.Question,
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if err := e.service.WithdrawAnswer(ctx, application.WithdrawArtifactAnswerCommand{
				ArtifactID: id, QuestionID: payload.Question, Authorization: authorization,
			}); err != nil {
				return nil, err
			}
			return map[string]any{"artifact_id": id, "question": payload.Question, "withdrawn": true}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the artifact capability handler is unsupported", false)
	}
}
