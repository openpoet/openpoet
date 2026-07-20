package automation

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"

	"openpoet/internal/application"
)

type commandAPI struct {
	capabilities *application.CapabilityRegistry
	platform     *PlatformCapabilityRegistry
	snapshot     SnapshotStore
	events       EventStore
	reports      DailyReportService
	approvals    ApprovalGrantStore
	waiter       OutboxWaiter
	sessions     SessionStateStore
	scopeStore   ProjectScopeStore
	random       io.Reader
	now          func() time.Time
}

type capabilityDescriptor struct {
	Name             application.CapabilityName        `json:"name"`
	Scope            application.CapabilityScope       `json:"scope"`
	Scopes           []application.CapabilityScope     `json:"scopes"`
	Risk             application.CapabilityRisk        `json:"risk"`
	Approval         application.ApprovalPolicy        `json:"approval"`
	Mutation         bool                              `json:"mutation"`
	Handler          application.CapabilityHandler     `json:"handler"`
	Service          application.CapabilityServiceName `json:"service"`
	Allowed          bool                              `json:"allowed"`
	ApprovalRequired bool                              `json:"approval_required"`
}

type capabilitiesResponse struct {
	APIVersion   string                 `json:"api_version"`
	Capabilities []capabilityDescriptor `json:"capabilities"`
}

// mergedCapabilityDescriptors builds the full, sorted capability catalog for an
// actor: legacy registry entries (minus platform mirrors) plus platform
// capabilities (which win on name collision). Both /capabilities and /health
// derive from this one source so their counts can never drift apart.
func (a *commandAPI) mergedCapabilityDescriptors(actor Actor) []capabilityDescriptor {
	merged := make(map[application.CapabilityName]capabilityDescriptor)
	if a.capabilities != nil {
		for _, capability := range a.capabilities.List() {
			if _, isPlatformService := capability.Service.(*platformCapabilityService); isPlatformService {
				continue
			}
			serviceName := application.CapabilityServiceName("")
			if capability.Service != nil {
				serviceName = capability.Service.CapabilityServiceName()
			}
			scopes := []application.CapabilityScope{capability.Scope}
			merged[capability.Name] = capabilityDescriptor{
				Name: capability.Name, Scope: capability.Scope, Scopes: scopes, Risk: capability.Risk,
				Approval: capability.Approval, Mutation: capability.Risk != application.CapabilityRiskRead,
				Handler: capability.Handler, Service: serviceName,
				Allowed:          actorHasPlatformScopes(actor, scopes),
				ApprovalRequired: capability.Approval == application.ApprovalExplicit,
			}
		}
	}
	if a.platform != nil {
		for _, capability := range a.platform.ListForActor(actor) {
			merged[capability.Name] = capabilityDescriptor{
				Name: capability.Name, Scope: capability.Scope,
				Scopes: append([]application.CapabilityScope(nil), capability.Scopes...),
				Risk:   capability.Risk, Approval: capability.Approval, Mutation: capability.Mutation,
				Handler: capability.Handler, Service: capability.Service,
				Allowed: capability.Allowed, ApprovalRequired: capability.ApprovalRequired,
			}
		}
	}
	names := make([]application.CapabilityName, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	descriptors := make([]capabilityDescriptor, 0, len(names))
	for _, name := range names {
		descriptors = append(descriptors, merged[name])
	}
	return descriptors
}

func (a *commandAPI) listCapabilities(w http.ResponseWriter, r *http.Request) {
	if a == nil || (a.capabilities == nil && a.platform == nil) {
		writeError(w, http.StatusServiceUnavailable, "capability_registry_unavailable", "the capability registry is unavailable", true)
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	response := capabilitiesResponse{APIVersion: APIVersion, Capabilities: a.mergedCapabilityDescriptors(actor)}
	writeJSON(w, http.StatusOK, response)
}

func (a *commandAPI) lookupCapability(name application.CapabilityName) (application.Capability, platformCapabilityBinding, bool, bool) {
	if a != nil && a.platform != nil {
		if binding, ok := a.platform.lookup(name); ok {
			return application.Capability{
				Name: binding.definition.Name, Scope: binding.definition.Scopes[0],
				Risk: binding.definition.Risk, Approval: binding.definition.Approval,
				Handler: binding.definition.Handler, Service: binding.service,
			}, binding, true, true
		}
	}
	if a != nil && a.capabilities != nil {
		if capability, ok := a.capabilities.Lookup(name); ok {
			if _, isPlatformService := capability.Service.(*platformCapabilityService); isPlatformService {
				return application.Capability{}, platformCapabilityBinding{}, false, false
			}
			return capability, platformCapabilityBinding{}, false, true
		}
	}
	return application.Capability{}, platformCapabilityBinding{}, false, false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
