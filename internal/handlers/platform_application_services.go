package handlers

import (
	"context"
	"errors"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/updater"
)

// tunnelMutationAdapter exposes the current tunnel and pairing managers to the
// application layer without routing through HTTP or returning stored secrets.
type tunnelMutationAdapter struct {
	api *API
}

func NewTunnelMutationAdapter(api *API) application.TunnelMutationPort {
	return tunnelMutationAdapter{api: api}
}

func (a tunnelMutationAdapter) EnableTunnel(ctx context.Context) (application.TunnelMutationResult, error) {
	if a.api == nil {
		return application.TunnelMutationResult{}, errors.New("tunnel API is unavailable")
	}
	a.api.tunnelMu.Lock()
	defer a.api.tunnelMu.Unlock()

	select {
	case <-ctx.Done():
		return application.TunnelMutationResult{}, ctx.Err()
	default:
	}
	if a.api.tunnelClient != nil && a.api.tunnelClient.Status() == "connected" {
		return application.TunnelMutationResult{
			Status: "already_connected", PublicURL: a.api.tunnelClient.PublicURL(),
		}, nil
	}
	if a.api.tunnelClient != nil {
		a.api.tunnelClient.Disconnect()
		a.api.tunnelClient = nil
	}
	client, err := a.api.createAndConnectTunnel(ctx)
	if err != nil {
		return application.TunnelMutationResult{}, err
	}
	if err := a.api.db.SetSetting(ctx, "tunnel_enabled", "true"); err != nil {
		client.Disconnect()
		return application.TunnelMutationResult{}, err
	}
	a.api.tunnelClient = client
	return application.TunnelMutationResult{Status: "connecting", PublicURL: client.PublicURL()}, nil
}

func (a tunnelMutationAdapter) DisableTunnel(ctx context.Context) (application.TunnelMutationResult, error) {
	if a.api == nil {
		return application.TunnelMutationResult{}, errors.New("tunnel API is unavailable")
	}
	a.api.tunnelMu.Lock()
	defer a.api.tunnelMu.Unlock()

	if err := a.api.db.SetSetting(ctx, "tunnel_enabled", "false"); err != nil {
		return application.TunnelMutationResult{}, err
	}
	if a.api.tunnelClient != nil {
		a.api.tunnelClient.Disconnect()
		a.api.tunnelClient = nil
	}
	return application.TunnelMutationResult{Status: "disconnected"}, nil
}

func (a tunnelMutationAdapter) ConfirmPairing(ctx context.Context, code string) (string, error) {
	if a.api == nil || a.api.pairingMgr == nil {
		return "", errors.New("pairing manager is unavailable")
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	return a.api.pairingMgr.ConfirmPairing(code)
}

func (a tunnelMutationAdapter) RevokePairedDevice(ctx context.Context, deviceID string) (bool, error) {
	if a.api == nil || a.api.db == nil {
		return false, errors.New("paired-device store is unavailable")
	}
	device, err := a.api.db.GetPairedDevice(ctx, deviceID)
	if err != nil {
		return false, err
	}
	if device.Revoked {
		return false, nil
	}
	if err := a.api.db.RevokePairedDevice(ctx, deviceID); err != nil {
		return false, err
	}
	return true, nil
}

func (a tunnelMutationAdapter) DeletePairedDevice(ctx context.Context, deviceID string) (bool, error) {
	if a.api == nil || a.api.db == nil {
		return false, errors.New("paired-device store is unavailable")
	}
	if _, err := a.api.db.GetPairedDevice(ctx, deviceID); err != nil {
		return false, err
	}
	if err := a.api.db.DeletePairedDevice(ctx, deviceID); err != nil {
		return false, err
	}
	return true, nil
}

func (a *API) ActiveSessionCount() int {
	if a == nil || a.sessionMgr == nil {
		return 0
	}
	return len(a.sessionMgr.ListRunningSessions())
}

func (a *API) PublishTunnelMutation(_ context.Context, change application.TunnelMutationChange) {
	if a == nil || a.hub == nil {
		return
	}
	if change.Action == "enabled" || change.Action == "disabled" {
		a.hub.BroadcastStateUpdate("tunnel", map[string]string{
			"action": change.Action, "status": change.Status,
		})
		return
	}
	// Device IDs are identifiers, not credentials, but the broadcast only needs
	// to request a refresh and therefore exposes no device record fields.
	a.hub.BroadcastStateUpdate("tunnel_devices", nil)
}

func (a *API) PublishUpdateMutation(_ context.Context, change application.UpdateMutationChange) {
	if a == nil || change.Action != "applied" {
		return
	}
	if a.hub != nil {
		a.hub.BroadcastStateUpdate("update", map[string]interface{}{
			"action": "restarting", "version": change.Version,
		})
	}
	time.AfterFunc(time.Second, func() {
		_ = updater.RestartSelf()
	})
}

var (
	_ application.TunnelMutationPort    = tunnelMutationAdapter{}
	_ application.ActiveSessionCounter  = (*API)(nil)
	_ application.TunnelMutationEffects = (*API)(nil)
	_ application.UpdateMutationEffects = (*API)(nil)
	_ application.BinaryUpdateManager   = (*updater.Updater)(nil)
)
