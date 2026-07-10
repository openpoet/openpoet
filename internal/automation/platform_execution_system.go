package automation

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/updater"
)

func hookExecutionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionDestructiveCapability("hooks.respond_permission", "hook_responses", "hooks:respond", "sessions:write"),
		executionWriteCapability("hooks.respond_task_notification", "hook_responses", "hooks:respond", "sessions:write"),
	}
}

type hookExecutionPlatformExecutor struct {
	service *application.HookResponseService
}

type hookPermissionPayload struct {
	Behavior              string            `json:"behavior"`
	Message               string            `json:"message,omitempty"`
	ToolName              string            `json:"tool_name,omitempty"`
	Answers               map[string]string `json:"answers,omitempty"`
	PermissionSuggestions []any             `json:"permission_suggestions,omitempty"`
}

func (e *hookExecutionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	sessionID, err := executionStringID(target, "session id")
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "hooks.respond_permission":
		var payload hookPermissionPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		behavior := strings.TrimSpace(payload.Behavior)
		if behavior != "allow" && behavior != "allowAlways" && behavior != "deny" && behavior != "passthrough" {
			return nil, platformFailure("platform_payload_invalid", "permission behavior is invalid", false)
		}
		if utf8.RuneCountInString(payload.Message) > 4000 || utf8.RuneCountInString(payload.ToolName) > 4000 || len(payload.Answers) > 20 {
			return nil, platformFailure("platform_payload_invalid", "permission response is too large", false)
		}
		for question, answer := range payload.Answers {
			if strings.TrimSpace(question) == "" || utf8.RuneCountInString(question) > 4000 || utf8.RuneCountInString(answer) > 4000 {
				return nil, platformFailure("platform_payload_invalid", "permission answers are invalid or too large", false)
			}
		}
		encoded, encodeErr := json.Marshal(payload.PermissionSuggestions)
		if encodeErr != nil || len(encoded) > 64<<10 {
			return nil, platformFailure("platform_payload_invalid", "permission suggestions exceed 64 KiB", false)
		}
		response := application.HookPermissionResponse{
			Behavior: behavior, Message: payload.Message, ToolName: payload.ToolName,
			Answers: payload.Answers, PermissionSuggestions: payload.PermissionSuggestions,
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{
			"session_id": sessionID, "behavior": behavior, "answer_count": len(payload.Answers),
			"suggestion_count": len(payload.PermissionSuggestions),
		}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			err := e.service.RespondPermission(ctx, application.RespondPermissionCommand{SessionID: sessionID, Response: response, Authorization: authorization})
			return map[string]any{"responded": err == nil, "session_id": sessionID, "behavior": behavior}, err
		}}, nil
	case "hooks.respond_task_notification":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			err := e.service.RespondTaskNotification(ctx, application.RespondTaskNotificationCommand{SessionID: sessionID, Authorization: authorization})
			return map[string]any{"responded": err == nil, "session_id": sessionID}, err
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the hook response capability handler is unsupported", false)
	}
}

func voiceExecutionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionPayloadLimit(platformMutation(executionReadCapability("voice.transcribe", "voice_transcription", "voice:use")), 48<<20),
	}
}

type voiceExecutionPlatformExecutor struct {
	service *application.VoiceTranscriptionService
}

type voiceTranscriptionPayload struct {
	Audio    string `json:"audio_base64"`
	Filename string `json:"filename,omitempty"`
	Language string `json:"language,omitempty"`
}

type voiceTranscriptionAutomationView struct {
	Text      string  `json:"text"`
	Duration  float64 `json:"duration,omitempty"`
	Truncated bool    `json:"truncated,omitempty"`
	Redacted  bool    `json:"redacted"`
}

