package application

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"openpoet/internal/updater"
)

func phase5Approval() ActionAuthorization {
	return ActionAuthorization{
		Actor: Actor{Type: "agent", ID: "helena"}, Approved: true,
		ApprovedBy: "presidente", Reason: "approved platform mutation",
	}
}

type phase5TunnelPort struct {
	enableResult  TunnelMutationResult
	disableResult TunnelMutationResult
	err           error
	enableCalls   int
	disableCalls  int
	pairCalls     int
	revokeIDs     []string
	deleteIDs     []string
	usedCode      bool
	revokeChanged bool
	deleteChanged bool
}

func (p *phase5TunnelPort) EnableTunnel(context.Context) (TunnelMutationResult, error) {
	p.enableCalls++
	return p.enableResult, p.err
}

func (p *phase5TunnelPort) DisableTunnel(context.Context) (TunnelMutationResult, error) {
	p.disableCalls++
	return p.disableResult, p.err
}

func (p *phase5TunnelPort) ConfirmPairing(_ context.Context, code string) (string, error) {
	p.pairCalls++
	if p.err != nil {
		return "", p.err
	}
	if code != "123456" || p.usedCode {
		return "", errors.New("invalid or already used pairing code")
	}
	p.usedCode = true
	return "device-1", nil
}

func (p *phase5TunnelPort) RevokePairedDevice(_ context.Context, id string) (bool, error) {
	p.revokeIDs = append(p.revokeIDs, id)
	return p.revokeChanged, p.err
}

func (p *phase5TunnelPort) DeletePairedDevice(_ context.Context, id string) (bool, error) {
	p.deleteIDs = append(p.deleteIDs, id)
	return p.deleteChanged, p.err
}

type phase5TunnelEffects struct{ changes []TunnelMutationChange }

func (e *phase5TunnelEffects) PublishTunnelMutation(_ context.Context, change TunnelMutationChange) {
	e.changes = append(e.changes, change)
}

func TestTunnelMutationsRequireR4AndPublishOnlyAfterSuccess(t *testing.T) {
	backendErr := errors.New("relay unavailable")
	port := &phase5TunnelPort{
		enableResult:  TunnelMutationResult{Status: "connecting", PublicURL: "https://relay-token@public.example/path?credential=secret#token", DeviceID: "must-not-leak"},
		disableResult: TunnelMutationResult{Status: "disconnected"},
	}
	effects := &phase5TunnelEffects{}
	service := NewTunnelMutationService(port, effects)

	if _, err := service.Enable(context.Background(), EnableTunnelCommand{
		Authorization: ActionAuthorization{Actor: Actor{Type: "agent", ID: "helena"}},
	}); !ErrorIsKind(err, ErrorValidation) || port.enableCalls != 0 {
		t.Fatalf("unapproved enable reached backend: err=%v calls=%d", err, port.enableCalls)
	}

	port.err = backendErr
	if _, err := service.Enable(context.Background(), EnableTunnelCommand{Authorization: phase5Approval()}); !errors.Is(err, backendErr) {
		t.Fatalf("expected backend error, got %v", err)
	}
	if len(effects.changes) != 0 {
		t.Fatalf("failed enable published effects: %#v", effects.changes)
	}

	port.err = nil
	result, err := service.Enable(context.Background(), EnableTunnelCommand{Authorization: phase5Approval()})
	if err != nil || result.Status != "connecting" || result.PublicURL != "https://public.example/path" || result.DeviceID != "" {
		t.Fatalf("unexpected enable result: %#v err=%v", result, err)
	}
	if _, err := service.Disable(context.Background(), DisableTunnelCommand{Authorization: phase5Approval()}); err != nil {
		t.Fatal(err)
	}
	if got := []string{effects.changes[0].Action, effects.changes[1].Action}; !reflect.DeepEqual(got, []string{"enabled", "disabled"}) {
		t.Fatalf("unexpected state transitions: %v", got)
	}
}

