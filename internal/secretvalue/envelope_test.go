package secretvalue

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

type testCodec struct{}

func (testCodec) Encrypt(value string) (string, string, error) {
	return base64.StdEncoding.EncodeToString([]byte(value)), "test-iv", nil
}

func TestEnvelopeRoundTripAndLegacyCompatibility(t *testing.T) {
	encrypted, err := Encrypt(testCodec{}, "private-value")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(encrypted) || strings.Contains(encrypted, "private-value") {
		t.Fatalf("unexpected envelope: %q", encrypted)
	}
	resolved, err := Resolve(encrypted, func(ciphertext, iv string) (string, error) {
		if iv != "test-iv" || ciphertext != base64.StdEncoding.EncodeToString([]byte("private-value")) {
			t.Fatalf("unexpected encrypted parts: %q %q", ciphertext, iv)
		}
		decoded, err := base64.StdEncoding.DecodeString(ciphertext)
		return string(decoded), err
	})
	if err != nil || resolved != "private-value" {
		t.Fatalf("resolved = %q, err=%v", resolved, err)
	}
	legacy := `{"TOKEN":"legacy-plaintext"}`
	resolved, err = Resolve(legacy, nil)
	if err != nil || resolved != legacy {
		t.Fatalf("legacy resolved = %q, err=%v", resolved, err)
	}
	legacyVersion := `{"version":"prod","TOKEN":"legacy-plaintext"}`
	resolved, err = Resolve(legacyVersion, nil)
	if err != nil || resolved != legacyVersion {
		t.Fatalf("legacy version env resolved = %q, err=%v", resolved, err)
	}
}

func TestEnvelopeMalformedValuesFailClosed(t *testing.T) {
	values := []string{
		`{"version":1,"ciphertext":"cipher"}`,
		`{"version":2,"ciphertext":"cipher","iv":"iv"}`,
		`{"iv":"iv"}`,
	}
	for _, value := range values {
		if _, err := Resolve(value, nil); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("Resolve(%q) error = %v", value, err)
		}
		if _, err := NeedsEncryption(value); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("NeedsEncryption(%q) error = %v", value, err)
		}
	}
}
