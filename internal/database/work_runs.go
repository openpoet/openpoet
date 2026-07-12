package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	WorkRunStatusRunning       = "running"
	WorkRunStatusPaused        = "paused"
	WorkRunStatusCompleted     = "completed"
	WorkRunStatusCancelled     = "cancelled"
	WorkRunStatusIndeterminate = "indeterminate"
)

var (
	ErrWorkRunVersionConflict = errors.New("work run version conflict")
	ErrPlanVersionConflict    = errors.New("plan version conflict")
)

type WorkRunExecutionTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type WorkRun struct {
	ID                       string                  `db:"id" json:"id"`
	Title                    string                  `db:"title" json:"title"`
	Description              string                  `db:"description" json:"description"`
	Source                   string                  `db:"source" json:"source"`
	Status                   string                  `db:"status" json:"status"`
	ExpectedMinutes          int                     `db:"expected_minutes" json:"expected_minutes"`
	AccumulatedActiveSeconds int64                   `db:"accumulated_active_seconds" json:"accumulated_active_seconds"`
	ActiveSeconds            int64                   `db:"-" json:"active_seconds"`
	StartedAt                time.Time               `db:"started_at" json:"started_at"`
	ActiveStartedAt          *time.Time              `db:"active_started_at" json:"active_started_at,omitempty"`
	PausedAt                 *time.Time              `db:"paused_at" json:"paused_at,omitempty"`
	CompletedAt              *time.Time              `db:"completed_at" json:"completed_at,omitempty"`
	CancelledAt              *time.Time              `db:"cancelled_at" json:"cancelled_at,omitempty"`
	ProjectTaskID            *int64                  `db:"project_task_id" json:"project_task_id,omitempty"`
	SessionID                *string                 `db:"session_id" json:"session_id,omitempty"`
	PlanItemRef              *string                 `db:"plan_item_ref" json:"plan_item_ref,omitempty"`
	ExecutionTargetType      string                  `db:"execution_target_type" json:"-"`
	ExecutionTargetID        string                  `db:"execution_target_id" json:"-"`
	ExecutionTarget          *WorkRunExecutionTarget `db:"-" json:"execution_target,omitempty"`
	Version                  int64                   `db:"version" json:"version"`
	CreatedByActor           string                  `db:"created_by_actor" json:"created_by_actor"`
	UpdatedByActor           string                  `db:"updated_by_actor" json:"updated_by_actor"`
	CorrelationID            string                  `db:"correlation_id" json:"correlation_id,omitempty"`
	IdempotencyKey           *string                 `db:"idempotency_key" json:"-"`
	CreatedAt                time.Time               `db:"created_at" json:"created_at"`
	UpdatedAt                time.Time               `db:"updated_at" json:"updated_at"`
}

func (r *WorkRun) Hydrate(now time.Time) {
	if r == nil {
		return
	}
	r.ActiveSeconds = r.AccumulatedActiveSeconds
	if r.Status == WorkRunStatusRunning && r.ActiveStartedAt != nil && now.After(*r.ActiveStartedAt) {
		r.ActiveSeconds += int64(now.Sub(*r.ActiveStartedAt) / time.Second)
	}
	if r.ExecutionTargetType != "" || r.ExecutionTargetID != "" {
		r.ExecutionTarget = &WorkRunExecutionTarget{Type: r.ExecutionTargetType, ID: r.ExecutionTargetID}
	} else {
		r.ExecutionTarget = nil
	}
}

type WorkRunAudit struct {
	ID             int64     `db:"id" json:"id"`
	WorkRunID      string    `db:"work_run_id" json:"work_run_id"`
	Action         string    `db:"action" json:"action"`
	FromStatus     string    `db:"from_status" json:"from_status"`
	ToStatus       string    `db:"to_status" json:"to_status"`
	Actor          string    `db:"actor" json:"actor"`
	CorrelationID  string    `db:"correlation_id" json:"correlation_id,omitempty"`
	IdempotencyKey *string   `db:"idempotency_key" json:"-"`
	Version        int64     `db:"version" json:"version"`
	DetailsJSON    string    `db:"details_json" json:"details"`
	OccurredAt     time.Time `db:"occurred_at" json:"occurred_at"`
}

type WorkRunFilter struct {
	Status     string
	Source     string
	ActiveOnly bool
	Limit      int
}

