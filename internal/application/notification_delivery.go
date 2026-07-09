package application

import (
	"context"
	"net/url"
	"strings"
)

type PushSubscriptionInput struct {
	Endpoint string
	P256dh   string
	Auth     string
}

type PushTestResult struct {
	Status  string
	Message string
}

type NotificationDeliveryBackend interface {
	SubscribePush(context.Context, PushSubscriptionInput) error
	UnsubscribePush(context.Context, string) error
	GetPushDisabled(context.Context) (bool, error)
	SetPushDisabled(context.Context, bool) error
	SendTestPush(context.Context) (PushTestResult, error)
}

type NotificationPreferenceView struct {
	Disabled bool `json:"disabled"`
}

type NotificationDeliveryView struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type NotificationDeliveryService struct {
	backend NotificationDeliveryBackend
	effects ApplicationEffects
}

func NewNotificationDeliveryService(backend NotificationDeliveryBackend, effects ApplicationEffects) *NotificationDeliveryService {
	return &NotificationDeliveryService{backend: backend, effects: effects}
}

func (s *NotificationDeliveryService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("notification_delivery")
}

func (s *NotificationDeliveryService) Subscribe(ctx context.Context, input PushSubscriptionInput, authorization ActionAuthorization) (NotificationDeliveryView, error) {
	if err := requireActionActor(authorization); err != nil {
		return NotificationDeliveryView{}, err
	}
	if s.backend == nil {
		return NotificationDeliveryView{}, validationError("push_backend_unavailable", "Push notification backend is unavailable")
	}
	endpoint, err := validatePushSubscription(input)
	if err != nil {
		return NotificationDeliveryView{}, err
	}
	input.Endpoint = endpoint
	if err = s.backend.SubscribePush(ctx, input); err != nil {
		return NotificationDeliveryView{}, safeBackendError("Push subscription failed", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "notifications", Action: "push_subscribed"})
	return NotificationDeliveryView{Status: "subscribed"}, nil
}

func (s *NotificationDeliveryService) Unsubscribe(ctx context.Context, endpoint string, authorization ActionAuthorization) (NotificationDeliveryView, error) {
	if err := requireExplicitActionApproval(authorization); err != nil {
		return NotificationDeliveryView{}, err
	}
	if s.backend == nil {
		return NotificationDeliveryView{}, validationError("push_backend_unavailable", "Push notification backend is unavailable")
	}
	validated, err := validatePushEndpoint(endpoint)
	if err != nil {
		return NotificationDeliveryView{}, err
	}
	if err = s.backend.UnsubscribePush(ctx, validated); err != nil {
		return NotificationDeliveryView{}, safeBackendError("Push unsubscription failed", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "notifications", Action: "push_unsubscribed"})
	return NotificationDeliveryView{Status: "unsubscribed"}, nil
}

func (s *NotificationDeliveryService) Preference(ctx context.Context) (NotificationPreferenceView, error) {
	if s.backend == nil {
		return NotificationPreferenceView{}, validationError("push_backend_unavailable", "Push notification backend is unavailable")
	}
	disabled, err := s.backend.GetPushDisabled(ctx)
	return NotificationPreferenceView{Disabled: disabled}, err
}

func (s *NotificationDeliveryService) SetPreference(ctx context.Context, disabled bool, authorization ActionAuthorization) (NotificationPreferenceView, error) {
	if err := requireActionActor(authorization); err != nil {
		return NotificationPreferenceView{}, err
	}
	if s.backend == nil {
		return NotificationPreferenceView{}, validationError("push_backend_unavailable", "Push notification backend is unavailable")
	}
	if err := s.backend.SetPushDisabled(ctx, disabled); err != nil {
		return NotificationPreferenceView{}, safeBackendError("Push preference update failed", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "notifications", Action: "preference_updated", Meta: map[string]any{"disabled": disabled}})
	return NotificationPreferenceView{Disabled: disabled}, nil
}

func (s *NotificationDeliveryService) Test(ctx context.Context, authorization ActionAuthorization) (NotificationDeliveryView, error) {
	if err := requireActionActor(authorization); err != nil {
		return NotificationDeliveryView{}, err
	}
	if s.backend == nil {
		return NotificationDeliveryView{}, validationError("push_backend_unavailable", "Push notification backend is unavailable")
	}
	result, err := s.backend.SendTestPush(ctx)
	if err != nil {
		return NotificationDeliveryView{}, safeBackendError("Test push failed", err)
	}
	status, _ := boundedRedactedOutput(defaultStatus(result.Status, "sent"), 100)
	message, _ := boundedRedactedOutput(result.Message, maxApplicationSummaryRunes)
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "notifications", Action: "test_push_sent"})
	return NotificationDeliveryView{Status: status, Message: message}, nil
}

func validatePushSubscription(input PushSubscriptionInput) (string, error) {
	endpoint, err := validatePushEndpoint(input.Endpoint)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(input.P256dh) == "" || strings.TrimSpace(input.Auth) == "" || len(input.P256dh) > 2048 || len(input.Auth) > 2048 {
		return "", validationError("invalid_push_keys", "Push subscription keys are required and bounded")
	}
	return endpoint, nil
}

func validatePushEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" || len(endpoint) > 4096 {
		return "", validationError("invalid_push_endpoint", "Push endpoint is required and bounded")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", validationError("invalid_push_endpoint", "Push endpoint must be an HTTPS URL without embedded credentials")
	}
	return parsed.String(), nil
}
