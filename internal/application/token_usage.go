package application

import "context"

type TokenUsageStore interface {
	ClearTokenUsage(context.Context) (int64, error)
}

type TokenUsageService struct {
	store   TokenUsageStore
	effects ApplicationEffects
}

func NewTokenUsageService(store TokenUsageStore, effects ApplicationEffects) *TokenUsageService {
	return &TokenUsageService{store: store, effects: effects}
}

func (s *TokenUsageService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("token_usage")
}

func (s *TokenUsageService) Clear(ctx context.Context, authorization ActionAuthorization) (int64, error) {
	if err := requireExplicitActionApproval(authorization); err != nil {
		return 0, err
	}
	if s.store == nil {
		return 0, validationError("token_usage_store_unavailable", "Token usage store is unavailable")
	}
	deleted, err := s.store.ClearTokenUsage(ctx)
	if err != nil {
		return 0, safeBackendError("Token usage could not be cleared", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "token_usage", Action: "cleared", Meta: map[string]any{"deleted": deleted}})
	return deleted, nil
}
