package application

import (
	"context"

	"openpoet/internal/database"
)

type NotificationBackend interface {
	GetRecent(context.Context, int) ([]database.Notification, error)
	GetActive(context.Context) ([]database.Notification, error)
	GetUnreadCount(context.Context) (int, error)
	MarkRead(context.Context, int64) error
	MarkAllRead(context.Context) error
}

type NotificationService struct {
	backend NotificationBackend
}

func NewNotificationService(backend NotificationBackend) *NotificationService {
	return &NotificationService{backend: backend}
}

func (s *NotificationService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("notifications")
}

func (s *NotificationService) List(ctx context.Context, limit int) ([]database.Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	items, err := s.backend.GetRecent(ctx, limit)
	if items == nil && err == nil {
		items = []database.Notification{}
	}
	return items, err
}

func (s *NotificationService) ListActive(ctx context.Context) ([]database.Notification, error) {
	items, err := s.backend.GetActive(ctx)
	if items == nil && err == nil {
		items = []database.Notification{}
	}
	return items, err
}

func (s *NotificationService) UnreadCount(ctx context.Context) (int, error) {
	return s.backend.GetUnreadCount(ctx)
}

func (s *NotificationService) MarkRead(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid_notification_id", "Notification ID must be positive")
	}
	return s.backend.MarkRead(ctx, id)
}

func (s *NotificationService) MarkAllRead(ctx context.Context) error {
	return s.backend.MarkAllRead(ctx)
}
