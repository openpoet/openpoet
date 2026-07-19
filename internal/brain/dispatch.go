package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/coordinator"
)

// coordinatorActor loads the scoped 'coordinator' automation client and builds
// the in-process dispatch actor (mirrors the HTTP auth middleware). Returns nil
// if the client is missing/disabled.
func (c *Consultant) coordinatorActor(ctx context.Context) *automation.Actor {
	client, err := c.db.GetAutomationClientByName(ctx, automation.CoordinatorClientName)
	if err != nil || client == nil || !client.Enabled {
		return nil
	}
	scopes, err := automation.ParseScopeSet(client.Scopes)
	if err != nil {
		return nil
	}
	return &automation.Actor{
		Type: "automation_client", ID: client.ID, Name: client.Name,
		Scopes: scopes, ClientID: client.ID,
	}
}

// dispatchSessionVerb routes stop/message/set_model. stop_session is destructive
// (ApprovalExplicit) — the coordinator holds no approvals scope, so it supplies
// NO approval and receives platform_approval_required cleanly: the brain
// proposes, a human grant still gates the stop. message/set_model are write-tier
// (ApprovalByPolicy) and self-approve.
func (c *Consultant) dispatchSessionVerb(ctx context.Context, inc coordinator.Incident, verb string, act action, cost float64) {
	actor := c.coordinatorActor(ctx)
	if actor == nil {
		c.record(ctx, inc, verb, "no_actor", cost)
		return
	}
	target, _ := json.Marshal(map[string]string{"type": "session", "id": act.SessionID})

	switch verb {
	case verbStopSession:
		req := automation.PlatformDispatchRequest{
			Capability: "sessions.stop", Target: target, Payload: json.RawMessage(`{}`),
			Actor: *actor, Reason: "coordinator: resolve conflict " + inc.ID,
			// No approval supplied on purpose → the human grant still gates it.
		}
		_, err := automation.DispatchPlatformCapability(ctx, c.registry, req)
		decision := "executed"
		if err != nil {
			if strings.Contains(err.Error(), "approval") {
				decision = "approval_required"
			} else {
				decision = "dispatch_error"
			}
		}
		c.record(ctx, inc, verb, decision, cost)
		if decision == "approval_required" {
			c.escalateIncident(ctx, inc, "Coordinator wants to stop a session", fmt.Sprintf("The coordinator proposed stopping session %s; issue a grant to proceed.", act.SessionID), "", cost)
		}
	case verbMessage:
		payload, _ := json.Marshal(map[string]interface{}{"text": strOr(act.Text, act.Reason), "await_ack": false, "force": true})
		c.dispatchWriteTier(ctx, inc, actor, "sessions.send_input", target, payload, verb, cost)
	case verbSetModel:
		payload, _ := json.Marshal(map[string]string{"model": act.Model})
		c.dispatchWriteTier(ctx, inc, actor, "sessions.set_model", target, payload, verb, cost)
	}
}

// dispatchSpawn runs delegate-mode spawn_session with the brief as custom_prompt.
func (c *Consultant) dispatchSpawn(ctx context.Context, inc coordinator.Incident, act action, cost float64) {
	actor := c.coordinatorActor(ctx)
	if actor == nil {
		c.record(ctx, inc, verbSpawnSession, "no_actor", cost)
		return
	}
	target, _ := json.Marshal(map[string]interface{}{"type": "project", "project_id": inc.ProjectID})
	payloadMap := map[string]interface{}{"custom_prompt": strOr(act.Brief, act.Reason)}
	if act.TaskID != nil && *act.TaskID > 0 {
		payloadMap["task_id"] = *act.TaskID
	}
	payload, _ := json.Marshal(payloadMap)
	c.dispatchWriteTier(ctx, inc, actor, "sessions.create", target, payload, verbSpawnSession, cost)
}

// dispatchWriteTier self-approves a write-tier (ApprovalByPolicy) capability —
// the acting client is its own policy approver, exactly as the HTTP command path
// does for write capabilities.
func (c *Consultant) dispatchWriteTier(ctx context.Context, inc coordinator.Incident, actor *automation.Actor, capName string, target, payload []byte, verb string, cost float64) {
	approval, apErr := automation.NewValidatedPlatformApproval(actor.ClientID)
	if apErr != nil {
		c.record(ctx, inc, verb, "dispatch_error", cost)
		return
	}
	req := automation.PlatformDispatchRequest{
		Capability: application.CapabilityName(capName), Target: target, Payload: payload,
		Actor: *actor, Reason: "coordinator: resolve conflict " + inc.ID, Approval: approval,
	}
	_, err := automation.DispatchPlatformCapability(ctx, c.registry, req)
	decision := "executed"
	if err != nil {
		if strings.Contains(err.Error(), "approval") {
			decision = "approval_required"
		} else {
			decision = "dispatch_error"
		}
	}
	c.record(ctx, inc, verb, decision, cost)
}