type Plan struct {
	ID             string     `db:"id" json:"id"`
	ExternalRef    string     `db:"external_ref" json:"external_ref"`
	Kind           string     `db:"kind" json:"kind"`
	Title          string     `db:"title" json:"title"`
	PeriodStart    string     `db:"period_start" json:"period_start"`
	PeriodEnd      string     `db:"period_end" json:"period_end"`
	Timezone       string     `db:"timezone" json:"timezone"`
	Version        int64      `db:"version" json:"version"`
	CreatedByActor string     `db:"created_by_actor" json:"created_by_actor"`
	UpdatedByActor string     `db:"updated_by_actor" json:"updated_by_actor"`
	CorrelationID  string     `db:"correlation_id" json:"correlation_id,omitempty"`
	IdempotencyKey *string    `db:"idempotency_key" json:"-"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
	Items          []PlanItem `db:"-" json:"items,omitempty"`
}

type PlanItem struct {
	ID             string    `db:"id" json:"id"`
	PlanID         string    `db:"plan_id" json:"plan_id"`
	ExternalRef    string    `db:"external_ref" json:"external_ref"`
	Title          string    `db:"title" json:"title"`
	Description    string    `db:"description" json:"description"`
	Status         string    `db:"status" json:"status"`
	SortOrder      int       `db:"sort_order" json:"sort_order"`
	ProjectTaskID  *int64    `db:"project_task_id" json:"project_task_id,omitempty"`
	Version        int64     `db:"version" json:"version"`
	UpdatedByActor string    `db:"updated_by_actor" json:"updated_by_actor"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

type PlanFilter struct {
	Kind        string
	ExternalRef string
	From        string
	To          string
	Limit       int
}

type WorkRunTx struct {
	tx *sqlx.Tx
}

func (d *DB) WithWorkRunTx(ctx context.Context, fn func(*WorkRunTx) error) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&WorkRunTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *WorkRunTx) GetWorkRun(ctx context.Context, id string) (*WorkRun, error) {
	var run WorkRun
	if err := t.tx.GetContext(ctx, &run, "SELECT * FROM work_runs WHERE id = ?", id); err != nil {
		return nil, err
	}
	return &run, nil
}

func (t *WorkRunTx) FindStartedWorkRun(ctx context.Context, actor, idempotencyKey string) (*WorkRun, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, nil
	}
	var run WorkRun
	err := t.tx.GetContext(ctx, &run, `
		SELECT * FROM work_runs
		WHERE created_by_actor = ? AND idempotency_key = ?`, actor, idempotencyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (t *WorkRunTx) InsertWorkRun(ctx context.Context, run *WorkRun) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO work_runs (
			id, title, description, source, status, expected_minutes,
			accumulated_active_seconds, started_at, active_started_at, paused_at,
			completed_at, cancelled_at, project_task_id, session_id, plan_item_ref,
			execution_target_type, execution_target_id, version, created_by_actor,
			updated_by_actor, correlation_id, idempotency_key, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Title, run.Description, run.Source, run.Status, run.ExpectedMinutes,
		run.AccumulatedActiveSeconds, run.StartedAt, run.ActiveStartedAt, run.PausedAt,
		run.CompletedAt, run.CancelledAt, run.ProjectTaskID, run.SessionID, run.PlanItemRef,
		run.ExecutionTargetType, run.ExecutionTargetID, run.Version, run.CreatedByActor,
		run.UpdatedByActor, run.CorrelationID, run.IdempotencyKey, run.CreatedAt, run.UpdatedAt)
	return err
}

func (t *WorkRunTx) UpdateWorkRun(ctx context.Context, run *WorkRun, previousVersion int64) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE work_runs SET
			status = ?, accumulated_active_seconds = ?, active_started_at = ?, paused_at = ?,
			completed_at = ?, cancelled_at = ?, version = ?, updated_by_actor = ?,
			correlation_id = ?, updated_at = ?
		WHERE id = ? AND version = ?`,
		run.Status, run.AccumulatedActiveSeconds, run.ActiveStartedAt, run.PausedAt,
		run.CompletedAt, run.CancelledAt, run.Version, run.UpdatedByActor,
		run.CorrelationID, run.UpdatedAt, run.ID, previousVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrWorkRunVersionConflict
	}
	return nil
}

func (t *WorkRunTx) GetAuditByIdempotency(ctx context.Context, runID, key string) (*WorkRunAudit, error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	var audit WorkRunAudit
	err := t.tx.GetContext(ctx, &audit, `
		SELECT * FROM work_run_audit WHERE work_run_id = ? AND idempotency_key = ?`, runID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &audit, nil
}

func (t *WorkRunTx) InsertAudit(ctx context.Context, audit *WorkRunAudit) error {
	result, err := t.tx.ExecContext(ctx, `
		INSERT INTO work_run_audit (
			work_run_id, action, from_status, to_status, actor, correlation_id,
			idempotency_key, version, details_json, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		audit.WorkRunID, audit.Action, audit.FromStatus, audit.ToStatus, audit.Actor,
		audit.CorrelationID, audit.IdempotencyKey, audit.Version, audit.DetailsJSON, audit.OccurredAt)
	if err != nil {
		return err
	}
	audit.ID, err = result.LastInsertId()
	return err
}

