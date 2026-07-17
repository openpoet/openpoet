package session

import "testing"

func TestProviderModelIDAcceptsContextSuffix(t *testing.T) {
	got, err := validateSessionModelID("gpt-5.6-sol[1m]")
	if err != nil {
		t.Fatalf("validateSessionModelID returned error: %v", err)
	}
	if got != "gpt-5.6-sol[1m]" {
		t.Fatalf("model = %q", got)
	}
}
