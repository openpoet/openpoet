package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	ErrAutomationApprovalInvalid         = errors.New("automation approval grant is invalid")
	ErrAutomationApprovalExpired         = errors.New("automation approval grant is expired")
	ErrAutomationApprovalUsed            = errors.New("automation approval grant was already used")
	ErrAutomationApprovalBindingMismatch = errors.New("automation approval grant binding does not match")
	ErrAutomationApprovalTargetInvalid   = errors.New("automation approval target client is unavailable")
)

type AutomationApprovalGrant struct {
	ID               string       `db:"id" json:"id"`
	TokenHash        []byte       `db:"token_hash" json:"-"`
	IssuedByClientID string       `db:"issued_by_client_id" json:"issued_by_client_id"`
	TargetClientID   string       `db:"target_client_id" json:"target_client_id"`
	Capability       string       `db:"capability" json:"capability"`
	CommandID        string       `db:"command_id" json:"command_id"`
	AuthorizationRef string       `db:"authorization_ref" json:"authorization_ref"`
	ExpiresAt        time.Time    `db:"expires_at" json:"expires_at"`
	UsedAt           sql.NullTime `db:"used_at" json:"used_at,omitempty"`
	CreatedAt        time.Time    `db:"created_at" json:"created_at"`
}

func (d *DB) CreateAutomationApprovalGrant(ctx context.Context, grant *AutomationApprovalGrant) error {
	if grant == nil || len(grant.TokenHash) != 32 {
		return ErrAutomationApprovalInvalid
	}
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var targetEnabled bool
	if err := tx.GetContext(ctx, &targetEnabled,
		`SELECT enabled FROM automation_clients WHERE id = ?`, grant.TargetClientID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAutomationApprovalTargetInvalid
		}
		return err
	}
	if !targetEnabled {
		return ErrAutomationApprovalTargetInvalid
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO automation_approval_grants
			(id, token_hash, issued_by_client_id, target_client_id, capability,
			 command_id, authorization_ref, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		grant.ID, grant.TokenHash, grant.IssuedByClientID, grant.TargetClientID,
		grant.Capability, grant.CommandID, grant.AuthorizationRef,
		grant.ExpiresAt.UTC(), grant.CreatedAt.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeAutomationApprovalGrant validates the complete grant binding and
// marks it used in the same transaction. Only one concurrent caller can move
// used_at from NULL to a timestamp.
func (d *DB) ConsumeAutomationApprovalGrant(
	ctx context.Context,
	tokenHash []byte,
	targetClientID string,
	capability string,
	commandID string,
	authorizationRef string,
	now time.Time,
) (*AutomationApprovalGrant, error) {
	if len(tokenHash) != 32 {
		return nil, ErrAutomationApprovalInvalid
	}
	now = now.UTC()
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var grant AutomationApprovalGrant
	if err := tx.GetContext(ctx, &grant,
		`SELECT * FROM automation_approval_grants WHERE token_hash = ?`, tokenHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAutomationApprovalInvalid
		}
		return nil, err
	}
	if grant.TargetClientID != targetClientID || grant.Capability != capability || grant.CommandID != commandID ||
		grant.AuthorizationRef != authorizationRef {
		return nil, ErrAutomationApprovalBindingMismatch
	}
	if grant.UsedAt.Valid {
		return nil, ErrAutomationApprovalUsed
	}
	if !grant.ExpiresAt.After(now) {
		return nil, ErrAutomationApprovalExpired
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE automation_approval_grants
		SET used_at = ?
		WHERE id = ? AND used_at IS NULL AND expires_at > ?
			AND target_client_id = ? AND capability = ? AND command_id = ?
			AND authorization_ref = ?`,
		now, grant.ID, now, targetClientID, capability, commandID, authorizationRef)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, ErrAutomationApprovalUsed
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	grant.UsedAt = sql.NullTime{Time: now, Valid: true}
	return &grant, nil
}

func (d *DB) GetAutomationApprovalGrant(ctx context.Context, id string) (*AutomationApprovalGrant, error) {
	var grant AutomationApprovalGrant
	if err := d.GetContext(ctx, &grant,
		`SELECT * FROM automation_approval_grants WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &grant, nil
}
