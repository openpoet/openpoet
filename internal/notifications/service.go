package notifications

import (
	"context"
	"openpoet/internal/database"
	"openpoet/internal/websocket"
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

// Send creates and broadcasts a notification.
// For session-scoped notifications, marks previous unread ones as read first (one-active-per-session).
// If link is empty and sessionID is provided, auto-generates a session link.
func (s *Service) Send(ctx context.Context, sessionID, notifType, title, body, link string) error {
	log.Printf("[NotifService] Send called: type=%s title=%q body=%q session=%s link=%q", notifType, title, body, sessionID, link)

	// Auto-generate link from sessionID if not provided
	if link == "" && sessionID != "" {
		link = "/app/session/" + sessionID
	}

	// Expire previous unread notifications for this session
	if sessionID != "" {
		if affected, err := s.db.MarkSessionNotificationsRead(ctx, sessionID); err != nil {
			log.Printf("[NotifService] Failed to mark old notifications read: %v", err)
		} else if affected > 0 {
			log.Printf("[NotifService] Expired %d old notifications for session %s", affected, sessionID)
		}
	}

	notification := &database.Notification{
		SessionID: sessionID,
		Type:      notifType,
		Title:     title,
		Body:      body,
		Link:      link,
	}

	// Save to database
	if err := s.db.CreateNotification(ctx, notification); err != nil {
		log.Printf("[NotifService] DB save failed: %v", err)
		return err
	}

	// Broadcast via WebSocket
	s.hub.BroadcastNotification(notification)

	// Send push notification
	if s.webpush != nil {
		go func() {
			log.Printf("[NotifService] Launching push for: %q", title)
			if err := s.webpush.SendToAll(context.Background(), title, body, map[string]string{
				"session_id": sessionID,
				"type":       notifType,
				"link":       link,
			}); err != nil {
				log.Printf("[NotifService] Push failed: %v", err)
			}
		}()
	} else {
		log.Printf("[NotifService] webpush is nil, skipping push")
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

	// Broadcast updated unread count
	s.BroadcastUnreadCount(ctx)

	return nil
}

// SendCancel sends a cancel push to dismiss notifications for a session
func (s *Service) SendCancel(ctx context.Context, sessionID string) {
	if s.webpush == nil {
		return
	}
	if err := s.webpush.SendCancelToAll(ctx, sessionID); err != nil {
		log.Printf("[NotifService] Cancel push failed for session %s: %v", sessionID, err)
	}
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

// GetActive returns unread notifications from active sessions only (one per session max).
func (s *Service) GetActive(ctx context.Context) ([]database.Notification, error) {
	return s.db.ListActiveNotifications(ctx)
}

// GetRecent returns recent notifications
func (s *Service) GetRecent(ctx context.Context, limit int) ([]database.Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.db.ListNotifications(ctx, limit)
}

// MarkRead marks a notification as read and broadcasts the updated count
func (s *Service) MarkRead(ctx context.Context, id int64) error {
	if err := s.db.MarkNotificationRead(ctx, id); err != nil {
		return err
	}
	s.BroadcastUnreadCount(ctx)
	return nil
}

// MarkAllRead marks all notifications as read and broadcasts the updated count
func (s *Service) MarkAllRead(ctx context.Context) error {
	if err := s.db.MarkAllNotificationsRead(ctx); err != nil {
		return err
	}
	s.BroadcastUnreadCount(ctx)
	return nil
}

// MarkSessionRead marks all unread notifications for a session as read,
// sends push cancel, and broadcasts the updated unread count.
func (s *Service) MarkSessionRead(ctx context.Context, sessionID string) {
	affected, err := s.db.MarkSessionNotificationsRead(ctx, sessionID)
	if err != nil {
		log.Printf("[NotifService] Failed to mark session notifications read for %s: %v", sessionID, err)
		return
	}
	if affected > 0 {
		log.Printf("[NotifService] Marked %d notifications read for session %s", affected, sessionID)
		s.SendCancel(ctx, sessionID)
		s.BroadcastUnreadCount(ctx)
	}
}

// GetUnreadCount returns the count of unread notifications
func (s *Service) GetUnreadCount(ctx context.Context) (int, error) {
	return s.db.CountUnreadNotifications(ctx)
}

// BroadcastUnreadCount sends the current unread count via WebSocket
func (s *Service) BroadcastUnreadCount(ctx context.Context) {
	count, err := s.db.CountUnreadNotifications(ctx)
	if err != nil {
		log.Printf("[NotifService] Failed to count unread: %v", err)
		return
	}
	s.hub.BroadcastNotificationCount(count)
}

// SendSessionStarted sends a notification when a session starts
func (s *Service) SendSessionStarted(ctx context.Context, sessionID string, projectName string) error {
	return s.Send(ctx, sessionID, "info", "Session Started", "Claude Code session started for "+projectName, "")
}

// SendSessionEnded sends a notification when a session ends
func (s *Service) SendSessionEnded(ctx context.Context, sessionID string, status string) error {
	title := "Session Completed"
	notifType := "info"
	if status == "error" {
		title = "Session Error"
		notifType = "error"
	}
	return s.Send(ctx, sessionID, notifType, title, "Session ended with status: "+status, "")
}

// SendQuestionWaiting sends a notification when Claude is waiting for input
func (s *Service) SendQuestionWaiting(ctx context.Context, sessionID string, question string) error {
	return s.Send(ctx, sessionID, "question", "Input Required", question, "")
}

// SendError sends an error notification
func (s *Service) SendError(ctx context.Context, sessionID string, errorMsg string) error {
	return s.Send(ctx, sessionID, "error", "Error", errorMsg, "")
}

// SendWarning sends a warning notification
func (s *Service) SendWarning(ctx context.Context, sessionID string, warning string) error {
	return s.Send(ctx, sessionID, "warning", "Warning", warning, "")
}
