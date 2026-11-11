package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/mcp"
)

// artifactChatTools are the HTML artifact tools of the chat assistant. They
// run in-process against the same ArtifactService as the REST routes and the
// MCP tools, and return the same text as the MCP tools.
var artifactChatTools = map[string]bool{
	"create_artifact":             true,
	"update_artifact":             true,
	"patch_artifact":              true,
	"read_artifact":               true,
	"list_artifacts":              true,
	"list_artifact_annotations":   true,
	"resolve_artifact_annotation": true,
}

func chatToolString(input map[string]interface{}, key string) string {
	switch value := input[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.FormatInt(int64(value), 10)
	case json.Number:
		return value.String()
	}
	return ""
}

func chatToolBool(input map[string]interface{}, key string) bool {
	switch value := input[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	}
	return false
}

// chatToolOptionalID parses an optional positive integer id. A value that is
// present but not a positive integer is an error, not "absent".
func chatToolOptionalID(input map[string]interface{}, key string) (*int64, error) {
	raw := chatToolString(input, key)
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("%s must be a positive integer", key)
	}
	return &id, nil
}

// executeArtifactChatTool runs one artifact tool for the chat assistant.
func (h *AIHandler) executeArtifactChatTool(ctx context.Context, name string, input map[string]interface{}, conversationID int64, authorization application.ActionAuthorization) (string, error) {
	services, ready := h.api.platformApplicationServices()
	if !ready || services.Collaboration.Artifacts == nil {
		return "", fmt.Errorf("artifact service is unavailable")
	}
	service := services.Collaboration.Artifacts
	encode := func(view any) []byte {
		data, _ := json.Marshal(view)
		return data
	}
	artifactID := strings.TrimPrefix(chatToolString(input, "artifact_id"), "artifact:")
	openAnnotations := func(id string) int {
		annotations, err := service.ListAnnotations(ctx, id, "open")
		if err != nil {
			return 0
		}
		return len(annotations)
	}

	// The chat assistant has no workspace: a file path only means something to
	// a session, whose files OpenPoet can reach.
	for _, key := range []string{"html_path", "output_path"} {
		if chatToolString(input, key) != "" {
			return "", fmt.Errorf("%s is only available to a session, which has a workspace; here, pass the document in html (and read it with include_html)", key)
		}
	}

	// A title or summary sent with an update or a patch replaces the artifact's.
	meta := mcp.ArtifactMetaParams(input)
	var title, summary *string
	if name == "update_artifact" || name == "patch_artifact" {
		if value, ok := meta["title"]; ok {
			title = &value
		}
		if value, ok := meta["summary"]; ok {
			summary = &value
		}
	}
	dryRun := chatToolBool(input, "dry_run")

	switch name {
	case "create_artifact":
		html, _ := input["html"].(string)
		taskID, err := chatToolOptionalID(input, "task_id")
		if err != nil {
			return "", err
		}
		command := application.CreateArtifactCommand{
			Title: chatToolString(input, "title"), Summary: chatToolString(input, "summary"), Content: html,
			TaskID: taskID, DryRun: dryRun, Authorization: authorization,
		}
		if conversationID > 0 {
			command.ConversationID = &conversationID
			// A conversation opened on a project publishes into that project.
			if projectID := h.getProjectIDFromConversation(ctx, conversationID); projectID > 0 && taskID == nil {
				command.ProjectID = &projectID
			}
		}
		result, err := service.Create(ctx, command)
		if err != nil {
			return "", err
		}
		return mcp.FormatArtifactPublished(encode(h.api.artifactPublishResponse(ctx, result, "")), ""), nil

	case "update_artifact":
		if artifactID == "" {
			return "", fmt.Errorf("artifact_id is required")
		}
		html, hasHTML := input["html"].(string)
		// A title or summary on its own changes them without a new version.
		if !hasHTML && (title != nil || summary != nil) {
			view, err := service.UpdateMeta(ctx, application.UpdateArtifactMetaCommand{
				ArtifactID: artifactID, Title: title, Summary: summary, Authorization: authorization,
			})
			if err != nil {
				return "", err
			}
			return mcp.FormatArtifactMetaUpdated(encode(view), meta), nil
		}
		baseVersion := 0
		if raw := chatToolString(input, "base_version"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				return "", fmt.Errorf("base_version must be a version number")
			}
			baseVersion = parsed
		}
		result, err := service.Update(ctx, application.UpdateArtifactCommand{
			ArtifactID: artifactID, Content: html, ChangeNote: chatToolString(input, "change_note"),
			BaseVersion: baseVersion, Title: title, Summary: summary, DryRun: dryRun, Authorization: authorization,
		})
		if err != nil {
			return "", err
		}
		body := encode(h.api.artifactPublishResponse(ctx, result, ""))
		return mcp.FormatArtifactUpdated(body, openAnnotations(artifactID), "") + mcp.ArtifactMetaNote(body, meta), nil

	case "patch_artifact":
		if artifactID == "" {
			return "", fmt.Errorf("artifact_id is required")
		}
		baseVersion, err := strconv.Atoi(chatToolString(input, "base_version"))
		if err != nil || baseVersion <= 0 {
			return "", fmt.Errorf("base_version is required: the version whose HTML your edits were written against")
		}
		// The edits are decoded the way the REST route decodes them.
		var edits []application.ArtifactEdit
		raw, _ := json.Marshal(input["edits"])
		if text, ok := input["edits"].(string); ok {
			raw = []byte(text)
		}
		if err := json.Unmarshal(raw, &edits); err != nil || len(edits) == 0 {
			return "", fmt.Errorf("edits is required: a list of {old, new} objects")
		}
		result, err := service.Patch(ctx, application.PatchArtifactCommand{
			ArtifactID: artifactID, BaseVersion: baseVersion, Edits: edits,
			ChangeNote: chatToolString(input, "change_note"), Title: title, Summary: summary, DryRun: dryRun,
			Authorization: authorization,
		})
		if err != nil {
			return "", err
		}
		body := encode(h.api.artifactPublishResponse(ctx, result, ""))
		return mcp.FormatArtifactUpdated(body, openAnnotations(artifactID), "") + mcp.ArtifactMetaNote(body, meta), nil

	case "read_artifact":
		if artifactID == "" {
			return "", fmt.Errorf("artifact_id is required")
		}
		view, err := service.Get(ctx, artifactID)
		if err != nil {
			return "", err
		}
		var annotationsJSON, versionJSON []byte
		if annotations, err := service.ListAnnotationsWithPlacement(ctx, artifactID, "open"); err == nil {
			annotationsJSON = encode(annotations)
		}
		if chatToolBool(input, "include_html") {
			version := 0
			if raw := chatToolString(input, "version"); raw != "" && raw != "current" {
				if version, err = strconv.Atoi(raw); err != nil || version <= 0 {
					return "", fmt.Errorf("version must be a version number")
				}
			}
			stored, err := service.GetVersion(ctx, artifactID, version)
			if err != nil {
				return "", err
			}
			versionJSON = encode(stored)
		}
		var questionsJSON []byte
		if questions, err := service.Questions(ctx, artifactID, 0); err == nil {
			questionsJSON = encode(questions)
		}
		return mcp.FormatArtifactDetail(encode(h.api.artifactDetailResponse(ctx, view)), annotationsJSON, versionJSON, "", questionsJSON), nil

	case "list_artifacts":
		query := application.ArtifactListQuery{
			SessionID: chatToolString(input, "session_id"), IncludeArchived: chatToolBool(input, "include_archived"),
		}
		projectID, err := chatToolOptionalID(input, "project_id")
		if err != nil {
			return "", err
		}
		taskID, err := chatToolOptionalID(input, "task_id")
		if err != nil {
			return "", err
		}
		if projectID != nil {
			query.ProjectID = *projectID
		}
		if taskID != nil {
			query.TaskID = *taskID
		}
		// Without a filter, a conversation opened on a project lists that project.
		if projectID == nil && taskID == nil && query.SessionID == "" && conversationID > 0 {
			query.ProjectID = h.getProjectIDFromConversation(ctx, conversationID)
		}
		// An agent restricted to some projects only lists those.
		if allowed := h.getAgentAllowedProjectIDs(ctx, conversationID); allowed != nil && conversationID > 0 && !allowed[query.ProjectID] {
			return "", fmt.Errorf("this agent can only list artifacts of its own projects: pass project_id")
		}
		views, err := service.List(ctx, query)
		if err != nil {
			return "", err
		}
		return mcp.FormatArtifactList(encode(views), ""), nil

	case "list_artifact_annotations":
		if artifactID == "" {
			return "", fmt.Errorf("artifact_id is required")
		}
		status := chatToolString(input, "status")
		if status == "" {
			status = "open"
		}
		annotations, err := service.ListAnnotationsWithPlacement(ctx, artifactID, status)
		if err != nil {
			return "", err
		}
		var questionsJSON, openJSON []byte
		if status != "resolved" {
			if questions, err := service.Questions(ctx, artifactID, 0); err == nil {
				questionsJSON = encode(questions)
			}
			if open, err := service.ListAnnotations(ctx, artifactID, "open"); err == nil {
				openJSON = encode(open)
			}
		}
		return mcp.FormatArtifactAnnotations(artifactID, status, encode(annotations), "", questionsJSON, openJSON), nil

	case "resolve_artifact_annotation":
		if artifactID == "" {
			return "", fmt.Errorf("artifact_id is required")
		}
		resolutions, err := mcp.ArtifactResolutionsParam(input, "")
		if err != nil {
			return "", err
		}
		outcome := make([]mcp.ArtifactResolutionOutcome, 0, len(resolutions))
		for _, resolution := range resolutions {
			_, err := service.ResolveAnnotation(ctx, application.ResolveArtifactAnnotationCommand{
				ArtifactID: artifactID, AnnotationID: resolution.AnnotationID, ResolutionNote: resolution.Note, Authorization: authorization,
			})
			outcome = append(outcome, mcp.ArtifactResolutionOutcome{AnnotationID: resolution.AnnotationID, Err: err})
		}
		return mcp.FormatArtifactAnnotationsResolved(artifactID, outcome, openAnnotations(artifactID))
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

// executeLinkChatTool runs get_link for the chat assistant: the link that
// opens an artifact, a Doc, a session or a task. It answers with the same text
// as the MCP tool.
func (h *AIHandler) executeLinkChatTool(ctx context.Context, input map[string]interface{}) (string, error) {
	version := 0
	if raw := chatToolString(input, "version"); raw != "" && raw != "current" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return "", fmt.Errorf("version must be a version number")
		}
		version = parsed
	}
	view, err := h.api.Links().Link(ctx, application.LinkQuery{
		Kind: chatToolString(input, "kind"), ID: chatToolString(input, "id"), Version: version,
	})
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(view)
	return mcp.FormatLink(data), nil
}
