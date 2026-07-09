package application

import (
	"context"
	"strings"
)

type ProposalKind string
type ProposalRisk string

const (
	ProposalMemory ProposalKind = "memory"
	ProposalTask   ProposalKind = "task"
	ProposalSkill  ProposalKind = "skill"
	ProposalTool   ProposalKind = "tool"

	ProposalRiskR2 ProposalRisk = "R2"
	ProposalRiskR3 ProposalRisk = "R3"
	ProposalRiskR4 ProposalRisk = "R4"
)

type ProposalRecord struct {
	ID             string
	Kind           ProposalKind
	Risk           ProposalRisk
	Status         string
	Title          string
	Summary        string
	ProjectID      int64
	ConversationID *int64
}

type ProposalAcceptance struct {
	Status       string
	Action       string
	Message      string
	Output       string
	CreatedCount int
	UpdatedCount int
	DeletedCount int
}

// ProposalBackend owns durable pending payloads. AcceptProposalAtomic and
// RejectProposalAtomic are transactional boundaries: status and every database
// mutation caused by the proposal must commit or roll back together.
type ProposalBackend interface {
	CreateMemoryProposalAtomic(context.Context, MemoryProposal) (*ProposalRecord, error)
	GetPendingProposal(context.Context, string, ProposalKind) (*ProposalRecord, error)
	AcceptProposalAtomic(context.Context, string, ProposalKind, ActionAuthorization) (ProposalAcceptance, error)
	RejectProposalAtomic(context.Context, string, ProposalKind, ActionAuthorization) error
}

type MemoryProposal struct {
	ProjectID      int64
	Content        string
	Summary        string
	ConversationID *int64
	Authorization  ActionAuthorization
}

type ProposalView struct {
	ID             string       `json:"id"`
	Kind           ProposalKind `json:"kind"`
	Risk           ProposalRisk `json:"risk"`
	Status         string       `json:"status"`
	Title          string       `json:"title,omitempty"`
	Summary        string       `json:"summary,omitempty"`
	ProjectID      int64        `json:"project_id,omitempty"`
	ConversationID *int64       `json:"conversation_id,omitempty"`
}

type ProposalAcceptanceView struct {
	Status       string `json:"status"`
	Action       string `json:"action,omitempty"`
	Message      string `json:"message,omitempty"`
	Output       string `json:"output,omitempty"`
	CreatedCount int    `json:"created,omitempty"`
	UpdatedCount int    `json:"updated,omitempty"`
	DeletedCount int    `json:"deleted,omitempty"`
	WasTruncated bool   `json:"was_truncated,omitempty"`
}

type ProposalService struct {
	backend ProposalBackend
	effects ApplicationEffects
}

func NewProposalService(backend ProposalBackend, effects ApplicationEffects) *ProposalService {
	return &ProposalService{backend: backend, effects: effects}
}

func (s *ProposalService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("proposals")
}

func (s *ProposalService) ProposeMemory(ctx context.Context, command MemoryProposal) (ProposalView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ProposalView{}, err
	}
	if err := validateBoundedID(command.ProjectID, "Project"); err != nil {
		return ProposalView{}, err
	}
	content, err := boundedRedactedInput(command.Content, maxApplicationContentRunes, "Memory proposal content", true)
	if err != nil {
		return ProposalView{}, err
	}
	summary, err := boundedRedactedInput(command.Summary, maxApplicationSummaryRunes, "Memory proposal summary", false)
	if err != nil {
		return ProposalView{}, err
	}
	if command.ConversationID != nil && *command.ConversationID <= 0 {
		return ProposalView{}, validationError("invalid_conversation_id", "Conversation ID must be positive")
	}
	if s.backend == nil {
		return ProposalView{}, validationError("proposal_backend_unavailable", "Proposal backend is unavailable")
	}
	command.Content, command.Summary = content, summary
	record, err := s.backend.CreateMemoryProposalAtomic(ctx, command)
	if err != nil {
		return ProposalView{}, safeBackendError("Memory proposal could not be created", err)
	}
	if record == nil {
		return ProposalView{}, safeBackendError("Memory proposal could not be created", nil)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "proposals", Action: "memory_created", ID: command.ProjectID, Meta: map[string]any{"proposal_id": record.ID}})
	return proposalView(record), nil
}

func (s *ProposalService) ApproveMemory(ctx context.Context, id string, authorization ActionAuthorization) (ProposalAcceptanceView, error) {
	return s.approve(ctx, id, ProposalMemory, authorization)
}

func (s *ProposalService) ApproveTask(ctx context.Context, id string, authorization ActionAuthorization) (ProposalAcceptanceView, error) {
	return s.approve(ctx, id, ProposalTask, authorization)
}

func (s *ProposalService) ApproveSkill(ctx context.Context, id string, authorization ActionAuthorization) (ProposalAcceptanceView, error) {
	return s.approve(ctx, id, ProposalSkill, authorization)
}

func (s *ProposalService) ApproveTool(ctx context.Context, id string, authorization ActionAuthorization) (ProposalAcceptanceView, error) {
	return s.approve(ctx, id, ProposalTool, authorization)
}

