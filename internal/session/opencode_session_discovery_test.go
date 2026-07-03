package session

import (
	"testing"
	"time"
)

func TestSelectOpenCodeProviderSessionIDPrefersMatchingNewestSession(t *testing.T) {
	workDir := "/tmp/project"
	since := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	data := []byte(`{
		"sessions": [
			{"id":"other-new","directory":"/tmp/other","updatedAt":"2026-07-03T12:03:00Z"},
			{"id":"project-old","directory":"/tmp/project","updatedAt":"2026-07-03T12:01:00Z"},
			{"id":"project-new","directory":"/tmp/project","updatedAt":"2026-07-03T12:02:00Z"}
		]
	}`)

	got := selectOpenCodeProviderSessionID(data, workDir, since)
	if got != "project-new" {
		t.Fatalf("provider session id = %q, want project-new", got)
	}
}

func TestSelectOpenCodeProviderSessionIDFallsBackWhenDirectoryMissing(t *testing.T) {
	since := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	data := []byte(`[
		{"id":"older","updatedAt":"2026-07-03T12:01:00Z"},
		{"id":"newer","updatedAt":"2026-07-03T12:02:00Z"}
	]`)

	got := selectOpenCodeProviderSessionID(data, "/tmp/project", since)
	if got != "newer" {
		t.Fatalf("provider session id = %q, want newer", got)
	}
}
