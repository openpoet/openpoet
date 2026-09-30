package automation

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	runtime "openpoet/internal/session"
	"openpoet/internal/sessionprompt"
)

type permissionModeRuntime struct {
	closeCompletedManager
	mode  string
	calls []string
}

func (r *permissionModeRuntime) SetSessionModel(context.Context, string, string) (*database.Session, error) {
	return nil, sql.ErrNoRows
}
func (r *permissionModeRuntime) SetSessionEffort(context.Context, string, string) (*database.Session, error) {
	return nil, sql.ErrNoRows
}
func (r *permissionModeRuntime) SetSessionPermissionMode(_ context.Context, id, mode string) (runtime.PermissionModeChange, error) {
	r.calls = append(r.calls, id+"="+mode)
	change := runtime.PermissionModeChange{From: r.mode, To: mode, Presses: 2}
	r.mode = mode
	return change, nil
}
func (r *permissionModeRuntime) SessionPermissionMode(string) (sessionprompt.PermissionModeState, bool) {
	return sessionprompt.PermissionModeState{Mode: r.mode, Source: sessionprompt.PermissionModeSourceScreen, ObservedAt: time.Now()}, true
}

type permissionModeRecorder struct {
	records []application.SessionPermissionModeChange
}

func (r *permissionModeRecorder) PublishSessionChange(context.Context, application.SessionChange) {}
func (r *permissionModeRecorder) RecordSessionPermissionModeChange(_ context.Context, change application.SessionPermissionModeChange) {
	r.records = append(r.records, change)
}

func permissionModeExecutor() (*sessionPlatformExecutor, *permissionModeRuntime, *permissionModeRecorder) {
	store := &closeCompletedStore{session: &database.Session{ID: "s1", ProjectID: 20, Status: "running", Backend: "claude_code"}}
	rt := &permissionModeRuntime{mode: "auto"}
	recorder := &permissionModeRecorder{}
	service := application.NewSessionService(store, rt, nil, nil, nil, nil, nil, recorder,
		application.SessionCreationCollaborators{Settings: rt})
	queries := questionSessionQueries{session: store.session}
	return &sessionPlatformExecutor{service: service, queries: queries, runtime: rt}, rt, recorder
}

func runSetPermissionMode(t *testing.T, executor *sessionPlatformExecutor, scope *ProjectScopeSet, correlationID, reason string) (any, error) {
	t.Helper()
	command, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: "sessions.set_permission_mode", Target: []byte(`{"id":"s1"}`), Payload: []byte(`{"mode":"acceptEdits"}`), ProjectScope: scope,
	})
	if err != nil {
		return nil, err
	}
	ctx := application.WithEventMetadata(context.Background(), application.EventMetadata{
		Actor: application.Actor{Type: "automation_client", ID: "helena"}, CorrelationID: correlationID,
	})
	return command.Execute(ctx, application.ActionAuthorization{
		Actor: application.Actor{Type: "automation_client", ID: "helena"}, Approved: true, ApprovedBy: "helena", Reason: reason,
	})
}

// The owner wants Helena to switch modes on his authorization without a manual
// approval each time: a policy-approved write whose correlation_id is the
// authorization reference.
func TestSetPermissionModeIsDescribedAsPolicyApprovedWrite(t *testing.T) {
	for _, definition := range sessionPlatformDefinitions() {
		if definition.Name != "sessions.set_permission_mode" {
			continue
		}
		if definition.Risk != application.CapabilityRiskWrite || definition.Approval != application.ApprovalByPolicy {
			t.Fatalf("risk=%s approval=%s, want write/policy", definition.Risk, definition.Approval)
		}
		if definition.Payload == nil || definition.Payload.Notes == "" || len(definition.Payload.Example) == 0 {
			t.Fatalf("set_permission_mode is not self-describing: %+v", definition.Payload)
		}
		return
	}
	t.Fatal("sessions.set_permission_mode is not registered")
}

func TestSetPermissionModeUsesCorrelationIDAsAuthorizationRef(t *testing.T) {
	executor, rt, recorder := permissionModeExecutor()
	result, err := runSetPermissionMode(t, executor, nil, "ain:292", "owner asked to approve the restart rule by hand")
	if err != nil {
		t.Fatal(err)
	}
	body := result.(map[string]any)
	if body["from"] != "auto" || body["to"] != "acceptEdits" || body["changed"] != true {
		t.Fatalf("result = %+v", body)
	}
	view := body["session"].(SessionAutomationView)
	if view.PermissionMode == nil || view.PermissionMode.Mode != "acceptEdits" {
		t.Fatalf("view.permission_mode = %+v", view.PermissionMode)
	}
	if len(rt.calls) != 1 || len(recorder.records) != 1 || recorder.records[0].AuthorizationRef != "ain:292" ||
		recorder.records[0].Reason != "owner asked to approve the restart rule by hand" {
		t.Fatalf("calls=%v records=%+v", rt.calls, recorder.records)
	}
}

func TestSetPermissionModeRefusesMissingReasonAndOutOfScopeSessions(t *testing.T) {
	executor, rt, _ := permissionModeExecutor()
	if _, err := runSetPermissionMode(t, executor, nil, "ain:292", ""); err == nil {
		t.Fatal("a change without a reason was accepted")
	}
	if _, err := runSetPermissionMode(t, executor, &ProjectScopeSet{Allowed: map[int64]bool{47: true}}, "ain:292", "reason"); err == nil {
		t.Fatal("a change outside the client's project_filter was accepted")
	}
	if len(rt.calls) != 0 {
		t.Fatalf("refused changes reached the session: %v", rt.calls)
	}
}
