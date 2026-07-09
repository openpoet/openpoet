package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/google/uuid"

	"openpoet/internal/database"
)

const (
	DefaultReportTimezone  = "America/Sao_Paulo"
	ReportEventUpdated     = "report.updated"
	ReportEventFinalized   = "report.finalized"
	maxDailyReportSessions = 1000
	maxDailyReportTurns    = 10000
	maxDailyReportBytes    = 16 << 20
)

var reportSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s]+`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret)\b\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

type ReportStore interface {
	WithReportTx(ctx context.Context, fn func(*database.ReportTx) error) error
}

type ReportServiceOptions struct {
	Location *time.Location
	Now      func() time.Time
}

type ReportService struct {
	store    ReportStore
	location *time.Location
	now      func() time.Time
}

func NewReportService(store ReportStore, options ...ReportServiceOptions) (*ReportService, error) {
	config := ReportServiceOptions{Now: time.Now}
	if len(options) > 0 {
		config = options[0]
		if config.Now == nil {
			config.Now = time.Now
		}
	}
	if config.Location == nil {
		location, err := time.LoadLocation(DefaultReportTimezone)
		if err != nil {
			return nil, err
		}
		config.Location = location
	}
	return &ReportService{store: store, location: config.Location, now: config.Now}, nil
}

type ReportEvidence struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Label string `json:"label,omitempty"`
}

type ReportVerification struct {
	Status       string   `json:"status"`
	Summary      string   `json:"summary,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type UpsertTurnReportCommand struct {
	SessionID         string
	TurnID            string
	Objective         string
	Outcome           string
	WorkSummary       string
	Decisions         []string
	Verification      ReportVerification
	Evidence          []ReportEvidence
	CompletedTaskIDs  []int64
	ChangedTaskIDs    []int64
	Incomplete        bool
	IncompleteReasons []string
	ThroughCursor     int64
	StartedAt         time.Time
	EndedAt           *time.Time
	Actor             Actor
	CorrelationID     string
}

type TurnReport struct {
	ReportID          string             `json:"report_id"`
	SessionID         string             `json:"session_id"`
	TurnID            string             `json:"turn_id"`
	State             string             `json:"state"`
	Incomplete        bool               `json:"incomplete"`
	Objective         string             `json:"objective"`
	Outcome           string             `json:"outcome"`
	WorkSummary       string             `json:"work_summary"`
	Decisions         []string           `json:"decisions"`
	Verification      ReportVerification `json:"verification"`
	Evidence          []ReportEvidence   `json:"evidence"`
	CompletedTaskIDs  []int64            `json:"completed_task_ids"`
	ChangedTaskIDs    []int64            `json:"changed_task_ids"`
	IncompleteReasons []string           `json:"incomplete_reasons"`
	ThroughCursor     int64              `json:"through_cursor"`
	StartedAt         time.Time          `json:"started_at"`
	EndedAt           *time.Time         `json:"ended_at,omitempty"`
	UpdatedAt         time.Time          `json:"updated_at"`
	FinalizedAt       *time.Time         `json:"finalized_at,omitempty"`
}

