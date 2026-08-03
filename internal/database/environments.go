package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// EnvironmentManifest is one approved manifest content hash (V67).
type EnvironmentManifest struct {
	ID            int64     `db:"id" json:"id"`
	ProjectID     int64     `db:"project_id" json:"project_id"`
	SourcePath    string    `db:"source_path" json:"source_path"`
	Content       string    `db:"content" json:"-"`
	ContentSHA256 string    `db:"content_sha256" json:"content_sha256"`
	ApprovedAt    time.Time `db:"approved_at" json:"approved_at"`
	ApprovedBy    string    `db:"approved_by" json:"approved_by"`
}

// EnvironmentResource is one provisioned resource — a port, a service (with its
// pid in runtime_ref), etc (V68).
type EnvironmentResource struct {
	ID          int64          `db:"id" json:"id"`
	ProjectID   int64          `db:"project_id" json:"project_id"`
	WorkspaceID sql.NullString `db:"workspace_id" json:"workspace_id,omitempty"`
	Kind        string         `db:"kind" json:"kind"`
	Name        string         `db:"name" json:"name"`
	Value       string         `db:"value" json:"value"`
	Driver      string         `db:"driver" json:"driver"`
	Status      string         `db:"status" json:"status"`
	HealthJSON  sql.NullString `db:"health_json" json:"health,omitempty"`
	RuntimeRef  string         `db:"runtime_ref" json:"runtime_ref"`
	CreatedAt   time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time      `db:"updated_at" json:"updated_at"`
}

// ErrNoFreePort is returned when the allocator exhausts its range.
var ErrNoFreePort = errors.New("no free port available in the configured range")

// ApproveManifest records an approved manifest SHA-256 for a project. Idempotent
// on (project_id, content_sha256).
func (d *DB) ApproveManifest(ctx context.Context, projectID int64, sourcePath, content, sha, approvedBy string) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO environment_manifests (project_id, source_path, content, content_sha256, approved_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id, content_sha256) DO UPDATE SET
			approved_at=CURRENT_TIMESTAMP, approved_by=excluded.approved_by, content=excluded.content`,
		projectID, sourcePath, content, sha, approvedBy)
	return err
}

// IsManifestApproved reports whether this exact content hash is approved for the
// project — the provisioner's load-bearing gate.
func (d *DB) IsManifestApproved(ctx context.Context, projectID int64, sha string) (bool, error) {
	var n int
	err := d.GetContext(ctx, &n, "SELECT COUNT(*) FROM environment_manifests WHERE project_id=? AND content_sha256=?", projectID, sha)
	return n > 0, err
}

// IsPortReserved reports whether a port is recorded as reserved (8080/8081/8090
// and any operator-declared rows) — turning a prose convention into data.
func (d *DB) IsPortReserved(ctx context.Context, port int) (bool, error) {
	var n int
	err := d.GetContext(ctx, &n,
		"SELECT COUNT(*) FROM environment_resources WHERE kind='port' AND status='reserved' AND value=?",
		strconv.Itoa(port))
	return n > 0, err
}

// portInUse reports whether a live (non-released) port row already claims value.
func (d *DB) portInUse(ctx context.Context, port int) (bool, error) {
	var n int
	err := d.GetContext(ctx, &n,
		"SELECT COUNT(*) FROM environment_resources WHERE kind='port' AND status != 'released' AND value=?",
		strconv.Itoa(port))
	return n > 0, err
}

// portRange resolves the allocator range from settings (defaults 8700-8789 — the
// reserved fake-service band that never collides with the sandbox 879x or the
// forbidden 8080/8081).
func (d *DB) portRange(ctx context.Context) (int, int) {
	min, max := 8700, 8789
	if v, err := d.GetSetting(ctx, "environment_port_min"); err == nil {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			min = n
		}
	}
	if v, err := d.GetSetting(ctx, "environment_port_max"); err == nil {
		if n, e := strconv.Atoi(v); e == nil && n >= min {
			max = n
		}
	}
	return min, max
}

// AllocatePort picks a free port in the configured range, skipping reserved and
// already-live ports, probing with net.Listen, and persisting a live port row.
// The partial-unique index on live port values makes the INSERT the final
// race-guard on the single connection.
func (d *DB) AllocatePort(ctx context.Context, projectID int64, workspaceID, name string) (int, error) {
	min, max := d.portRange(ctx)
	for port := min; port <= max; port++ {
		if reserved, err := d.IsPortReserved(ctx, port); err != nil {
			return 0, err
		} else if reserved {
			continue
		}
		if used, err := d.portInUse(ctx, port); err != nil {
			return 0, err
		} else if used {
			continue
		}
		if !probeFreePort(port) {
			continue
		}
		id, err := d.insertResource(ctx, EnvironmentResource{
			ProjectID: projectID, WorkspaceID: nullStr(workspaceID), Kind: "port",
			Name: name, Value: strconv.Itoa(port), Driver: "none", Status: "ready",
		})
		if err != nil {
			// A concurrent allocator won the partial-unique index — try the next port.
			continue
		}
		_ = id
		return port, nil
	}
	return 0, ErrNoFreePort
}

// AllocateSpecificPort claims an explicit port (a manifest that hard-codes one).
// It fails closed if the port is reserved or already live.
func (d *DB) AllocateSpecificPort(ctx context.Context, projectID int64, workspaceID, name string, port int) error {
	if reserved, err := d.IsPortReserved(ctx, port); err != nil {
		return err
	} else if reserved {
		return fmt.Errorf("port_reserved: %d is reserved and cannot be allocated", port)
	}
	if used, err := d.portInUse(ctx, port); err != nil {
		return err
	} else if used {
		return fmt.Errorf("port_in_use: %d is already allocated", port)
	}
	if !probeFreePort(port) {
		return fmt.Errorf("port_in_use: %d is not free on the host", port)
	}
	_, err := d.insertResource(ctx, EnvironmentResource{
		ProjectID: projectID, WorkspaceID: nullStr(workspaceID), Kind: "port",
		Name: name, Value: strconv.Itoa(port), Driver: "none", Status: "ready",
	})
	return err
}

func (d *DB) insertResource(ctx context.Context, r EnvironmentResource) (int64, error) {
	res, err := d.ExecContext(ctx, `
		INSERT INTO environment_resources (project_id, workspace_id, kind, name, value, driver, status, runtime_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProjectID, r.WorkspaceID, r.Kind, r.Name, r.Value, driverOrNone(r.Driver), statusOrAlloc(r.Status), r.RuntimeRef)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertServiceResource records a spawned service (driver=process) with its pid.
