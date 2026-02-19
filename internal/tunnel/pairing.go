package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
	"sync"
	"time"

	"devmanager/internal/database"
	"devmanager/internal/security"

	"github.com/google/uuid"
)

const (
	pairingExpiry = 2 * time.Minute
	codeLength    = 6
)

// PendingPairing represents a pairing session initiated by a remote device.
// The remote device displays the code; the local admin enters it to approve.
type PendingPairing struct {
	Code          string
	SessionID     string
	UserAgent     string
	ExpiresAt     time.Time
	Approved      bool
	SessionToken  string // set after approval
	EncryptionKey string // set after approval
}

// PairingManager handles the reversed pairing flow:
// remote device generates/displays code, local admin enters it to approve.
type PairingManager struct {
	db        *database.DB
	encryptor *security.Encryptor
	jwtSecret []byte

	// Tunnel-wide encryption key (base64-encoded raw AES-256 key)
	tunnelEncKey string

	pendingSessions map[string]*PendingPairing // sessionID → PendingPairing
	pendingByCode   map[string]string          // code → sessionID
	mu              sync.Mutex
}

// NewPairingManager creates a new PairingManager.
func NewPairingManager(db *database.DB, encryptor *security.Encryptor, jwtSecret []byte) *PairingManager {
	pm := &PairingManager{
		db:              db,
		encryptor:       encryptor,
		jwtSecret:       jwtSecret,
		pendingSessions: make(map[string]*PendingPairing),
		pendingByCode:   make(map[string]string),
	}

	pm.loadOrGenerateEncKey()

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			pm.cleanupExpired()
		}
	}()

	return pm
}

// loadOrGenerateEncKey loads the tunnel encryption key from settings,
// or generates a new one if none exists.
func (pm *PairingManager) loadOrGenerateEncKey() {
	ctx := context.Background()
	encValue, err := pm.db.GetSetting(ctx, "tunnel_enc_key")
	if err == nil && encValue != "" {
		iv, ivErr := pm.db.GetSetting(ctx, "tunnel_enc_key_iv")
		if ivErr == nil && iv != "" {
			decrypted, decErr := pm.encryptor.Decrypt(encValue, iv)
			if decErr == nil {
				pm.tunnelEncKey = decrypted
				return
			}
		}
	}

	key, err := security.GenerateKey()
	if err != nil {
		return
	}
	pm.tunnelEncKey = key

	cipher, iv, err := pm.encryptor.Encrypt(key)
	if err != nil {
		return
	}
	pm.db.SetSetting(ctx, "tunnel_enc_key", cipher)
	pm.db.SetSetting(ctx, "tunnel_enc_key_iv", iv)
}

// GetEncryptionKeyRaw returns the raw AES-256 key bytes for use by the tunnel client.
func (pm *PairingManager) GetEncryptionKeyRaw() ([]byte, error) {
	if pm.tunnelEncKey == "" {
		return nil, fmt.Errorf("no encryption key available")
	}
	return base64.StdEncoding.DecodeString(pm.tunnelEncKey)
}

// CreatePendingPairing creates a new pending pairing session or reuses an existing
// valid one from the same user agent. This way, refreshing the page keeps the same
// code and countdown instead of invalidating a code the admin may be typing.
// Returns the 6-digit code, session ID, and seconds until expiry.
func (pm *PairingManager) CreatePendingPairing(userAgent string) (code string, sessionID string, expirySecs int, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()

	// Reuse existing valid pending session from same user agent if available
	for _, p := range pm.pendingSessions {
		if p.UserAgent == userAgent && !p.Approved && now.Before(p.ExpiresAt) {
			remaining := int(p.ExpiresAt.Sub(now).Seconds())
			return p.Code, p.SessionID, remaining, nil
		}
	}

	// Clean up any expired/used sessions from same user agent
	for id, p := range pm.pendingSessions {
		if p.UserAgent == userAgent {
			delete(pm.pendingByCode, p.Code)
			delete(pm.pendingSessions, id)
		}
	}

	// Generate a code that doesn't collide with any active pending session
	for attempts := 0; attempts < 10; attempts++ {
		code, err = generateNumericCode(codeLength)
		if err != nil {
			return "", "", 0, fmt.Errorf("generate code: %w", err)
		}
		if _, exists := pm.pendingByCode[code]; !exists {
			break
		}
	}

	sessionID = uuid.New().String()
	pm.pendingSessions[sessionID] = &PendingPairing{
		Code:      code,
		SessionID: sessionID,
		UserAgent: userAgent,
		ExpiresAt: now.Add(pairingExpiry),
	}
	pm.pendingByCode[code] = sessionID

	return code, sessionID, int(pairingExpiry.Seconds()), nil
}

