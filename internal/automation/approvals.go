package automation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

const (
	approvalTokenScheme       = "opagv1"
	defaultApprovalGrantTTL   = 2 * time.Minute
	maximumApprovalGrantTTL   = 5 * time.Minute
	maxAuthorizationRefLength = maxCommandFieldLength
)

type ApprovalGrantStore interface {
	CreateAutomationApprovalGrant(context.Context, *database.AutomationApprovalGrant) error
	ConsumeAutomationApprovalGrant(context.Context, []byte, string, string, string, string, time.Time) (*database.AutomationApprovalGrant, error)
}

type approvalGrantRequest struct {
	TargetClientID   string                     `json:"target_client_id"`
	Capability       application.CapabilityName `json:"capability"`
	CommandID        string                     `json:"command_id"`
	AuthorizationRef string                     `json:"authorization_ref"`
	ExpiresInSeconds int                        `json:"expires_in_seconds,omitempty"`
}

type approvalGrantResponse struct {
	APIVersion       string                     `json:"api_version"`
	GrantID          string                     `json:"grant_id"`
	TargetClientID   string                     `json:"target_client_id"`
	Capability       application.CapabilityName `json:"capability"`
	CommandID        string                     `json:"command_id"`
	AuthorizationRef string                     `json:"authorization_ref"`
	ExpiresAt        time.Time                  `json:"expires_at"`
	ApprovalToken    string                     `json:"approval_token"`
}

func (a *commandAPI) issueApprovalGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
		return
	}
	if a == nil || a.approvals == nil || a.random == nil {
		writeError(w, http.StatusServiceUnavailable, "approval_broker_unavailable", "approval grant broker is unavailable", true)
		return
	}
	if a.capabilities == nil && a.platform == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_registry_unavailable", "the capability registry is unavailable", true)
		return
	}

	var request approvalGrantRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "approval_request_invalid", "approval grant request is invalid", false)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "approval_request_invalid", "approval grant request must contain one JSON value", false)
		return
	}
	request.TargetClientID = strings.TrimSpace(request.TargetClientID)
	request.CommandID = strings.TrimSpace(request.CommandID)
	if request.TargetClientID == "" || len(request.TargetClientID) > maxCommandFieldLength {
		writeError(w, http.StatusBadRequest, "approval_target_invalid", "target_client_id is required and bounded", false)
		return
	}
	if request.CommandID == "" || len(request.CommandID) > maxCommandFieldLength {
		writeError(w, http.StatusBadRequest, "command_id_invalid", "command_id is required and bounded", false)
		return
	}
	if !validAuthorizationRef(request.AuthorizationRef) {
		writeError(w, http.StatusBadRequest, "authorization_ref_invalid", "authorization_ref must use an approved audit prefix and contain no whitespace or control characters", false)
		return
	}
	capability, _, _, exists := a.lookupCapability(request.Capability)
	if !exists {
		writeError(w, http.StatusNotFound, "capability_not_found", "the requested capability is not registered", false)
		return
	}
	if capability.Approval != application.ApprovalExplicit {
		writeError(w, http.StatusUnprocessableEntity, "approval_not_required", "the requested capability does not accept explicit approval grants", false)
		return
	}
	ttl := defaultApprovalGrantTTL
	if request.ExpiresInSeconds != 0 {
		ttl = time.Duration(request.ExpiresInSeconds) * time.Second
	}
	if ttl <= 0 || ttl > maximumApprovalGrantTTL {
		writeError(w, http.StatusBadRequest, "approval_expiry_invalid", "expires_in_seconds must be between 1 and 300", false)
		return
	}

	idBytes, err := randomBytes(a.random, 16)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "approval_broker_unavailable", "approval grant ID could not be generated", true)
		return
	}
	secretBytes, err := randomBytes(a.random, 32)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "approval_broker_unavailable", "approval grant token could not be generated", true)
		return
	}
	approvalToken := approvalTokenScheme + "_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	tokenHash := sha256.Sum256([]byte(approvalToken))
	now := a.now().UTC()
	grant := &database.AutomationApprovalGrant{
		ID: hex.EncodeToString(idBytes), TokenHash: tokenHash[:], IssuedByClientID: actor.ClientID,
		TargetClientID: request.TargetClientID, Capability: string(request.Capability),
		CommandID: request.CommandID, AuthorizationRef: request.AuthorizationRef,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := a.approvals.CreateAutomationApprovalGrant(r.Context(), grant); err != nil {
		if errors.Is(err, database.ErrAutomationApprovalTargetInvalid) {
			writeError(w, http.StatusUnprocessableEntity, "approval_target_invalid", "target automation client is unavailable", false)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "approval_grant_failed", "approval grant could not be persisted", true)
		return
	}
	writeJSON(w, http.StatusCreated, approvalGrantResponse{
		APIVersion: APIVersion, GrantID: grant.ID, TargetClientID: grant.TargetClientID,
		Capability: request.Capability, CommandID: grant.CommandID,
		AuthorizationRef: grant.AuthorizationRef, ExpiresAt: grant.ExpiresAt,
		ApprovalToken: approvalToken,
	})
}

func (a *commandAPI) consumeExplicitApproval(ctx context.Context, actor Actor, capability application.Capability, command *commandEnvelope) (*database.AutomationApprovalGrant, error) {
	tokenHash, ok := parseApprovalToken(command.ApprovalToken)
	if !ok {
		return nil, &commandFailure{status: http.StatusConflict, code: "approval_invalid", message: "approval_token is invalid"}
	}
	grant, err := a.approvals.ConsumeAutomationApprovalGrant(
		ctx, tokenHash, actor.ClientID, string(capability.Name), command.CommandID, command.CorrelationID, a.now().UTC(),
	)
	switch {
	case err == nil:
		return grant, nil
	case errors.Is(err, database.ErrAutomationApprovalExpired):
		return nil, &commandFailure{status: http.StatusConflict, code: "approval_expired", message: "approval grant has expired"}
	case errors.Is(err, database.ErrAutomationApprovalUsed):
		return nil, &commandFailure{status: http.StatusConflict, code: "approval_used", message: "approval grant was already used"}
	case errors.Is(err, database.ErrAutomationApprovalBindingMismatch):
		return nil, &commandFailure{status: http.StatusConflict, code: "approval_mismatch", message: "approval grant does not match this command"}
	case errors.Is(err, database.ErrAutomationApprovalInvalid):
		return nil, &commandFailure{status: http.StatusConflict, code: "approval_invalid", message: "approval_token is invalid"}
	default:
		return nil, &commandFailure{status: http.StatusServiceUnavailable, code: "approval_unavailable", message: "approval grant could not be validated", retryable: true}
	}
}

func validAuthorizationRef(value string) bool {
	if value == "" || len(value) > maxAuthorizationRefLength || strings.TrimSpace(value) != value {
		return false
	}
	prefix, suffix, ok := strings.Cut(value, ":")
	if !ok || suffix == "" {
		return false
	}
	switch prefix {
	case "ain", "inbound", "signal", "task", "policy":
	default:
		return false
	}
	for _, character := range suffix {
		if !unicode.IsGraphic(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func parseApprovalToken(token string) ([]byte, bool) {
	if strings.TrimSpace(token) != token {
		return nil, false
	}
	prefix := approvalTokenScheme + "_"
	if !strings.HasPrefix(token, prefix) || strings.Contains(token, " ") {
		return nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, prefix))
	if err != nil || len(secret) != 32 {
		return nil, false
	}
	hash := sha256.Sum256([]byte(token))
	return hash[:], true
}
