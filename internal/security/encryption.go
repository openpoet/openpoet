package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

var ErrInvalidKey = errors.New("encryption key must be set via OPENPOET_ENCRYPT_KEY environment variable")

type Encryptor struct {
	key []byte
}

func NewEncryptor(key string) (*Encryptor, error) {
	if key == "" {
		// Generate a random key and store it if not provided
		key = os.Getenv("OPENPOET_ENCRYPT_KEY")
		if key == "" {
			// For convenience, derive a key from hostname+username
			// In production, users should set OPENPOET_ENCRYPT_KEY
			hostname, _ := os.Hostname()
			username := os.Getenv("USER")
			if username == "" {
				username = "openpoet"
			}
			key = fmt.Sprintf("openpoet-%s-%s-default-key", hostname, username)
		}
	}

	// Use SHA-256 to derive a 32-byte key
	hash := sha256.Sum256([]byte(key))
	return &Encryptor{key: hash[:]}, nil
}

// Encrypt encrypts plaintext using AES-256-GCM
// Returns base64-encoded ciphertext and base64-encoded IV
func (e *Encryptor) Encrypt(plaintext string) (ciphertext string, iv string, err error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	encrypted := gcm.Seal(nil, nonce, []byte(plaintext), nil)

	return base64.StdEncoding.EncodeToString(encrypted), base64.StdEncoding.EncodeToString(nonce), nil
}

// Decrypt decrypts base64-encoded ciphertext using AES-256-GCM
func (e *Encryptor) Decrypt(ciphertext string, iv string) (string, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	encryptedBytes, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	nonce, err := base64.StdEncoding.DecodeString(iv)
	if err != nil {
		return "", fmt.Errorf("failed to decode IV: %w", err)
	}

	decrypted, err := gcm.Open(nil, nonce, encryptedBytes, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(decrypted), nil
}

// GenerateKey generates a random 32-byte key encoded as base64
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