func (e *voiceExecutionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	if input.Handler != "voice.transcribe" {
		return nil, platformFailure("platform_handler_unsupported", "the voice capability handler is unsupported", false)
	}
	if _, err := decodeExecutionTarget(input.Target); err != nil {
		return nil, err
	}
	var payload voiceTranscriptionPayload
	if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
		return nil, err
	}
	audio, err := decodeExecutionBase64(payload.Audio, 32<<20, "voice audio")
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(payload.Filename) > 255 || utf8.RuneCountInString(payload.Language) > 50 {
		return nil, platformFailure("platform_payload_invalid", "voice metadata is too large", false)
	}
	return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"audio_bytes": len(audio), "has_filename": strings.TrimSpace(payload.Filename) != "", "has_language": strings.TrimSpace(payload.Language) != ""}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
		result, err := e.service.Transcribe(ctx, application.TranscribeVoiceCommand{Audio: audio, Filename: payload.Filename, Language: payload.Language, Authorization: authorization})
		if err != nil {
			return nil, err
		}
		text, truncated := boundedExecutionText(result.Text, maxExecutionTranscript)
		return voiceTranscriptionAutomationView{Text: text, Duration: result.Duration, Truncated: truncated, Redacted: true}, nil
	}}, nil
}

type TunnelStatusAutomationView struct {
	Status      string `json:"status"`
	PublicURL   string `json:"public_url,omitempty"`
	DeviceCount int    `json:"device_count"`
	HasToken    bool   `json:"has_token"`
}

