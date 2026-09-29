package application

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"openpoet/internal/database"
)

type closureRecordingEffects struct {
	phase3SessionEffects
	closures []SessionClosure
}

func (e *closureRecordingEffects) RecordSessionClosure(_ context.Context, closure SessionClosure) {
	e.closures = append(e.closures, closure)
}

var closeCompletedNow = time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)

// closeCompletedAuthorization is what the platform hands a policy-approved
// command: the client approves itself, no human approval_token.
func closeCompletedAuthorization() ActionAuthorization {
	return ActionAuthorization{
		Actor: Actor{Type: "automation_client", ID: "helena"}, Approved: true,
		ApprovedBy: "helena", Reason: "task #849 validated",
	}
}

func closeCompletedFixture(taskStatus, mode string, lastActivity time.Duration) (*SessionService, *phase3Store, *closureRecordingEffects) {
	session := &database.Session{
		ID: "s1", ProjectID: 20, Status: "running", TaskID: sql.NullInt64{Int64: 849, Valid: true},
		StartTime: closeCompletedNow.Add(-2 * time.Hour),
	}
	if lastActivity > 0 {
		session.LastActivityAt = sql.NullTime{Time: closeCompletedNow.Add(-lastActivity), Valid: true}
	}
	store := &phase3Store{session: session, task: &database.ProjectTask{ID: 849, ProjectID: 20, Status: taskStatus}}
	effects := &closureRecordingEffects{}
	service := NewSessionService(store, &phase3SessionManager{running: true}, nil, nil, nil, nil, nil, effects,
		SessionCreationCollaborators{Signals: &fakeSignals{mode: mode}, Now: func() time.Time { return closeCompletedNow }})
	return service, store, effects
}

func TestCloseCompletedStopsIdleSessionOfDoneTaskAndRecordsWhy(t *testing.T) {
	service, store, effects := closeCompletedFixture(TaskStatusDone, "idle", time.Minute)
	stopped, err := service.CloseCompleted(context.Background(), CloseCompletedSessionCommand{
		SessionID: "s1", Authorization: closeCompletedAuthorization(),
	})
	if err != nil {
		t.Fatalf("close completed: %v", err)
	}
	if stopped.Status != "stopped" || store.session.Status != "stopped" {
		t.Fatalf("session status = %q, want stopped", stopped.Status)
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "closed_completed" {
		t.Fatalf("published changes = %+v, want one closed_completed", effects.changes)
	}
	if len(effects.closures) != 1 {
		t.Fatalf("closures = %d, want 1", len(effects.closures))
	}
	got := effects.closures[0]
	if got.TaskID != 849 || got.Reason != "task #849 validated" || got.Actor.ID != "helena" || got.Mode != "idle" || got.SessionID != "s1" {
		t.Fatalf("closure record = %+v", got)
	}
}

func TestCloseCompletedRefusesUnlessWorkIsFinishedAndSessionQuiet(t *testing.T) {
	cases := []struct {
		name         string
		taskStatus   string
		mode         string
		lastActivity time.Duration
		noTask       bool
		awaiting     bool
		reason       string
		want         string
	}{
		{name: "task open", taskStatus: TaskStatusInProgress, mode: "idle", want: "session_task_not_done"},
		{name: "task awaiting approval", taskStatus: TaskStatusAwaitingApproval, mode: "idle", want: "session_task_not_done"},
		{name: "no linked task", noTask: true, mode: "idle", want: "session_task_missing"},
		{name: "mid turn", taskStatus: TaskStatusDone, mode: "executing", want: "session_busy"},
		{name: "plan mode turn", taskStatus: TaskStatusDone, mode: "plan_mode", want: "session_busy"},
		{name: "unknown mode recently active", taskStatus: TaskStatusDone, mode: "", lastActivity: 3 * time.Minute, want: "session_busy"},
		{name: "awaiting input", taskStatus: TaskStatusDone, mode: "idle", awaiting: true, want: "session_awaiting_input"},
		{name: "missing reason", taskStatus: TaskStatusDone, mode: "idle", reason: " ", want: "reason_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, store, effects := closeCompletedFixture(tc.taskStatus, tc.mode, tc.lastActivity)
			if tc.noTask {
				store.session.TaskID = sql.NullInt64{}
			}
			authorization := closeCompletedAuthorization()
			if tc.reason != "" {
				authorization.Reason = tc.reason
			}
			_, err := service.CloseCompleted(context.Background(), CloseCompletedSessionCommand{
				SessionID: "s1", AwaitingInput: tc.awaiting, Authorization: authorization,
			})
			if code := sendErrCode(err); code != tc.want {
				t.Fatalf("error = %v (code %q), want %s", err, code, tc.want)
			}
			if store.session.Status != "running" || len(effects.changes) != 0 || len(effects.closures) != 0 {
				t.Fatalf("refused close still acted: status=%q changes=%d closures=%d", store.session.Status, len(effects.changes), len(effects.closures))
			}
		})
	}
}

func TestCloseCompletedTreatsLongQuietUnknownModeAsIdle(t *testing.T) {
	service, store, _ := closeCompletedFixture(TaskStatusDone, "", CloseCompletedQuietPeriod+time.Minute)
	if _, err := service.CloseCompleted(context.Background(), CloseCompletedSessionCommand{
		SessionID: "s1", Authorization: closeCompletedAuthorization(),
	}); err != nil {
		t.Fatalf("close quiet unknown-mode session: %v", err)
	}
	if store.session.Status != "stopped" {
		t.Fatalf("status = %q, want stopped", store.session.Status)
	}
}

func TestCloseCompletedIsIdempotentForStoppedSession(t *testing.T) {
	service, store, effects := closeCompletedFixture(TaskStatusInProgress, "executing", 0)
	store.session.Status = "stopped"
	stopped, err := service.CloseCompleted(context.Background(), CloseCompletedSessionCommand{
		SessionID: "s1", Authorization: closeCompletedAuthorization(),
	})
	if err != nil || stopped.Status != "stopped" || len(effects.changes) != 0 {
		t.Fatalf("stopped session: err=%v session=%+v changes=%d", err, stopped, len(effects.changes))
	}
}
