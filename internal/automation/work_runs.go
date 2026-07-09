package automation

import (
	"net/http"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

type workRunListPayload struct {
	Status     string `json:"status,omitempty"`
	Source     string `json:"source,omitempty"`
	ActiveOnly bool   `json:"active_only,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type workRunReferencePayload struct {
	WorkRunID string `json:"work_run_id,omitempty"`
}

type workRunStartPayload struct {
	Title           string                           `json:"title"`
	Description     string                           `json:"description,omitempty"`
	Source          string                           `json:"source,omitempty"`
	ExpectedMinutes int                              `json:"expected_minutes"`
	ProjectTaskID   *int64                           `json:"project_task_id,omitempty"`
	SessionID       *string                          `json:"session_id,omitempty"`
	PlanItemRef     *string                          `json:"plan_item_ref,omitempty"`
	ExecutionTarget *database.WorkRunExecutionTarget `json:"execution_target,omitempty"`
}

type workRunTransitionPayload struct {
	WorkRunID           string `json:"work_run_id,omitempty"`
	CompleteProjectTask bool   `json:"complete_project_task,omitempty"`
}

type planUpsertPayload struct {
	ExternalRef string                  `json:"external_ref"`
	Kind        string                  `json:"kind"`
	Title       string                  `json:"title"`
	PeriodStart string                  `json:"period_start"`
	PeriodEnd   string                  `json:"period_end"`
	Timezone    string                  `json:"timezone"`
	Items       []planUpsertItemPayload `json:"items"`
}

type planUpsertItemPayload struct {
	ExternalRef   string `json:"external_ref"`
	Title         string `json:"title"`
	Description   string `json:"description,omitempty"`
	Status        string `json:"status,omitempty"`
	SortOrder     int    `json:"sort_order,omitempty"`
	ProjectTaskID *int64 `json:"project_task_id,omitempty"`
}

type planListPayload struct {
	Kind        string `json:"kind,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type planItemsPayload struct {
	PlanRef string `json:"plan_ref,omitempty"`
}

func dispatchWorkRunCommand(r *http.Request, service *application.WorkRunService, capability application.Capability, command *commandEnvelope, actor Actor) (any, error) {
	applicationActor := application.Actor{Type: actor.Type, ID: actor.ClientID}
	correlationID := strings.TrimSpace(command.CorrelationID)
	idempotencyKey := strings.TrimSpace(command.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = idempotencyKeyFromContext(r.Context())
	}
	commandContext := application.WithEventMetadata(r.Context(), application.EventMetadata{Actor: applicationActor, CorrelationID: correlationID})

	switch capability.Handler {
	case application.CapabilityHandlerWorkRunsList:
		var payload workRunListPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"status": payload.Status, "active_only": payload.ActiveOnly}), nil
		}
		return service.List(commandContext, database.WorkRunFilter{Status: payload.Status, Source: payload.Source, ActiveOnly: payload.ActiveOnly, Limit: payload.Limit})

	case application.CapabilityHandlerWorkRunsGet:
		var payload workRunReferencePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		id := command.Target.resolveStringID("work_run", payload.WorkRunID)
		if id == "" {
			return nil, invalidTarget("work_run_id is required")
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"work_run_id": id}), nil
		}
		return service.Get(commandContext, id)

	case application.CapabilityHandlerWorkRunsStart:
		var payload workRunStartPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"title": payload.Title, "expected_minutes": payload.ExpectedMinutes}), nil
		}
		return service.Start(commandContext, application.StartWorkRunCommand{
			Title: payload.Title, Description: payload.Description, Source: payload.Source,
			ExpectedMinutes: payload.ExpectedMinutes, ProjectTaskID: payload.ProjectTaskID,
			SessionID: payload.SessionID, PlanItemRef: payload.PlanItemRef, ExecutionTarget: payload.ExecutionTarget,
			Actor: applicationActor, CorrelationID: correlationID, IdempotencyKey: idempotencyKey,
		})

	case application.CapabilityHandlerWorkRunsPause, application.CapabilityHandlerWorkRunsResume,
		application.CapabilityHandlerWorkRunsComplete, application.CapabilityHandlerWorkRunsCancel:
		var payload workRunTransitionPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		id := command.Target.resolveStringID("work_run", payload.WorkRunID)
		if id == "" {
			return nil, invalidTarget("work_run_id is required")
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"work_run_id": id, "expected_version": command.ExpectedVersion}), nil
		}
		transition := application.TransitionWorkRunCommand{
			WorkRunID: id, ExpectedVersion: command.ExpectedVersion, CompleteProjectTask: payload.CompleteProjectTask,
			Reason: command.Reason, Actor: applicationActor, CorrelationID: correlationID, IdempotencyKey: idempotencyKey,
		}
		switch capability.Handler {
		case application.CapabilityHandlerWorkRunsPause:
			return service.Pause(commandContext, transition)
		case application.CapabilityHandlerWorkRunsResume:
			return service.Resume(commandContext, transition)
		case application.CapabilityHandlerWorkRunsComplete:
			return service.Complete(commandContext, transition)
		default:
			return service.Cancel(commandContext, transition)
		}

	case application.CapabilityHandlerPlansUpsert:
		var payload planUpsertPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"external_ref": payload.ExternalRef, "item_count": len(payload.Items)}), nil
		}
		items := make([]application.UpsertPlanItem, len(payload.Items))
		for index, item := range payload.Items {
			items[index] = application.UpsertPlanItem{
				ExternalRef: item.ExternalRef, Title: item.Title, Description: item.Description,
				Status: item.Status, SortOrder: item.SortOrder, ProjectTaskID: item.ProjectTaskID,
			}
		}
		return service.UpsertPlan(commandContext, application.UpsertPlanCommand{
			ExternalRef: payload.ExternalRef, Kind: payload.Kind, Title: payload.Title,
			PeriodStart: payload.PeriodStart, PeriodEnd: payload.PeriodEnd, Timezone: payload.Timezone,
			Items: items, ExpectedVersion: command.ExpectedVersion, Actor: applicationActor,
			CorrelationID: correlationID, IdempotencyKey: idempotencyKey,
		})

	case application.CapabilityHandlerPlansList:
		var payload planListPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"kind": payload.Kind, "external_ref": payload.ExternalRef}), nil
		}
		return service.ListPlans(commandContext, database.PlanFilter{
			Kind: payload.Kind, ExternalRef: payload.ExternalRef, From: payload.From, To: payload.To, Limit: payload.Limit,
		})

	case application.CapabilityHandlerPlansItems:
		var payload planItemsPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return nil, err
		}
		ref := command.Target.resolveStringID("plan", payload.PlanRef)
		if ref == "" {
			return nil, invalidTarget("plan_ref is required")
		}
		if command.DryRun {
			return dryRunResult(capability, map[string]any{"plan_ref": ref}), nil
		}
		return service.ListPlanItems(commandContext, ref)
	default:
		return nil, &commandFailure{status: http.StatusNotImplemented, code: "capability_handler_unsupported", message: "the capability handler is not implemented"}
	}
}