type TunnelDeviceAutomationView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	UserAgent  string    `json:"user_agent,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Revoked    bool      `json:"revoked"`
}

type TunnelOperationalReadPort interface {
	OperationalTunnelStatus(context.Context) (TunnelStatusAutomationView, error)
	ListPairedDevices(context.Context) ([]database.PairedDevice, error)
}

func tunnelExecutionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("tunnel.status", "tunnel_operations", "tunnel:read"),
		executionReadCapability("tunnel.devices", "tunnel_operations", "tunnel:read"),
		executionUnsafeCapability("tunnel.enable", "tunnel_mutations", "tunnel:admin", "credentials:use"),
		executionUnsafeCapability("tunnel.disable", "tunnel_mutations", "tunnel:admin"),
		executionUnsafeCapability("tunnel.revoke_device", "tunnel_mutations", "tunnel:admin", "credentials:write"),
		executionUnsafeCapability("tunnel.delete_device", "tunnel_mutations", "tunnel:admin", "credentials:write"),
		executionUnsafeCapability("tunnel.confirm_pairing", "tunnel_mutations", "tunnel:admin", "credentials:write"),
	}
}

type tunnelExecutionPlatformExecutor struct {
	service *application.TunnelMutationService
	reader  TunnelOperationalReadPort
}

type tunnelPairingPayload struct {
	Code string `json:"code"`
}

func (e *tunnelExecutionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "tunnel.status":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			status, err := e.reader.OperationalTunnelStatus(ctx)
			status.Status, _ = boundedExecutionText(status.Status, 100)
			status.PublicURL = sanitizeExecutionPublicURL(status.PublicURL)
			if status.DeviceCount < 0 {
				status.DeviceCount = 0
			}
			return status, err
		}}, nil
	case "tunnel.devices":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			devices, err := e.reader.ListPairedDevices(ctx)
			if err != nil {
				return nil, err
			}
			if len(devices) > maxExecutionResultEntries {
				devices = devices[:maxExecutionResultEntries]
			}
			views := make([]TunnelDeviceAutomationView, len(devices))
			for i, device := range devices {
				views[i] = tunnelDeviceAutomationView(device)
			}
			return views, nil
		}}, nil
	case "tunnel.enable", "tunnel.disable":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, nil), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if input.Handler == "tunnel.enable" {
				return e.service.Enable(ctx, application.EnableTunnelCommand{Authorization: authorization})
			}
			return e.service.Disable(ctx, application.DisableTunnelCommand{Authorization: authorization})
		}}, nil
	case "tunnel.confirm_pairing":
		var payload tunnelPairingPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if len(payload.Code) != 6 {
			return nil, platformFailure("platform_payload_invalid", "pairing code must contain exactly six digits", false)
		}
		for _, char := range payload.Code {
			if char < '0' || char > '9' {
				return nil, platformFailure("platform_payload_invalid", "pairing code must contain exactly six digits", false)
			}
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"has_pairing_code": true}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			return e.service.ConfirmPairing(ctx, application.ConfirmTunnelPairingCommand{Code: payload.Code, Authorization: authorization})
		}}, nil
	case "tunnel.revoke_device", "tunnel.delete_device":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		deviceID, err := executionStringID(target, "device id")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"device_id": deviceID}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if input.Handler == "tunnel.revoke_device" {
				return e.service.RevokeDevice(ctx, application.RevokeTunnelDeviceCommand{DeviceID: deviceID, Authorization: authorization})
			}
			return e.service.DeleteDevice(ctx, application.DeleteTunnelDeviceCommand{DeviceID: deviceID, Authorization: authorization})
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the tunnel capability handler is unsupported", false)
	}
}

func tunnelDeviceAutomationView(device database.PairedDevice) TunnelDeviceAutomationView {
	name, _ := boundedExecutionText(device.DeviceName, 500)
	userAgent, _ := boundedExecutionText(device.UserAgent, 1000)
	id, _ := boundedExecutionText(device.ID, maxExecutionIDRunes)
	return TunnelDeviceAutomationView{
		ID: id, Name: name, UserAgent: userAgent, CreatedAt: device.CreatedAt,
		LastSeenAt: device.LastSeenAt, Revoked: device.Revoked,
	}
}

type UpdateOperationalReadPort interface {
	LastCheck() *updater.UpdateStatus
	CheckForUpdate(context.Context) (*updater.UpdateStatus, error)
}

type UpdateStatusAutomationView struct {
	Checked        bool   `json:"checked"`
	Available      bool   `json:"available"`
	CurrentVersion string `json:"current_version,omitempty"`
	LatestVersion  string `json:"latest_version,omitempty"`
	ReleaseURL     string `json:"release_url,omitempty"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	Managed        string `json:"managed,omitempty"`
	CheckedAt      string `json:"checked_at,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
}

func updateExecutionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("update.status", "update_operations", "update:read"),
		executionReadCapability("update.check", "update_operations", "update:read"),
		executionUnsafeCapability("update.apply", "update_mutations", "update:execute"),
	}
}

type updateExecutionPlatformExecutor struct {
	service *application.UpdateMutationService
	reader  UpdateOperationalReadPort
}

type updateApplyPayload struct {
	Force                     bool `json:"force,omitempty"`
	AcknowledgeActiveSessions bool `json:"acknowledge_active_sessions,omitempty"`
}

func (e *updateExecutionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	if _, err := decodeExecutionTarget(input.Target); err != nil {
		return nil, err
	}
	switch input.Handler {
	case "update.status":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, nil), execute: func(_ context.Context, _ application.ActionAuthorization) (any, error) {
			return updateStatusAutomationView(e.reader.LastCheck()), nil
		}}, nil
	case "update.check":
		if err := requireEmptyExecutionPayload(input.Payload); err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, nil), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			status, err := e.reader.CheckForUpdate(ctx)
			return updateStatusAutomationView(status), err
		}}, nil
	case "update.apply":
		var payload updateApplyPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		if payload.Force != payload.AcknowledgeActiveSessions {
			return nil, platformFailure("platform_payload_invalid", "force update requires a literal active-session acknowledgement and acknowledgement is only valid with force", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"force": payload.Force, "active_sessions_acknowledged": payload.AcknowledgeActiveSessions}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			command := application.ApplyUpdateCommand{Force: payload.Force, Authorization: authorization}
			if payload.Force {
				command.ForceAuthorization = &application.ForceUpdateAuthorization{Authorization: authorization, AcknowledgesActiveSessions: true}
			}
			return e.service.Apply(ctx, command)
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the update capability handler is unsupported", false)
	}
}

func updateStatusAutomationView(status *updater.UpdateStatus) UpdateStatusAutomationView {
	if status == nil {
		return UpdateStatusAutomationView{}
	}
	notes, truncated := boundedExecutionText(status.ReleaseNotes, 16<<10)
	current, _ := boundedExecutionText(status.CurrentVersion, 100)
	latest, _ := boundedExecutionText(status.LatestVersion, 100)
	managed, _ := boundedExecutionText(status.Managed, 100)
	checkedAt, _ := boundedExecutionText(status.CheckedAt, 100)
	errorCode := ""
	if strings.TrimSpace(status.Error) != "" {
		errorCode = "update_check_failed"
	}
	return UpdateStatusAutomationView{
		Checked: true, Available: status.Available, CurrentVersion: current, LatestVersion: latest,
		ReleaseURL: sanitizeExecutionPublicURL(status.ReleaseURL), ReleaseNotes: notes, Managed: managed,
		CheckedAt: checkedAt, ErrorCode: errorCode, Truncated: truncated,
	}
}
