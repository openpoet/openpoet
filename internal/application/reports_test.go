package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
)

func reportTestService(t *testing.T, db *database.DB, now time.Time) *ReportService {
	t.Helper()
	location, err := time.LoadLocation(DefaultReportTimezone)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewReportService(db, ReportServiceOptions{
		Location: location,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func createReportSession(
	t *testing.T,
	db *database.DB,
	projectID int64,
	taskID *int64,
	id, status string,
	startedAt time.Time,
	endedAt *time.Time,
) {
	t.Helper()
	session := &database.Session{
		ID: id, ProjectID: projectID, Status: status, Name: "Session " + id,
		StartTime: startedAt, Backend: "claude_code",
	}
	if taskID != nil {
		session.TaskID = sql.NullInt64{Int64: *taskID, Valid: true}
	}
	if err := db.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if endedAt != nil {
		if _, err := db.ExecContext(context.Background(),
			"UPDATE sessions SET end_time = ? WHERE id = ?", *endedAt, id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStructuredDailyReportContract(t *testing.T) {
	db := applicationTestDB(t)
	ctx := context.Background()
	project := createApplicationProject(t, db, "reports-contract")
	taskService := NewProjectTaskService(db, nil)
	primary := createTaskThroughService(t, taskService, project.ID, "Primary report task", nil)
	secondary := createTaskThroughService(t, taskService, project.ID, "Secondary report task", nil)
	if _, err := taskService.ChangeStatus(ctx, ChangeTaskStatusCommand{
		ProjectID: project.ID, TaskID: secondary.ID, Status: TaskStatusDone, Actor: UserActor(),
	}); err != nil {
		t.Fatal(err)
	}

	dayStart := time.Date(2026, 7, 9, 3, 0, 0, 0, time.UTC)
	completedEnd := dayStart.Add(3 * time.Hour)
	createReportSession(t, db, project.ID, &primary.ID, "completed-session", "completed",
		dayStart.Add(2*time.Hour), &completedEnd)
	createReportSession(t, db, project.ID, nil, "boundary-running", "running",
		dayStart, nil)
	createReportSession(t, db, project.ID, nil, "carryover-running", "running",
		dayStart.Add(-2*time.Hour), nil)
	createReportSession(t, db, project.ID, &primary.ID, "error-session", "error",
		dayStart.Add(23*time.Hour+59*time.Minute), nil)
	createReportSession(t, db, project.ID, nil, "previous-local-day", "completed",
		dayStart.Add(-time.Minute), nil)
	createReportSession(t, db, project.ID, nil, "next-local-day", "completed",
		dayStart.Add(24*time.Hour), nil)

	if _, err := db.ExecContext(ctx,
		"UPDATE task_history SET created_at = ? WHERE task_id = ?",
		dayStart.Add(4*time.Hour), secondary.ID); err != nil {
		t.Fatal(err)
	}
	throughCursor, err := db.SnapshotEventOutboxCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service := reportTestService(t, db, dayStart.Add(25*time.Hour))
	finalized, err := service.FinalizeTurn(ctx, UpsertTurnReportCommand{
		SessionID: "completed-session", TurnID: "turn-1",
		Objective:   "Implement structured reports",
		Outcome:     "Reports persisted and verified",
		WorkSummary: "Implemented the report pipeline with api_key=supersecret",
		Decisions:   []string{"Use deterministic JSON", "Use typed evidence"},
		Verification: ReportVerification{
			Status: "passed", Summary: "Contract tests passed",
			EvidenceRefs: []string{"test:reports"},
		},
		Evidence: []ReportEvidence{
			{Kind: "verification", Ref: "test:reports", Label: "Authorization: Bearer hidden-token"},
			{Kind: "file", Ref: "internal/application/reports.go"},
		},
		CompletedTaskIDs: []int64{primary.ID}, ChangedTaskIDs: []int64{primary.ID},
		ThroughCursor: throughCursor, StartedAt: dayStart.Add(2 * time.Hour), EndedAt: &completedEnd,
		Actor: Actor{Type: "automation", ID: "helena"}, CorrelationID: "report-command-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.State != "finalized" || finalized.FinalizedAt == nil {
		t.Fatalf("turn was not finalized: %+v", finalized)
	}
	if strings.Contains(finalized.WorkSummary, "supersecret") || !strings.Contains(finalized.WorkSummary, "[REDACTED]") {
		t.Fatalf("secret was not redacted: %q", finalized.WorkSummary)
	}
	if strings.Contains(finalized.Evidence[1].Label, "hidden-token") {
		t.Fatalf("evidence leaked bearer token: %+v", finalized.Evidence)
	}
	if _, err := service.UpdateTurn(ctx, UpsertTurnReportCommand{
		SessionID: "error-session", TurnID: "turn-incomplete",
		Objective: "Investigate failure", WorkSummary: "Captured persisted failure metadata",
		Incomplete: true, IncompleteReasons: []string{"backend_failed"}, ThroughCursor: throughCursor,
	}); err != nil {
		t.Fatal(err)
	}

	daily, err := service.MaterializeDaily(ctx, "2026-07-09")
	if err != nil {
		t.Fatal(err)
	}
	if daily.Timezone != DefaultReportTimezone || len(daily.Sessions) != 4 {
		t.Fatalf("timezone/session window contract failed: timezone=%s sessions=%+v", daily.Timezone, daily.Sessions)
	}
	byID := make(map[string]DailySessionReport)
	for _, session := range daily.Sessions {
		byID[session.SessionID] = session
	}
	if _, exists := byID["previous-local-day"]; exists {
		t.Fatal("session from previous Sao Paulo date was included")
	}
	if _, exists := byID["next-local-day"]; exists {
		t.Fatal("session at exclusive Sao Paulo day end was included")
	}
	completed := byID["completed-session"]
	if completed.Incomplete || completed.ProjectName != project.Name || completed.TaskID == nil || *completed.TaskID != primary.ID {
		t.Fatalf("completed session materialization is incomplete: %+v", completed)
	}
	if !containsReportString(completed.Verification, "Contract tests passed") || len(completed.Evidence) != 2 {
		t.Fatalf("typed evidence/verification missing: %+v", completed)
	}
	running := byID["boundary-running"]
	if !running.Incomplete || !containsReportString(running.IncompleteReasons, "missing_structured_report") ||
		!containsReportString(running.IncompleteReasons, "session_active") {
		t.Fatalf("running session incomplete reasons=%v", running.IncompleteReasons)
	}
	carryover := byID["carryover-running"]
	if !carryover.Incomplete || !containsReportString(carryover.IncompleteReasons, "missing_structured_report") {
		t.Fatalf("active carryover session was not included as incomplete: %+v", carryover)
	}
	failed := byID["error-session"]
	for _, reason := range []string{"backend_failed", "report_not_finalized", "session_error"} {
		if !containsReportString(failed.IncompleteReasons, reason) {
			t.Fatalf("error session missing %q: %+v", reason, failed)
		}
	}
	for _, taskID := range []int64{primary.ID, secondary.ID} {
		if !containsReportTaskID(daily.CompletedTaskIDs, taskID) {
			t.Fatalf("completed task %d missing from %v", taskID, daily.CompletedTaskIDs)
		}
	}

	record, err := db.GetStructuredDailyReport(ctx, "2026-07-09")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(record.ReportJSON))
	if hex.EncodeToString(digest[:]) != record.ContentSHA256 {
		t.Fatal("daily report content hash is not verifiable")
	}
	if strings.Contains(record.ReportJSON, "supersecret") || strings.Contains(record.ReportJSON, "hidden-token") {
		t.Fatal("materialized report persisted a secret")
	}
	var contract struct {
		SchemaVersion int    `json:"schema_version"`
		ReportID      string `json:"report_id"`
		Date          string `json:"date"`
		ThroughCursor string `json:"through_cursor"`
		Sessions      []struct {
			ProjectName  string           `json:"project_name"`
			Title        string           `json:"title"`
			WorkSummary  []string         `json:"work_summary"`
			Verification []string         `json:"verification"`
			Evidence     []ReportEvidence `json:"evidence"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(record.ReportJSON), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.SchemaVersion != 1 || contract.ReportID != "report-2026-07-09" ||
		contract.Date != "2026-07-09" || contract.ThroughCursor == "" {
		t.Fatalf("Helena daily report identity contract changed: %+v", contract)
	}
	if len(contract.Sessions) != 4 || contract.Sessions[1].ProjectName == "" || contract.Sessions[1].Title == "" {
		t.Fatalf("Helena session contract changed: %+v", contract.Sessions)
	}
	var rawContract map[string]json.RawMessage
	if err := json.Unmarshal([]byte(record.ReportJSON), &rawContract); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"report_date", "timezone", "content_sha256"} {
		if _, exists := rawContract[forbidden]; exists {
			t.Fatalf("non-contract field %q leaked into daily report", forbidden)
		}
	}

	eventsBefore, err := db.ListEventOutboxAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	dailyAgain, err := service.MaterializeDaily(ctx, "2026-07-09")
	if err != nil {
		t.Fatal(err)
	}
	eventsAfter, err := db.ListEventOutboxAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(daily)
	afterJSON, _ := json.Marshal(dailyAgain)
	if string(afterJSON) != string(beforeJSON) || len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("deterministic rematerialization changed hash/events: before=%d after=%d", len(eventsBefore), len(eventsAfter))
	}
	reportEvents := 0
	for _, event := range eventsAfter {
		if event.EventType == ReportEventUpdated || event.EventType == ReportEventFinalized {
			reportEvents++
		}
	}
	if reportEvents != 3 {
		t.Fatalf("report events=%d, want finalized turn + updated turn + daily materialization", reportEvents)
	}
}

func TestStructuredReportSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports-restart.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project := createApplicationProject(t, db, "reports-restart")
	startedAt := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	createReportSession(t, db, project.ID, nil, "restart-session", "completed", startedAt, &startedAt)
	service := reportTestService(t, db, startedAt.Add(time.Hour))
	if _, err := service.FinalizeTurn(ctx, UpsertTurnReportCommand{
		SessionID: "restart-session", TurnID: "restart-turn",
		Objective: "Persist across restart", Outcome: "Persisted",
		Evidence: []ReportEvidence{{Kind: "document", Ref: "report:restart"}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := service.MaterializeDaily(ctx, "2026-07-09")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restartedService := reportTestService(t, reopened, startedAt.Add(2*time.Hour))
	after, err := restartedService.MaterializeDaily(ctx, "2026-07-09")
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(afterJSON) != string(beforeJSON) || len(after.Sessions) != 1 || len(after.Sessions[0].Turns) != 1 {
		t.Fatalf("restart changed durable report: before=%+v after=%+v", before, after)
	}
	if len(after.Sessions[0].Evidence) != 1 || after.Sessions[0].Evidence[0].Ref != "report:restart" {
		t.Fatalf("evidence did not survive restart: %+v", after.Sessions[0].Evidence)
	}
}

func TestStructuredReportRollsBackWhenOutboxFails(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "reports-outbox-rollback")
	startedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	createReportSession(t, db, project.ID, nil, "rollback-report-session", "completed", startedAt, &startedAt)
	if _, err := db.Exec(`
		CREATE TRIGGER reject_report_event BEFORE INSERT ON event_outbox
		WHEN NEW.aggregate_type = 'session_report'
		BEGIN SELECT RAISE(ABORT, 'report outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	service := reportTestService(t, db, startedAt.Add(time.Hour))
	_, err := service.FinalizeTurn(context.Background(), UpsertTurnReportCommand{
		SessionID: "rollback-report-session", TurnID: "turn-rollback", Outcome: "must roll back",
	})
	if err == nil {
		t.Fatal("expected report event failure")
	}
	var count int
	if err := db.GetContext(context.Background(), &count,
		"SELECT COUNT(*) FROM structured_session_reports WHERE session_id = ?", "rollback-report-session"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("report committed without outbox event: count=%d", count)
	}
}

func TestDailyReportRollsBackWhenOutboxFails(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "daily-report-outbox-rollback")
	startedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	createReportSession(t, db, project.ID, nil, "daily-rollback-session", "running", startedAt, nil)
	if _, err := db.Exec(`
		CREATE TRIGGER reject_daily_report_event BEFORE INSERT ON event_outbox
		WHEN NEW.aggregate_type = 'daily_report'
		BEGIN SELECT RAISE(ABORT, 'daily report outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	service := reportTestService(t, db, startedAt.Add(time.Hour))
	if _, err := service.MaterializeDaily(context.Background(), "2026-07-09"); err == nil {
		t.Fatal("expected daily report event failure")
	}
	var count int
	if err := db.GetContext(context.Background(), &count,
		"SELECT COUNT(*) FROM structured_daily_reports WHERE report_date = ?", "2026-07-09"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("daily report committed without outbox event: count=%d", count)
	}
}

func TestFinalizedReportCannotBeDowngraded(t *testing.T) {
	db := applicationTestDB(t)
	project := createApplicationProject(t, db, "reports-finalized-state")
	startedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	createReportSession(t, db, project.ID, nil, "finalized-state-session", "completed", startedAt, &startedAt)
	service := reportTestService(t, db, startedAt.Add(time.Hour))
	if _, err := service.FinalizeTurn(context.Background(), UpsertTurnReportCommand{
		SessionID: "finalized-state-session", TurnID: "final-turn", Outcome: "done",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := service.UpdateTurn(context.Background(), UpsertTurnReportCommand{
		SessionID: "finalized-state-session", TurnID: "final-turn", Outcome: "downgrade",
	})
	if !ErrorIsKind(err, ErrorConflict) {
		t.Fatalf("downgrade error=%v", err)
	}
}

func TestLifecycleCaptureCoversUserStopRestartAndSummaryEnrichment(t *testing.T) {
	db := applicationTestDB(t)
	ctx := context.Background()
	project := createApplicationProject(t, db, "reports-lifecycle")
	startedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	createReportSession(t, db, project.ID, nil, "user-stopped-session", "stopped", startedAt, &startedAt)
	createReportSession(t, db, project.ID, nil, "restart-running-session", "running", startedAt, nil)
	service := reportTestService(t, db, startedAt.Add(time.Hour))

	stopped, err := service.CaptureSessionLifecycle(ctx, "user-stopped-session")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "finalized" || !stopped.Incomplete ||
		!containsReportString(stopped.IncompleteReasons, "session_stopped_by_user") {
		t.Fatalf("user-stopped lifecycle report=%+v", stopped)
	}
	running, err := service.CaptureSessionLifecycle(ctx, "restart-running-session")
	if err != nil {
		t.Fatal(err)
	}
	if running.State != "updated" || !containsReportString(running.IncompleteReasons, "session_restart_pending") {
		t.Fatalf("restart lifecycle report=%+v", running)
	}

	endedAt := startedAt.Add(2 * time.Hour)
	if _, err := db.ExecContext(ctx,
		"UPDATE sessions SET status = 'completed', end_time = ? WHERE id = ?",
		endedAt, "restart-running-session"); err != nil {
		t.Fatal(err)
	}
	enriched, err := service.EnrichSessionSummary(ctx, "restart-running-session", "Durable AI session summary")
	if err != nil {
		t.Fatal(err)
	}
	if enriched.ReportID != running.ReportID || enriched.State != "finalized" || enriched.Incomplete ||
		enriched.WorkSummary != "Durable AI session summary" {
		t.Fatalf("summary did not enrich the restart report in place: before=%+v after=%+v", running, enriched)
	}
}

func containsReportString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsReportTaskID(values []int64, expected int64) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestReportEvidenceJSONContract(t *testing.T) {
	evidence := ReportEvidence{Kind: "verification", Ref: "test:contract"}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"kind":"verification","ref":"test:contract"}` {
		t.Fatalf("typed evidence JSON changed: %s", encoded)
	}
}