type DailySessionReport struct {
	SessionID         string           `json:"session_id"`
	ProjectID         int64            `json:"project_id"`
	ProjectName       string           `json:"project_name"`
	Title             string           `json:"title"`
	TaskID            *int64           `json:"task_id,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	EndedAt           *time.Time       `json:"ended_at,omitempty"`
	Status            string           `json:"status"`
	Objective         string           `json:"objective,omitempty"`
	Outcome           string           `json:"outcome"`
	Decisions         []string         `json:"decisions"`
	WorkSummary       []string         `json:"work_summary"`
	Verification      []string         `json:"verification"`
	Evidence          []ReportEvidence `json:"evidence"`
	Incomplete        bool             `json:"incomplete,omitempty"`
	LastActivityAt    *time.Time       `json:"-"`
	IncompleteReasons []string         `json:"-"`
	ThroughCursor     int64            `json:"-"`
	CompletedTaskIDs  []int64          `json:"-"`
	ChangedTaskIDs    []int64          `json:"-"`
	Turns             []TurnReport     `json:"-"`
}

type DailyReport struct {
	SchemaVersion     int                  `json:"schema_version"`
	ReportID          string               `json:"report_id"`
	Date              string               `json:"date"`
	GeneratedAt       time.Time            `json:"generated_at"`
	ThroughCursor     string               `json:"through_cursor"`
	Sessions          []DailySessionReport `json:"sessions"`
	CompletedTaskIDs  []int64              `json:"completed_task_ids"`
	ChangedTaskIDs    []int64              `json:"changed_task_ids"`
	Decisions         []string             `json:"decisions"`
	IncompleteReasons []string             `json:"incomplete_reasons"`
	Timezone          string               `json:"-"`
}

func (s *ReportService) UpdateTurn(ctx context.Context, command UpsertTurnReportCommand) (*TurnReport, error) {
	return s.upsertTurn(ctx, command, "updated")
}

func (s *ReportService) FinalizeTurn(ctx context.Context, command UpsertTurnReportCommand) (*TurnReport, error) {
	return s.upsertTurn(ctx, command, "finalized")
}

func (s *ReportService) CaptureSessionLifecycle(ctx context.Context, sessionID string) (*TurnReport, error) {
	return s.captureSessionLifecycle(ctx, sessionID, "", Actor{Type: "system"})
}

func (s *ReportService) EnrichSessionSummary(ctx context.Context, sessionID, summary string) (*TurnReport, error) {
	return s.captureSessionLifecycle(ctx, sessionID, summary, Actor{Type: "ai"})
}

func (s *ReportService) captureSessionLifecycle(
	ctx context.Context,
	sessionID string,
	summary string,
	actor Actor,
) (*TurnReport, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, validationError("report_target_required", "session_id is required")
	}
	type lifecycleSource struct {
		session         *database.StructuredReportSessionSource
		activity        []database.StructuredReportTaskActivity
		documents       []database.StructuredReportDocumentRef
		transcriptCount int
		throughCursor   int64
	}
	var source lifecycleSource
	if err := s.store.WithReportTx(ctx, func(tx *database.ReportTx) error {
		var err error
		source.session, err = tx.GetSessionSource(ctx, sessionID)
		if err != nil {
			return err
		}
		source.activity, err = tx.ListTaskActivityForSession(ctx, sessionID)
		if err != nil {
			return err
		}
		source.documents, err = tx.ListSessionDocumentRefs(ctx, sessionID)
		if err != nil {
			return err
		}
		source.transcriptCount, err = tx.CountCodexTranscriptEvents(ctx, sessionID)
		if err != nil {
			return err
		}
		source.throughCursor, err = tx.SnapshotEventOutboxCursor(ctx)
		return err
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundError("session_not_found", "Session not found", err)
		}
		return nil, err
	}

	objective := source.session.Name
	if source.session.TaskID.Valid && source.session.TaskTitle != "" {
		objective = source.session.TaskTitle
	}
	completed := make([]int64, 0)
	changed := make([]int64, 0)
	eventTypes := make([]string, 0, len(source.activity))
	persistedSummary := ""
	for _, activity := range source.activity {
		changed = append(changed, activity.TaskID)
		if taskActivityCompleted(activity) {
			completed = append(completed, activity.TaskID)
		}
		eventTypes = append(eventTypes, activity.EventType)
		if activity.EventType == "session_ended" {
			var details struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal([]byte(activity.Details), &details) == nil && strings.TrimSpace(details.Summary) != "" {
				persistedSummary = details.Summary
			}
		}
	}
	if strings.TrimSpace(summary) == "" {
		summary = persistedSummary
	}
	if strings.TrimSpace(summary) == "" {
		uniqueEvents, _ := normalizeReportStrings(eventTypes, 200, true)
		if len(uniqueEvents) > 0 {
			summary = "Persisted lifecycle activity: " + strings.Join(uniqueEvents, ", ") + "."
		} else {
			summary = "Session lifecycle persisted without a generated summary."
		}
	}

	evidence := []ReportEvidence{{Kind: "session", Ref: "openpoet:session:" + sessionID}}
	if source.session.TaskID.Valid {
		evidence = append(evidence, ReportEvidence{
			Kind: "task", Ref: "openpoet:task:" + strconv.FormatInt(source.session.TaskID.Int64, 10),
		})
	}
	if strings.TrimSpace(source.session.PlanContent) != "" {
		evidence = append(evidence, ReportEvidence{
			Kind: "document", Ref: "openpoet:session:" + sessionID + ":plan", Label: "Persisted session plan",
		})
	}
	if source.transcriptCount > 0 {
		evidence = append(evidence, ReportEvidence{
			Kind: "session", Ref: "openpoet:session:" + sessionID + ":transcript",
			Label: fmt.Sprintf("%d persisted structured transcript events", source.transcriptCount),
		})
	}
	for _, document := range source.documents {
		evidence = append(evidence, ReportEvidence{
			Kind: "document", Ref: "openpoet:document:" + document.ID,
			Label: document.Title + " (" + document.Status + ")",
		})
	}

	incomplete := false
	reasons := []string{}
	verificationStatus := "partial"
	verificationSummary := "Lifecycle state persisted; no explicit verification result was recorded."
	outcome := "Session completed."
	stateFinal := true
	switch source.session.Status {
	case "starting", "running":
		stateFinal = false
		incomplete = true
		reasons = append(reasons, "session_restart_pending")
		outcome = "Session remains active and is expected to resume."
	case "stopped":
		incomplete = true
		reasons = append(reasons, "session_stopped_by_user")
		outcome = "Session stopped by user."
	case "error":
		incomplete = true
		reasons = append(reasons, "session_error")
		verificationStatus = "failed"
		verificationSummary = "Session ended with an error."
		outcome = "Session ended with an error."
	}
	if strings.TrimSpace(persistedSummary) == "" && strings.TrimSpace(summary) != "" && actor.Type != "ai" {
		incomplete = true
		reasons = append(reasons, "structured_summary_fallback")
	}
	if source.transcriptCount == 0 && source.session.Backend == "codex" {
		incomplete = true
		reasons = append(reasons, "durable_transcript_unavailable")
	}
	command := UpsertTurnReportCommand{
		SessionID: sessionID, TurnID: "session-lifecycle", Objective: objective,
		Outcome: outcome, WorkSummary: summary,
		Verification: ReportVerification{
			Status: verificationStatus, Summary: verificationSummary,
		},
		Evidence: evidence, CompletedTaskIDs: completed, ChangedTaskIDs: changed,
		Incomplete: incomplete, IncompleteReasons: reasons,
		ThroughCursor: source.throughCursor, StartedAt: source.session.StartTime,
		EndedAt: nullTimePointer(source.session.EndTime), Actor: actor, CorrelationID: sessionID,
	}
	if stateFinal {
		return s.FinalizeTurn(ctx, command)
	}
	return s.UpdateTurn(ctx, command)
}

func (s *ReportService) upsertTurn(ctx context.Context, command UpsertTurnReportCommand, state string) (*TurnReport, error) {
	normalized, err := normalizeTurnReportCommand(command)
	if err != nil {
		return nil, err
	}
	var result *TurnReport
	err = s.store.WithReportTx(ctx, func(tx *database.ReportTx) error {
		source, err := tx.GetSessionSource(ctx, normalized.SessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFoundError("session_not_found", "Session not found", err)
			}
			return err
		}
		snapshot, err := tx.SnapshotEventOutboxCursor(ctx)
		if err != nil {
			return err
		}
		if normalized.ThroughCursor > snapshot {
			return validationError("report_cursor_ahead", "through_cursor is ahead of the durable event snapshot")
		}
		existing, err := tx.GetSessionReport(ctx, normalized.SessionID, normalized.TurnID)
		if err != nil {
			return err
		}
		if existing != nil && existing.State == "finalized" && state != "finalized" {
			return conflictError("report_already_finalized", "A finalized turn report cannot return to updated state")
		}

		startedAt := normalized.StartedAt
		if startedAt.IsZero() {
			startedAt = source.StartTime
		}
		if normalized.EndedAt != nil && normalized.EndedAt.Before(startedAt) {
			return validationError("report_time_invalid", "ended_at cannot be before started_at")
		}
		reportID := uuid.NewString()
		if existing != nil {
			reportID = existing.ReportID
		}
		decisionsJSON, _ := json.Marshal(normalized.Decisions)
		verificationJSON, _ := json.Marshal(normalized.Verification)
		evidenceJSON, _ := json.Marshal(normalized.Evidence)
		completedJSON, _ := json.Marshal(normalized.CompletedTaskIDs)
		changedJSON, _ := json.Marshal(normalized.ChangedTaskIDs)
		reasonsJSON, _ := json.Marshal(normalized.IncompleteReasons)
		record := &database.StructuredSessionReportRecord{
			ReportID: reportID, SessionID: normalized.SessionID, TurnID: normalized.TurnID,
			ReportDate: source.StartTime.In(s.location).Format(time.DateOnly), Timezone: s.location.String(),
			ProjectID: source.ProjectID, ProjectName: source.ProjectName,
			TaskID: source.TaskID, TaskTitle: source.TaskTitle, SessionName: source.Name, SessionStatus: source.Status,
			SessionStartedAt: source.StartTime, SessionEndedAt: source.EndTime,
			State: state, Incomplete: normalized.Incomplete,
			Objective: normalized.Objective, Outcome: normalized.Outcome, WorkSummary: normalized.WorkSummary,
			DecisionsJSON: string(decisionsJSON), VerificationJSON: string(verificationJSON),
			EvidenceJSON: string(evidenceJSON), CompletedTaskIDsJSON: string(completedJSON),
			ChangedTaskIDsJSON: string(changedJSON), IncompleteReasonsJSON: string(reasonsJSON),
			ThroughCursor: normalized.ThroughCursor, TurnStartedAt: startedAt,
		}
		if normalized.EndedAt != nil {
			record.TurnEndedAt = sql.NullTime{Time: *normalized.EndedAt, Valid: true}
		}
		if err := tx.UpsertSessionReport(ctx, record); err != nil {
			return err
		}
		persisted, err := tx.GetSessionReport(ctx, normalized.SessionID, normalized.TurnID)
		if err != nil {
			return err
		}
		result, err = turnReportFromRecord(*persisted)
		if err != nil {
			return err
		}
		metadata := eventMetadataFromContext(ctx)
		actor := normalized.Actor
		if strings.TrimSpace(metadata.Actor.Type) != "" || strings.TrimSpace(metadata.Actor.ID) != "" {
			actor = metadata.Actor
		}
		correlationID := normalized.CorrelationID
		if strings.TrimSpace(metadata.CorrelationID) != "" {
			correlationID = metadata.CorrelationID
		}
		eventType := ReportEventUpdated
		if state == "finalized" {
			eventType = ReportEventFinalized
		}
		payload, _ := json.Marshal(map[string]any{
			"report_id": reportID, "session_id": normalized.SessionID,
			"turn_id": normalized.TurnID, "report_date": record.ReportDate,
			"through_cursor": record.ThroughCursor,
		})
		_, err = tx.AppendEventOutbox(ctx, database.EventOutboxAppend{
			EventID: uuid.NewString(), EventType: eventType, AggregateType: "session_report",
			AggregateID: reportID, Actor: reportActorValue(actor), CorrelationID: correlationID,
			SchemaVersion: 1, PayloadJSON: string(payload), OccurredAt: s.now().UTC(),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *ReportService) MaterializeDaily(ctx context.Context, reportDate string) (*DailyReport, error) {
	dayStart, dayEnd, normalizedDate, err := s.reportWindow(reportDate)
	if err != nil {
		return nil, err
	}
	var result *DailyReport
	err = s.store.WithReportTx(ctx, func(tx *database.ReportTx) error {
		sessions, err := tx.ListSessionsForWindow(ctx, dayStart, dayEnd)
		if err != nil {
			return err
		}
		reports, err := tx.ListSessionReportsForWindow(ctx, dayStart, dayEnd)
		if err != nil {
			return err
		}
		activity, err := tx.ListTaskActivityForWindow(ctx, dayStart, dayEnd)
		if err != nil {
			return err
		}
		throughCursor, err := tx.SnapshotReportSourceCursor(ctx)
		if err != nil {
			return err
		}
		daily, err := buildDailyReport(normalizedDate, s.location.String(), throughCursor, sessions, reports, activity)
		if err != nil {
			return err
		}
		existing, err := tx.GetDailyReport(ctx, normalizedDate)
		if err != nil {
			return err
		}
		if existing != nil {
			daily.GeneratedAt = existing.GeneratedAt
		}
		encoded, err := json.Marshal(daily)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		hash := hex.EncodeToString(digest[:])
		changed := existing == nil || existing.ContentSHA256 != hash
		if changed {
			daily.GeneratedAt = s.now().UTC()
			encoded, err = json.Marshal(daily)
			if err != nil {
				return err
			}
			if len(encoded) > maxDailyReportBytes {
				return validationError("daily_report_too_large", "daily report exceeds the materialization size limit")
			}
			digest = sha256.Sum256(encoded)
			hash = hex.EncodeToString(digest[:])
			incompleteCount := 0
			for _, session := range daily.Sessions {
				if session.Incomplete {
					incompleteCount++
				}
			}
			record := &database.StructuredDailyReportRecord{
				ReportDate: normalizedDate, Timezone: s.location.String(), ThroughCursor: throughCursor,
				ReportJSON: string(encoded), ContentSHA256: hash,
				SessionCount: len(daily.Sessions), IncompleteSessionCount: incompleteCount,
				GeneratedAt: daily.GeneratedAt,
			}
			if err := tx.UpsertDailyReport(ctx, record); err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]any{
				"report_date": normalizedDate, "content_sha256": hash,
				"through_cursor": throughCursor, "session_count": len(daily.Sessions),
			})
			metadata := eventMetadataFromContext(ctx)
			if _, err := tx.AppendEventOutbox(ctx, database.EventOutboxAppend{
				EventID: uuid.NewString(), EventType: ReportEventUpdated, AggregateType: "daily_report",
				AggregateID: normalizedDate, Actor: reportActorValue(metadata.Actor),
				CorrelationID: metadata.CorrelationID, SchemaVersion: 1,
				PayloadJSON: string(payload), OccurredAt: daily.GeneratedAt,
			}); err != nil {
				return err
			}
		}
		result = daily
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *ReportService) reportWindow(reportDate string) (time.Time, time.Time, string, error) {
	reportDate = strings.TrimSpace(reportDate)
	day, err := time.ParseInLocation(time.DateOnly, reportDate, s.location)
	if err != nil {
		return time.Time{}, time.Time{}, "", validationError("report_date_invalid", "date must use YYYY-MM-DD")
	}
	return day.UTC(), day.AddDate(0, 0, 1).UTC(), day.Format(time.DateOnly), nil
}

func normalizeTurnReportCommand(command UpsertTurnReportCommand) (UpsertTurnReportCommand, error) {
	command.SessionID = strings.TrimSpace(command.SessionID)
	command.TurnID = strings.TrimSpace(command.TurnID)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	if command.SessionID == "" || command.TurnID == "" {
		return command, validationError("report_target_required", "session_id and turn_id are required")
	}
	if utf8.RuneCountInString(command.SessionID) > 200 || utf8.RuneCountInString(command.TurnID) > 200 {
		return command, validationError("report_target_invalid", "session_id and turn_id must not exceed 200 characters")
	}
	var err error
	if command.Objective, err = boundedReportText(command.Objective, 2000, "objective"); err != nil {
		return command, err
	}
	if command.Outcome, err = boundedReportText(command.Outcome, 4000, "outcome"); err != nil {
		return command, err
	}
	if command.WorkSummary, err = boundedReportText(command.WorkSummary, 4000, "work_summary"); err != nil {
		return command, err
	}
	if len(command.Decisions) > 50 || len(command.Evidence) > 100 || len(command.IncompleteReasons) > 50 {
		return command, validationError("report_content_too_large", "report collection limits were exceeded")
	}
	command.Decisions, err = normalizeReportStrings(command.Decisions, 500, false)
	if err != nil {
		return command, err
	}
	command.IncompleteReasons, err = normalizeReportStrings(command.IncompleteReasons, 500, true)
	if err != nil {
		return command, err
	}
	command.Evidence, err = normalizeReportEvidence(command.Evidence)
	if err != nil {
		return command, err
	}
	command.Verification, err = normalizeReportVerification(command.Verification)
	if err != nil {
		return command, err
	}
	command.CompletedTaskIDs, err = normalizeTaskIDs(command.CompletedTaskIDs)
	if err != nil {
		return command, err
	}
	command.ChangedTaskIDs, err = normalizeTaskIDs(command.ChangedTaskIDs)
	if err != nil {
		return command, err
	}
	if command.ThroughCursor < 0 {
		return command, validationError("report_cursor_invalid", "through_cursor cannot be negative")
	}
	if len(command.IncompleteReasons) > 0 {
		command.Incomplete = true
	}
	if command.Incomplete && len(command.IncompleteReasons) == 0 {
		command.IncompleteReasons = []string{"unspecified"}
	}
	return command, nil
}

func boundedReportText(value string, limit int, field string) (string, error) {
	value = strings.TrimSpace(redactReportSecrets(value))
	if utf8.RuneCountInString(value) > limit {
		return "", validationError("report_content_too_large", field+" exceeds its size limit")
	}
	return value, nil
}

func redactReportSecrets(value string) string {
	for _, pattern := range reportSecretPatterns {
		if strings.Contains(strings.ToLower(pattern.String()), "authorization") {
			value = pattern.ReplaceAllString(value, `${1}[REDACTED]`)
			continue
		}
		value = pattern.ReplaceAllString(value, `[REDACTED]`)
	}
	return value
}

func normalizeReportStrings(values []string, limit int, sorted bool) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		normalized, err := boundedReportText(value, limit, "report item")
		if err != nil {
			return nil, err
		}
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, normalized)
	}
	if sorted {
		sort.Strings(result)
	}
	return result, nil
}

func normalizeReportEvidence(evidence []ReportEvidence) ([]ReportEvidence, error) {
	allowed := map[string]bool{
		"task": true, "session": true, "document": true, "file": true,
		"verification": true, "event": true,
	}
	result := make([]ReportEvidence, 0, len(evidence))
	for _, item := range evidence {
		item.Kind = strings.TrimSpace(item.Kind)
		if !allowed[item.Kind] {
			return nil, validationError("report_evidence_type_invalid", "evidence type is not supported")
		}
		var err error
		item.Ref, err = boundedReportText(item.Ref, 500, "evidence.ref")
		if err != nil || item.Ref == "" {
			if err != nil {
				return nil, err
			}
			return nil, validationError("report_evidence_reference_required", "evidence reference is required")
		}
		item.Label, err = boundedReportText(item.Label, 1000, "evidence.label")
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Ref != result[j].Ref {
			return result[i].Ref < result[j].Ref
		}
		return result[i].Label < result[j].Label
	})
	return result, nil
}

func normalizeReportVerification(verification ReportVerification) (ReportVerification, error) {
	verification.Status = strings.TrimSpace(verification.Status)
	if verification.Status == "" {
		verification.Status = "not_run"
	}
	allowed := map[string]bool{"not_run": true, "passed": true, "failed": true, "partial": true}
	if !allowed[verification.Status] {
		return verification, validationError("report_verification_invalid", "verification status is invalid")
	}
	var err error
	verification.Summary, err = boundedReportText(verification.Summary, 2000, "verification.summary")
	if err != nil {
		return verification, err
	}
	if len(verification.EvidenceRefs) > 100 {
		return verification, validationError("report_content_too_large", "verification evidence limit was exceeded")
	}
	verification.EvidenceRefs, err = normalizeReportStrings(verification.EvidenceRefs, 500, true)
	return verification, err
}

func normalizeTaskIDs(ids []int64) ([]int64, error) {
	if len(ids) > 1000 {
		return nil, validationError("report_content_too_large", "task id limit was exceeded")
	}
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, validationError("report_task_id_invalid", "task ids must be positive")
		}
		set[id] = true
	}
	result := make([]int64, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func buildDailyReport(
	reportDate string,
	timezone string,
	throughCursor int64,
	sources []database.StructuredReportSessionSource,
	records []database.StructuredSessionReportRecord,
	activity []database.StructuredReportTaskActivity,
) (*DailyReport, error) {
	if len(sources) > maxDailyReportSessions || len(records) > maxDailyReportTurns {
		return nil, validationError("daily_report_too_large", "daily report source limits were exceeded")
	}
	type sessionBuild struct {
		report  DailySessionReport
		records []database.StructuredSessionReportRecord
	}
	bySession := make(map[string]*sessionBuild, len(sources)+len(records))
	for _, source := range sources {
		bySession[source.SessionID] = &sessionBuild{report: DailySessionReport{
			SessionID: source.SessionID, Title: source.Name,
			ProjectID: source.ProjectID, ProjectName: source.ProjectName,
			TaskID:    nullInt64Pointer(source.TaskID),
			StartedAt: source.StartTime, EndedAt: nullTimePointer(source.EndTime),
			LastActivityAt: nullTimePointer(source.LastActivityAt), Status: source.Status,
		}}
	}
	for _, record := range records {
		build := bySession[record.SessionID]
		if build == nil {
			build = &sessionBuild{report: DailySessionReport{
				SessionID: record.SessionID, Title: record.SessionName,
				ProjectID: record.ProjectID, ProjectName: record.ProjectName,
				TaskID:    nullInt64Pointer(record.TaskID),
				StartedAt: record.SessionStartedAt, EndedAt: nullTimePointer(record.SessionEndedAt),
				Status: record.SessionStatus,
			}}
			bySession[record.SessionID] = build
		}
		build.records = append(build.records, record)
	}
	if len(bySession) > maxDailyReportSessions {
		return nil, validationError("daily_report_too_large", "daily report session limit was exceeded")
	}
	completed := make(map[int64]bool)
	changed := make(map[int64]bool)
	for _, item := range activity {
		changed[item.TaskID] = true
		if taskActivityCompleted(item) {
			completed[item.TaskID] = true
		}
		if item.SessionID.Valid {
			if build := bySession[item.SessionID.String]; build != nil {
				build.report.ChangedTaskIDs = append(build.report.ChangedTaskIDs, item.TaskID)
				if taskActivityCompleted(item) {
					build.report.CompletedTaskIDs = append(build.report.CompletedTaskIDs, item.TaskID)
				}
			}
		}
	}

	sessions := make([]DailySessionReport, 0, len(bySession))
	for _, build := range bySession {
		if err := populateDailySessionReport(&build.report, build.records); err != nil {
			return nil, err
		}
		build.report.CompletedTaskIDs = mergeTaskIDs(build.report.CompletedTaskIDs)
		build.report.ChangedTaskIDs = mergeTaskIDs(build.report.ChangedTaskIDs)
		for _, id := range build.report.CompletedTaskIDs {
			completed[id] = true
		}
		for _, id := range build.report.ChangedTaskIDs {
			changed[id] = true
		}
		sessions = append(sessions, build.report)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].StartedAt.Equal(sessions[j].StartedAt) {
			return sessions[i].StartedAt.Before(sessions[j].StartedAt)
		}
		return sessions[i].SessionID < sessions[j].SessionID
	})
	reasons := make([]string, 0)
	decisions := make([]string, 0)
	for _, session := range sessions {
		decisions = append(decisions, session.Decisions...)
		for _, reason := range session.IncompleteReasons {
			reasons = append(reasons, "session:"+session.SessionID+":"+reason)
		}
	}
	sort.Strings(reasons)
	decisions, _ = normalizeReportStrings(decisions, 500, false)
	return &DailyReport{
		SchemaVersion: 1, ReportID: "report-" + reportDate, Date: reportDate,
		Timezone: timezone, ThroughCursor: strconv.FormatInt(throughCursor, 10),
		Sessions: sessions, CompletedTaskIDs: sortedTaskIDSet(completed),
		ChangedTaskIDs: sortedTaskIDSet(changed), Decisions: decisions, IncompleteReasons: reasons,
	}, nil
}

func populateDailySessionReport(session *DailySessionReport, records []database.StructuredSessionReportRecord) error {
	sort.Slice(records, func(i, j int) bool {
		if !records[i].TurnStartedAt.Equal(records[j].TurnStartedAt) {
			return records[i].TurnStartedAt.Before(records[j].TurnStartedAt)
		}
		return records[i].TurnID < records[j].TurnID
	})
	session.Turns = []TurnReport{}
	session.Decisions = []string{}
	session.Evidence = []ReportEvidence{}
	session.WorkSummary = []string{}
	session.Verification = []string{}
	session.CompletedTaskIDs = mergeTaskIDs(session.CompletedTaskIDs)
	session.ChangedTaskIDs = mergeTaskIDs(session.ChangedTaskIDs)
	reasons := make(map[string]bool)
	finalized := false
	for _, record := range records {
		turn, err := turnReportFromRecord(record)
		if err != nil {
			return err
		}
		session.Turns = append(session.Turns, *turn)
		if record.State == "finalized" {
			finalized = true
		}
		if session.Objective == "" && turn.Objective != "" {
			session.Objective = turn.Objective
		}
		if turn.Outcome != "" {
			session.Outcome = turn.Outcome
		}
		if turn.WorkSummary != "" {
			session.WorkSummary = append(session.WorkSummary, turn.WorkSummary)
		}
		session.Decisions = append(session.Decisions, turn.Decisions...)
		session.Evidence = append(session.Evidence, turn.Evidence...)
		if turn.Verification.Summary != "" {
			session.Verification = append(session.Verification, turn.Verification.Summary)
		} else if turn.Verification.Status != "" && turn.Verification.Status != "not_run" {
			session.Verification = append(session.Verification, turn.Verification.Status)
		}
		if turn.ThroughCursor > session.ThroughCursor {
			session.ThroughCursor = turn.ThroughCursor
		}
		session.CompletedTaskIDs = append(session.CompletedTaskIDs, turn.CompletedTaskIDs...)
		session.ChangedTaskIDs = append(session.ChangedTaskIDs, turn.ChangedTaskIDs...)
		if turn.Incomplete {
			for _, reason := range turn.IncompleteReasons {
				reasons[reason] = true
			}
		}
	}
	if len(session.WorkSummary) > 500 {
		session.WorkSummary = session.WorkSummary[:500]
	}
	session.Decisions, _ = normalizeReportStrings(session.Decisions, 500, false)
	if len(session.Decisions) > 500 {
		session.Decisions = session.Decisions[:500]
	}
	session.Evidence = deduplicateEvidence(session.Evidence)
	if len(session.Evidence) > 1000 {
		session.Evidence = session.Evidence[:1000]
	}
	if len(records) == 0 {
		reasons["missing_structured_report"] = true
	} else if !finalized {
		reasons["report_not_finalized"] = true
	}
	switch session.Status {
	case "starting", "running":
		reasons["session_active"] = true
	case "error":
		reasons["session_error"] = true
	}
	session.IncompleteReasons = sortedStringSet(reasons)
	session.Incomplete = len(session.IncompleteReasons) > 0
	if strings.TrimSpace(session.Title) == "" {
		session.Title = "Session " + session.SessionID
	}
	if strings.TrimSpace(session.Outcome) == "" {
		if len(records) == 0 {
			session.Outcome = "Structured report unavailable for this session."
		} else {
			session.Outcome = "Outcome not provided."
		}
	}
	return nil
}

func turnReportFromRecord(record database.StructuredSessionReportRecord) (*TurnReport, error) {
	turn := &TurnReport{
		ReportID: record.ReportID, SessionID: record.SessionID, TurnID: record.TurnID,
		State: record.State, Incomplete: record.Incomplete,
		Objective: record.Objective, Outcome: record.Outcome, WorkSummary: record.WorkSummary,
		ThroughCursor: record.ThroughCursor, StartedAt: record.TurnStartedAt,
		EndedAt: nullTimePointer(record.TurnEndedAt), UpdatedAt: record.UpdatedAt,
		FinalizedAt: nullTimePointer(record.FinalizedAt),
	}
	encoded := []struct {
		raw    string
		target any
	}{
		{record.DecisionsJSON, &turn.Decisions},
		{record.VerificationJSON, &turn.Verification},
		{record.EvidenceJSON, &turn.Evidence},
		{record.CompletedTaskIDsJSON, &turn.CompletedTaskIDs},
		{record.ChangedTaskIDsJSON, &turn.ChangedTaskIDs},
		{record.IncompleteReasonsJSON, &turn.IncompleteReasons},
	}
	for _, value := range encoded {
		if err := json.Unmarshal([]byte(value.raw), value.target); err != nil {
			return nil, fmt.Errorf("decode structured report %s/%s: %w", record.SessionID, record.TurnID, err)
		}
	}
	if turn.Decisions == nil {
		turn.Decisions = []string{}
	}
	if turn.Evidence == nil {
		turn.Evidence = []ReportEvidence{}
	}
	if turn.CompletedTaskIDs == nil {
		turn.CompletedTaskIDs = []int64{}
	}
	if turn.ChangedTaskIDs == nil {
		turn.ChangedTaskIDs = []int64{}
	}
	if turn.IncompleteReasons == nil {
		turn.IncompleteReasons = []string{}
	}
	return turn, nil
}

func taskActivityCompleted(activity database.StructuredReportTaskActivity) bool {
	if activity.EventType == "verification_approved" {
		return true
	}
	if activity.EventType != "status_change" && activity.EventType != "task_created" {
		return false
	}
	var details map[string]any
	if json.Unmarshal([]byte(activity.Details), &details) != nil {
		return false
	}
	if status, _ := details["new"].(string); status == TaskStatusDone {
		return true
	}
	status, _ := details["status"].(string)
	return status == TaskStatusDone
}

func reportActorValue(actor Actor) string {
	actorType := strings.TrimSpace(actor.Type)
	if actorType == "" {
		actorType = "system"
	}
	if actorID := strings.TrimSpace(actor.ID); actorID != "" {
		return actorType + ":" + actorID
	}
	return actorType
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func mergeTaskIDs(ids []int64) []int64 {
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 {
			set[id] = true
		}
	}
	return sortedTaskIDSet(set)
}

func sortedTaskIDSet(set map[int64]bool) []int64 {
	result := make([]int64, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func sortedStringSet(set map[string]bool) []string {
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func deduplicateEvidence(evidence []ReportEvidence) []ReportEvidence {
	seen := make(map[string]bool, len(evidence))
	result := make([]ReportEvidence, 0, len(evidence))
	for _, item := range evidence {
		key := item.Kind + "\x00" + item.Ref + "\x00" + item.Label
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Ref < result[j].Ref
	})
	return result
}