func TestTunnelPairingCodeIsBoundedAndBackendSingleUse(t *testing.T) {
	port := &phase5TunnelPort{}
	effects := &phase5TunnelEffects{}
	service := NewTunnelMutationService(port, effects)

	for _, code := range []string{"12345", "1234567", "12345a", "１２３４５６", " 123456 "} {
		if _, err := service.ConfirmPairing(context.Background(), ConfirmTunnelPairingCommand{
			Code: code, Authorization: phase5Approval(),
		}); !ErrorIsKind(err, ErrorValidation) {
			t.Fatalf("pairing code %q must be rejected, got %v", code, err)
		}
	}
	if port.pairCalls != 0 {
		t.Fatalf("invalid pairing code reached backend %d times", port.pairCalls)
	}

	result, err := service.ConfirmPairing(context.Background(), ConfirmTunnelPairingCommand{
		Code: "123456", Authorization: phase5Approval(),
	})
	if err != nil || result.Status != "paired" || result.DeviceID != "device-1" {
		t.Fatalf("unexpected pairing result: %#v err=%v", result, err)
	}
	if _, err := service.ConfirmPairing(context.Background(), ConfirmTunnelPairingCommand{
		Code: "123456", Authorization: phase5Approval(),
	}); err == nil {
		t.Fatal("backend single-use pairing code was accepted twice")
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "paired" {
		t.Fatalf("only successful pairing may publish: %#v", effects.changes)
	}
}

func TestTunnelRevokeAndPermanentDeleteRemainDistinct(t *testing.T) {
	port := &phase5TunnelPort{revokeChanged: true, deleteChanged: true}
	effects := &phase5TunnelEffects{}
	service := NewTunnelMutationService(port, effects)

	revoked, err := service.RevokeDevice(context.Background(), RevokeTunnelDeviceCommand{
		DeviceID: "device-1", Authorization: phase5Approval(),
	})
	if err != nil || revoked.Status != "revoked" {
		t.Fatalf("unexpected revoke: %#v err=%v", revoked, err)
	}
	deleted, err := service.DeleteDevice(context.Background(), DeleteTunnelDeviceCommand{
		DeviceID: "device-2", Authorization: phase5Approval(),
	})
	if err != nil || deleted.Status != "deleted" {
		t.Fatalf("unexpected delete: %#v err=%v", deleted, err)
	}
	if !reflect.DeepEqual(port.revokeIDs, []string{"device-1"}) || !reflect.DeepEqual(port.deleteIDs, []string{"device-2"}) {
		t.Fatalf("revoke/delete crossed backend operations: revoke=%v delete=%v", port.revokeIDs, port.deleteIDs)
	}
	if got := []string{effects.changes[0].Action, effects.changes[1].Action}; !reflect.DeepEqual(got, []string{"revoked", "deleted"}) {
		t.Fatalf("revoke/delete effects are not distinct: %v", got)
	}
}

type phase5Updater struct {
	managed    string
	status     *updater.UpdateStatus
	checkErr   error
	applyErr   error
	checkCalls int
	applyCalls int
}

func (u *phase5Updater) DetectPackageManager() string { return u.managed }
func (u *phase5Updater) CheckForUpdate(context.Context) (*updater.UpdateStatus, error) {
	u.checkCalls++
	return u.status, u.checkErr
}
func (u *phase5Updater) DownloadAndApply(context.Context, *updater.UpdateStatus) error {
	u.applyCalls++
	return u.applyErr
}

type phase5Sessions struct{ count int }

func (s phase5Sessions) ActiveSessionCount() int { return s.count }

type phase5UpdateEffects struct{ changes []UpdateMutationChange }

func (e *phase5UpdateEffects) PublishUpdateMutation(_ context.Context, change UpdateMutationChange) {
	e.changes = append(e.changes, change)
}

func phase5ForceApproval() *ForceUpdateAuthorization {
	boundary := phase5Approval()
	boundary.Reason = "force update despite active sessions"
	return &ForceUpdateAuthorization{Authorization: boundary, AcknowledgesActiveSessions: true}
}

func TestUpdateForceRequiresDistinctMatchingAuthorization(t *testing.T) {
	manager := &phase5Updater{status: &updater.UpdateStatus{Available: true, LatestVersion: "2.0.0"}}
	effects := &phase5UpdateEffects{}
	service := NewUpdateMutationService(manager, phase5Sessions{count: 2}, effects)

	if _, err := service.Apply(context.Background(), ApplyUpdateCommand{Authorization: phase5Approval()}); !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("active sessions without force must conflict, got %v", err)
	}
	if _, err := service.Apply(context.Background(), ApplyUpdateCommand{
		Force: true, Authorization: phase5Approval(),
	}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("force without separate approval must fail, got %v", err)
	}
	mismatch := phase5ForceApproval()
	mismatch.Authorization.Actor.ID = "other-agent"
	if _, err := service.Apply(context.Background(), ApplyUpdateCommand{
		Force: true, Authorization: phase5Approval(), ForceAuthorization: mismatch,
	}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("force approval actor mismatch must fail, got %v", err)
	}
	if manager.checkCalls != 0 || manager.applyCalls != 0 {
		t.Fatalf("unauthorized force reached updater: check=%d apply=%d", manager.checkCalls, manager.applyCalls)
	}

	result, err := service.Apply(context.Background(), ApplyUpdateCommand{
		Force: true, Authorization: phase5Approval(), ForceAuthorization: phase5ForceApproval(),
	})
	if err != nil || result.Status != "applied" || !result.Forced || manager.applyCalls != 1 {
		t.Fatalf("authorized force update failed: result=%#v apply=%d err=%v", result, manager.applyCalls, err)
	}
	if len(effects.changes) != 1 || !effects.changes[0].Forced {
		t.Fatalf("forced apply effect missing: %#v", effects.changes)
	}
}

