package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func createApprovalTestClient(t *testing.T, db *DB, id string) {
	t.Helper()
	client := &AutomationClient{
		ID: id, Name: id, TokenPrefix: "prefix-" + id,
		TokenHash: bytes.Repeat([]byte{id[0]}, sha256.Size), Scopes: `[]`, Enabled: true,
	}
	if err := db.CreateAutomationClient(context.Background(), client); err != nil {
		t.Fatal(err)
	}
}

func TestAutomationApprovalGrantIsHashOnlyBoundAndOneTime(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"issuer", "target", "other"} {
		createApprovalTestClient(t, db, id)
	}
	now := time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC)
	rawToken := "opagv1_plaintext-must-never-be-stored"
	tokenHash := sha256.Sum256([]byte(rawToken))
	grant := &AutomationApprovalGrant{
		ID: "grant-1", TokenHash: tokenHash[:], IssuedByClientID: "issuer", TargetClientID: "target",
		Capability: "tasks.delete", CommandID: "command-1", AuthorizationRef: "task:external:1",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := db.CreateAutomationApprovalGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetAutomationApprovalGrant(ctx, grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.TokenHash, tokenHash[:]) || stored.UsedAt.Valid {
		t.Fatalf("stored grant=%+v", stored)
	}
	if stored.IssuedByClientID != "issuer" || stored.TargetClientID != "target" ||
		stored.Capability != "tasks.delete" || stored.CommandID != "command-1" ||
		stored.AuthorizationRef != "task:external:1" || !stored.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("stored grant binding=%+v", stored)
	}
	encoded, _ := json.Marshal(stored)
	if bytes.Contains(encoded, []byte(rawToken)) || bytes.Contains(encoded, []byte("token_hash")) {
		t.Fatalf("grant JSON exposed token material: %s", encoded)
	}
	var textFields string
	if err := db.GetContext(ctx, &textFields, `
		SELECT id || issued_by_client_id || target_client_id || capability || command_id || authorization_ref
		FROM automation_approval_grants WHERE id = ?`, grant.ID); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(textFields), []byte(rawToken)) {
		t.Fatal("raw approval token was persisted in a text field")
	}

	for name, binding := range map[string][4]string{
		"cross-client":  {"other", "tasks.delete", "command-1", "task:external:1"},
		"capability":    {"target", "tasks.approve_verification", "command-1", "task:external:1"},
		"command":       {"target", "tasks.delete", "command-2", "task:external:1"},
		"authorization": {"target", "tasks.delete", "command-1", "signal:other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ConsumeAutomationApprovalGrant(ctx, tokenHash[:], binding[0], binding[1], binding[2], binding[3], now); !errors.Is(err, ErrAutomationApprovalBindingMismatch) {
				t.Fatalf("binding error=%v", err)
			}
		})
	}
	consumed, err := db.ConsumeAutomationApprovalGrant(ctx, tokenHash[:], "target", "tasks.delete", "command-1", "task:external:1", now)
	if err != nil || !consumed.UsedAt.Valid {
		t.Fatalf("consume grant=%+v err=%v", consumed, err)
	}
	if _, err := db.ConsumeAutomationApprovalGrant(ctx, tokenHash[:], "target", "tasks.delete", "command-1", "task:external:1", now); !errors.Is(err, ErrAutomationApprovalUsed) {
		t.Fatalf("second consume error=%v", err)
	}
}

func TestAutomationApprovalGrantExpiryAndConcurrentSingleWinner(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"issuer", "target"} {
		createApprovalTestClient(t, db, id)
	}
	now := time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC)
	expiredHash := sha256.Sum256([]byte("expired"))
	if err := db.CreateAutomationApprovalGrant(ctx, &AutomationApprovalGrant{
		ID: "expired", TokenHash: expiredHash[:], IssuedByClientID: "issuer", TargetClientID: "target",
		Capability: "tasks.delete", CommandID: "expired-command", AuthorizationRef: "task:expired",
		CreatedAt: now, ExpiresAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeAutomationApprovalGrant(ctx, expiredHash[:], "target", "tasks.delete", "expired-command", "task:expired", now.Add(time.Second)); !errors.Is(err, ErrAutomationApprovalExpired) {
		t.Fatalf("expiry error=%v", err)
	}

	concurrentHash := sha256.Sum256([]byte("concurrent"))
	if err := db.CreateAutomationApprovalGrant(ctx, &AutomationApprovalGrant{
		ID: "concurrent", TokenHash: concurrentHash[:], IssuedByClientID: "issuer", TargetClientID: "target",
		Capability: "tasks.delete", CommandID: "concurrent-command", AuthorizationRef: "task:concurrent",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.ConsumeAutomationApprovalGrant(ctx, concurrentHash[:], "target", "tasks.delete", "concurrent-command", "task:concurrent", now)
			if err == nil {
				winners.Add(1)
				return
			}
			if !errors.Is(err, ErrAutomationApprovalUsed) {
				errCh <- fmt.Errorf("unexpected consume error: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if winners.Load() != 1 {
		t.Fatalf("approval winners=%d, want 1", winners.Load())
	}
}
