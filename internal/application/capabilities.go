package application

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type CapabilityName string
type CapabilityScope string
type CapabilityRisk string
type ApprovalPolicy string
type CapabilityServiceName string
type CapabilityHandler string

const (
	CapabilityRiskRead        CapabilityRisk = "read"
	CapabilityRiskWrite       CapabilityRisk = "write"
	CapabilityRiskDestructive CapabilityRisk = "destructive"
	CapabilityRiskUnsafe      CapabilityRisk = "unsafe"

	ApprovalNone     ApprovalPolicy = "none"
	ApprovalByPolicy ApprovalPolicy = "policy"
	ApprovalExplicit ApprovalPolicy = "explicit"

	CapabilityServiceProjectTasks CapabilityServiceName = "project_tasks"
)

const (
	CapabilityWorkRunsList     CapabilityName = "work_runs.list"
	CapabilityWorkRunsGet      CapabilityName = "work_runs.get"
	CapabilityWorkRunsStart    CapabilityName = "work_runs.start"
	CapabilityWorkRunsPause    CapabilityName = "work_runs.pause"
	CapabilityWorkRunsResume   CapabilityName = "work_runs.resume"
	CapabilityWorkRunsComplete CapabilityName = "work_runs.complete"
	CapabilityWorkRunsCancel   CapabilityName = "work_runs.cancel"
	CapabilityPlansUpsert      CapabilityName = "plans.upsert"
	CapabilityPlansList        CapabilityName = "plans.list"
	CapabilityPlansItems       CapabilityName = "plans.items"
)

const (
	CapabilityTasksList                    CapabilityName = "tasks.list"
	CapabilityTasksGet                     CapabilityName = "tasks.get"
	CapabilityTasksCreate                  CapabilityName = "tasks.create"
	CapabilityTasksUpdate                  CapabilityName = "tasks.update"
	CapabilityTasksChangeStatus            CapabilityName = "tasks.change_status"
	CapabilityTasksDelete                  CapabilityName = "tasks.delete"
	CapabilityTasksDuplicate               CapabilityName = "tasks.duplicate"
	CapabilityTasksReorderProject          CapabilityName = "tasks.reorder_project"
	CapabilityTasksReorderGlobal           CapabilityName = "tasks.reorder_global"
	CapabilityTasksApproveVerification     CapabilityName = "tasks.approve_verification"
	CapabilityTasksApproveVerificationBulk CapabilityName = "tasks.approve_verification_bulk"
	CapabilityTasksRejectVerification      CapabilityName = "tasks.reject_verification"
	CapabilityTasksLinkSession             CapabilityName = "tasks.link_session"
	CapabilityTasksUnlinkSession           CapabilityName = "tasks.unlink_session"
	CapabilityTasksAddComment              CapabilityName = "tasks.add_comment"
)

const (
	CapabilityHandlerWorkRunsList     CapabilityHandler = "work_runs.list"
	CapabilityHandlerWorkRunsGet      CapabilityHandler = "work_runs.get"
	CapabilityHandlerWorkRunsStart    CapabilityHandler = "work_runs.start"
	CapabilityHandlerWorkRunsPause    CapabilityHandler = "work_runs.pause"
	CapabilityHandlerWorkRunsResume   CapabilityHandler = "work_runs.resume"
	CapabilityHandlerWorkRunsComplete CapabilityHandler = "work_runs.complete"
	CapabilityHandlerWorkRunsCancel   CapabilityHandler = "work_runs.cancel"
	CapabilityHandlerPlansUpsert      CapabilityHandler = "plans.upsert"
	CapabilityHandlerPlansList        CapabilityHandler = "plans.list"
	CapabilityHandlerPlansItems       CapabilityHandler = "plans.items"
)

