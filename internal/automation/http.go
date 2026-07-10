package automation

import (
	"crypto/rand"
	"encoding/json"
	"io"
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
	ApprovalGrantStore
}

type Dependencies struct {
	Capabilities         *application.CapabilityRegistry
	PlatformCapabilities *PlatformCapabilityRegistry
	Snapshot             SnapshotStore
	Events               EventStore
	Reports              DailyReportService
	ApprovalRandom       io.Reader
	Now                  func() time.Time
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
	if deps.ApprovalRandom == nil {
		deps.ApprovalRandom = rand.Reader
	}
	api := &commandAPI{
		capabilities: deps.Capabilities, platform: deps.PlatformCapabilities, snapshot: deps.Snapshot,
		events: deps.Events, reports: deps.Reports, approvals: store,
		random: deps.ApprovalRandom, now: deps.Now,
	}

	router := chi.NewRouter()
	router.Use(BodyLimit(DefaultBodyLimit))
	router.Use(NewAuthenticator(store).Middleware)
	router.Get("/health", api.health)
	router.Get("/capabilities", api.listCapabilities)
	// Approval tokens are intentionally outside the idempotency response cache:
	// the broker receives the plaintext exactly once and only its digest is
	// persisted. Commands retain the durable replay ledger.
	router.With(RequireScopes(ScopeApprovalsGrant)).Post("/approvals", api.issueApprovalGrant)
	router.With(NewIdempotency(store, IdempotencyOptions{
		IsMutation: registeredCapabilityMutationClassifier(deps.Capabilities, deps.PlatformCapabilities),
	}).Middleware).Post("/commands", api.executeCommand)
	router.With(RequireScopes(ScopeEventsRead)).Get("/events", api.listEvents)
	router.With(RequireScopes(ScopeEventsRead)).Post("/events/ack", api.ackEvents)
	router.With(RequireScopes(ScopeReportsRead)).Get("/reports/daily", api.getDailyReport)
	router.With(RequireScopes(
		ScopeProjectsRead,
		ScopeTasksRead,
		ScopeSessionsRead,
		ScopeNotificationsRead,
		ScopeWorkRunsRead,
		ScopePlansRead,
	)).Get("/snapshot", api.getSnapshot)
	return router
}

func registeredCapabilityMutationClassifier(capabilities *application.CapabilityRegistry, platform *PlatformCapabilityRegistry) func(string) bool {
	return func(name string) bool {
		capabilityName := application.CapabilityName(name)
		if platform != nil {
			if _, exists := platform.lookup(capabilityName); exists {
				return platform.IsMutation(capabilityName)
			}
		}
		if capabilities != nil {
			if capability, exists := capabilities.Lookup(capabilityName); exists {
				return capability.Risk != application.CapabilityRiskRead
			}
		}
		return false
	}
}

func (a *commandAPI) health(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFromContext(r.Context())
	total, mutations := 0, 0
	if a != nil && a.platform != nil {
		for _, descriptor := range a.platform.ListForActor(actor) {
			total++
			if descriptor.Mutation {
				mutations++
			}
		}
	}
	reads := total - mutations
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":                "ok",
		"version":               APIVersion,
		"client_id":             actor.ClientID,
		"platform_ready":        total == 153 && mutations == 104 && reads == 49,
		"platform_capabilities": total,
		"platform_mutations":    mutations,
		"platform_reads":        reads,
	})
}
