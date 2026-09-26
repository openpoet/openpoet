package handlers

import (
	"net/http"

	"openpoet/internal/modelcatalog"
)

var modelCatalog = modelcatalog.NewService()

// ListHarnessModels returns the models a harness CLI reports it can start,
// e.g. GET /api/models?harness=claude_code[&refresh=1].
func (a *API) ListHarnessModels(w http.ResponseWriter, r *http.Request) {
	harness := r.URL.Query().Get("harness")
	if !modelCatalog.Supported(harness) {
		respondError(w, http.StatusBadRequest, "harness must be claude_code, codex or opencode")
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	respondJSON(w, http.StatusOK, modelCatalog.Get(r.Context(), harness, refresh))
}
