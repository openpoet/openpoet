package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

type StructuredSessionReportRecord struct {
	ReportID              string        `db:"report_id"`
	SessionID             string        `db:"session_id"`
	TurnID                string        `db:"turn_id"`
	ReportDate            string        `db:"report_date"`
	Timezone              string        `db:"timezone"`
	ProjectID             int64         `db:"project_id"`
	ProjectName           string        `db:"project_name"`
	TaskID                sql.NullInt64 `db:"task_id"`
	TaskTitle             string        `db:"task_title"`
	SessionName           string        `db:"session_name"`
	SessionStatus         string        `db:"session_status"`
	SessionStartedAt      time.Time     `db:"session_started_at"`
	SessionEndedAt        sql.NullTime  `db:"session_ended_at"`
	State                 string        `db:"state"`
	Incomplete            bool          `db:"incomplete"`
	Objective             string        `db:"objective"`
	Outcome               string        `db:"outcome"`
	WorkSummary           string        `db:"work_summary"`
	DecisionsJSON         string        `db:"decisions_json"`
	VerificationJSON      string        `db:"verification_json"`
	EvidenceJSON          string        `db:"evidence_json"`
	CompletedTaskIDsJSON  string        `db:"completed_task_ids_json"`
	ChangedTaskIDsJSON    string        `db:"changed_task_ids_json"`
	IncompleteReasonsJSON string        `db:"incomplete_reasons_json"`
	ThroughCursor         int64         `db:"through_cursor"`
	TurnStartedAt         time.Time     `db:"turn_started_at"`
	TurnEndedAt           sql.NullTime  `db:"turn_ended_at"`
	CreatedAt             time.Time     `db:"created_at"`
	UpdatedAt             time.Time     `db:"updated_at"`
	FinalizedAt           sql.NullTime  `db:"finalized_at"`
}

type StructuredDailyReportRecord struct {
	ReportDate             string    `db:"report_date"`
	Timezone               string    `db:"timezone"`
	ThroughCursor          int64     `db:"through_cursor"`
	ReportJSON             string    `db:"report_json"`
	ContentSHA256          string    `db:"content_sha256"`
	SessionCount           int       `db:"session_count"`
	IncompleteSessionCount int       `db:"incomplete_session_count"`
	GeneratedAt            time.Time `db:"generated_at"`
	CreatedAt              time.Time `db:"created_at"`
	UpdatedAt              time.Time `db:"updated_at"`
}

type StructuredReportSessionSource struct {
	SessionID      string        `db:"session_id"`
	ProjectID      int64         `db:"project_id"`
	ProjectName    string        `db:"project_name"`
	TaskID         sql.NullInt64 `db:"task_id"`
	TaskTitle      string        `db:"task_title"`
	Status         string        `db:"status"`
	Name           string        `db:"name"`
	StartTime      time.Time     `db:"start_time"`
	EndTime        sql.NullTime  `db:"end_time"`
	LastActivityAt sql.NullTime  `db:"last_activity_at"`
	PlanContent    string        `db:"plan_content"`
	Backend        string        `db:"backend"`
}

type StructuredReportTaskActivity struct {
	TaskID    int64          `db:"task_id"`
	ProjectID int64          `db:"project_id"`
	EventType string         `db:"event_type"`
	Details   string         `db:"details"`
	SessionID sql.NullString `db:"session_id"`
	CreatedAt time.Time      `db:"created_at"`
}

type StructuredReportDocumentRef struct {
	ID     string `db:"id"`
	Title  string `db:"title"`
	Status string `db:"status"`
}

type ReportTx struct {
	tx *sqlx.Tx
}

func (d *DB) WithReportTx(ctx context.Context, fn func(*ReportTx) error) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&ReportTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.NotifyOutboxAppended()
	return nil
}

func (t *ReportTx) AppendEventOutbox(ctx context.Context, input EventOutboxAppend) (*EventOutboxEvent, error) {
	return AppendEventOutbox(ctx, t.tx, input)
}

func (t *ReportTx) SnapshotEventOutboxCursor(ctx context.Context) (int64, error) {
	var cursor int64
	err := t.tx.GetContext(ctx, &cursor, eventOutboxSnapshotQuery)
	return cursor, err
}

func (t *ReportTx) SnapshotReportSourceCursor(ctx context.Context) (int64, error) {
	var cursor int64
	err := t.tx.GetContext(ctx, &cursor, `
		SELECT COALESCE(MAX(sequence), 0)
		FROM event_outbox
		WHERE event_type NOT IN ('report.updated', 'report.finalized')`)
	return cursor, err
}