func (s *ProposalService) RejectMemory(ctx context.Context, id string, authorization ActionAuthorization) error {
	return s.reject(ctx, id, ProposalMemory, authorization)
}

func (s *ProposalService) RejectTask(ctx context.Context, id string, authorization ActionAuthorization) error {
	return s.reject(ctx, id, ProposalTask, authorization)
}

func (s *ProposalService) RejectSkill(ctx context.Context, id string, authorization ActionAuthorization) error {
	return s.reject(ctx, id, ProposalSkill, authorization)
}

func (s *ProposalService) RejectTool(ctx context.Context, id string, authorization ActionAuthorization) error {
	return s.reject(ctx, id, ProposalTool, authorization)
}

func (s *ProposalService) approve(ctx context.Context, id string, kind ProposalKind, authorization ActionAuthorization) (ProposalAcceptanceView, error) {
	if err := requireActionActor(authorization); err != nil {
		return ProposalAcceptanceView{}, err
	}
	id, err := validateProposalID(id)
	if err != nil {
		return ProposalAcceptanceView{}, err
	}
	if s.backend == nil {
		return ProposalAcceptanceView{}, validationError("proposal_backend_unavailable", "Proposal backend is unavailable")
	}
	record, err := s.backend.GetPendingProposal(ctx, id, kind)
	if err != nil || record == nil {
		return ProposalAcceptanceView{}, notFoundError("proposal_not_found", "Pending proposal not found", err)
	}
	if record.Status != "pending" {
		return ProposalAcceptanceView{}, conflictError("proposal_already_processed", "Proposal has already been processed")
	}
	risk := record.Risk
	if risk == "" {
		if kind == ProposalTool {
			risk = ProposalRiskR4
		} else if kind == ProposalTask {
			risk = ProposalRiskR3
		} else {
			risk = ProposalRiskR2
		}
	}
	if kind == ProposalTask && risk == ProposalRiskR2 {
		// A task proposal is a batch and may contain deletes. The application
		// boundary stays R3 even if an adapter under-classifies its payload.
		risk = ProposalRiskR3
	}
	if kind == ProposalTool && risk != ProposalRiskR4 {
		return ProposalAcceptanceView{}, validationError("invalid_tool_proposal_risk", "Tool proposals must be classified R4")
	}
	if risk == ProposalRiskR3 || risk == ProposalRiskR4 {
		if err = requireExplicitActionApproval(authorization); err != nil {
			return ProposalAcceptanceView{}, err
		}
	}
	accepted, err := s.backend.AcceptProposalAtomic(ctx, id, kind, authorization)
	if err != nil {
		return ProposalAcceptanceView{}, safeBackendError("Proposal approval failed", err)
	}
	message, messageTruncated := boundedRedactedOutput(accepted.Message, maxApplicationSummaryRunes)
	output, outputTruncated := boundedRedactedOutput(accepted.Output, maxApplicationOutputRunes)
	status, statusTruncated := boundedRedactedOutput(defaultStatus(accepted.Status, "approved"), 100)
	action, actionTruncated := boundedRedactedOutput(accepted.Action, 100)
	view := ProposalAcceptanceView{Status: status, Action: action, Message: message, Output: output, CreatedCount: max(0, accepted.CreatedCount), UpdatedCount: max(0, accepted.UpdatedCount), DeletedCount: max(0, accepted.DeletedCount), WasTruncated: messageTruncated || outputTruncated || statusTruncated || actionTruncated}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "proposals", Action: string(kind) + "_approved", Meta: map[string]any{"proposal_id": id}})
	return view, nil
}

func (s *ProposalService) reject(ctx context.Context, id string, kind ProposalKind, authorization ActionAuthorization) error {
	if err := requireActionActor(authorization); err != nil {
		return err
	}
	id, err := validateProposalID(id)
	if err != nil {
		return err
	}
	if s.backend == nil {
		return validationError("proposal_backend_unavailable", "Proposal backend is unavailable")
	}
	record, err := s.backend.GetPendingProposal(ctx, id, kind)
	if err != nil || record == nil {
		return notFoundError("proposal_not_found", "Pending proposal not found", err)
	}
	if record.Status != "pending" {
		return conflictError("proposal_already_processed", "Proposal has already been processed")
	}
	if err = s.backend.RejectProposalAtomic(ctx, id, kind, authorization); err != nil {
		return safeBackendError("Proposal rejection failed", err)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "proposals", Action: string(kind) + "_rejected", Meta: map[string]any{"proposal_id": id}})
	return nil
}

func proposalView(record *ProposalRecord) ProposalView {
	title, _ := boundedRedactedOutput(record.Title, maxApplicationTitleRunes)
	summary, _ := boundedRedactedOutput(record.Summary, maxApplicationSummaryRunes)
	return ProposalView{ID: record.ID, Kind: record.Kind, Risk: record.Risk, Status: record.Status, Title: title, Summary: summary, ProjectID: record.ProjectID, ConversationID: record.ConversationID}
}

func validateProposalID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 100 {
		return "", validationError("invalid_proposal_id", "Proposal ID is invalid")
	}
	return id, nil
}

func defaultStatus(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
