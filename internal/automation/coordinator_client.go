package automation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"openpoet/internal/database"
)

// CoordinatorClientName is the reserved name of the built-in coordinator's
// automation client.
const CoordinatorClientName = "coordinator"

// coordinatorScopes are the pinned scopes the in-process coordinator is allowed
// to hold. Deliberately EXCLUDES approvals:grant and approvals:self — the
// coordinator's destructive verbs (stop, remove, merge) stay grant-gated, so a
// human warden remains in the loop.
var coordinatorScopes = []Scope{
	ScopeEventsRead,
	ScopeSessionsRead,
	ScopeSessionsWrite,
	ScopeConflictsRead,
	ScopeWorkspacesRead,
	ScopeWorkspacesWrite,
	ScopeWorkRunsRead,
	ScopeWorkRunsWrite,
	ScopeTasksRead,
	ScopeTasksWrite,
	ScopeNotificationsRead,
}

// coordinatorClientStore is the DB surface EnsureCoordinatorClient needs.
type coordinatorClientStore interface {
	ClientProvisioner
	GetAutomationClientByName(ctx context.Context, name string) (*database.AutomationClient, error)
	SetAutomationClientEnabled(ctx context.Context, id string, enabled bool) error
}

// EnsureCoordinatorClient provisions the 'coordinator' automation client if it
// does not exist yet, writing its one-time plaintext token to tokenPath (mode
// 0600, O_EXCL). Idempotent: if the client already exists it is a no-op and no
// token is written. Single-user box; the on-disk token is an accepted risk
// documented in the checkpoint.
func EnsureCoordinatorClient(ctx context.Context, store coordinatorClientStore, tokenPath string) error {
	if store == nil {
		return fmt.Errorf("coordinator client store is required")
	}
	existing, err := store.GetAutomationClientByName(ctx, CoordinatorClientName)
	if err != nil {
		return fmt.Errorf("checking for coordinator client: %w", err)
	}
	if existing != nil {
		return nil // already provisioned
	}
	provisioned, err := ProvisionClient(ctx, store, CoordinatorClientName, coordinatorScopes, nil)
	if err != nil {
		return fmt.Errorf("provisioning coordinator client: %w", err)
	}
	if tokenPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		return fmt.Errorf("preparing coordinator token dir: %w", err)
	}
	f, err := os.OpenFile(tokenPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// The client exists but we could not persist its token: disable it so a
		// dangling, unreachable credential never lingers (CLI precedent).
		_ = store.SetAutomationClientEnabled(ctx, provisioned.Client.ID, false)
		return fmt.Errorf("writing coordinator token: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(provisioned.Token); err != nil {
		_ = store.SetAutomationClientEnabled(ctx, provisioned.Client.ID, false)
		return fmt.Errorf("writing coordinator token: %w", err)
	}
	return nil
}
