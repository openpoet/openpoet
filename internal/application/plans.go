package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"openpoet/internal/database"
)

type UpsertPlanItem struct {
	ExternalRef   string
	Title         string
	Description   string
	Status        string
	SortOrder     int
	ProjectTaskID *int64
}

type UpsertPlanCommand struct {
	ExternalRef     string
	Kind            string
	Title           string
	PeriodStart     string
	PeriodEnd       string
	Timezone        string
	Items           []UpsertPlanItem
	ExpectedVersion *int64
	Actor           Actor
	CorrelationID   string
	IdempotencyKey  string
}

func (s *WorkRunService) UpsertPlan(ctx context.Context, command UpsertPlanCommand) (*database.Plan, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("work run service is unavailable")
	}
	if err := validatePlanCommand(command); err != nil {
		return nil, err
	}
	if err := validateWorkRunMetadata(command.Actor, command.CorrelationID, command.IdempotencyKey); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	actor := command.Actor.eventValue()
	correlationID := strings.TrimSpace(command.CorrelationID)
	idempotencyKey := strings.TrimSpace(command.IdempotencyKey)
	var result *database.Plan
	err := s.store.WithWorkRunTx(ctx, func(tx *database.WorkRunTx) error {
		plan, err := tx.GetPlanByExternalRef(ctx, strings.TrimSpace(command.ExternalRef))
		creating := errors.Is(err, sql.ErrNoRows)
		if err != nil && !creating {
			return err
		}
		if creating && command.ExpectedVersion != nil && *command.ExpectedVersion != 0 {
			return conflictError("plan_version_conflict", "A new plan requires expected_version 0 or no expected_version")
		}
		var existingItems []database.PlanItem
		if creating {
			plan = &database.Plan{
				ID: s.newID(), ExternalRef: strings.TrimSpace(command.ExternalRef), Version: 1,
				CreatedByActor: actor, CreatedAt: now,
			}
		} else {
			items, err := tx.ListPlanItems(ctx, plan.ID)
			if err != nil {
				return err
			}
			plan.Items = items
			existingItems = items
			if idempotencyKey != "" && plan.IdempotencyKey != nil && *plan.IdempotencyKey == idempotencyKey {
				if !samePlanCommand(plan, command) {
					return conflictError("plan_idempotency_conflict", "Idempotency key was already used for a different plan payload")
				}
				result = plan
				return nil
			}
			if command.ExpectedVersion != nil && plan.Version != *command.ExpectedVersion {
				return conflictError("plan_version_conflict", fmt.Sprintf("Plan version is %d, expected %d", plan.Version, *command.ExpectedVersion))
			}
			plan.Version++
		}

		plan.Kind = strings.TrimSpace(command.Kind)
		plan.Title = strings.TrimSpace(command.Title)
		plan.PeriodStart = strings.TrimSpace(command.PeriodStart)
		plan.PeriodEnd = strings.TrimSpace(command.PeriodEnd)
		plan.Timezone = strings.TrimSpace(command.Timezone)
		plan.UpdatedByActor = actor
		plan.CorrelationID = correlationID
		plan.IdempotencyKey = nullableString(idempotencyKey)
		plan.UpdatedAt = now
		if creating {
			if err := tx.InsertPlan(ctx, plan); err != nil {
				return err
			}
		} else if err := tx.UpdatePlan(ctx, plan, plan.Version-1); err != nil {
			if errors.Is(err, database.ErrPlanVersionConflict) {
				return conflictError("plan_version_conflict", "Plan changed concurrently")
			}
			return err
		}

		processed := make(map[string]struct{}, len(command.Items))
		for _, input := range command.Items {
			if input.ProjectTaskID != nil {
				if *input.ProjectTaskID <= 0 {
					return validationError("project_task_invalid", "project_task_id must be positive")
				}
				if _, err := tx.GetProjectTask(ctx, *input.ProjectTaskID); err != nil {
					return validationError("project_task_invalid", "Project task not found")
				}
			}
			externalRef := strings.TrimSpace(input.ExternalRef)
			processed[externalRef] = struct{}{}
			item, err := tx.GetPlanItemByRef(ctx, externalRef)
			itemCreating := errors.Is(err, sql.ErrNoRows)
			if err != nil && !itemCreating {
				return err
			}
			if itemCreating {
				item = &database.PlanItem{ID: s.newID(), PlanID: plan.ID, ExternalRef: externalRef, Version: 1, CreatedAt: now}
			} else if item.PlanID != plan.ID {
				return conflictError("plan_item_ref_conflict", "Plan item external_ref belongs to another plan")
			} else {
				item.Version++
			}
			item.Title = strings.TrimSpace(input.Title)
			item.Description = input.Description
			item.Status = normalizedPlanItemStatus(input.Status)
			item.SortOrder = input.SortOrder
			item.ProjectTaskID = input.ProjectTaskID
			item.UpdatedByActor = actor
			item.UpdatedAt = now
			if itemCreating {
				if err := tx.InsertPlanItem(ctx, item); err != nil {
					return err
				}
			} else if err := tx.UpdatePlanItem(ctx, item, item.Version-1); err != nil {
				return conflictError("plan_version_conflict", "Plan item changed concurrently")
			}
		}
		for index := range existingItems {
			item := &existingItems[index]
			if _, ok := processed[item.ExternalRef]; ok || item.Status == "cancelled" {
				continue
			}
			previousVersion := item.Version
			item.Status = "cancelled"
			item.Version++
			item.UpdatedByActor = actor
			item.UpdatedAt = now
			if err := tx.UpdatePlanItem(ctx, item, previousVersion); err != nil {
				return conflictError("plan_version_conflict", "Plan item changed concurrently")
			}
		}
		items, err := tx.ListPlanItems(ctx, plan.ID)
		if err != nil {
			return err
		}
		plan.Items = items
		payload, err := json.Marshal(map[string]any{
			"plan_id": plan.ID, "external_ref": plan.ExternalRef, "kind": plan.Kind,
			"version": plan.Version, "item_count": len(items), "submitted_item_count": len(command.Items),
		})
		if err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, database.EventOutboxAppend{
			EventID: uuid.NewString(), EventType: "plan.upserted", AggregateType: "plan",
			AggregateID: plan.ID, Actor: command.Actor.eventValue(), CorrelationID: correlationID,
			SchemaVersion: 1, PayloadJSON: string(payload), OccurredAt: now,
		}); err != nil {
			return err
		}
		result = plan
		return nil
	})
	return result, err
}