const (
	CapabilityHandlerTasksList                    CapabilityHandler = "project_tasks.list"
	CapabilityHandlerTasksGet                     CapabilityHandler = "project_tasks.get"
	CapabilityHandlerTasksCreate                  CapabilityHandler = "project_tasks.create"
	CapabilityHandlerTasksUpdate                  CapabilityHandler = "project_tasks.update"
	CapabilityHandlerTasksChangeStatus            CapabilityHandler = "project_tasks.change_status"
	CapabilityHandlerTasksDelete                  CapabilityHandler = "project_tasks.delete"
	CapabilityHandlerTasksDuplicate               CapabilityHandler = "project_tasks.duplicate"
	CapabilityHandlerTasksReorderProject          CapabilityHandler = "project_tasks.reorder_project"
	CapabilityHandlerTasksReorderGlobal           CapabilityHandler = "project_tasks.reorder_global"
	CapabilityHandlerTasksApproveVerification     CapabilityHandler = "project_tasks.approve_verification"
	CapabilityHandlerTasksApproveVerificationBulk CapabilityHandler = "project_tasks.approve_verification_bulk"
	CapabilityHandlerTasksRejectVerification      CapabilityHandler = "project_tasks.reject_verification"
	CapabilityHandlerTasksLinkSession             CapabilityHandler = "project_tasks.link_session"
	CapabilityHandlerTasksUnlinkSession           CapabilityHandler = "project_tasks.unlink_session"
	CapabilityHandlerTasksAddComment              CapabilityHandler = "project_tasks.add_comment"
)

const (
	CapabilityScopeTasksRead     CapabilityScope = "tasks:read"
	CapabilityScopeTasksWrite    CapabilityScope = "tasks:write"
	CapabilityScopeWorkRunsRead  CapabilityScope = "work_runs:read"
	CapabilityScopeWorkRunsWrite CapabilityScope = "work_runs:write"
	CapabilityScopePlansRead     CapabilityScope = "plans:read"
	CapabilityScopePlansWrite    CapabilityScope = "plans:write"
)

type CapabilityService interface {
	CapabilityServiceName() CapabilityServiceName
}

func RegisterWorkRunCapabilities(registry *CapabilityRegistry, service *WorkRunService) error {
	if registry == nil || service == nil {
		return errors.New("capability registry and work run service are required")
	}
	definitions := []Capability{
		{Name: CapabilityWorkRunsList, Scope: CapabilityScopeWorkRunsRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerWorkRunsList, Service: service},
		{Name: CapabilityWorkRunsGet, Scope: CapabilityScopeWorkRunsRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerWorkRunsGet, Service: service},
		{Name: CapabilityWorkRunsStart, Scope: CapabilityScopeWorkRunsWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerWorkRunsStart, Service: service},
		{Name: CapabilityWorkRunsPause, Scope: CapabilityScopeWorkRunsWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerWorkRunsPause, Service: service},
		{Name: CapabilityWorkRunsResume, Scope: CapabilityScopeWorkRunsWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerWorkRunsResume, Service: service},
		{Name: CapabilityWorkRunsComplete, Scope: CapabilityScopeWorkRunsWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerWorkRunsComplete, Service: service},
		{Name: CapabilityWorkRunsCancel, Scope: CapabilityScopeWorkRunsWrite, Risk: CapabilityRiskDestructive, Approval: ApprovalExplicit, Handler: CapabilityHandlerWorkRunsCancel, Service: service},
		{Name: CapabilityPlansUpsert, Scope: CapabilityScopePlansWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerPlansUpsert, Service: service},
		{Name: CapabilityPlansList, Scope: CapabilityScopePlansRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerPlansList, Service: service},
		{Name: CapabilityPlansItems, Scope: CapabilityScopePlansRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerPlansItems, Service: service},
	}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			return err
		}
	}
	return nil
}

type Capability struct {
	Name     CapabilityName
	Scope    CapabilityScope
	Risk     CapabilityRisk
	Approval ApprovalPolicy
	Handler  CapabilityHandler
	Service  CapabilityService
}

type CapabilityRegistry struct {
	mu           sync.RWMutex
	capabilities map[CapabilityName]Capability
}

func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{capabilities: make(map[CapabilityName]Capability)}
}

