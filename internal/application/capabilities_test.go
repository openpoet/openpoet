package application

import "testing"

func TestProjectTaskCapabilityRegistry(t *testing.T) {
	service := NewProjectTaskService(nil, nil)
	registry, err := NewProjectTaskCapabilityRegistry(service)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := registry.List()
	if len(capabilities) != 14 {
		t.Fatalf("capability count = %d, want 14", len(capabilities))
	}
	deletion, ok := registry.Lookup(CapabilityTasksDelete)
	if !ok {
		t.Fatal("delete capability not registered")
	}
	if deletion.Scope != CapabilityScopeTasksWrite || deletion.Risk != CapabilityRiskDestructive || deletion.Approval != ApprovalExplicit {
		t.Fatalf("unexpected delete capability: %+v", deletion)
	}
	if deletion.Handler != CapabilityHandlerTasksDelete {
		t.Fatalf("unexpected delete handler: %q", deletion.Handler)
	}
	if deletion.Service != service || deletion.Service.CapabilityServiceName() != CapabilityServiceProjectTasks {
		t.Fatal("capability is not bound to the project-task service")
	}
	if err := registry.Register(deletion); err == nil {
		t.Fatal("expected duplicate capability error")
	}
}

func TestCapabilityRegistryRejectsIncompleteDefinitions(t *testing.T) {
	registry := NewCapabilityRegistry()
	service := NewProjectTaskService(nil, nil)
	tests := []Capability{
		{Scope: CapabilityScopeTasksRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerTasksList, Service: service},
		{Name: "invalid.scope", Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerTasksList, Service: service},
		{Name: "invalid.risk", Scope: CapabilityScopeTasksRead, Risk: "invalid", Approval: ApprovalNone, Handler: CapabilityHandlerTasksList, Service: service},
		{Name: "invalid.approval", Scope: CapabilityScopeTasksRead, Risk: CapabilityRiskRead, Approval: "invalid", Handler: CapabilityHandlerTasksList, Service: service},
		{Name: "invalid.handler", Scope: CapabilityScopeTasksRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Service: service},
		{Name: "invalid.service", Scope: CapabilityScopeTasksRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerTasksList},
	}
	for _, capability := range tests {
		if err := registry.Register(capability); err == nil {
			t.Fatalf("expected invalid capability error: %+v", capability)
		}
	}
}

func TestProjectTaskCapabilityRegistryRejectsNilService(t *testing.T) {
	if _, err := NewProjectTaskCapabilityRegistry(nil); err == nil {
		t.Fatal("expected nil service error")
	}
}
