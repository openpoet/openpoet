package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

const (
	maxCollaborationIDRunes      = 200
	maxCollaborationInputRunes   = 128 << 10
	maxCollaborationOutputBytes  = 64 << 10
	maxCollaborationSummaryBytes = 8 << 10
	maxCollaborationMessageBytes = 128 << 10
	maxCollaborationListItems    = 200
	maxCollaborationNotification = 8 << 10
)

var (
	collaborationNamedSecretPattern  = regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?(?:TOKEN|KEY)|REFRESH[_-]?TOKEN|AUTH[_-]?TOKEN|PRIVATE[_-]?KEY|SSH[_-]?KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?)[A-Z0-9_]*\b\s*[:=]\s*[^\s,;]+`)
	collaborationOpaqueSecretPattern = regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`)
)

type AICollaborationReadPort interface {
	ListAIConversations(context.Context) ([]database.AIConversation, error)
	GetAIConversation(context.Context, int64) (*database.AIConversation, error)
	ListAIMessages(context.Context, int64) ([]database.AIMessage, error)
	ListPendingAISuggestions(context.Context) ([]database.AISuggestion, error)
	GetAISuggestion(context.Context, int64) (*database.AISuggestion, error)
	CountUnreadProactive(context.Context) (int, error)
}

type TokenUsageCollaborationReadPort interface {
	GetTokenUsageSummary(context.Context, time.Time, *int64) (*database.TokenUsageSummary, error)
}

type CollaborationPlatformServices struct {
	Documents            *application.DocumentService
	Proposals            *application.ProposalService
	AI                   *application.AIAssistantService
	Notifications        *application.NotificationService
	NotificationDelivery *application.NotificationDeliveryService
	TokenUsage           *application.TokenUsageService
	AIQueries            AICollaborationReadPort
	TokenUsageQueries    TokenUsageCollaborationReadPort
}

func RegisterCollaborationPlatformCapabilities(registry *PlatformCapabilityRegistry, services CollaborationPlatformServices) error {
	if registry == nil {
		return errors.New("platform capability registry is required")
	}
	if services.Documents == nil || services.Proposals == nil || services.AI == nil ||
		services.Notifications == nil || services.NotificationDelivery == nil || services.TokenUsage == nil ||
		services.AIQueries == nil || services.TokenUsageQueries == nil {
		return errors.New("all collaboration platform services and read ports are required")
	}
	groups := []struct {
		definitions []PlatformCapabilityDefinition
		executor    PlatformDomainExecutor
	}{
		{documentCollaborationDefinitions(), &documentCollaborationExecutor{service: services.Documents}},
		{proposalCollaborationDefinitions(), &proposalCollaborationExecutor{service: services.Proposals}},
		{aiCollaborationDefinitions(), &aiCollaborationExecutor{service: services.AI, queries: services.AIQueries}},
		{notificationCollaborationDefinitions(), &notificationCollaborationExecutor{service: services.Notifications}},
		{notificationDeliveryCollaborationDefinitions(), &notificationDeliveryCollaborationExecutor{service: services.NotificationDelivery}},
		{tokenUsageCollaborationDefinitions(), &tokenUsageCollaborationExecutor{service: services.TokenUsage, queries: services.TokenUsageQueries}},
	}
	for _, group := range groups {
		for _, definition := range group.definitions {
			if err := registry.Register(definition, group.executor); err != nil {
				return err
			}
		}
	}
	return nil
}

func collaborationCapability(name, service string, risk application.CapabilityRisk, approval application.ApprovalPolicy, scopes ...string) PlatformCapabilityDefinition {
	required := make([]application.CapabilityScope, len(scopes))
	for i, scope := range scopes {
		required[i] = application.CapabilityScope(scope)
	}
	return PlatformCapabilityDefinition{
		Name: application.CapabilityName(name), Scopes: required, Risk: risk, Approval: approval, Mutation: risk != application.CapabilityRiskRead,
		Handler: application.CapabilityHandler(name), Service: application.CapabilityServiceName(service),
	}
}

func collaborationReadCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return collaborationCapability(name, service, application.CapabilityRiskRead, application.ApprovalNone, scopes...)
}

func collaborationWriteCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return collaborationCapability(name, service, application.CapabilityRiskWrite, application.ApprovalByPolicy, scopes...)
}

func collaborationDestructiveCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return collaborationCapability(name, service, application.CapabilityRiskDestructive, application.ApprovalExplicit, scopes...)
}

func collaborationUnsafeCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return collaborationCapability(name, service, application.CapabilityRiskUnsafe, application.ApprovalExplicit, scopes...)
}

func collaborationPayloadLimit(definition PlatformCapabilityDefinition, maximum int) PlatformCapabilityDefinition {
	definition.Limits.MaxPayloadBytes = maximum
	return definition
}

