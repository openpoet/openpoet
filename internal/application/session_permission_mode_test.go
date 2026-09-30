package application

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"openpoet/internal/database"
	runtime "openpoet/internal/session"
)

type permissionModeSettings struct {
	phase3SessionSettings
	calls  []string
	change runtime.PermissionModeChange
	err    error
}

func (s *permissionModeSettings) SetSessionPermissionMode(_ context.Context, id, mode string) (runtime.PermissionModeChange, error) {
	s.calls = append(s.calls, id+"="+mode)
	return s.change, s.err
}

type permissionModeEffects struct {
	phase3SessionEffects
	records []SessionPermissionModeChange
}

func (e *permissionModeEffects) RecordSessionPermissionModeChange(_ context.Context, change SessionPermissionModeChange) {
	e.records = append(e.records, change)
}

func permissionModeService(settings *permissionModeSettings, effects *permissionModeEffects) *SessionService {
	store := &phase3Store{session: &database.Session{ID: "s1", ProjectID: 7, Status: "running", TaskID: sql.NullInt64{Int64: 866, Valid: true}}}
	return NewSessionService(store, &phase3SessionManager{running: true}, nil, nil, nil, nil, nil, effects, SessionCreationCollaborators{Settings: settings})
}

func TestSetPermissionModeRecordsWhoChangedWhatAndWhy(t *testing.T) {
	settings := &permissionModeSettings{change: runtime.PermissionModeChange{From: "auto", To: "acceptEdits", Presses: 2}}
	effects := &permissionModeEffects{}
	service := permissionModeService(settings, effects)

	result, err := service.SetPermissionMode(context.Background(), SetSessionPermissionModeCommand{
		SessionID: "s1", Mode: "accept_edits", AuthorizationRef: "ain:292", Authorization: phase3Approval(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.From != "auto" || result.To != "acceptEdits" || len(settings.calls) != 1 || settings.calls[0] != "s1=acceptEdits" {
		t.Fatalf("result=%+v calls=%v", result, settings.calls)
	}
	if len(effects.records) != 1 {
		t.Fatalf("records = %+v", effects.records)
	}
	record := effects.records[0]
	if record.Outcome != "changed" || record.From != "auto" || record.To != "acceptEdits" || record.Requested != "acceptEdits" ||
		record.AuthorizationRef != "ain:292" || record.Reason != "requested operation" || record.ApprovedBy != "presidente" ||
		record.Actor.ID != "helena" || record.TaskID != 866 || record.ProjectID != 7 {
		t.Fatalf("record = %+v", record)
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "permission_mode_changed" {
		t.Fatalf("changes = %+v", effects.changes)
	}
}

func TestSetPermissionModeRequiresReasonReferenceAndAnOwnerTierCaller(t *testing.T) {
	settings := &permissionModeSettings{change: runtime.PermissionModeChange{From: "auto", To: "plan", Presses: 3}}
	effects := &permissionModeEffects{}
	service := permissionModeService(settings, effects)

	noReason := phase3Approval()
	noReason.Reason = " "
	sessionTier := phase3Approval()
	sessionTier.Approved, sessionTier.ApprovedBy = false, ""
	cases := []struct {
		name    string
		command SetSessionPermissionModeCommand
		code    string
	}{
		{"no reason", SetSessionPermissionModeCommand{SessionID: "s1", Mode: "plan", AuthorizationRef: "ain:292", Authorization: noReason}, "reason_required"},
		{"no reference", SetSessionPermissionModeCommand{SessionID: "s1", Mode: "plan", Authorization: phase3Approval()}, "authorization_ref_required"},
		{"reference with spaces", SetSessionPermissionModeCommand{SessionID: "s1", Mode: "plan", AuthorizationRef: "ain: 292", Authorization: phase3Approval()}, "authorization_ref_required"},
		{"session agent", SetSessionPermissionModeCommand{SessionID: "s1", Mode: "plan", AuthorizationRef: "ain:292", Authorization: sessionTier}, "action_approval_required"},
		{"bypass", SetSessionPermissionModeCommand{SessionID: "s1", Mode: "bypassPermissions", AuthorizationRef: "ain:292", Authorization: phase3Approval()}, "session_setting_invalid"},
	}
	for _, tc := range cases {
		_, err := service.SetPermissionMode(context.Background(), tc.command)
		appErr, ok := err.(*Error)
		if !ok || appErr.Code != tc.code {
			t.Errorf("%s: err = %v, want code %s", tc.name, err, tc.code)
		}
	}
	if len(settings.calls) != 0 || len(effects.records) != 0 || len(effects.changes) != 0 {
		t.Fatalf("refused requests reached the session: calls=%v records=%v changes=%v", settings.calls, effects.records, effects.changes)
	}
}

func TestSetPermissionModeRecordsAFailedAttemptThatPressedKeys(t *testing.T) {
	settings := &permissionModeSettings{
		change: runtime.PermissionModeChange{From: "default", To: "unknown", Presses: 1},
		err:    fmt.Errorf("%w: no mode indicator was painted", runtime.ErrPermissionModeUnconfirmed),
	}
	effects := &permissionModeEffects{}
	service := permissionModeService(settings, effects)

	_, err := service.SetPermissionMode(context.Background(), SetSessionPermissionModeCommand{
		SessionID: "s1", Mode: "plan", AuthorizationRef: "ain:292", Authorization: phase3Approval(),
	})
	if appErr, ok := err.(*Error); !ok || appErr.Code != "permission_mode_unconfirmed" || appErr.Kind != ErrorConflict {
		t.Fatalf("err = %v", err)
	}
	if len(effects.records) != 1 || effects.records[0].Outcome != "failed" || effects.records[0].Error == "" || effects.records[0].To != "unknown" {
		t.Fatalf("records = %+v", effects.records)
	}
	if len(effects.changes) != 0 {
		t.Fatalf("a failed change must not publish: %+v", effects.changes)
	}

	// A refusal before any key press (a dialog is open) leaves no record.
	settings.change = runtime.PermissionModeChange{From: "auto", To: "auto"}
	settings.err = fmt.Errorf("%w: answer it first", runtime.ErrSessionAwaitingInput)
	_, err = service.SetPermissionMode(context.Background(), SetSessionPermissionModeCommand{
		SessionID: "s1", Mode: "plan", AuthorizationRef: "ain:292", Authorization: phase3Approval(),
	})
	if appErr, ok := err.(*Error); !ok || appErr.Code != "session_awaiting_input" {
		t.Fatalf("err = %v", err)
	}
	if len(effects.records) != 1 {
		t.Fatalf("records = %+v", effects.records)
	}
}

func TestSetPermissionModeAlreadyInModeIsUnchanged(t *testing.T) {
	settings := &permissionModeSettings{change: runtime.PermissionModeChange{From: "acceptEdits", To: "acceptEdits"}}
	effects := &permissionModeEffects{}
	service := permissionModeService(settings, effects)
	result, err := service.SetPermissionMode(context.Background(), SetSessionPermissionModeCommand{
		SessionID: "s1", Mode: "acceptEdits", AuthorizationRef: "task:867", Authorization: phase3Approval(),
	})
	if err != nil || result.Changed || result.To != "acceptEdits" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(effects.records) != 1 || effects.records[0].Outcome != "unchanged" || len(effects.changes) != 0 {
		t.Fatalf("records=%+v changes=%+v", effects.records, effects.changes)
	}
}