// ConfirmPairing validates a 6-digit code entered by the local admin.
// Creates the paired device and stores the session token for the remote device to pick up.
func (pm *PairingManager) ConfirmPairing(code string) (deviceID string, err error) {
	pm.mu.Lock()

	sessionID, ok := pm.pendingByCode[code]
	if !ok {
		pm.mu.Unlock()
		return "", fmt.Errorf("invalid pairing code")
	}

	pending, ok := pm.pendingSessions[sessionID]
	if !ok || time.Now().After(pending.ExpiresAt) {
		delete(pm.pendingByCode, code)
		if ok {
			delete(pm.pendingSessions, sessionID)
		}
		pm.mu.Unlock()
		return "", fmt.Errorf("pairing code expired")
	}

	if pending.Approved {
		pm.mu.Unlock()
		return "", fmt.Errorf("pairing already completed")
	}

	// Remove the code mapping (one-time use)
	delete(pm.pendingByCode, code)

	// Unlock before calling CompletePairing (it doesn't need the lock)
	userAgent := pending.UserAgent
	pm.mu.Unlock()

	// Create the device and session
	deviceID, sessionToken, encKey, err := pm.completePairing(userAgent)
	if err != nil {
		return "", err
	}

	// Store result for the polling remote device
	pm.mu.Lock()
	pending.Approved = true
	pending.SessionToken = sessionToken
	pending.EncryptionKey = encKey
	pm.mu.Unlock()

	return deviceID, nil
}

// CheckPairingStatus checks if a pending pairing session has been approved.
// Called by the remote device polling /pair/status.
func (pm *PairingManager) CheckPairingStatus(sessionID string) (approved bool, sessionToken string, encKey string, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pending, ok := pm.pendingSessions[sessionID]
	if !ok {
		return false, "", "", fmt.Errorf("session not found")
	}

	if time.Now().After(pending.ExpiresAt) {
		delete(pm.pendingSessions, sessionID)
		delete(pm.pendingByCode, pending.Code)
		return false, "", "", fmt.Errorf("session expired")
	}

	if !pending.Approved {
		return false, "", "", nil
	}

	// Approved — return session info and clean up
	sessionToken = pending.SessionToken
	encKey = pending.EncryptionKey
	delete(pm.pendingSessions, sessionID)

	return true, sessionToken, encKey, nil
}

// completePairing creates a paired device and returns a session JWT.
func (pm *PairingManager) completePairing(userAgent string) (deviceID string, sessionToken string, encryptionKey string, err error) {
	deviceID = uuid.New().String()

	encKeyCipher, encKeyIV := "", ""
	if pm.tunnelEncKey != "" {
		encKeyCipher, encKeyIV, _ = pm.encryptor.Encrypt(pm.tunnelEncKey)
	}

	deviceName := parseDeviceName(userAgent)

	dev := &database.PairedDevice{
		ID:              deviceID,
		DeviceName:      deviceName,
		UserAgent:       userAgent,
		EncryptionKey:   encKeyCipher,
		EncryptionKeyIV: encKeyIV,
	}

	if err := pm.db.CreatePairedDevice(context.Background(), dev); err != nil {
		return "", "", "", fmt.Errorf("create device: %w", err)
	}

	sessionToken, err = CreateSessionToken(deviceID, pm.jwtSecret)
	if err != nil {
		return "", "", "", fmt.Errorf("create session token: %w", err)
	}

	return deviceID, sessionToken, pm.tunnelEncKey, nil
}

func (pm *PairingManager) cleanupExpired() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()
	for id, p := range pm.pendingSessions {
		if now.After(p.ExpiresAt) {
			delete(pm.pendingByCode, p.Code)
			delete(pm.pendingSessions, id)
		}
	}
}

func generateNumericCode(length int) (string, error) {
	max := new(big.Int)
	max.Exp(big.NewInt(10), big.NewInt(int64(length)), nil)

	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}

	format := fmt.Sprintf("%%0%dd", length)
	return fmt.Sprintf(format, n), nil
}

func parseDeviceName(ua string) string {
	if ua == "" {
		return "Unknown Device"
	}

	patterns := []struct {
		contains string
		name     string
	}{
		{"iPhone", "iPhone"},
		{"iPad", "iPad"},
		{"Android", "Android Device"},
		{"Macintosh", "Mac"},
		{"Windows", "Windows PC"},
		{"Linux", "Linux PC"},
		{"Chrome", "Chrome Browser"},
		{"Firefox", "Firefox Browser"},
		{"Safari", "Safari Browser"},
	}

	for _, p := range patterns {
		if contains(ua, p.contains) {
			return p.name
		}
	}

	if len(ua) > 30 {
		return ua[:30] + "..."
	}
	return ua
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