func (t *ReportTx) GetSessionSource(ctx context.Context, sessionID string) (*StructuredReportSessionSource, error) {
	var source StructuredReportSessionSource
	err := t.tx.GetContext(ctx, &source, `
		SELECT session.id AS session_id, session.project_id, project.name AS project_name,
		       session.task_id, COALESCE(task.title, '') AS task_title,
		       session.status, session.name, session.start_time, session.end_time,
		       session.last_activity_at, session.plan_content, session.backend
		FROM sessions session
		JOIN projects project ON project.id = session.project_id
		LEFT JOIN project_tasks task ON task.id = session.task_id
		WHERE session.id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	return &source, nil
}

func (t *ReportTx) GetSessionReport(ctx context.Context, sessionID, turnID string) (*StructuredSessionReportRecord, error) {
	var report StructuredSessionReportRecord
	err := t.tx.GetContext(ctx, &report, `
		SELECT * FROM structured_session_reports
		WHERE session_id = ? AND turn_id = ?`, sessionID, turnID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &report, nil
}

func (t *ReportTx) UpsertSessionReport(ctx context.Context, report *StructuredSessionReportRecord) error {
	if report == nil {
		return errors.New("structured session report is required")
	}
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO structured_session_reports (
			report_id, session_id, turn_id, report_date, timezone,
			project_id, project_name, task_id, task_title, session_name,
			session_status, session_started_at, session_ended_at,
			state, incomplete, objective, outcome, work_summary,
			decisions_json, verification_json, evidence_json,
			completed_task_ids_json, changed_task_ids_json, incomplete_reasons_json,
			through_cursor, turn_started_at, turn_ended_at, finalized_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			CASE WHEN ? = 'finalized' THEN CURRENT_TIMESTAMP ELSE NULL END)
		ON CONFLICT(session_id, turn_id) DO UPDATE SET
			report_date = excluded.report_date,
			timezone = excluded.timezone,
			project_id = excluded.project_id,
			project_name = excluded.project_name,
			task_id = excluded.task_id,
			task_title = excluded.task_title,
			session_name = excluded.session_name,
			session_status = excluded.session_status,
			session_started_at = excluded.session_started_at,
			session_ended_at = excluded.session_ended_at,
			state = CASE
				WHEN structured_session_reports.state = 'finalized' THEN 'finalized'
				ELSE excluded.state
			END,
			incomplete = excluded.incomplete,
			objective = excluded.objective,
			outcome = excluded.outcome,
			work_summary = excluded.work_summary,
			decisions_json = excluded.decisions_json,
			verification_json = excluded.verification_json,
			evidence_json = excluded.evidence_json,
			completed_task_ids_json = excluded.completed_task_ids_json,
			changed_task_ids_json = excluded.changed_task_ids_json,
			incomplete_reasons_json = excluded.incomplete_reasons_json,
			through_cursor = MAX(structured_session_reports.through_cursor, excluded.through_cursor),
			turn_started_at = excluded.turn_started_at,
			turn_ended_at = excluded.turn_ended_at,
			updated_at = CURRENT_TIMESTAMP,
			finalized_at = CASE
				WHEN excluded.state = 'finalized' THEN COALESCE(structured_session_reports.finalized_at, CURRENT_TIMESTAMP)
				ELSE structured_session_reports.finalized_at
			END`,
		report.ReportID, report.SessionID, report.TurnID, report.ReportDate, report.Timezone,
		report.ProjectID, report.ProjectName, report.TaskID, report.TaskTitle, report.SessionName,
		report.SessionStatus, report.SessionStartedAt, report.SessionEndedAt,
		report.State, report.Incomplete, report.Objective, report.Outcome, report.WorkSummary,
		report.DecisionsJSON, report.VerificationJSON, report.EvidenceJSON,
		report.CompletedTaskIDsJSON, report.ChangedTaskIDsJSON, report.IncompleteReasonsJSON,
		report.ThroughCursor, report.TurnStartedAt, report.TurnEndedAt, report.State)
	return err
}

func (t *ReportTx) ListSessionsForWindow(ctx context.Context, start, end time.Time) ([]StructuredReportSessionSource, error) {
	var sessions []StructuredReportSessionSource
	err := t.tx.SelectContext(ctx, &sessions, `
		SELECT session.id AS session_id, session.project_id, project.name AS project_name,
		       session.task_id, COALESCE(task.title, '') AS task_title,
		       session.status, session.name, session.start_time, session.end_time,
		       session.last_activity_at, session.plan_content, session.backend
		FROM sessions session
		JOIN projects project ON project.id = session.project_id
		LEFT JOIN project_tasks task ON task.id = session.task_id
		WHERE session.start_time < ?
		  AND (
			COALESCE(session.end_time, session.last_activity_at, session.start_time) >= ?
			OR session.status IN ('starting', 'running')
		  )
		ORDER BY session.start_time ASC, session.id ASC`, end, start)
	if sessions == nil {
		sessions = []StructuredReportSessionSource{}
	}
	return sessions, err
}

