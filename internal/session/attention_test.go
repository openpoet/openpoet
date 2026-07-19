package session

import (
	"testing"
	"time"
)

func sentinelWithClock() (*AttentionSentinel, *time.Time) {
	s := NewAttentionSentinel()
	clock := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	return s, &clock
}

func TestAttentionSentinelDetectsQuestion(t *testing.T) {
	s, _ := sentinelWithClock()
	got := s.Feed("s1", []byte("Do you want to proceed? (y/n)\n"))
	if len(got) != 1 {
		t.Fatalf("detections = %d, want 1 (%+v)", len(got), got)
	}
	if got[0].Kind != "question" {
		t.Fatalf("kind = %q, want question", got[0].Kind)
	}
	// A prompt with NO trailing newline (the realistic case) must also fire,
	// and its later completion must not double-fire (hash dedup).
	got = s.Feed("s2", []byte("❯ Would you like to continue? [y/N] "))
	if len(got) != 1 {
		t.Fatalf("partial-line prompt detections = %d, want 1", len(got))
	}
	got = s.Feed("s2", []byte("\n"))
	if len(got) != 0 {
		t.Fatalf("completing the same prompt re-fired: %+v", got)
	}
}

// TestAttentionSentinelIgnoresBuildNoise is the regex floor the critique
// demanded: the dormant DefaultTriggers would fire on every one of these
// lines; the sentinel must fire on none.
func TestAttentionSentinelIgnoresBuildNoise(t *testing.T) {
	s, _ := sentinelWithClock()
	noise := "compiling calc.go...\n" +
		"build done in 3.2s\n" +
		"error: undefined variable x\n" +
		"warning: unused import\n" +
		"Task completed successfully.\n" +
		"tests finished\n"
	if got := s.Feed("s1", []byte(noise)); len(got) != 0 {
		t.Fatalf("build noise produced detections: %+v", got)
	}
}

func TestAttentionSentinelStripsANSIAndReassemblesChunks(t *testing.T) {
	s, _ := sentinelWithClock()
	// The question arrives styled and split across two PTY chunks.
	first := s.Feed("s1", []byte("\x1b[1mDo you want to pro"))
	second := s.Feed("s1", []byte("ceed? (y/n)\x1b[0m\n"))
	if len(first)+len(second) != 1 {
		t.Fatalf("split ANSI question detections = %d, want exactly 1", len(first)+len(second))
	}
}

func TestAttentionSentinelCooldownAndDedup(t *testing.T) {
	s, clock := sentinelWithClock()
	if got := s.Feed("s1", []byte("Do you want to proceed? (y/n)\n")); len(got) != 1 {
		t.Fatalf("first question detections = %d, want 1", len(got))
	}
	// TUI redraw of the same line: hash-deduplicated.
	if got := s.Feed("s1", []byte("Do you want to proceed? (y/n)\n")); len(got) != 0 {
		t.Fatalf("redraw re-fired: %+v", got)
	}
	// A different question within the per-session cooldown: suppressed.
	*clock = clock.Add(2 * time.Second)
	if got := s.Feed("s1", []byte("Are you sure you want to delete this? (y/n)\n")); len(got) != 0 {
		t.Fatalf("question inside cooldown fired: %+v", got)
	}
	// After the cooldown a genuinely new question fires again.
	*clock = clock.Add(attentionSessionCooldown)
	if got := s.Feed("s1", []byte("Are you sure you want to delete this? (y/n)\n")); len(got) != 1 {
		t.Fatalf("new question after cooldown detections = %d, want 1", len(got))
	}
	// Other sessions are independent.
	if got := s.Feed("s2", []byte("Do you want to proceed? (y/n)\n")); len(got) != 1 {
		t.Fatalf("second session detections = %d, want 1", len(got))
	}
}
