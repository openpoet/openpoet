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
	Waiter               OutboxWaiter
	Sessions             SessionStateStore
	ProjectScope         ProjectScopeStore
	ApprovalRandom       io.Reader
	Now                  func() time.Time
	// MergePredictor (Phase 7.5): the workspace merge-prediction port for the
	// coordinator tier.
	MergePredictor MergePredictor
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
	if deps.Waiter == nil {
		deps.Waiter, _ = store.(OutboxWaiter)
	}
	if deps.Sessions == nil {
		deps.Sessions, _ = store.(SessionStateStore)
	}
	if deps.ProjectScope == nil {
		deps.ProjectScope, _ = store.(ProjectScopeStore)
	}
	if deps.PlatformCapabilities != nil && deps.ProjectScope != nil {
		deps.PlatformCapabilities.SetProjectScopeStore(deps.ProjectScope)
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
		waiter: deps.Waiter, sessions: deps.Sessions, scopeStore: deps.ProjectScope,
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
	router.With(RequireScopes(ScopeEventsRead)).Get("/events/await", api.awaitEvents)
	router.With(RequireScopes(ScopeEventsRead)).Get("/events/stream", api.streamEvents)
	router.With(RequireScopes(ScopeEventsRead)).Post("/events/ack", api.ackEvents)
	router.With(RequireScopes(ScopeSessionsRead)).Get("/sessions/{id}/wait", api.waitForSession)
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
	// Counts are DERIVED from the live registry — the exact catalog /capabilities
	// serves — so readiness can never silently drift from what is registered.
	// The per-phase expected inventory is enforced separately at composition time
	// (validatePlatformCapabilityInventory), not by magic numbers here.
	total, mutations := 0, 0
	if a != nil {
		for _, descriptor := range a.mergedCapabilityDescriptors(actor) {
			total++
			if descriptor.Mutation {
				mutations++
			}
		}
	}
	reads := total - mutations
	platformReady := a != nil && a.platform != nil && total > 0
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":                "ok",
		"version":               APIVersion,
		"client_id":             actor.ClientID,
		"platform_ready":        platformReady,
		"platform_capabilities": total,
		"platform_mutations":    mutations,
		"platform_reads":        reads,
	})
}