func (t *ReportTx) ListSessionReportsForWindow(ctx context.Context, start, end time.Time) ([]StructuredSessionReportRecord, error) {
	var reports []StructuredSessionReportRecord
	err := t.tx.SelectContext(ctx, &reports, `
		SELECT * FROM structured_session_reports
		WHERE turn_started_at < ?
		  AND (
			COALESCE(turn_ended_at, session_ended_at, turn_started_at) >= ?
			OR session_status IN ('starting', 'running')
		  )
		ORDER BY session_started_at ASC, session_id ASC, turn_started_at ASC, turn_id ASC`, end, start)
	if reports == nil {
		reports = []StructuredSessionReportRecord{}
	}
	return reports, err
}

func (t *ReportTx) ListTaskActivityForWindow(ctx context.Context, start, end time.Time) ([]StructuredReportTaskActivity, error) {
	var activity []StructuredReportTaskActivity
	err := t.tx.SelectContext(ctx, &activity, `
		SELECT task_id, project_id, event_type, details, session_id, created_at
		FROM task_history
		WHERE created_at >= ? AND created_at < ?
		ORDER BY created_at ASC, id ASC`, start, end)
	if activity == nil {
		activity = []StructuredReportTaskActivity{}
	}
	return activity, err
}

func (t *ReportTx) ListTaskActivityForSession(ctx context.Context, sessionID string) ([]StructuredReportTaskActivity, error) {
	var activity []StructuredReportTaskActivity
	err := t.tx.SelectContext(ctx, &activity, `
		SELECT task_id, project_id, event_type, details, session_id, created_at
		FROM task_history
		WHERE session_id = ?
		ORDER BY created_at ASC, id ASC`, sessionID)
	if activity == nil {
		activity = []StructuredReportTaskActivity{}
	}
	return activity, err
}

func (t *ReportTx) ListSessionDocumentRefs(ctx context.Context, sessionID string) ([]StructuredReportDocumentRef, error) {
	var documents []StructuredReportDocumentRef
	err := t.tx.SelectContext(ctx, &documents, `
		SELECT id, title, status
		FROM temp_documents
		WHERE session_id = ?
		ORDER BY created_at ASC, id ASC`, sessionID)
	if documents == nil {
		documents = []StructuredReportDocumentRef{}
	}
	return documents, err
}

func (t *ReportTx) CountCodexTranscriptEvents(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := t.tx.GetContext(ctx, &count,
		"SELECT COUNT(*) FROM codex_transcript_events WHERE session_id = ?", sessionID)
	return count, err
}

func (t *ReportTx) GetDailyReport(ctx context.Context, reportDate string) (*StructuredDailyReportRecord, error) {
	var report StructuredDailyReportRecord
	err := t.tx.GetContext(ctx, &report,
		"SELECT * FROM structured_daily_reports WHERE report_date = ?", reportDate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &report, nil
}

func (t *ReportTx) UpsertDailyReport(ctx context.Context, report *StructuredDailyReportRecord) error {
	if report == nil {
		return errors.New("structured daily report is required")
	}
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO structured_daily_reports (
			report_date, timezone, through_cursor, report_json, content_sha256,
			session_count, incomplete_session_count, generated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(report_date) DO UPDATE SET
			timezone = excluded.timezone,
			through_cursor = excluded.through_cursor,
			report_json = excluded.report_json,
			content_sha256 = excluded.content_sha256,
			session_count = excluded.session_count,
			incomplete_session_count = excluded.incomplete_session_count,
			generated_at = excluded.generated_at,
			updated_at = CURRENT_TIMESTAMP`,
		report.ReportDate, report.Timezone, report.ThroughCursor, report.ReportJSON,
		report.ContentSHA256, report.SessionCount, report.IncompleteSessionCount,
		report.GeneratedAt)
	return err
}

func (d *DB) GetStructuredDailyReport(ctx context.Context, reportDate string) (*StructuredDailyReportRecord, error) {
	var report StructuredDailyReportRecord
	err := d.GetContext(ctx, &report,
		"SELECT * FROM structured_daily_reports WHERE report_date = ?", reportDate)
	if err != nil {
		return nil, err
	}
	return &report, nil
}