func (t *WorkRunTx) GetProjectTask(ctx context.Context, id int64) (*ProjectTask, error) {
	return (&ProjectTaskTx{tx: t.tx}).GetTask(ctx, id)
}

func (t *WorkRunTx) GetSession(ctx context.Context, id string) (*Session, error) {
	return (&ProjectTaskTx{tx: t.tx}).GetSession(ctx, id)
}

func (t *WorkRunTx) ChangeProjectTaskStatus(ctx context.Context, id int64, status string) error {
	return changeTaskStatusInTx(ctx, t.tx, id, status)
}

func (t *WorkRunTx) CreateProjectTaskHistory(ctx context.Context, history *TaskHistory) error {
	return (&ProjectTaskTx{tx: t.tx}).CreateHistory(ctx, history)
}

func (t *WorkRunTx) AppendEvent(ctx context.Context, input EventOutboxAppend) (*EventOutboxEvent, error) {
	return AppendEventOutbox(ctx, t.tx, input)
}

func (t *WorkRunTx) GetPlanItemByRef(ctx context.Context, externalRef string) (*PlanItem, error) {
	var item PlanItem
	if err := t.tx.GetContext(ctx, &item, "SELECT * FROM plan_items WHERE external_ref = ?", externalRef); err != nil {
		return nil, err
	}
	return &item, nil
}

func (t *WorkRunTx) GetPlanByExternalRef(ctx context.Context, externalRef string) (*Plan, error) {
	var plan Plan
	if err := t.tx.GetContext(ctx, &plan, "SELECT * FROM plans WHERE external_ref = ?", externalRef); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (t *WorkRunTx) GetPlanByID(ctx context.Context, id string) (*Plan, error) {
	var plan Plan
	if err := t.tx.GetContext(ctx, &plan, "SELECT * FROM plans WHERE id = ?", id); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (t *WorkRunTx) InsertPlan(ctx context.Context, plan *Plan) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO plans (
			id, external_ref, kind, title, period_start, period_end, timezone, version,
			created_by_actor, updated_by_actor, correlation_id, idempotency_key, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.ID, plan.ExternalRef, plan.Kind, plan.Title, plan.PeriodStart, plan.PeriodEnd,
		plan.Timezone, plan.Version, plan.CreatedByActor, plan.UpdatedByActor,
		plan.CorrelationID, plan.IdempotencyKey, plan.CreatedAt, plan.UpdatedAt)
	return err
}

func (t *WorkRunTx) UpdatePlan(ctx context.Context, plan *Plan, previousVersion int64) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE plans SET kind = ?, title = ?, period_start = ?, period_end = ?, timezone = ?,
			version = ?, updated_by_actor = ?, correlation_id = ?, idempotency_key = ?, updated_at = ?
		WHERE id = ? AND version = ?`,
		plan.Kind, plan.Title, plan.PeriodStart, plan.PeriodEnd, plan.Timezone,
		plan.Version, plan.UpdatedByActor, plan.CorrelationID, plan.IdempotencyKey,
		plan.UpdatedAt, plan.ID, previousVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrPlanVersionConflict
	}
	return nil
}

func (t *WorkRunTx) InsertPlanItem(ctx context.Context, item *PlanItem) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO plan_items (
			id, plan_id, external_ref, title, description, status, sort_order,
			project_task_id, version, updated_by_actor, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.PlanID, item.ExternalRef, item.Title, item.Description, item.Status,
		item.SortOrder, item.ProjectTaskID, item.Version, item.UpdatedByActor,
		item.CreatedAt, item.UpdatedAt)
	return err
}

func (t *WorkRunTx) UpdatePlanItem(ctx context.Context, item *PlanItem, previousVersion int64) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE plan_items SET title = ?, description = ?, status = ?, sort_order = ?,
			project_task_id = ?, version = ?, updated_by_actor = ?, updated_at = ?
		WHERE id = ? AND version = ?`,
		item.Title, item.Description, item.Status, item.SortOrder, item.ProjectTaskID,
		item.Version, item.UpdatedByActor, item.UpdatedAt, item.ID, previousVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrPlanVersionConflict
	}
	return nil
}

