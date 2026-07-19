package database

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWritersNoDatabaseLocked: hammer the write handle from many
// goroutines (blackboard CAS + outbox appends) while the read pool serves
// reads — no "database is locked", and every CAS key ends at exactly v1.
func TestConcurrentWritersNoDatabaseLocked(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "hammer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	const writers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, writers*3)
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			expected := int64(0)
			if _, err := db.BlackboardPut(ctx, BlackboardPutInput{
				ScopeType: "global", Key: fmt.Sprintf("hammer-%d", i),
				ValueJSON: `{"n":1}`, ExpectedVersion: &expected, Actor: "t",
			}); err != nil {
				errCh <- fmt.Errorf("blackboard %d: %w", i, err)
			}
			if err := db.AppendMissionEvent(ctx, "test.hammer", int64(i), map[string]any{"i": i}); err != nil {
				errCh <- fmt.Errorf("outbox %d: %w", i, err)
			}
			if _, _, err := db.ReadEventOutboxPage(ctx, 0, 50); err != nil {
				errCh <- fmt.Errorf("read %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy") {
			t.Fatalf("database-is-locked class failure under concurrency: %v", err)
		}
		t.Fatalf("concurrent write failed: %v", err)
	}
	for i := 0; i < writers; i++ {
		entry, err := db.BlackboardGet(ctx, "global", 0, fmt.Sprintf("hammer-%d", i))
		if err != nil || entry == nil || entry.Version != 1 {
			t.Fatalf("CAS semantics broken for key %d: %+v err=%v", i, entry, err)
		}
	}

	// CAS single-winner under contention on ONE key.
	var winners int64
	var mu sync.Mutex
	wg = sync.WaitGroup{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			expected := int64(0)
			if _, err := db.BlackboardPut(ctx, BlackboardPutInput{
				ScopeType: "global", Key: "hammer-single",
				ValueJSON: `{"w":1}`, ExpectedVersion: &expected, Actor: "t",
			}); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			} else if !errors.Is(err, ErrBlackboardCASConflict) {
				t.Errorf("loser must lose by CAS conflict, got %v", err)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("expected exactly 1 CAS winner, got %d", winners)
	}
}

// TestReadPoolServesReadsDuringWrite: while the single write connection holds
// an open IMMEDIATE transaction, the read pool still answers — long reads and
// writes no longer serialize behind one connection.
func TestReadPoolServesReadsDuringWrite(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.AppendMissionEvent(ctx, "test.seed", 1, map[string]any{"seed": true}); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTxx(ctx, nil) // occupies THE write connection (immediate)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	done := make(chan error, 1)
	go func() {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, _, err := db.ReadEventOutboxPage(readCtx, 0, 10)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read pool failed while writer held the connection: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("read starved behind the write connection — read pool not effective")
	}
}