func (d *DB) InsertServiceResource(ctx context.Context, projectID int64, workspaceID, name, driver, pid, status string) (int64, error) {
	return d.insertResource(ctx, EnvironmentResource{
		ProjectID: projectID, WorkspaceID: nullStr(workspaceID), Kind: "service",
		Name: name, Value: name, Driver: driver, Status: status, RuntimeRef: pid,
	})
}

// SetResourceStatus updates a resource row's status (and optional health blob).
func (d *DB) SetResourceStatus(ctx context.Context, id int64, status string) error {
	_, err := d.ExecContext(ctx, "UPDATE environment_resources SET status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", status, id)
	return err
}

// ListWorkspaceResources returns all resources for a workspace.
func (d *DB) ListWorkspaceResources(ctx context.Context, workspaceID string) ([]EnvironmentResource, error) {
	var rows []EnvironmentResource
	err := d.SelectContext(ctx, &rows, "SELECT * FROM environment_resources WHERE workspace_id=? ORDER BY id", workspaceID)
	return rows, err
}

// ReleaseWorkspaceResources marks every live resource of a workspace released
// (the driver kills the actual process; this frees the port for reallocation).
func (d *DB) ReleaseWorkspaceResources(ctx context.Context, workspaceID string) error {
	_, err := d.ExecContext(ctx,
		"UPDATE environment_resources SET status='released', updated_at=CURRENT_TIMESTAMP WHERE workspace_id=? AND status != 'released'",
		workspaceID)
	return err
}

// PickIdleWorkspace returns one ready, unleased workspace for a project (the
// pool) or nil when none is idle — the isolation:auto lease target.
func (d *DB) PickIdleWorkspace(ctx context.Context, projectID int64) (*Workspace, error) {
	var ws Workspace
	err := d.GetContext(ctx, &ws,
		`SELECT * FROM workspaces WHERE project_id=? AND status='ready' AND (leased_by_session_id IS NULL OR leased_by_session_id='')
		 ORDER BY updated_at ASC LIMIT 1`, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

// ProjectMainPathBusy reports whether the project has a live session running on
// its MAIN checkout (no workspace) — the signal isolation:auto uses to decide it
// must lease a sandbox instead of colliding on the main path.
func (d *DB) ProjectMainPathBusy(ctx context.Context, projectID int64) (bool, error) {
	var n int
	err := d.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM sessions WHERE project_id=? AND status IN ('starting','running')
		 AND (workspace_id IS NULL OR workspace_id='')`, projectID)
	return n > 0, err
}

// SessionWriteFootprint lists the distinct paths a session has WRITTEN, as
// recorded by the conflict radar's ledger. Used before moving a session to a new
// working tree: whatever it wrote and has not committed would be left behind.
func (d *DB) SessionWriteFootprint(ctx context.Context, sessionID string) ([]string, error) {
	var paths []string
	err := d.Reader().SelectContext(ctx, &paths,
		`SELECT DISTINCT path FROM session_file_activity WHERE session_id = ? AND kind = 'write' ORDER BY path`,
		sessionID)
	return paths, err
}

// SetWorkspaceResources snapshots the rendered environment (ports/env) and the
// approved manifest hash onto the workspace row, so a reopened session inherits
// identical coordinates.
func (d *DB) SetWorkspaceResources(ctx context.Context, workspaceID, resourcesJSON, manifestSHA string) error {
	_, err := d.ExecContext(ctx,
		"UPDATE workspaces SET resources_json=?, manifest_sha256=?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
		nullStr(resourcesJSON), nullStr(manifestSHA), workspaceID)
	return err
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func driverOrNone(d string) string {
	if d == "" {
		return "none"
	}
	return d
}

func statusOrAlloc(s string) string {
	if s == "" {
		return "allocating"
	}
	return s
}

// probeFreePort confirms nothing is currently listening on the port by trying to
// bind it (then releasing) — mirrors availableLoopbackPort in providerbridge.
func probeFreePort(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