type collaborationCommandTarget struct {
	Type      string          `json:"type,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	ID        json.RawMessage `json:"id,omitempty"`
	ProjectID int64           `json:"project_id,omitempty"`
}

func decodeCollaborationTarget(raw json.RawMessage) (collaborationCommandTarget, error) {
	var target collaborationCommandTarget
	if err := decodeCollaborationJSON(raw, &target); err != nil || target.ProjectID < 0 {
		return collaborationCommandTarget{}, platformFailure("platform_target_invalid", "the collaboration target is invalid", false)
	}
	target.Type = strings.TrimSpace(target.Type)
	target.Kind = strings.TrimSpace(target.Kind)
	if utf8.RuneCountInString(target.Type) > 100 || utf8.RuneCountInString(target.Kind) > 100 {
		return collaborationCommandTarget{}, platformFailure("platform_target_invalid", "the collaboration target type is too large", false)
	}
	return target, nil
}

func collaborationIntegerID(target collaborationCommandTarget, fallback int64, label string) (int64, error) {
	if len(bytes.TrimSpace(target.ID)) == 0 {
		if fallback > 0 {
			return fallback, nil
		}
		return 0, platformFailure("platform_target_invalid", label+" is required", false)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(target.ID))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0, platformFailure("platform_target_invalid", label+" must be a positive integer", false)
	}
	var raw string
	switch typed := value.(type) {
	case json.Number:
		raw = typed.String()
	case string:
		raw = strings.TrimSpace(typed)
	default:
		return 0, platformFailure("platform_target_invalid", label+" must be a positive integer", false)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, platformFailure("platform_target_invalid", label+" must be a positive integer", false)
	}
	return id, nil
}

func collaborationStringID(target collaborationCommandTarget, fallback, label string) (string, error) {
	value := strings.TrimSpace(fallback)
	if len(bytes.TrimSpace(target.ID)) > 0 {
		if err := json.Unmarshal(target.ID, &value); err != nil {
			return "", platformFailure("platform_target_invalid", label+" must be a string", false)
		}
		value = strings.TrimSpace(value)
	}
	if value == "" || utf8.RuneCountInString(value) > maxCollaborationIDRunes || hasCollaborationControlRune(value) {
		return "", platformFailure("platform_target_invalid", label+" is invalid or too large", false)
	}
	return value, nil
}

func collaborationProjectID(target collaborationCommandTarget, fallback int64) (int64, error) {
	if target.ProjectID > 0 {
		return target.ProjectID, nil
	}
	return collaborationIntegerID(target, fallback, "project id")
}

func decodeCollaborationPayload[T any](raw json.RawMessage, output *T) error {
	if err := decodeCollaborationJSON(raw, output); err != nil {
		return platformFailure("platform_payload_invalid", "the collaboration payload is invalid", false)
	}
	return nil
}

func decodeCollaborationJSON(raw json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func requireEmptyCollaborationPayload(raw json.RawMessage) error {
	var payload map[string]json.RawMessage
	if err := decodeCollaborationPayload(raw, &payload); err != nil {
		return err
	}
	if len(payload) != 0 {
		return platformFailure("platform_payload_invalid", "this capability does not accept payload fields", false)
	}
	return nil
}

type collaborationValidatedCommand struct {
	preview any
	execute func(context.Context, application.ActionAuthorization) (any, error)
}

func (c *collaborationValidatedCommand) DryRunResult() any {
	if c == nil {
		return nil
	}
	return c.preview
}

func (c *collaborationValidatedCommand) Execute(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
	if c == nil || c.execute == nil {
		return nil, errors.New("collaboration command executor is unavailable")
	}
	return c.execute(ctx, authorization)
}

func collaborationPreview(handler application.CapabilityHandler, values map[string]any) map[string]any {
	result := map[string]any{"handler": handler, "valid": true}
	for key, value := range values {
		result[key] = value
	}
	return result
}

func boundedCollaborationInput(value, label string, maximum int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", platformFailure("platform_payload_invalid", label+" is required", false)
	}
	if utf8.RuneCountInString(value) > maximum || hasCollaborationNUL(value) {
		return "", platformFailure("platform_payload_invalid", label+" exceeds its bounded limit", false)
	}
	return value, nil
}

func boundedCollaborationOutput(value string, maximum int) (string, bool) {
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, value)
	value = collaborationNamedSecretPattern.ReplaceAllString(value, "[REDACTED]")
	value = collaborationOpaqueSecretPattern.ReplaceAllString(value, "[REDACTED]")
	value = strings.TrimSpace(value)
	if maximum <= 0 || len(value) <= maximum {
		return value, false
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value) + "\n...[TRUNCATED]", true
}

func boundedCollaborationStrings(values []string, maximumItems, maximumBytes int) ([]string, bool) {
	if maximumItems <= 0 {
		return []string{}, len(values) > 0
	}
	result := make([]string, 0, min(len(values), maximumItems))
	remaining := maximumBytes
	truncated := len(values) > maximumItems
	for _, value := range values {
		if len(result) == maximumItems || remaining <= 0 {
			truncated = true
			break
		}
		item, itemTruncated := boundedCollaborationOutput(value, min(remaining, maxCollaborationSummaryBytes))
		result = append(result, item)
		remaining -= len(item)
		truncated = truncated || itemTruncated
	}
	return result, truncated
}

func sanitizeCollaborationURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return ""
		}
		parsed.User = nil
	} else if !strings.HasPrefix(parsed.Path, "/") {
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func hasCollaborationControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func hasCollaborationNUL(value string) bool {
	return strings.IndexByte(value, 0) >= 0
}
