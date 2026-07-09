package automation

import (
	"encoding/json"
	"net/http"
	"time"

	"openpoet/internal/application"

	"github.com/go-chi/chi/v5"
)

const DefaultBodyLimit int64 = 1 << 20
const APIVersion = "v1"

type Store interface {
	ClientAuthenticator
	IdempotencyStore
}

type Dependencies struct {
	Capabilities *application.CapabilityRegistry
	Snapshot     SnapshotStore
	Events       EventStore
	Reports      DailyReportService
	Now          func() time.Time
}

func NewHandler(store Store, dependencies ...Dependencies) http.Handler {
	deps := Dependencies{}
	if len(dependencies) > 0 {
		deps = dependencies[0]
	}
	if deps.Snapshot == nil {
		deps.Snapshot, _ = store.(SnapshotStore)
	}
	if deps.Events == nil {
		deps.Events, _ = store.(EventStore)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	api := &commandAPI{
		capabilities: deps.Capabilities, snapshot: deps.Snapshot,
		events: deps.Events, reports: deps.Reports, now: deps.Now,
	}

	router := chi.NewRouter()
	router.Use(BodyLimit(DefaultBodyLimit))
	router.Use(NewAuthenticator(store).Middleware)
	router.Use(NewIdempotency(store, IdempotencyOptions{}).Middleware)
	router.Get("/health", health)
	router.Get("/capabilities", api.listCapabilities)
	router.Post("/commands", api.executeCommand)
	router.With(RequireScopes(ScopeEventsRead)).Get("/events", api.listEvents)
	router.With(RequireScopes(ScopeEventsRead)).Post("/events/ack", api.ackEvents)
	router.With(RequireScopes(ScopeReportsRead)).Get("/reports/daily", api.getDailyReport)
	router.With(RequireScopes(
		ScopeProjectsRead,
		ScopeTasksRead,
		ScopeSessionsRead,
		ScopeNotificationsRead,
	)).Get("/snapshot", api.getSnapshot)
	return router
}

func health(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"version":   APIVersion,
		"client_id": actor.ClientID,
	})
}
