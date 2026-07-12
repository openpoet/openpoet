package automation

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"openpoet/internal/application"
)

type DailyReportService interface {
	MaterializeDaily(ctx context.Context, reportDate string) (*application.DailyReport, error)
}

func (a *commandAPI) getDailyReport(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.reports == nil {
		writeError(w, http.StatusServiceUnavailable, "daily_report_unavailable", "daily reports are unavailable", true)
		return
	}
	reportDate := strings.TrimSpace(r.URL.Query().Get("date"))
	if reportDate == "" {
		writeError(w, http.StatusBadRequest, "report_date_required", "date is required in YYYY-MM-DD format", false)
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	ctx := application.WithEventMetadata(r.Context(), application.EventMetadata{
		Actor: application.Actor{Type: actor.Type, ID: actor.ClientID},
	})
	report, err := a.reports.MaterializeDaily(ctx, reportDate)
	if err != nil {
		var applicationError *application.Error
		if errors.As(err, &applicationError) && applicationError.Kind == application.ErrorValidation {
			writeError(w, http.StatusBadRequest, applicationError.Code, applicationError.Message, false)
			return
		}
		writeError(w, http.StatusInternalServerError, "daily_report_failed", "daily report could not be materialized", true)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
