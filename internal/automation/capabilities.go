package automation

import (
	"encoding/json"
	"net/http"
	"time"

	"openpoet/internal/application"
)

type commandAPI struct {
	capabilities *application.CapabilityRegistry
	snapshot     SnapshotStore
	events       EventStore
	reports      DailyReportService
	now          func() time.Time
}

type capabilityDescriptor struct {
	Name             application.CapabilityName        `json:"name"`
	Scope            application.CapabilityScope       `json:"scope"`
	Risk             application.CapabilityRisk        `json:"risk"`
	Approval         application.ApprovalPolicy        `json:"approval"`
	Handler          application.CapabilityHandler     `json:"handler"`
	Service          application.CapabilityServiceName `json:"service"`
	Allowed          bool                              `json:"allowed"`
	ApprovalRequired bool                              `json:"approval_required"`
}

type capabilitiesResponse struct {
	APIVersion   string                 `json:"api_version"`
	Capabilities []capabilityDescriptor `json:"capabilities"`
}

func (a *commandAPI) listCapabilities(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.capabilities == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_registry_unavailable", "the capability registry is unavailable", true)
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	registered := a.capabilities.List()
	response := capabilitiesResponse{APIVersion: APIVersion, Capabilities: make([]capabilityDescriptor, 0, len(registered))}
	for _, capability := range registered {
		serviceName := application.CapabilityServiceName("")
		if capability.Service != nil {
			serviceName = capability.Service.CapabilityServiceName()
		}
		response.Capabilities = append(response.Capabilities, capabilityDescriptor{
			Name: capability.Name, Scope: capability.Scope, Risk: capability.Risk,
			Approval: capability.Approval, Handler: capability.Handler, Service: serviceName,
			Allowed:          actor.Scopes.Has(Scope(capability.Scope)),
			ApprovalRequired: capability.Approval == application.ApprovalExplicit,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
