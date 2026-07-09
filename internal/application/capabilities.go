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
	CapabilityTasksList                CapabilityName = "tasks.list"
	CapabilityTasksGet                 CapabilityName = "tasks.get"
	CapabilityTasksCreate              CapabilityName = "tasks.create"
	CapabilityTasksUpdate              CapabilityName = "tasks.update"
	CapabilityTasksChangeStatus        CapabilityName = "tasks.change_status"
	CapabilityTasksDelete              CapabilityName = "tasks.delete"
	CapabilityTasksDuplicate           CapabilityName = "tasks.duplicate"
	CapabilityTasksReorderProject      CapabilityName = "tasks.reorder_project"
	CapabilityTasksReorderGlobal       CapabilityName = "tasks.reorder_global"
	CapabilityTasksApproveVerification CapabilityName = "tasks.approve_verification"
	CapabilityTasksRejectVerification  CapabilityName = "tasks.reject_verification"
	CapabilityTasksLinkSession         CapabilityName = "tasks.link_session"
	CapabilityTasksUnlinkSession       CapabilityName = "tasks.unlink_session"
	CapabilityTasksAddComment          CapabilityName = "tasks.add_comment"
)

const (
	CapabilityHandlerTasksList                CapabilityHandler = "project_tasks.list"
	CapabilityHandlerTasksGet                 CapabilityHandler = "project_tasks.get"
	CapabilityHandlerTasksCreate              CapabilityHandler = "project_tasks.create"
	CapabilityHandlerTasksUpdate              CapabilityHandler = "project_tasks.update"
	CapabilityHandlerTasksChangeStatus        CapabilityHandler = "project_tasks.change_status"
	CapabilityHandlerTasksDelete              CapabilityHandler = "project_tasks.delete"
	CapabilityHandlerTasksDuplicate           CapabilityHandler = "project_tasks.duplicate"
	CapabilityHandlerTasksReorderProject      CapabilityHandler = "project_tasks.reorder_project"
	CapabilityHandlerTasksReorderGlobal       CapabilityHandler = "project_tasks.reorder_global"
	CapabilityHandlerTasksApproveVerification CapabilityHandler = "project_tasks.approve_verification"
	CapabilityHandlerTasksRejectVerification  CapabilityHandler = "project_tasks.reject_verification"
	CapabilityHandlerTasksLinkSession         CapabilityHandler = "project_tasks.link_session"
	CapabilityHandlerTasksUnlinkSession       CapabilityHandler = "project_tasks.unlink_session"
	CapabilityHandlerTasksAddComment          CapabilityHandler = "project_tasks.add_comment"
)

const (
	CapabilityScopeTasksRead  CapabilityScope = "tasks:read"
	CapabilityScopeTasksWrite CapabilityScope = "tasks:write"
)

type CapabilityService interface {
	CapabilityServiceName() CapabilityServiceName
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
