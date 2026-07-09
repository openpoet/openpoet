package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAutomationClientAndCommandPersistence(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	client := &AutomationClient{
		ID:          "helena-client",
		Name:        "helena",
		TokenPrefix: "prefix",
		TokenHash:   []byte("01234567890123456789012345678901"),
		Scopes:      `["tasks:read"]`,
		Enabled:     true,
	}
	if err := db.CreateAutomationClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	found, err := db.GetAutomationClientByTokenPrefix(ctx, client.TokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != client.ID || !found.Enabled || found.Scopes != client.Scopes {
		t.Fatalf("unexpected persisted client: %+v", found)
	}

	command := &AutomationCommand{
		ID:                 "command-1",
		ClientID:           client.ID,
		IdempotencyKey:     "key-1",
		RequestFingerprint: "fingerprint-1",
		Operation:          "POST /test",
	}
	claimed, created, err := db.ClaimAutomationCommand(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if !created || claimed.Status != "processing" || claimed.ID != command.ID {
		t.Fatalf("unexpected first claim: created=%v command=%+v", created, claimed)
	}

	duplicate := *command
	duplicate.ID = "command-2"
	claimed, created, err = db.ClaimAutomationCommand(ctx, &duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if created || claimed.ID != command.ID {
		t.Fatalf("duplicate claim created=%v command=%+v", created, claimed)
	}

	body := []byte(`{"ok":true}`)
	if err := db.CompleteAutomationCommand(ctx, command.ID, "succeeded", 201, "application/json", body, ""); err != nil {
		t.Fatal(err)
	}
	completed, err := db.GetAutomationCommand(ctx, command.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "succeeded" || completed.ResponseStatus != 201 || string(completed.ResponseBody) != string(body) {
		t.Fatalf("unexpected completed command: %+v", completed)
	}
	if err := db.CompleteAutomationCommand(ctx, command.ID, "failed", 500, "", nil, "late"); !errors.Is(err, ErrAutomationCommandNotProcessing) {
		t.Fatalf("second completion error = %v", err)
	}
}

func TestClaimAutomationCommandHasSingleWinner(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	client := &AutomationClient{
		ID: "client", Name: "client", TokenPrefix: "prefix",
		TokenHash: []byte("01234567890123456789012345678901"), Scopes: `[]`, Enabled: true,
	}
	if err := db.CreateAutomationClient(ctx, client); err != nil {
		t.Fatal(err)
	}

	var winners atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, created, err := db.ClaimAutomationCommand(ctx, &AutomationCommand{
				ID: fmt.Sprintf("command-%d", n), ClientID: client.ID,
				IdempotencyKey: "same-key", RequestFingerprint: "same-fingerprint", Operation: "POST /test",
			})
			if err != nil {
				errs <- err
				return
			}
			if created {
				winners.Add(1)
			}
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if winners.Load() != 1 {
		t.Fatalf("claim winners = %d, want 1", winners.Load())
	}
}

func TestAutomationClientForeignKeyAndRevocation(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	_, _, err := db.ClaimAutomationCommand(ctx, &AutomationCommand{
		ID: "orphan", ClientID: "missing", IdempotencyKey: "key",
		RequestFingerprint: "fp", Operation: "POST /test",
	})
	if err == nil {
		t.Fatal("expected foreign key error")
	}

	client := &AutomationClient{
		ID: "client", Name: "client", TokenPrefix: "prefix",
		TokenHash: []byte("01234567890123456789012345678901"), Scopes: `[]`, Enabled: true,
	}
	if err := db.CreateAutomationClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAutomationClientEnabled(ctx, client.ID, false); err != nil {
		t.Fatal(err)
	}
	found, err := db.GetAutomationClientByTokenPrefix(ctx, client.TokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if found.Enabled {
		t.Fatal("client remained enabled")
	}
	if _, err := db.GetAutomationClientByTokenPrefix(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing client error = %v", err)
	}
}
