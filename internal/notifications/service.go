package notifications

import (
	"context"
	"devmanager/internal/database"
	"devmanager/internal/websocket"
	"log"
	"sync"
)

type Service struct {
	db      *database.DB
	hub     *websocket.Hub
	webpush *WebPushService

	mu       sync.RWMutex
	handlers map[string]NotificationHandler
}

type NotificationHandler func(n *database.Notification)

func NewService(db *database.DB, hub *websocket.Hub, webpush *WebPushService) *Service {
	return &Service{
		db:       db,
		hub:      hub,
		webpush:  webpush,
		handlers: make(map[string]NotificationHandler),
	}
}

// Send creates and broadcasts a notification
func (s *Service) Send(ctx context.Context, sessionID, notifType, title, body string) error {
	notification := &database.Notification{
		SessionID: sessionID,
		Type:      notifType,
		Title:     title,
		Body:      body,
	}

	// Save to database
	if err := s.db.CreateNotification(ctx, notification); err != nil {
		return err
	}

	// Broadcast via WebSocket
	s.hub.BroadcastNotification(notification)

	// Send push notification
	if s.webpush != nil {
		go func() {
			if err := s.webpush.SendToAll(ctx, title, body, map[string]string{
				"session_id": sessionID,
				"type":       notifType,
			}); err != nil {
				log.Printf("Failed to send push notification: %v", err)
			}
		}()
	}

	// Call registered handlers
	s.mu.RLock()
	handlers := make([]NotificationHandler, 0, len(s.handlers))
	for _, h := range s.handlers {
		handlers = append(handlers, h)
	}
	s.mu.RUnlock()

	for _, handler := range handlers {
		handler(notification)
	}

	return nil
}

// RegisterHandler registers a notification handler
func (s *Service) RegisterHandler(name string, handler NotificationHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[name] = handler
}

// UnregisterHandler removes a notification handler
func (s *Service) UnregisterHandler(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handlers, name)
}

// GetUnread returns unread notifications
func (s *Service) GetUnread(ctx context.Context) ([]database.Notification, error) {
	return s.db.ListUnreadNotifications(ctx)
}

// GetRecent returns recent notifications
func (s *Service) GetRecent(ctx context.Context, limit int) ([]database.Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.db.ListNotifications(ctx, limit)
}

// MarkRead marks a notification as read
func (s *Service) MarkRead(ctx context.Context, id int64) error {
	return s.db.MarkNotificationRead(ctx, id)
}

// MarkAllRead marks all notifications as read
func (s *Service) MarkAllRead(ctx context.Context) error {
	return s.db.MarkAllNotificationsRead(ctx)
}

// SendSessionStarted sends a notification when a session starts
func (s *Service) SendSessionStarted(ctx context.Context, sessionID string, projectName string) error {
	return s.Send(ctx, sessionID, "info", "Session Started", "Claude Code session started for "+projectName)
}

// SendSessionEnded sends a notification when a session ends
func (s *Service) SendSessionEnded(ctx context.Context, sessionID string, status string) error {
	title := "Session Completed"
	notifType := "info"
	if status == "error" {
		title = "Session Error"
		notifType = "error"
	}
	return s.Send(ctx, sessionID, notifType, title, "Session ended with status: "+status)
}

// SendQuestionWaiting sends a notification when Claude is waiting for input
func (s *Service) SendQuestionWaiting(ctx context.Context, sessionID string, question string) error {
	return s.Send(ctx, sessionID, "question", "Input Required", question)
}

// SendError sends an error notification
func (s *Service) SendError(ctx context.Context, sessionID string, errorMsg string) error {
	return s.Send(ctx, sessionID, "error", "Error", errorMsg)
}

// SendWarning sends a warning notification
func (s *Service) SendWarning(ctx context.Context, sessionID string, warning string) error {
	return s.Send(ctx, sessionID, "warning", "Warning", warning)
}
