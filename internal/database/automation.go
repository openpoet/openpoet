package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrAutomationCommandNotProcessing = errors.New("automation command is not processing")

type AutomationClient struct {
	ID          string       `db:"id" json:"id"`
	Name        string       `db:"name" json:"name"`
	TokenPrefix string       `db:"token_prefix" json:"token_prefix"`
	TokenHash   []byte       `db:"token_hash" json:"-"`
	Scopes      string       `db:"scopes" json:"scopes"`
	Enabled     bool         `db:"enabled" json:"enabled"`
	LastUsedAt  sql.NullTime `db:"last_used_at" json:"last_used_at,omitempty"`
	CreatedAt   time.Time    `db:"created_at" json:"created_at"`
	RotatedAt   sql.NullTime `db:"rotated_at" json:"rotated_at,omitempty"`
}

type AutomationCommand struct {
	ID                  string       `db:"id" json:"id"`
	ClientID            string       `db:"client_id" json:"client_id"`
	IdempotencyKey      string       `db:"idempotency_key" json:"idempotency_key"`
	RequestFingerprint  string       `db:"request_fingerprint" json:"request_fingerprint"`
	Operation           string       `db:"operation" json:"operation"`
	Status              string       `db:"status" json:"status"`
	ResourceType        string       `db:"resource_type" json:"resource_type"`
	ResourceID          string       `db:"resource_id" json:"resource_id"`
	ResponseStatus      int          `db:"response_status" json:"response_status"`
	ResponseContentType string       `db:"response_content_type" json:"response_content_type"`
	ResponseBody        []byte       `db:"response_body" json:"response_body"`
	ErrorCode           string       `db:"error_code" json:"error_code"`
	CreatedAt           time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt           time.Time    `db:"updated_at" json:"updated_at"`
	ExpiresAt           sql.NullTime `db:"expires_at" json:"expires_at,omitempty"`
}

func (d *DB) CreateAutomationClient(ctx context.Context, client *AutomationClient) error {
	if client == nil {
		return errors.New("automation client is required")
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO automation_clients (id, name, token_prefix, token_hash, scopes, enabled)
		VALUES (?, ?, ?, ?, ?, ?)`,
		client.ID, client.Name, client.TokenPrefix, client.TokenHash, client.Scopes, client.Enabled)
	return err
}

func (d *DB) GetAutomationClientByTokenPrefix(ctx context.Context, prefix string) (*AutomationClient, error) {
	var client AutomationClient
	if err := d.GetContext(ctx, &client,
		"SELECT * FROM automation_clients WHERE token_prefix = ?", prefix); err != nil {
		return nil, err
	}
	return &client, nil
}

// GetAutomationClientByName returns the client with this exact name, or
// (nil, nil) if none exists. Used for idempotent server-side provisioning.
func (d *DB) GetAutomationClientByName(ctx context.Context, name string) (*AutomationClient, error) {
	var client AutomationClient
	err := d.GetContext(ctx, &client, "SELECT * FROM automation_clients WHERE name = ?", name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &client, nil
}

// DeleteAutomationClient removes a client row (used to re-provision a
// coordinator whose token was never persisted).
func (d *DB) DeleteAutomationClient(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM automation_clients WHERE id = ?", id)
	return err
}

func (d *DB) SetAutomationClientEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := d.ExecContext(ctx, "UPDATE automation_clients SET enabled = ? WHERE id = ?", enabled, id)
	return err
}

func (d *DB) TouchAutomationClient(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "UPDATE automation_clients SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?", id)
	return err
}

// ClaimAutomationCommand atomically creates an idempotency claim or returns
// the claim that already owns the client/key pair. The bool is true only for
// the caller that created the claim.
func (d *DB) ClaimAutomationCommand(ctx context.Context, command *AutomationCommand) (*AutomationCommand, bool, error) {
	if command == nil {
		return nil, false, errors.New("automation command is required")
	}
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO automation_commands
			(id, client_id, idempotency_key, request_fingerprint, operation, status, expires_at)
		VALUES (?, ?, ?, ?, ?, 'processing', ?)`,
		command.ID, command.ClientID, command.IdempotencyKey,
		command.RequestFingerprint, command.Operation, command.ExpiresAt)
	if err != nil {
		return nil, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}

	var claimed AutomationCommand
	if err := tx.GetContext(ctx, &claimed, `
		SELECT * FROM automation_commands
		WHERE client_id = ? AND idempotency_key = ?`,
		command.ClientID, command.IdempotencyKey); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &claimed, rows == 1, nil
}

func (d *DB) CompleteAutomationCommand(
	ctx context.Context,
	id string,
	status string,
	responseStatus int,
	contentType string,
	body []byte,
	errorCode string,
) error {
	return d.CompleteAutomationCommandWithEvent(ctx, id, status, responseStatus, contentType, body, errorCode, nil)
}

// CompleteAutomationCommandWithEvent closes the command ledger and optionally
// appends its success audit event in the same transaction. A failure in either
// write rolls back both, leaving the command in processing for reconciliation.
func (d *DB) CompleteAutomationCommandWithEvent(
	ctx context.Context,
	id string,
	status string,
	responseStatus int,
	contentType string,
	body []byte,
	errorCode string,
	event *EventOutboxAppend,
) error {
	if status != "succeeded" && status != "failed" && status != "indeterminate" {
		return fmt.Errorf("invalid automation command status %q", status)
	}
	if event != nil && status != "succeeded" {
		return errors.New("automation command event requires succeeded ledger status")
	}
	if body == nil {
		body = []byte{}
	}
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE automation_commands
		SET status = ?, response_status = ?, response_content_type = ?,
			response_body = ?, error_code = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'processing'`,
		status, responseStatus, contentType, body, errorCode, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAutomationCommandNotProcessing
	}
	if event != nil {
		if _, err := AppendEventOutbox(ctx, tx, *event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if event != nil {
		d.NotifyOutboxAppended()
	}
	return nil
}

func (d *DB) GetAutomationCommand(ctx context.Context, id string) (*AutomationCommand, error) {
	var command AutomationCommand
	if err := d.GetContext(ctx, &command, "SELECT * FROM automation_commands WHERE id = ?", id); err != nil {
		return nil, err
	}
	return &command, nil
}
