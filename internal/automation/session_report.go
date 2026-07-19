package automation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// Phase 7.2 (Maestro): condensed communication. A worker session emits a DENSE
// per-milestone structured report — the coordinator's primary channel, never
// the raw transcript. The report's session is ALWAYS the verified opst1 token's
// session: there is no session_id parameter to spoof.

// SessionReportService is the slice of application.ReportService the milestone
// emitter needs.
type SessionReportService interface {
	UpdateTurn(ctx context.Context, command application.UpsertTurnReportCommand) (*application.TurnReport, error)
	FinalizeTurn(ctx context.Context, command application.UpsertTurnReportCommand) (*application.TurnReport, error)
}

// SessionReportReader reads the latest dense report of a session.
// *database.DB satisfies it.
type SessionReportReader interface {
	LatestStructuredSessionReport(ctx context.Context, sessionID string) (*database.StructuredSessionReportRecord, error)
}

type sessionReportAPI struct {
	coordinator *coordinatorAPI // reused for the verified-session middleware
	reports     SessionReportService
}

// NewSessionReportHandler mounts the worker-side self-report surface
// (POST /report), authenticated by the session's own opst1 bearer.
func NewSessionReportHandler(store CoordinatorStore, reports SessionReportService) http.Handler {
	s := &sessionReportAPI{
		coordinator: &coordinatorAPI{store: store},
		reports:     reports,
	}
	router := chi.NewRouter()
	router.Use(BodyLimit(DefaultBodyLimit))
	router.Use(s.coordinator.sessionAuthMiddleware)
	router.Post("/report", s.emit)
	return router
}

// emitSessionReportRequest is the dense milestone report shape from the spec:
// {objetivo, status, resumo, decisões, arquivos, verificação, bloqueios,
//
//	o-que-precisa-da-coordenadora, próximo}.
type emitSessionReportRequest struct {
	TurnID               string                         `json:"turn_id"`
	Objective            string                         `json:"objective"`
	Outcome              string                         `json:"outcome"`
	Summary              string                         `json:"summary"`
	Decisions            []string                       `json:"decisions"`
	Verification         application.ReportVerification `json:"verification"`
	Files                []string                       `json:"files"`
	Blockers             []string                       `json:"blockers"`
	NeedsFromCoordinator []string                       `json:"needs_from_coordinator"`
	Next                 string                         `json:"next"`
	CompletedTaskIDs     []int64                        `json:"completed_task_ids"`
	Finalize             bool                           `json:"finalize"`
}

func (s *sessionReportAPI) emit(w http.ResponseWriter, r *http.Request) {
	cs, ok := coordinatorSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "session identity is missing", false)
		return
	}
	if s.reports == nil {
		writeError(w, http.StatusServiceUnavailable, "report_service_unavailable", "the report service is unavailable", true)
		return
	}
	var req emitSessionReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "report_body_invalid", "the report body must be valid JSON", false)
		return
	}
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		turnID = "milestone"
	}
	evidence := make([]application.ReportEvidence, 0, len(req.Files))
	for _, file := range req.Files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		evidence = append(evidence, application.ReportEvidence{Kind: "file", Ref: file})
	}
	command := application.UpsertTurnReportCommand{
		SessionID:            cs.SessionID, // identity = verified token, never a param
		TurnID:               turnID,
		Objective:            req.Objective,
		Outcome:              req.Outcome,
		WorkSummary:          req.Summary,
		Decisions:            req.Decisions,
		Verification:         req.Verification,
		Evidence:             evidence,
		CompletedTaskIDs:     req.CompletedTaskIDs,
		IncompleteReasons:    req.Blockers,
		NextStep:             req.Next,
		NeedsFromCoordinator: req.NeedsFromCoordinator,
		Actor:                application.Actor{Type: "session", ID: cs.SessionID},
		CorrelationID:        "session-report:" + cs.SessionID,
	}
	var report *application.TurnReport
	var err error
	if req.Finalize {
		report, err = s.reports.FinalizeTurn(r.Context(), command)
	} else {
		report, err = s.reports.UpdateTurn(r.Context(), command)
	}
	if err != nil {
		writeApplicationReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func writeApplicationReportError(w http.ResponseWriter, err error) {
	var appErr *application.Error
	if errors.As(err, &appErr) {
		status := http.StatusBadRequest
		switch appErr.Kind {
		case application.ErrorNotFound:
			status = http.StatusNotFound
		case application.ErrorConflict:
			status = http.StatusConflict
		}
		writeError(w, status, appErr.Code, appErr.Message, false)
		return
	}
	writeError(w, http.StatusInternalServerError, "report_write_failed", "the report could not be written", true)
}

// getWorkerReport is the coordinator-tier read: the latest dense report of a
// group session (progressive disclosure — transcript drill-down stays a
// separate, deliberate step).
func (c *coordinatorAPI) getWorkerReport(w http.ResponseWriter, r *http.Request) {
	cs, group, ok := c.requireCoordinator(w, r, nil)
	if !ok {
		return
	}
	targetID := strings.TrimSpace(chi.URLParam(r, "id"))
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "session_id_required", "session id is required", false)
		return
	}
	if !c.sessionInScope(r.Context(), cs, group, targetID, w) {
		return
	}
	reader, ok := c.store.(SessionReportReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "report_service_unavailable", "the report store is unavailable", true)
		return
	}
	record, err := reader.LatestStructuredSessionReport(r.Context(), targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "report_read_failed", "the report could not be read", true)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusOK, map[string]any{"session_id": targetID, "report": nil})
		return
	}
	writeJSON(w, http.StatusOK, denseReportView(record))
}

// denseReportView shapes the stored record into the dense report the
// coordinator consumes (JSON blobs decoded, storage names dropped).
func denseReportView(record *database.StructuredSessionReportRecord) map[string]any {
	decode := func(raw string) any {
		var value any
		if json.Unmarshal([]byte(raw), &value) == nil {
			return value
		}
		return nil
	}
	view := map[string]any{
		"report_id":              record.ReportID,
		"session_id":             record.SessionID,
		"turn_id":                record.TurnID,
		"state":                  record.State,
		"incomplete":             record.Incomplete,
		"objective":              record.Objective,
		"outcome":                record.Outcome,
		"work_summary":           record.WorkSummary,
		"decisions":              decode(record.DecisionsJSON),
		"verification":           decode(record.VerificationJSON),
		"evidence":               decode(record.EvidenceJSON),
		"blockers":               decode(record.IncompleteReasonsJSON),
		"needs_from_coordinator": decode(record.NeedsFromCoordinator),
		"next_step":              record.NextStep,
		"updated_at":             record.UpdatedAt,
	}
	if record.FinalizedAt.Valid {
		view["finalized_at"] = record.FinalizedAt.Time
	}
	return view
}
