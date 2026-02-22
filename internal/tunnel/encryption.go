package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// EncryptedMessage wraps encrypted data sent through the tunnel.
// The phone browser detects this format and decrypts using the shared key.
type EncryptedMessage struct {
	Encrypted  bool   `json:"_encrypted"`
	Data       string `json:"data"`     // base64-encoded AES-256-GCM ciphertext
	IV         string `json:"iv"`       // base64-encoded nonce
	OrigIsText bool   `json:"_is_text"` // whether the original frame was text
}

// EncryptFrame encrypts a WebSocket frame using AES-256-GCM.
// Returns the encrypted message as a JSON string.
func EncryptFrame(plaintext []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	msg := &EncryptedMessage{
		Encrypted: true,
		Data:      base64.StdEncoding.EncodeToString(ciphertext),
		IV:        base64.StdEncoding.EncodeToString(nonce),
	}

	return json.Marshal(msg)
}

// DecryptFrame decrypts an EncryptedMessage using AES-256-GCM.
func DecryptFrame(encMsg *EncryptedMessage, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encMsg.Data)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}

	nonce, err := base64.StdEncoding.DecodeString(encMsg.IV)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}
