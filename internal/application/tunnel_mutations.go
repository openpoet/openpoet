package application

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

var (
	pairingCodePattern = regexp.MustCompile(`^[0-9]{6}$`)
	deviceIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,199}$`)
)

// TunnelMutationResult is deliberately redacted. Relay credentials, session
// tokens, encryption keys and paired-device secrets never cross this boundary.
type TunnelMutationResult struct {
	Status    string
	PublicURL string
	DeviceID  string
}

type TunnelMutationPort interface {
	EnableTunnel(context.Context) (TunnelMutationResult, error)
	DisableTunnel(context.Context) (TunnelMutationResult, error)
	ConfirmPairing(context.Context, string) (deviceID string, err error)
	RevokePairedDevice(context.Context, string) (changed bool, err error)
	DeletePairedDevice(context.Context, string) (deleted bool, err error)
}

type TunnelMutationChange struct {
	Action   string
	Status   string
	DeviceID string
	Actor    Actor
}

type TunnelMutationEffects interface {
	PublishTunnelMutation(context.Context, TunnelMutationChange)
}

type TunnelMutationService struct {
	manager TunnelMutationPort
	effects TunnelMutationEffects
}

func NewTunnelMutationService(manager TunnelMutationPort, effects TunnelMutationEffects) *TunnelMutationService {
	return &TunnelMutationService{manager: manager, effects: effects}
}

type EnableTunnelCommand struct {
	Authorization ActionAuthorization
}

func (s *TunnelMutationService) Enable(ctx context.Context, command EnableTunnelCommand) (TunnelMutationResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return TunnelMutationResult{}, err
	}
	if s.manager == nil {
		return TunnelMutationResult{}, validationError("tunnel_manager_unavailable", "Tunnel manager is unavailable")
	}
	result, err := s.manager.EnableTunnel(ctx)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	result = redactTunnelResult(result)
	s.publish(ctx, TunnelMutationChange{
		Action: "enabled", Status: result.Status, Actor: command.Authorization.Actor,
	})
	return result, nil
}

type DisableTunnelCommand struct {
	Authorization ActionAuthorization
}

func (s *TunnelMutationService) Disable(ctx context.Context, command DisableTunnelCommand) (TunnelMutationResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return TunnelMutationResult{}, err
	}
	if s.manager == nil {
		return TunnelMutationResult{}, validationError("tunnel_manager_unavailable", "Tunnel manager is unavailable")
	}
	result, err := s.manager.DisableTunnel(ctx)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	result = redactTunnelResult(result)
	s.publish(ctx, TunnelMutationChange{
		Action: "disabled", Status: result.Status, Actor: command.Authorization.Actor,
	})
	return result, nil
}

type ConfirmTunnelPairingCommand struct {
	Code          string
	Authorization ActionAuthorization
}

func (s *TunnelMutationService) ConfirmPairing(ctx context.Context, command ConfirmTunnelPairingCommand) (TunnelMutationResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return TunnelMutationResult{}, err
	}
	code := command.Code
	if !pairingCodePattern.MatchString(code) {
		return TunnelMutationResult{}, validationError("pairing_code_invalid", "Pairing code must contain exactly 6 digits")
	}
	if s.manager == nil {
		return TunnelMutationResult{}, validationError("tunnel_manager_unavailable", "Tunnel manager is unavailable")
	}
	deviceID, err := s.manager.ConfirmPairing(ctx, code)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	deviceID, err = validateTunnelDeviceID(deviceID)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	result := TunnelMutationResult{Status: "paired", DeviceID: deviceID}
	s.publish(ctx, TunnelMutationChange{
		Action: "paired", Status: result.Status, DeviceID: deviceID, Actor: command.Authorization.Actor,
	})
	return result, nil
}

type RevokeTunnelDeviceCommand struct {
	DeviceID      string
	Authorization ActionAuthorization
}

func (s *TunnelMutationService) RevokeDevice(ctx context.Context, command RevokeTunnelDeviceCommand) (TunnelMutationResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return TunnelMutationResult{}, err
	}
	deviceID, err := validateTunnelDeviceID(command.DeviceID)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	if s.manager == nil {
		return TunnelMutationResult{}, validationError("tunnel_manager_unavailable", "Tunnel manager is unavailable")
	}
	changed, err := s.manager.RevokePairedDevice(ctx, deviceID)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	status := "already_revoked"
	if changed {
		status = "revoked"
	}
	result := TunnelMutationResult{Status: status, DeviceID: deviceID}
	s.publish(ctx, TunnelMutationChange{
		Action: "revoked", Status: status, DeviceID: deviceID, Actor: command.Authorization.Actor,
	})
	return result, nil
}

type DeleteTunnelDeviceCommand struct {
	DeviceID      string
	Authorization ActionAuthorization
}

func (s *TunnelMutationService) DeleteDevice(ctx context.Context, command DeleteTunnelDeviceCommand) (TunnelMutationResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return TunnelMutationResult{}, err
	}
	deviceID, err := validateTunnelDeviceID(command.DeviceID)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	if s.manager == nil {
		return TunnelMutationResult{}, validationError("tunnel_manager_unavailable", "Tunnel manager is unavailable")
	}
	deleted, err := s.manager.DeletePairedDevice(ctx, deviceID)
	if err != nil {
		return TunnelMutationResult{}, err
	}
	if !deleted {
		return TunnelMutationResult{}, notFoundError("paired_device_not_found", "Paired device not found", nil)
	}
	result := TunnelMutationResult{Status: "deleted", DeviceID: deviceID}
	s.publish(ctx, TunnelMutationChange{
		Action: "deleted", Status: result.Status, DeviceID: deviceID, Actor: command.Authorization.Actor,
	})
	return result, nil
}

func validateTunnelDeviceID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !deviceIDPattern.MatchString(value) {
		return "", validationError("paired_device_id_invalid", "Paired device ID is invalid or too large")
	}
	return value, nil
}

func redactTunnelResult(result TunnelMutationResult) TunnelMutationResult {
	// Copy only the explicit public fields, so future backend result extensions
	// cannot accidentally surface a credential through this service.
	return TunnelMutationResult{
		Status: strings.TrimSpace(result.Status), PublicURL: sanitizePublicTunnelURL(result.PublicURL),
	}
}

func sanitizePublicTunnelURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	// Userinfo, query and fragment may contain tokens. They are never necessary
	// for the public tunnel URL exposed by this boundary.
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func (s *TunnelMutationService) publish(ctx context.Context, change TunnelMutationChange) {
	if s.effects != nil {
		s.effects.PublishTunnelMutation(ctx, change)
	}
}
