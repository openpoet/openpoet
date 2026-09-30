package application

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/database"
	runtime "openpoet/internal/session"
)

// SessionPermissionModeSetter is optionally implemented by
// SessionRuntimeSettings: it moves a running session to a permission mode and
// reports the mode read back from the session afterwards.
type SessionPermissionModeSetter interface {
	SetSessionPermissionMode(context.Context, string, string) (runtime.PermissionModeChange, error)
}

type SetSessionPermissionModeCommand struct {
	SessionID string
	Mode      string
	// AuthorizationRef names the owner's authorization (e.g. "ain:292"); it is
	// required and kept in the audit record with the reason.
	AuthorizationRef string
	Authorization    ActionAuthorization
}

// SessionPermissionModeResult is what the session reported after the change.
type SessionPermissionModeResult struct {
	Session *database.Session
	From    string
	To      string
	Presses int
	Changed bool
}

// SessionPermissionModeChange is the audit record of a permission mode change
// attempt. Outcome is changed, unchanged (already in the mode) or failed; a
// failed attempt that pressed keys may have left the session in another mode.
type SessionPermissionModeChange struct {
	SessionID        string
	ProjectID        int64
	TaskID           int64
	Requested        string
	From             string
	To               string
	Presses          int
	Outcome          string
	Error            string
	Reason           string
	AuthorizationRef string
	Actor            Actor
	ApprovedBy       string
}

// SessionPermissionModeRecorder is optionally implemented by SessionEffects to
// keep who changed a session's permission mode, from what to what, and why.
type SessionPermissionModeRecorder interface {
	RecordSessionPermissionModeChange(context.Context, SessionPermissionModeChange)
}

const maxAuthorizationRefRunes = 200

// SetPermissionMode switches a running Claude Code session between auto,
// acceptEdits, default and plan. Every attempt needs a reason and the owner's
// authorization reference, and is recorded whether it succeeds or not.
func (s *SessionService) SetPermissionMode(ctx context.Context, command SetSessionPermissionModeCommand) (*SessionPermissionModeResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(command.Authorization.Reason)
	if reason == "" {
		return nil, validationError("reason_required", "A reason is required to change a session's permission mode")
	}
	// Session-tier callers (an agent's own MCP token) are never approved, so no
	// agent can lift its own or a sibling's permission prompts.
	if !command.Authorization.Approved || strings.TrimSpace(command.Authorization.ApprovedBy) == "" {
		return nil, validationError("action_approval_required", "Changing a session's permission mode needs an owner-tier caller (UI, paired device or automation client); session agents cannot do it")
	}
	ref := strings.TrimSpace(command.AuthorizationRef)
	if !validPermissionModeAuthorizationRef(ref) {
		return nil, validationError("authorization_ref_required", "An authorization_ref (e.g. ain:292) naming the owner's authorization is required to change a session's permission mode")
	}
	mode, err := runtime.ValidateSettablePermissionMode(command.Mode)
	if err != nil {
		return nil, sessionSettingApplicationError(err)
	}
	session, err := s.requireRunningSession(ctx, command.SessionID)
	if err != nil {
		return nil, err
	}
	setter, ok := s.creation.Settings.(SessionPermissionModeSetter)
	if !ok {
		return nil, validationError("session_settings_unavailable", "Session permission mode changes are unavailable")
	}

	change, setErr := setter.SetSessionPermissionMode(ctx, session.ID, mode)
	record := SessionPermissionModeChange{
		SessionID: session.ID, ProjectID: session.ProjectID, Requested: mode,
		From: change.From, To: change.To, Presses: change.Presses,
		Reason: reason, AuthorizationRef: ref,
		Actor: command.Authorization.Actor, ApprovedBy: command.Authorization.ApprovedBy,
	}
	if session.TaskID.Valid {
		record.TaskID = session.TaskID.Int64
	}
	switch {
	case setErr != nil:
		record.Outcome = "failed"
		record.Error = setErr.Error()
	case change.Presses == 0:
		record.Outcome = "unchanged"
	default:
		record.Outcome = "changed"
	}
	// A refused request (invalid mode, not running, dialog open) touched
	// nothing; anything that reached the keyboard is on the record.
	if setErr == nil || change.Presses > 0 {
		if recorder, ok := s.effects.(SessionPermissionModeRecorder); ok {
			recorder.RecordSessionPermissionModeChange(ctx, record)
		}
	}
	if setErr != nil {
		return nil, permissionModeApplicationError(setErr)
	}
	if record.Outcome == "changed" {
		s.publish(ctx, SessionChange{Action: "permission_mode_changed", Session: session, ID: session.ID, Actor: command.Authorization.Actor})
	}
	return &SessionPermissionModeResult{Session: session, From: change.From, To: change.To, Presses: change.Presses, Changed: record.Outcome == "changed"}, nil
}

func permissionModeApplicationError(err error) error {
	switch {
	case errors.Is(err, runtime.ErrSessionAwaitingInput):
		return &Error{Kind: ErrorConflict, Code: "session_awaiting_input", Message: err.Error(), Cause: err}
	case errors.Is(err, runtime.ErrPermissionModeUnconfirmed):
		return &Error{Kind: ErrorConflict, Code: "permission_mode_unconfirmed", Message: err.Error(), Cause: err}
	default:
		return sessionSettingApplicationError(err)
	}
}

// validPermissionModeAuthorizationRef accepts a bounded "<source>:<id>"
// reference with no whitespace or control characters. The Automation API
// additionally restricts the source to its audit prefixes.
func validPermissionModeAuthorizationRef(ref string) bool {
	if ref == "" || utf8.RuneCountInString(ref) > maxAuthorizationRefRunes {
		return false
	}
	source, id, ok := strings.Cut(ref, ":")
	if !ok || source == "" || id == "" {
		return false
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || !unicode.IsGraphic(r) {
			return false
		}
	}
	return true
}
