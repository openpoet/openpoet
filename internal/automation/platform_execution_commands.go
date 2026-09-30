package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// AutomationCommandLedgerPort reads the idempotency ledger. *database.DB
// satisfies it directly.
type AutomationCommandLedgerPort interface {
	FindAutomationCommand(ctx context.Context, clientID, idempotencyKey, commandID string) (*database.AutomationCommand, error)
}

const automationCommandsGetNotes = "Reads the outcome of one of YOUR OWN earlier commands from the idempotency ledger. Target {}; payload {\"idempotency_key\":\"<key>\"} (exact match) or {\"command_id\":\"<id>\"} (most recent command with that id). " +
	"Use it after a timeout, abort or disconnect instead of guessing: the server never stops a command because its caller went away, and it records the real result. " +
	"state: pending (still running; ask again shortly), applied (succeeded; result holds the recorded command result), failed (error holds the recorded error), " +
	"indeterminate (the server restarted while it ran; check the target's state before acting again). found=false means the command never reached the server: resend it with the SAME envelope. " +
	"For sessions.send_input, acknowledged tells whether the agent accepted the prompt. " +
	"Never retry a timed-out write with a NEW idempotency_key: resend the identical envelope (same idempotency_key and command_id), which replays the recorded result and waits up to 12 s for one still running, or query here. " +
	"Each status query needs its own fresh idempotency_key (a replayed query returns its first answer)."

func automationCommandPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		withPayloadSchema(executionPayloadLimit(executionReadCapability("automation.commands.get", "automation_commands", "events:read"), 4<<10),
			"{}", automationCommandGetPayload{}, `{"idempotency_key":"mylifeos:helena:ain238-doc-d1f920bb"}`, automationCommandsGetNotes),
	}
}

type automationCommandPlatformExecutor struct {
	ledger AutomationCommandLedgerPort
}

type automationCommandGetPayload struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"idempotency key of the command to look up"`
	CommandID      string `json:"command_id,omitempty" doc:"command_id of the command to look up, when the key is not known"`
}

// AutomationCommandView is what automation.commands.get returns.
type AutomationCommandView struct {
	Found          bool             `json:"found"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
	CommandID      string           `json:"command_id,omitempty"`
	Capability     string           `json:"capability,omitempty"`
	State          string           `json:"state,omitempty"`
	LedgerStatus   string           `json:"ledger_status,omitempty"`
	HTTPStatus     int              `json:"http_status,omitempty"`
	Result         json.RawMessage  `json:"result,omitempty"`
	Error          *automationError `json:"error,omitempty"`
	Acknowledged   *bool            `json:"acknowledged,omitempty"`
	CreatedAt      *time.Time       `json:"created_at,omitempty"`
	UpdatedAt      *time.Time       `json:"updated_at,omitempty"`
}

func (e *automationCommandPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	if input.Handler != "automation.commands.get" {
		return nil, platformFailure("platform_handler_unsupported", "the automation command capability handler is unsupported", false)
	}
	if _, err := decodeExecutionTarget(input.Target); err != nil {
		return nil, err
	}
	var payload automationCommandGetPayload
	if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(payload.IdempotencyKey)
	commandID := strings.TrimSpace(payload.CommandID)
	if key == "" && commandID == "" {
		return nil, missingPayloadField("idempotency_key", "the idempotency_key (or command_id) of the command to look up")
	}
	if len(key) > maxIdempotencyKeyLength || len(commandID) > maxCommandFieldLength {
		return nil, platformFailure("platform_payload_invalid", "idempotency_key and command_id must not exceed 200 bytes", false)
	}
	return &executionValidatedCommand{
		preview: executionPreview(input.Handler, map[string]any{"idempotency_key": key, "command_id": commandID}),
		execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			if e.ledger == nil {
				return nil, platformFailure("automation_ledger_unavailable", "the command ledger is unavailable", true)
			}
			command, err := e.ledger.FindAutomationCommand(ctx, authorization.Actor.ID, key, commandID)
			if errors.Is(err, sql.ErrNoRows) {
				return AutomationCommandView{Found: false, IdempotencyKey: key, CommandID: commandID}, nil
			}
			if err != nil {
				return nil, platformFailure("automation_ledger_unavailable", "the command ledger could not be read", true)
			}
			return automationCommandView(command), nil
		},
	}, nil
}

func automationCommandView(command *database.AutomationCommand) AutomationCommandView {
	created, updated := command.CreatedAt, command.UpdatedAt
	view := AutomationCommandView{
		Found: true, IdempotencyKey: command.IdempotencyKey, CommandID: command.CommandID,
		Capability: command.Capability, LedgerStatus: command.Status, HTTPStatus: command.ResponseStatus,
		CreatedAt: &created, UpdatedAt: &updated,
	}
	switch command.Status {
	case "processing":
		view.State = "pending"
		view.HTTPStatus = 0
	case "succeeded":
		view.State = "applied"
	case "failed":
		view.State = "failed"
	default:
		view.State = "indeterminate"
	}
	var recorded struct {
		CommandID  string           `json:"command_id"`
		Capability string           `json:"capability"`
		Result     json.RawMessage  `json:"result"`
		Error      *automationError `json:"error"`
	}
	if len(command.ResponseBody) > 0 && json.Unmarshal(command.ResponseBody, &recorded) == nil {
		// Rows written before the ledger recorded these fields.
		if view.CommandID == "" {
			view.CommandID = recorded.CommandID
		}
		if view.Capability == "" {
			view.Capability = recorded.Capability
		}
		if view.State == "applied" && len(recorded.Result) > 0 {
			view.Result = recorded.Result
		}
		if view.State == "failed" {
			view.Error = recorded.Error
		}
	}
	if view.State == "indeterminate" && command.ErrorCode != "" {
		view.Error = &automationError{Code: command.ErrorCode, Message: "the server restarted while the command ran; its outcome is unknown", Retryable: false}
	}
	if view.Capability == "sessions.send_input" && view.State == "applied" {
		var sent struct {
			Acknowledged *bool `json:"acknowledged"`
		}
		if json.Unmarshal(view.Result, &sent) == nil {
			view.Acknowledged = sent.Acknowledged
		}
	}
	return view
}