func NewProjectTaskCapabilityRegistry(service *ProjectTaskService) (*CapabilityRegistry, error) {
	if service == nil {
		return nil, errors.New("project task service is required")
	}
	registry := NewCapabilityRegistry()
	definitions := []Capability{
		{Name: CapabilityTasksList, Scope: CapabilityScopeTasksRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerTasksList, Service: service},
		{Name: CapabilityTasksGet, Scope: CapabilityScopeTasksRead, Risk: CapabilityRiskRead, Approval: ApprovalNone, Handler: CapabilityHandlerTasksGet, Service: service},
		{Name: CapabilityTasksCreate, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksCreate, Service: service},
		{Name: CapabilityTasksUpdate, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksUpdate, Service: service},
		{Name: CapabilityTasksChangeStatus, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksChangeStatus, Service: service},
		{Name: CapabilityTasksDelete, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskDestructive, Approval: ApprovalExplicit, Handler: CapabilityHandlerTasksDelete, Service: service},
		{Name: CapabilityTasksDuplicate, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksDuplicate, Service: service},
		{Name: CapabilityTasksReorderProject, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksReorderProject, Service: service},
		{Name: CapabilityTasksReorderGlobal, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksReorderGlobal, Service: service},
		{Name: CapabilityTasksApproveVerification, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalExplicit, Handler: CapabilityHandlerTasksApproveVerification, Service: service},
		{Name: CapabilityTasksApproveVerificationBulk, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalExplicit, Handler: CapabilityHandlerTasksApproveVerificationBulk, Service: service},
		{Name: CapabilityTasksRejectVerification, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalExplicit, Handler: CapabilityHandlerTasksRejectVerification, Service: service},
		{Name: CapabilityTasksLinkSession, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksLinkSession, Service: service},
		{Name: CapabilityTasksUnlinkSession, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksUnlinkSession, Service: service},
		{Name: CapabilityTasksAddComment, Scope: CapabilityScopeTasksWrite, Risk: CapabilityRiskWrite, Approval: ApprovalByPolicy, Handler: CapabilityHandlerTasksAddComment, Service: service},
	}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *CapabilityRegistry) Register(capability Capability) error {
	if r == nil {
		return errors.New("capability registry is nil")
	}
	if strings.TrimSpace(string(capability.Name)) == "" {
		return errors.New("capability name is required")
	}
	if strings.TrimSpace(string(capability.Scope)) == "" {
		return fmt.Errorf("capability %s scope is required", capability.Name)
	}
	if !validCapabilityRisk(capability.Risk) {
		return fmt.Errorf("capability %s has invalid risk %q", capability.Name, capability.Risk)
	}
	if !validApprovalPolicy(capability.Approval) {
		return fmt.Errorf("capability %s has invalid approval policy %q", capability.Name, capability.Approval)
	}
	if strings.TrimSpace(string(capability.Handler)) == "" {
		return fmt.Errorf("capability %s handler is required", capability.Name)
	}
	if capability.Service == nil {
		return fmt.Errorf("capability %s service is required", capability.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.capabilities[capability.Name]; exists {
		return fmt.Errorf("capability %s is already registered", capability.Name)
	}
	r.capabilities[capability.Name] = capability
	return nil
}

func (r *CapabilityRegistry) Lookup(name CapabilityName) (Capability, bool) {
	if r == nil {
		return Capability{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	capability, ok := r.capabilities[name]
	return capability, ok
}

func (r *CapabilityRegistry) List() []Capability {
	if r == nil {
		return []Capability{}
	}
	r.mu.RLock()
	capabilities := make([]Capability, 0, len(r.capabilities))
	for _, capability := range r.capabilities {
		capabilities = append(capabilities, capability)
	}
	r.mu.RUnlock()
	sort.Slice(capabilities, func(i, j int) bool {
		return capabilities[i].Name < capabilities[j].Name
	})
	return capabilities
}

func validCapabilityRisk(risk CapabilityRisk) bool {
	switch risk {
	case CapabilityRiskRead, CapabilityRiskWrite, CapabilityRiskDestructive, CapabilityRiskUnsafe:
		return true
	default:
		return false
	}
}

func validApprovalPolicy(policy ApprovalPolicy) bool {
	switch policy {
	case ApprovalNone, ApprovalByPolicy, ApprovalExplicit:
		return true
	default:
		return false
	}
}
