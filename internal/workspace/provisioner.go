package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"openpoet/internal/database"
)

// ProvisionStore is the DB surface the provisioner needs (*database.DB satisfies it).
type ProvisionStore interface {
	IsManifestApproved(ctx context.Context, projectID int64, sha string) (bool, error)
	AllocatePort(ctx context.Context, projectID int64, workspaceID, name string) (int, error)
	AllocateSpecificPort(ctx context.Context, projectID int64, workspaceID, name string, port int) error
	InsertServiceResource(ctx context.Context, projectID int64, workspaceID, name, driver, pid, status string) (int64, error)
	SetResourceStatus(ctx context.Context, id int64, status string) error
	ReleaseWorkspaceResources(ctx context.Context, workspaceID string) error
	ListWorkspaceResources(ctx context.Context, workspaceID string) ([]database.EnvironmentResource, error)
}

// ApprovalRequiredError is returned when a manifest's content hash is not
// approved. It carries the SHA so the caller can surface it for approval.
type ApprovalRequiredError struct{ SHA string }

func (e *ApprovalRequiredError) Error() string {
	return "manifest_approval_required: content_sha256=" + e.SHA
}

// ProvisionResult is the rendered environment a session inherits.
type ProvisionResult struct {
	Port int               `json:"port"`
	Env  map[string]string `json:"env"`
}

// Provisioner runs manifest environments and tracks live service drivers so they
// can be torn down.
type Provisioner struct {
	store       ProvisionStore
	setupBudget time.Duration
	mu          sync.Mutex
	drivers     map[string]*ProcessDriver // workspaceID → live process driver
}

func NewProvisioner(store ProvisionStore) *Provisioner {
	return &Provisioner{store: store, setupBudget: 5 * time.Minute, drivers: map[string]*ProcessDriver{}}
}

// Provision executes an approved manifest for a workspace: setup commands, port
// allocation, per-sandbox service spawn, and health check. Returns the rendered
// env (for resources_json). On an unapproved hash it returns *ApprovalRequiredError
// (nothing is executed). A failed setup or health check returns an error with no
// orphan process left running.
func (p *Provisioner) Provision(ctx context.Context, projectID int64, workspaceID, workDir string, manifestContent []byte) (*ProvisionResult, error) {
	sha := ManifestSHA256(manifestContent)
	approved, err := p.store.IsManifestApproved(ctx, projectID, sha)
	if err != nil {
		return nil, err
	}
	if !approved {
		return nil, &ApprovalRequiredError{SHA: sha}
	}
	m, err := ParseManifest(manifestContent)
	if err != nil {
		return nil, err
	}

	// Setup commands run FIRST, in the workspace cwd with a scrubbed env. Any
	// non-zero exit aborts provisioning before a service is ever spawned.
	for _, c := range m.Setup {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if err := p.runSetup(ctx, c, workDir); err != nil {
			return nil, fmt.Errorf("setup %q failed: %w", c, err)
		}
	}

	result := &ProvisionResult{Env: map[string]string{}}
	for name, svc := range m.Services {
		if strings.EqualFold(svc.Scope, "shared") {
			continue // shared services are declare-only in the MVP (driver=none)
		}
		port, allocErr := p.allocatePort(ctx, projectID, workspaceID, name, svc.Port)
		if allocErr != nil {
			return nil, allocErr // port_reserved / no free port bubbles up
		}
		env := m.RenderedEnv(port)
		driver := &ProcessDriver{}
		pid, spErr := driver.Spawn(RenderTemplate(svc.Command, port), workDir, env)
		if spErr != nil {
			return nil, fmt.Errorf("service %q spawn failed: %w", name, spErr)
		}
		p.mu.Lock()
		p.drivers[workspaceID] = driver
		p.mu.Unlock()
		svcID, _ := p.store.InsertServiceResource(ctx, projectID, workspaceID, name, "process", strconv.Itoa(pid), "allocating")

		if !WaitHealthy(RenderTemplate(svc.Health, port), 30*time.Second) {
			KillPID(pid)
			driver.Kill()
			_ = p.store.SetResourceStatus(ctx, svcID, "unhealthy")
			return nil, fmt.Errorf("service %q failed its health check", name)
		}
		_ = p.store.SetResourceStatus(ctx, svcID, "ready")
		result.Port = port
		for k, v := range env {
			result.Env[k] = v
		}
	}
	return result, nil
}

func (p *Provisioner) allocatePort(ctx context.Context, projectID int64, workspaceID, name, portField string) (int, error) {
	if PortIsTemplated(portField) {
		return p.store.AllocatePort(ctx, projectID, workspaceID, name)
	}
	lit, err := strconv.Atoi(strings.TrimSpace(portField))
	if err != nil || lit <= 0 {
		return 0, fmt.Errorf("manifest: service %q has an invalid port %q", name, portField)
	}
	if err := p.store.AllocateSpecificPort(ctx, projectID, workspaceID, name, lit); err != nil {
		return 0, err // port_reserved / port_in_use
	}
	return lit, nil
}

// runSetup runs one setup command to completion in the workspace cwd.
func (p *Provisioner) runSetup(ctx context.Context, command, workDir string) error {
	ctx, cancel := context.WithTimeout(ctx, p.setupBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	cmd.Env = scrubbedEnv()
	return cmd.Run()
}

// Teardown kills a workspace's live services (by live driver, or by the persisted
// pid after a restart) and marks its resources released, freeing the port.
func (p *Provisioner) Teardown(ctx context.Context, workspaceID string) error {
	p.mu.Lock()
	driver := p.drivers[workspaceID]
	delete(p.drivers, workspaceID)
	p.mu.Unlock()
	if driver != nil {
		driver.Kill()
	}
	// Also kill by persisted pid (covers restart / driver-less teardown).
	if rows, err := p.store.ListWorkspaceResources(ctx, workspaceID); err == nil {
		for _, r := range rows {
			if r.Kind == "service" && r.RuntimeRef != "" {
				if pid, e := strconv.Atoi(r.RuntimeRef); e == nil {
					KillPID(pid)
				}
			}
		}
	}
	return p.store.ReleaseWorkspaceResources(ctx, workspaceID)
}