func TestUpdateFailureDoesNotPublishEffect(t *testing.T) {
	applyErr := errors.New("checksum mismatch")
	manager := &phase5Updater{
		status: &updater.UpdateStatus{Available: true, LatestVersion: "2.0.0"}, applyErr: applyErr,
	}
	effects := &phase5UpdateEffects{}
	service := NewUpdateMutationService(manager, phase5Sessions{}, effects)

	if _, err := service.Apply(context.Background(), ApplyUpdateCommand{Authorization: phase5Approval()}); !errors.Is(err, applyErr) {
		t.Fatalf("expected apply failure, got %v", err)
	}
	if len(effects.changes) != 0 {
		t.Fatalf("failed update published effect: %#v", effects.changes)
	}

	manager.applyErr = nil
	manager.status = &updater.UpdateStatus{Available: false, CurrentVersion: "1.0.0"}
	result, err := service.Apply(context.Background(), ApplyUpdateCommand{Authorization: phase5Approval()})
	if err != nil || result.Status != "already_up_to_date" || manager.applyCalls != 1 || len(effects.changes) != 0 {
		t.Fatalf("no-update result must not apply/publish: result=%#v apply=%d effects=%v err=%v", result, manager.applyCalls, effects.changes, err)
	}
}

func TestManagedUpdateStopsBeforeNetworkCheck(t *testing.T) {
	manager := &phase5Updater{managed: "homebrew", status: &updater.UpdateStatus{Available: true, LatestVersion: "2.0.0"}}
	service := NewUpdateMutationService(manager, phase5Sessions{}, nil)
	if _, err := service.Apply(context.Background(), ApplyUpdateCommand{Authorization: phase5Approval()}); !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("managed install must conflict, got %v", err)
	}
	if manager.checkCalls != 0 || manager.applyCalls != 0 {
		t.Fatalf("managed install reached network/apply boundary: check=%d apply=%d", manager.checkCalls, manager.applyCalls)
	}
}