func (t *WorkRunTx) ListPlanItems(ctx context.Context, planID string) ([]PlanItem, error) {
	var items []PlanItem
	if err := t.tx.SelectContext(ctx, &items,
		"SELECT * FROM plan_items WHERE plan_id = ? ORDER BY sort_order, created_at", planID); err != nil {
		return nil, err
	}
	if items == nil {
		items = []PlanItem{}
	}
	return items, nil
}

func (d *DB) GetWorkRun(ctx context.Context, id string) (*WorkRun, error) {
	var run WorkRun
	if err := d.GetContext(ctx, &run, "SELECT * FROM work_runs WHERE id = ?", id); err != nil {
		return nil, err
	}
	run.Hydrate(time.Now())
	return &run, nil
}

func (d *DB) ListWorkRuns(ctx context.Context, filter WorkRunFilter) ([]WorkRun, error) {
	query := "SELECT * FROM work_runs WHERE 1=1"
	args := []any{}
	if filter.ActiveOnly {
		query += " AND status IN ('running','paused','indeterminate')"
	} else if strings.TrimSpace(filter.Status) != "" {
		query += " AND status = ?"
		args = append(args, strings.TrimSpace(filter.Status))
	}
	if strings.TrimSpace(filter.Source) != "" {
		query += " AND source = ?"
		args = append(args, strings.TrimSpace(filter.Source))
	}
	query += " ORDER BY CASE status WHEN 'running' THEN 0 WHEN 'paused' THEN 1 WHEN 'indeterminate' THEN 2 ELSE 3 END, started_at DESC"
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		return nil, errors.New("work run limit cannot exceed 1000")
	}
	query += " LIMIT ?"
	args = append(args, limit)
	var runs []WorkRun
	if err := d.SelectContext(ctx, &runs, query, args...); err != nil {
		return nil, err
	}
	if runs == nil {
		runs = []WorkRun{}
	}
	now := time.Now()
	for index := range runs {
		runs[index].Hydrate(now)
	}
	return runs, nil
}

func (d *DB) ListWorkRunAudit(ctx context.Context, runID string, limit int) ([]WorkRunAudit, error) {
	if limit <= 0 {
		limit = 100
	}
	var audit []WorkRunAudit
	if err := d.SelectContext(ctx, &audit, `
		SELECT * FROM work_run_audit WHERE work_run_id = ? ORDER BY id DESC LIMIT ?`, runID, limit); err != nil {
		return nil, err
	}
	if audit == nil {
		audit = []WorkRunAudit{}
	}
	return audit, nil
}

func (d *DB) ListPlans(ctx context.Context, filter PlanFilter) ([]Plan, error) {
	query := "SELECT * FROM plans WHERE 1=1"
	args := []any{}
	if filter.Kind != "" {
		query += " AND kind = ?"
		args = append(args, filter.Kind)
	}
	if filter.ExternalRef != "" {
		query += " AND external_ref = ?"
		args = append(args, filter.ExternalRef)
	}
	if filter.From != "" {
		query += " AND period_end >= ?"
		args = append(args, filter.From)
	}
	if filter.To != "" {
		query += " AND period_start <= ?"
		args = append(args, filter.To)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		return nil, errors.New("plan limit cannot exceed 500")
	}
	query += " ORDER BY period_start DESC, created_at DESC LIMIT ?"
	args = append(args, limit)
	var plans []Plan
	if err := d.SelectContext(ctx, &plans, query, args...); err != nil {
		return nil, err
	}
	if plans == nil {
		plans = []Plan{}
	}
	for index := range plans {
		items, err := d.ListPlanItems(ctx, plans[index].ID)
		if err != nil {
			return nil, err
		}
		plans[index].Items = items
	}
	return plans, nil
}

func (d *DB) GetPlan(ctx context.Context, idOrExternalRef string) (*Plan, error) {
	var plan Plan
	if err := d.GetContext(ctx, &plan,
		"SELECT * FROM plans WHERE id = ? OR external_ref = ?", idOrExternalRef, idOrExternalRef); err != nil {
		return nil, err
	}
	items, err := d.ListPlanItems(ctx, plan.ID)
	if err != nil {
		return nil, err
	}
	plan.Items = items
	return &plan, nil
}

func (d *DB) ListPlanItems(ctx context.Context, planIDOrExternalRef string) ([]PlanItem, error) {
	var items []PlanItem
	if err := d.SelectContext(ctx, &items, `
		SELECT item.* FROM plan_items item
		JOIN plans plan ON plan.id = item.plan_id
		WHERE plan.id = ? OR plan.external_ref = ?
		ORDER BY item.sort_order, item.created_at`, planIDOrExternalRef, planIDOrExternalRef); err != nil {
		return nil, err
	}
	if items == nil {
		items = []PlanItem{}
	}
	return items, nil
}