func (s *WorkRunService) ListPlans(ctx context.Context, filter database.PlanFilter) ([]database.Plan, error) {
	if filter.Kind != "" && filter.Kind != "daily" && filter.Kind != "weekly" {
		return nil, validationError("plan_kind_invalid", "kind must be daily or weekly")
	}
	return s.store.ListPlans(ctx, filter)
}

func (s *WorkRunService) ListPlanItems(ctx context.Context, planIDOrExternalRef string) ([]database.PlanItem, error) {
	ref := strings.TrimSpace(planIDOrExternalRef)
	if ref == "" {
		return nil, validationError("plan_ref_required", "plan_id or external_ref is required")
	}
	items, err := s.store.ListPlanItems(ctx, ref)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func validatePlanCommand(command UpsertPlanCommand) error {
	if strings.TrimSpace(command.ExternalRef) == "" || len(strings.TrimSpace(command.ExternalRef)) > 200 {
		return validationError("plan_external_ref_invalid", "external_ref is required and must not exceed 200 characters")
	}
	kind := strings.TrimSpace(command.Kind)
	if kind != "daily" && kind != "weekly" {
		return validationError("plan_kind_invalid", "kind must be daily or weekly")
	}
	if strings.TrimSpace(command.Title) == "" || len(strings.TrimSpace(command.Title)) > 500 {
		return validationError("plan_title_invalid", "title is required and must not exceed 500 characters")
	}
	start, err := time.Parse("2006-01-02", strings.TrimSpace(command.PeriodStart))
	if err != nil {
		return validationError("plan_period_invalid", "period_start must use YYYY-MM-DD")
	}
	end, err := time.Parse("2006-01-02", strings.TrimSpace(command.PeriodEnd))
	if err != nil || end.Before(start) {
		return validationError("plan_period_invalid", "period_end must use YYYY-MM-DD and not precede period_start")
	}
	if _, err := time.LoadLocation(strings.TrimSpace(command.Timezone)); err != nil {
		return validationError("plan_timezone_invalid", "timezone must be a valid IANA timezone")
	}
	seen := make(map[string]struct{}, len(command.Items))
	if len(command.Items) > 2_000 {
		return validationError("plan_items_invalid", "A plan cannot contain more than 2000 submitted items")
	}
	for _, item := range command.Items {
		ref := strings.TrimSpace(item.ExternalRef)
		if ref == "" || len(ref) > 200 {
			return validationError("plan_item_ref_invalid", "Every plan item needs an external_ref of at most 200 characters")
		}
		if _, exists := seen[ref]; exists {
			return validationError("plan_item_ref_duplicate", "Plan item external_ref values must be unique")
		}
		seen[ref] = struct{}{}
		if strings.TrimSpace(item.Title) == "" || len(strings.TrimSpace(item.Title)) > 500 {
			return validationError("plan_item_title_invalid", "Every plan item needs a title of at most 500 characters")
		}
		if len(item.Description) > maxWorkRunDescriptionLength {
			return validationError("plan_item_description_invalid", "Plan item description is too long")
		}
		status := normalizedPlanItemStatus(item.Status)
		if status != "planned" && status != "active" && status != "completed" && status != "cancelled" {
			return validationError("plan_item_status_invalid", "Plan item status is invalid")
		}
	}
	return nil
}

func normalizedPlanItemStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "planned"
	}
	return status
}

func samePlanCommand(plan *database.Plan, command UpsertPlanCommand) bool {
	if plan.Kind != strings.TrimSpace(command.Kind) || plan.Title != strings.TrimSpace(command.Title) ||
		plan.PeriodStart != strings.TrimSpace(command.PeriodStart) || plan.PeriodEnd != strings.TrimSpace(command.PeriodEnd) ||
		plan.Timezone != strings.TrimSpace(command.Timezone) {
		return false
	}
	byRef := make(map[string]database.PlanItem, len(plan.Items))
	for _, item := range plan.Items {
		byRef[item.ExternalRef] = item
	}
	for _, input := range command.Items {
		item, ok := byRef[strings.TrimSpace(input.ExternalRef)]
		if !ok || item.Title != strings.TrimSpace(input.Title) || item.Description != input.Description ||
			item.Status != normalizedPlanItemStatus(input.Status) || item.SortOrder != input.SortOrder ||
			!equalInt64Pointers(item.ProjectTaskID, input.ProjectTaskID) {
			return false
		}
	}
	for _, item := range plan.Items {
		if _, submitted := bySubmittedRef(command.Items, item.ExternalRef); !submitted && item.Status != "cancelled" {
			return false
		}
	}
	return true
}

func bySubmittedRef(items []UpsertPlanItem, ref string) (UpsertPlanItem, bool) {
	for _, item := range items {
		if strings.TrimSpace(item.ExternalRef) == ref {
			return item, true
		}
	}
	return UpsertPlanItem{}, false
}
