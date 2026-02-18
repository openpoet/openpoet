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

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	pairingTokenExpiry = 5 * time.Minute
	codeExpiry         = 5 * time.Minute
	codeLength         = 6
)

// PairingCode is an active 6-digit code for device pairing.
type PairingCode struct {
	Code      string
	ExpiresAt time.Time
}

// PairingManager handles QR code and 6-digit code pairing flows.
type PairingManager struct {
	db        *database.DB
	encryptor *security.Encryptor
	jwtSecret []byte

	// Tunnel-wide encryption key (base64-encoded raw AES-256 key)
	// Shared with all devices during pairing, used by tunnel client for encryption.
	tunnelEncKey string

	codes map[string]*PairingCode // code → PairingCode
	mu    sync.Mutex
}

// NewPairingManager creates a new PairingManager.
func NewPairingManager(db *database.DB, encryptor *security.Encryptor, jwtSecret []byte) *PairingManager {
	pm := &PairingManager{
		db:        db,
		encryptor: encryptor,
		jwtSecret: jwtSecret,
		codes:     make(map[string]*PairingCode),
	}

	// Load or generate tunnel-wide encryption key
	pm.loadOrGenerateEncKey()

	// Periodic cleanup of expired codes
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			pm.cleanupExpiredCodes()
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

	// Generate new key
	key, err := security.GenerateKey()
	if err != nil {
		return
	}
	pm.tunnelEncKey = key

	// Store encrypted
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

// GenerateQRToken creates a short-lived JWT for QR code pairing.
func (pm *PairingManager) GenerateQRToken() (string, error) {
	claims := &jwt.RegisteredClaims{
		Subject:   "pair",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(pairingTokenExpiry)),
		ID:        uuid.New().String(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(pm.jwtSecret)
}

// ValidateQRToken validates a QR pairing token.
func (pm *PairingManager) ValidateQRToken(tokenStr string) (bool, error) {
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (interface{}, error) {
		return pm.jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return false, err
	}
	if claims.Subject != "pair" {
		return false, fmt.Errorf("invalid token subject")
	}
	return true, nil
}

// GenerateCode creates a random 6-digit code for device pairing.
func (pm *PairingManager) GenerateCode() (string, error) {
	code, err := generateNumericCode(codeLength)
	if err != nil {
		return "", err
	}

	pm.mu.Lock()
	pm.codes[code] = &PairingCode{
		Code:      code,
		ExpiresAt: time.Now().Add(codeExpiry),
	}
	pm.mu.Unlock()

	return code, nil
}

// ValidateCode checks if a 6-digit code is valid and removes it (one-time use).
func (pm *PairingManager) ValidateCode(code string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pc, ok := pm.codes[code]
	if !ok {
		return false
	}
	delete(pm.codes, code)

	return time.Now().Before(pc.ExpiresAt)
}

// ActiveCodeExists returns true if there is an active (non-expired) pairing code.
func (pm *PairingManager) ActiveCodeExists() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for code, pc := range pm.codes {
		if time.Now().Before(pc.ExpiresAt) {
			_ = code
			return true
		}
	}
	return false
}

// CompletePairing creates a paired device and returns a session JWT.
// The returned encryptionKey is the tunnel-wide key that the device needs for decryption.
func (pm *PairingManager) CompletePairing(userAgent string) (deviceID string, sessionToken string, encryptionKey string, err error) {
	deviceID = uuid.New().String()

	// Use tunnel-wide encryption key (not per-device)
	encKeyCipher, encKeyIV := "", ""
	if pm.tunnelEncKey != "" {
		encKeyCipher, encKeyIV, _ = pm.encryptor.Encrypt(pm.tunnelEncKey)
	}

	// Derive device name from user-agent
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

	// Create session JWT
	sessionToken, err = CreateSessionToken(deviceID, pm.jwtSecret)
	if err != nil {
		return "", "", "", fmt.Errorf("create session token: %w", err)
	}

	return deviceID, sessionToken, pm.tunnelEncKey, nil
}

func (pm *PairingManager) cleanupExpiredCodes() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()
	for code, pc := range pm.codes {
		if now.After(pc.ExpiresAt) {
			delete(pm.codes, code)
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
	// Simple extraction of device/browser from User-Agent
	if ua == "" {
		return "Unknown Device"
	}

	// Common patterns
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
