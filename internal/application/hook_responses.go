package application

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxHookTextRunes       = 4000
	maxHookAnswers         = 20
	maxHookSuggestionsSize = 64 << 10
)

type HookPermissionResponse struct {
	Behavior              string
	Message               string
	ToolName              string
	Answers               map[string]string
	PermissionSuggestions []any
}

type HookResponsePort interface {
	RespondPermission(context.Context, string, HookPermissionResponse) error
	RespondTaskNotification(context.Context, string) error
}

type HookResponseChange struct {
	Action    string
	SessionID string
	Behavior  string
	Actor     Actor
}

type HookResponseEffects interface {
	PublishHookResponse(context.Context, HookResponseChange)
}

type HookResponseService struct {
	port    HookResponsePort
	effects HookResponseEffects
}

func NewHookResponseService(port HookResponsePort, effects HookResponseEffects) *HookResponseService {
	return &HookResponseService{port: port, effects: effects}
}

type RespondPermissionCommand struct {
	SessionID     string
	Response      HookPermissionResponse
	Authorization ActionAuthorization
}

func (s *HookResponseService) RespondPermission(ctx context.Context, command RespondPermissionCommand) error {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return err
	}
	sessionID, err := validateActionSessionID(command.SessionID)
	if err != nil {
		return err
	}
	response, err := validateHookPermissionResponse(command.Response)
	if err != nil {
		return err
	}
	if s.port == nil {
		return validationError("hook_responder_unavailable", "Hook responder is unavailable")
	}
	if err := s.port.RespondPermission(ctx, sessionID, response); err != nil {
		return err
	}
	s.publish(ctx, HookResponseChange{
		Action: "permission_responded", SessionID: sessionID,
		Behavior: response.Behavior, Actor: command.Authorization.Actor,
	})
	return nil
}

type RespondTaskNotificationCommand struct {
	SessionID     string
	Authorization ActionAuthorization
}

func (s *HookResponseService) RespondTaskNotification(ctx context.Context, command RespondTaskNotificationCommand) error {
	if err := requireActionActor(command.Authorization); err != nil {
		return err
	}
	sessionID, err := validateActionSessionID(command.SessionID)
	if err != nil {
		return err
	}
	if s.port == nil {
		return validationError("hook_responder_unavailable", "Hook responder is unavailable")
	}
	if err := s.port.RespondTaskNotification(ctx, sessionID); err != nil {
		return err
	}
	s.publish(ctx, HookResponseChange{
		Action: "task_notification_responded", SessionID: sessionID, Actor: command.Authorization.Actor,
	})
	return nil
}

func validateHookPermissionResponse(response HookPermissionResponse) (HookPermissionResponse, error) {
	response.Behavior = strings.TrimSpace(response.Behavior)
	switch response.Behavior {
	case "allow", "allowAlways", "deny", "passthrough":
	default:
		return HookPermissionResponse{}, validationError("permission_behavior_invalid", "Permission behavior is invalid")
	}
	response.Message = strings.TrimSpace(response.Message)
	response.ToolName = strings.TrimSpace(response.ToolName)
	if utf8.RuneCountInString(response.Message) > maxHookTextRunes || utf8.RuneCountInString(response.ToolName) > maxHookTextRunes {
		return HookPermissionResponse{}, validationError("permission_response_too_large", "Permission response text exceeds 4000 characters")
	}
	if len(response.Answers) > maxHookAnswers {
		return HookPermissionResponse{}, validationError("permission_answers_too_large", "Permission response has too many answers")
	}
	for question, answer := range response.Answers {
		if strings.TrimSpace(question) == "" || utf8.RuneCountInString(question) > maxHookTextRunes || utf8.RuneCountInString(answer) > maxHookTextRunes {
			return HookPermissionResponse{}, validationError("permission_answer_invalid", "Permission answers must have bounded non-empty question keys")
		}
	}
	encoded, err := json.Marshal(response.PermissionSuggestions)
	if err != nil || len(encoded) > maxHookSuggestionsSize {
		return HookPermissionResponse{}, validationError("permission_suggestions_invalid", "Permission suggestions must be valid and at most 64 KiB")
	}
	return response, nil
}

func validateActionSessionID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 200 || strings.IndexByte(value, 0) >= 0 {
		return "", validationError("invalid_session_id", "Session ID is required and must not exceed 200 characters")
	}
	return value, nil
}

func (s *HookResponseService) publish(ctx context.Context, change HookResponseChange) {
	if s.effects != nil {
		s.effects.PublishHookResponse(ctx, change)
	}
}
