package notifications

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"devmanager/internal/database"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type WebPushService struct {
	db         *database.DB
	vapidEmail string
	privateKey string
	publicKey  string
}

func NewWebPushService(db *database.DB, vapidEmail string) (*WebPushService, error) {
	service := &WebPushService{
		db:         db,
		vapidEmail: vapidEmail,
	}

	// Try to load existing VAPID keys from settings
	ctx := context.Background()
	privateKey, err := db.GetSetting(ctx, "vapid_private_key")
	if err != nil || privateKey == "" {
		// Generate new VAPID keys
		if err := service.generateVAPIDKeys(ctx); err != nil {
			return nil, err
		}
	} else {
		service.privateKey = privateKey
		publicKey, _ := db.GetSetting(ctx, "vapid_public_key")
		service.publicKey = publicKey
	}

	return service, nil
}

func (s *WebPushService) generateVAPIDKeys(ctx context.Context) error {
	// Generate ECDSA key pair
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate VAPID keys: %w", err)
	}

	// Encode private key
	privBytes := privateKey.D.Bytes()
	// Pad to 32 bytes if necessary
	if len(privBytes) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(privBytes):], privBytes)
		privBytes = padded
	}
	s.privateKey = base64.RawURLEncoding.EncodeToString(privBytes)

	// Encode public key (uncompressed format)
	pubBytes := elliptic.Marshal(elliptic.P256(), privateKey.PublicKey.X, privateKey.PublicKey.Y)
	s.publicKey = base64.RawURLEncoding.EncodeToString(pubBytes)

	// Save to database
	if err := s.db.SetSetting(ctx, "vapid_private_key", s.privateKey); err != nil {
		return err
	}
	if err := s.db.SetSetting(ctx, "vapid_public_key", s.publicKey); err != nil {
		return err
	}

	log.Printf("Generated new VAPID keys")
	return nil
}

// GetPublicKey returns the VAPID public key for client subscription
func (s *WebPushService) GetPublicKey() string {
	return s.publicKey
}

// Subscribe stores a new push subscription
func (s *WebPushService) Subscribe(ctx context.Context, endpoint, p256dh, auth string) error {
	sub := &database.PushSubscription{
		Endpoint: endpoint,
		P256dh:   p256dh,
		Auth:     auth,
	}
	return s.db.CreatePushSubscription(ctx, sub)
}

// Unsubscribe removes a push subscription
func (s *WebPushService) Unsubscribe(ctx context.Context, endpoint string) error {
	return s.db.DeletePushSubscription(ctx, endpoint)
}

// Send sends a push notification to a specific subscription
func (s *WebPushService) Send(ctx context.Context, sub *database.PushSubscription, title, body string, data map[string]string) error {
	payload := map[string]interface{}{
		"title": title,
		"body":  body,
		"data":  data,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	subscription := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dh,
			Auth:   sub.Auth,
		},
	}

	resp, err := webpush.SendNotification(payloadJSON, subscription, &webpush.Options{
		Subscriber:      s.vapidEmail,
		VAPIDPublicKey:  s.publicKey,
		VAPIDPrivateKey: s.privateKey,
		TTL:             30,
	})
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}
	defer resp.Body.Close()

	// Check for invalid subscription (410 Gone)
	if resp.StatusCode == 410 {
		// Subscription is no longer valid, remove it
		s.db.DeletePushSubscription(ctx, sub.Endpoint)
		return fmt.Errorf("subscription expired")
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("push notification failed with status %d", resp.StatusCode)
	}

	return nil
}

// SendToAll sends a push notification to all subscriptions
func (s *WebPushService) SendToAll(ctx context.Context, title, body string, data map[string]string) error {
	subs, err := s.db.ListPushSubscriptions(ctx)
	if err != nil {
		return err
	}

	var lastErr error
	for _, sub := range subs {
		if err := s.Send(ctx, &sub, title, body, data); err != nil {
			log.Printf("Failed to send push to %s: %v", sub.Endpoint[:50], err)
			lastErr = err
		}
	}

	return lastErr
}
