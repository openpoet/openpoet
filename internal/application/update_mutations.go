package application

import (
	"context"
	"strings"

	"openpoet/internal/updater"
)

type BinaryUpdateManager interface {
	DetectPackageManager() string
	CheckForUpdate(context.Context) (*updater.UpdateStatus, error)
	DownloadAndApply(context.Context, *updater.UpdateStatus) error
}

type ActiveSessionCounter interface {
	ActiveSessionCount() int
}

type UpdateApplyResult struct {
	Status  string
	Version string
	Forced  bool
}

type UpdateMutationChange struct {
	Action  string
	Version string
	Forced  bool
	Actor   Actor
}

type UpdateMutationEffects interface {
	PublishUpdateMutation(context.Context, UpdateMutationChange)
}

// ForceUpdateAuthorization is intentionally separate from the approval that
// authorizes update.apply. It acknowledges interruption of active sessions.
type ForceUpdateAuthorization struct {
	Authorization              ActionAuthorization
	AcknowledgesActiveSessions bool
}

type UpdateMutationService struct {
	manager  BinaryUpdateManager
	sessions ActiveSessionCounter
	effects  UpdateMutationEffects
}

func NewUpdateMutationService(manager BinaryUpdateManager, sessions ActiveSessionCounter, effects UpdateMutationEffects) *UpdateMutationService {
	return &UpdateMutationService{manager: manager, sessions: sessions, effects: effects}
}

type ApplyUpdateCommand struct {
	Force              bool
	Authorization      ActionAuthorization
	ForceAuthorization *ForceUpdateAuthorization
}

func (s *UpdateMutationService) Apply(ctx context.Context, command ApplyUpdateCommand) (UpdateApplyResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return UpdateApplyResult{}, err
	}
	if s.manager == nil {
		return UpdateApplyResult{}, validationError("updater_unavailable", "Updater is unavailable")
	}
	if packageManager := strings.TrimSpace(s.manager.DetectPackageManager()); packageManager != "" {
		return UpdateApplyResult{}, conflictError("managed_install", "Binary is managed by "+packageManager)
	}

	activeSessions := 0
	if s.sessions != nil {
		activeSessions = s.sessions.ActiveSessionCount()
	}
	if command.Force {
		if err := validateForceUpdateAuthorization(command.Authorization, command.ForceAuthorization); err != nil {
			return UpdateApplyResult{}, err
		}
	} else if activeSessions > 0 {
		return UpdateApplyResult{}, conflictError("active_sessions", "Active sessions must be stopped or force must be separately authorized")
	}

	status, err := s.manager.CheckForUpdate(ctx)
	if err != nil {
		return UpdateApplyResult{}, err
	}
	if status == nil {
		return UpdateApplyResult{}, validationError("update_status_invalid", "Updater returned no status")
	}
	if !status.Available {
		currentVersion := strings.TrimSpace(status.CurrentVersion)
		if len(currentVersion) > 100 {
			return UpdateApplyResult{}, validationError("update_version_invalid", "Current update version is too large")
		}
		return UpdateApplyResult{Status: "already_up_to_date", Version: currentVersion, Forced: command.Force}, nil
	}
	version := strings.TrimSpace(status.LatestVersion)
	if version == "" || len(version) > 100 {
		return UpdateApplyResult{}, validationError("update_version_invalid", "Update version is invalid or too large")
	}
	if err := s.manager.DownloadAndApply(ctx, status); err != nil {
		return UpdateApplyResult{}, err
	}
	result := UpdateApplyResult{Status: "applied", Version: version, Forced: command.Force}
	if s.effects != nil {
		s.effects.PublishUpdateMutation(ctx, UpdateMutationChange{
			Action: "applied", Version: version, Forced: command.Force, Actor: command.Authorization.Actor,
		})
	}
	return result, nil
}

func validateForceUpdateAuthorization(primary ActionAuthorization, force *ForceUpdateAuthorization) error {
	if force == nil || !force.AcknowledgesActiveSessions {
		return validationError("force_update_approval_required", "Force update requires a distinct active-session acknowledgement")
	}
	if err := requireExplicitActionApproval(force.Authorization); err != nil {
		return validationError("force_update_approval_required", "Force update requires a distinct explicit approval")
	}
	if strings.TrimSpace(force.Authorization.Actor.Type) != strings.TrimSpace(primary.Actor.Type) ||
		strings.TrimSpace(force.Authorization.Actor.ID) != strings.TrimSpace(primary.Actor.ID) {
		return validationError("force_update_actor_mismatch", "Force update approval must belong to the same actor")
	}
	return nil
}
