package automation

import (
	"context"
	"net/url"
	"strings"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type notificationCollaborationExecutor struct {
	service *application.NotificationService
}

type notificationListPayload struct {
	Limit int `json:"limit,omitempty"`
}

type notificationAutomationView struct {
	ID           int64     `json:"id"`
	SessionID    string    `json:"session_id,omitempty"`
	Type         string    `json:"type"`
	Title        string    `json:"title"`
	Body         string    `json:"body,omitempty"`
	Link         string    `json:"link,omitempty"`
	Read         bool      `json:"read"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	WasTruncated bool      `json:"was_truncated,omitempty"`
}

func notificationCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationReadCapability("notifications.list", "notifications", "notifications:read"),
		collaborationReadCapability("notifications.active", "notifications", "notifications:read"),
		collaborationReadCapability("notifications.unread_count", "notifications", "notifications:read"),
		platformMutation(collaborationReadCapability("notifications.mark_read", "notifications", "notifications:write")),
		platformMutation(collaborationReadCapability("notifications.mark_all_read", "notifications", "notifications:write")),
	}
}

func (e *notificationCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeCollaborationTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "notifications.list":
		var payload notificationListPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.Limit == 0 {
			payload.Limit = 50
		}
		if payload.Limit < 1 || payload.Limit > maxCollaborationListItems {
			return nil, platformFailure("platform_payload_invalid", "notification list limit is invalid", false)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.service.List(ctx, payload.Limit)
			if err != nil {
				return nil, err
			}
			return notificationViews(items), nil
		}}, nil
	case "notifications.active":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.service.ListActive(ctx)
			if err != nil {
				return nil, err
			}
			if len(items) > maxCollaborationListItems {
				items = items[:maxCollaborationListItems]
			}
			return notificationViews(items), nil
		}}, nil
	case "notifications.unread_count":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			count, err := e.service.UnreadCount(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{"unread": max(0, count)}, nil
		}}, nil
	case "notifications.mark_read":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		id, err := collaborationIntegerID(target, 0, "notification id")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"notification_id": id}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.MarkRead(ctx, id); err != nil {
				return nil, err
			}
			return map[string]any{"notification_id": id, "status": "read"}, nil
		}}, nil
	case "notifications.mark_all_read":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			if err := e.service.MarkAllRead(ctx); err != nil {
				return nil, err
			}
			return map[string]any{"status": "all_read"}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the notification capability handler is unsupported", false)
	}
}

func notificationViews(items []database.Notification) []notificationAutomationView {
	views := make([]notificationAutomationView, len(items))
	for i, item := range items {
		sessionID, sessionTruncated := boundedCollaborationOutput(item.SessionID, 200)
		typeName, typeTruncated := boundedCollaborationOutput(item.Type, 100)
		title, titleTruncated := boundedCollaborationOutput(item.Title, 1000)
		body, bodyTruncated := boundedCollaborationOutput(item.Body, maxCollaborationNotification)
		views[i] = notificationAutomationView{
			ID: item.ID, SessionID: sessionID, Type: typeName, Title: title, Body: body,
			Link: sanitizeCollaborationURL(item.Link), Read: item.Read, CreatedAt: item.CreatedAt,
			WasTruncated: sessionTruncated || typeTruncated || titleTruncated || bodyTruncated,
		}
	}
	return views
}

type notificationDeliveryCollaborationExecutor struct {
	service *application.NotificationDeliveryService
}

type notificationSubscriptionPayload struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

type notificationEndpointPayload struct {
	Endpoint string `json:"endpoint"`
}

type notificationPreferencePayload struct {
	Disabled bool `json:"disabled"`
}

type notificationDeliveryAutomationView struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Disabled *bool  `json:"disabled,omitempty"`
}

func notificationDeliveryCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationReadCapability("notifications.preference", "notification_delivery", "notifications:read"),
		collaborationPayloadLimit(collaborationWriteCapability("notifications.subscribe_push", "notification_delivery", "notifications:write"), 16<<10),
		collaborationPayloadLimit(collaborationDestructiveCapability("notifications.unsubscribe_push", "notification_delivery", "notifications:write"), 8<<10),
		collaborationWriteCapability("notifications.test_push", "notification_delivery", "notifications:write"),
		collaborationWriteCapability("notifications.update_preference", "notification_delivery", "notifications:write"),
	}
}

func (e *notificationDeliveryCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	if _, err := decodeCollaborationTarget(input.Target); err != nil {
		return nil, err
	}
	switch input.Handler {
	case "notifications.preference":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			view, err := e.service.Preference(ctx)
			if err != nil {
				return nil, err
			}
			return notificationPreferenceForAutomation(view), nil
		}}, nil
	case "notifications.subscribe_push":
		var payload notificationSubscriptionPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		host, err := validateCollaborationPushInput(payload.Endpoint, payload.P256dh, payload.Auth)
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"endpoint_host": host, "has_keys": true}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.Subscribe(ctx, application.PushSubscriptionInput{Endpoint: payload.Endpoint, P256dh: payload.P256dh, Auth: payload.Auth}, authorization)
			if err != nil {
				return nil, err
			}
			return notificationDeliveryForAutomation(view), nil
		}}, nil
	case "notifications.unsubscribe_push":
		var payload notificationEndpointPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		host, err := validateCollaborationPushInput(payload.Endpoint, "unused", "unused")
		if err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"endpoint_host": host}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.Unsubscribe(ctx, payload.Endpoint, authorization)
			if err != nil {
				return nil, err
			}
			return notificationDeliveryForAutomation(view), nil
		}}, nil
	case "notifications.test_push":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.Test(ctx, authorization)
			if err != nil {
				return nil, err
			}
			return notificationDeliveryForAutomation(view), nil
		}}, nil
	case "notifications.update_preference":
		var payload notificationPreferencePayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"disabled": payload.Disabled}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			view, err := e.service.SetPreference(ctx, payload.Disabled, authorization)
			if err != nil {
				return nil, err
			}
			return notificationPreferenceForAutomation(view), nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the notification delivery capability handler is unsupported", false)
	}
}

func validateCollaborationPushInput(endpoint, p256dh, auth string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || len(endpoint) > 4096 {
		return "", platformFailure("platform_payload_invalid", "push endpoint must be a bounded HTTPS URL", false)
	}
	if strings.TrimSpace(p256dh) == "" || strings.TrimSpace(auth) == "" || len(p256dh) > 2048 || len(auth) > 2048 {
		return "", platformFailure("platform_payload_invalid", "push keys must be present and bounded", false)
	}
	return parsed.Host, nil
}

func notificationDeliveryForAutomation(view application.NotificationDeliveryView) notificationDeliveryAutomationView {
	status, _ := boundedCollaborationOutput(view.Status, 100)
	message, _ := boundedCollaborationOutput(view.Message, maxCollaborationSummaryBytes)
	return notificationDeliveryAutomationView{Status: status, Message: message}
}

func notificationPreferenceForAutomation(view application.NotificationPreferenceView) notificationDeliveryAutomationView {
	disabled := view.Disabled
	return notificationDeliveryAutomationView{Status: "configured", Disabled: &disabled}
}

type tokenUsageCollaborationExecutor struct {
	service *application.TokenUsageService
	queries TokenUsageCollaborationReadPort
}

type tokenUsageSummaryPayload struct {
	Since     string `json:"since,omitempty"`
	Days      int    `json:"days,omitempty"`
	ProjectID *int64 `json:"project_id,omitempty"`
}

type tokenUsageAutomationView struct {
	Since             time.Time `json:"since"`
	ProjectID         *int64    `json:"project_id,omitempty"`
	TotalInputTokens  int64     `json:"total_input_tokens"`
	TotalOutputTokens int64     `json:"total_output_tokens"`
	TotalCostUSD      float64   `json:"total_cost_usd"`
	TotalRequests     int64     `json:"total_requests"`
}

func tokenUsageCollaborationDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		collaborationReadCapability("token_usage.summary", "token_usage", "token_usage:read"),
		collaborationDestructiveCapability("token_usage.clear", "token_usage", "token_usage:write"),
	}
}

func (e *tokenUsageCollaborationExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	if _, err := decodeCollaborationTarget(input.Target); err != nil {
		return nil, err
	}
	switch input.Handler {
	case "token_usage.summary":
		var payload tokenUsageSummaryPayload
		if err := decodeCollaborationPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.ProjectID != nil && *payload.ProjectID <= 0 || payload.Days < 0 || payload.Days > 3650 || payload.Days > 0 && payload.Since != "" {
			return nil, platformFailure("platform_payload_invalid", "token usage filters are invalid", false)
		}
		var since time.Time
		var err error
		if payload.Since != "" {
			since, err = time.Parse(time.RFC3339, payload.Since)
			if err != nil {
				return nil, platformFailure("platform_payload_invalid", "token usage since must be RFC3339", false)
			}
		} else {
			days := payload.Days
			if days == 0 {
				days = 30
			}
			since = time.Now().UTC().AddDate(0, 0, -days)
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, map[string]any{"since": since, "project_id": payload.ProjectID}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			summary, err := e.queries.GetTokenUsageSummary(ctx, since, payload.ProjectID)
			if err != nil {
				return nil, err
			}
			if summary == nil {
				return nil, platformFailure("token_usage_unavailable", "token usage summary is unavailable", false)
			}
			return tokenUsageAutomationView{
				Since: since, ProjectID: payload.ProjectID, TotalInputTokens: max(int64(0), summary.TotalInputTokens),
				TotalOutputTokens: max(int64(0), summary.TotalOutputTokens), TotalCostUSD: max(float64(0), summary.TotalCostUSD),
				TotalRequests: max(int64(0), summary.TotalRequests),
			}, nil
		}}, nil
	case "token_usage.clear":
		if err := requireEmptyCollaborationPayload(input.Payload); err != nil {
			return nil, err
		}
		return &collaborationValidatedCommand{preview: collaborationPreview(input.Handler, nil), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			deleted, err := e.service.Clear(ctx, authorization)
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": max(int64(0), deleted)}, nil
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the token usage capability handler is unsupported", false)
	}
}
