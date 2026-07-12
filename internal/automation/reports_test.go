package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func TestDailyReportEndpointMatchesHelenaContract(t *testing.T) {
	db := automationTestDB(t)
	ctx := context.Background()
	project := automationContractProject(t, db, "automation-daily-report")
	startedAt := time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(time.Hour)
	session := &database.Session{
		ID: "automation-report-session", ProjectID: project.ID, Status: "completed",
		Name: "Automation report", StartTime: startedAt, Backend: "claude_code",
		EndTime: sql.NullTime{Time: endedAt, Valid: true},
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE sessions SET end_time = ? WHERE id = ?", endedAt, session.ID); err != nil {
		t.Fatal(err)
	}
	reports, err := application.NewReportService(db, application.ReportServiceOptions{
		Now: func() time.Time { return endedAt.Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reports.FinalizeTurn(ctx, application.UpsertTurnReportCommand{
		SessionID: session.ID, TurnID: "turn-api", Objective: "Expose daily report",
		Outcome: "Endpoint available", WorkSummary: "Returned the Helena contract",
		Evidence: []application.ReportEvidence{{Kind: "session", Ref: "openpoet:session:" + session.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	client := provisionTestClient(t, db, ScopeReportsRead)
	handler := CapturePeerAddress(NewHandler(db, Dependencies{Reports: reports}))
	response := automationRequest(t, handler, client.Token, http.MethodGet,
		"/reports/daily?date=2026-07-09", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("daily report status=%d body=%s", response.Code, response.Body.String())
	}
	var report application.DailyReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || report.Date != "2026-07-09" || report.ReportID != "report-2026-07-09" ||
		report.ThroughCursor == "" || len(report.Sessions) != 1 {
		t.Fatalf("unexpected daily report: %+v", report)
	}
	if report.Sessions[0].Title != session.Name || report.Sessions[0].ProjectName != project.Name ||
		len(report.Sessions[0].WorkSummary) != 1 || len(report.Sessions[0].Evidence) != 1 {
		t.Fatalf("session does not match Helena contract: %+v", report.Sessions[0])
	}

	events, err := db.ListEventOutboxAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundAutomationActor := false
	for _, event := range events {
		if event.AggregateType == "daily_report" && event.Actor == "automation_client:"+client.Client.ID {
			foundAutomationActor = true
		}
	}
	if !foundAutomationActor {
		t.Fatalf("daily materialization did not preserve bearer actor: %+v", events)
	}
}

func TestDailyReportEndpointRequiresScopeAndValidDate(t *testing.T) {
	db := automationTestDB(t)
	reports, err := application.NewReportService(db)
	if err != nil {
		t.Fatal(err)
	}
	handler := CapturePeerAddress(NewHandler(db, Dependencies{Reports: reports}))
	withoutScope := provisionTestClient(t, db, ScopeTasksRead)
	response := automationRequest(t, handler, withoutScope.Token, http.MethodGet,
		"/reports/daily?date=2026-07-09", "", nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing reports scope status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := db.ExecContext(context.Background(),
		`UPDATE automation_clients SET scopes = '["reports:read","tasks:read"]' WHERE id = ?`,
		withoutScope.Client.ID); err != nil {
		t.Fatal(err)
	}
	response = automationRequest(t, handler, withoutScope.Token, http.MethodGet,
		"/reports/daily?date=not-a-date", "", nil)
	if response.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, response) != "report_date_invalid" {
		t.Fatalf("invalid date status=%d body=%s", response.Code, response.Body.String())
	}
}
