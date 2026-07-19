package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"openpoet/internal/database"
	"openpoet/internal/workspace"
)

// EnvironmentService is the automation-plane surface for environment manifest
// approval (Phase 6). Approving a manifest records its content SHA-256 so the
// provisioner will execute its setup/service commands — the one place repo
// content becomes server-run commands, so it is deliberately unsafe/explicit and
// automation-plane-only.
type EnvironmentService struct {
	db *database.DB
}

func NewEnvironmentService(db *database.DB) *EnvironmentService {
	return &EnvironmentService{db: db}
}

func (s *EnvironmentService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("environments")
}

// ApproveManifest verifies the provided hash matches the project's CURRENT
// on-disk manifest (you cannot approve a hash that does not exist) and records it
// as approved. A drifted manifest (different bytes) re-requires approval.
func (s *EnvironmentService) ApproveManifest(ctx context.Context, projectID int64, providedSHA, approvedBy string) (map[string]any, error) {
	project, err := s.db.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return nil, validationError("project_not_found", "the project does not exist")
	}
	b, err := os.ReadFile(filepath.Join(project.Path, ".openpoet", "environment.yaml"))
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return nil, validationError("manifest_not_found", "the project ships no .openpoet/environment.yaml")
	}
	sha := workspace.ManifestSHA256(b)
	if strings.TrimSpace(providedSHA) != "" && !strings.EqualFold(strings.TrimSpace(providedSHA), sha) {
		return nil, validationError("manifest_sha_mismatch", "the provided content_sha256 does not match the current manifest")
	}
	if err := s.db.ApproveManifest(ctx, projectID, ".openpoet/environment.yaml", string(b), sha, approvedBy); err != nil {
		return nil, err
	}
	return map[string]any{"approved": true, "project_id": projectID, "content_sha256": sha}, nil
}
