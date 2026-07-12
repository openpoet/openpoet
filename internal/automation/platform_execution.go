package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/application"
)

const (
	maxExecutionIDRunes       = 200
	maxExecutionPathRunes     = 4096
	maxExecutionHistoryBytes  = 64 << 10
	maxExecutionDiffBytes     = 256 << 10
	maxExecutionTranscript    = 128 << 10
	maxExecutionResultEntries = 1000
)

var (
	executionNamedSecretPattern  = regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?(?:TOKEN|KEY)|REFRESH[_-]?TOKEN|AUTH[_-]?TOKEN|PRIVATE[_-]?KEY|SSH[_-]?KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?)[A-Z0-9_]*\b\s*[:=]\s*[^\s,;]+`)
	executionOpaqueSecretPattern = regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`)
	executionANSIPattern         = regexp.MustCompile("\\x1b\\[[?0-9;]*[a-zA-Z]|\\x1b\\][^\\x07]*(?:\\x07|\\x1b\\\\)|\\x1b[^[\\]].?")
)

type ExecutionPlatformServices struct {
	Sessions           *application.SessionService
	SessionWatchers    *application.SessionEventWatcherService
	SessionSuggestions *application.SessionTaskSuggestionService
	FileMutations      *application.FileMutationService
	GitMutations       *application.GitMutationService
	HookResponses      *application.HookResponseService
	Voice              *application.VoiceTranscriptionService
	TunnelMutations    *application.TunnelMutationService
	UpdateMutations    *application.UpdateMutationService

	SessionQueries SessionOperationalReadPort
	SessionRuntime SessionRuntimeReadPort
	SessionEvents  SessionEventStatusReadPort
	Files          FileOperationalReadPort
	Git            GitOperationalReadPort
	Tunnel         TunnelOperationalReadPort
	Updates        UpdateOperationalReadPort
}

func RegisterExecutionPlatformCapabilities(registry *PlatformCapabilityRegistry, services ExecutionPlatformServices) error {
	if registry == nil {
		return errors.New("platform capability registry is required")
	}
	if services.Sessions == nil || services.SessionWatchers == nil || services.SessionSuggestions == nil ||
		services.FileMutations == nil || services.GitMutations == nil || services.HookResponses == nil ||
		services.Voice == nil || services.TunnelMutations == nil || services.UpdateMutations == nil ||
		services.SessionQueries == nil || services.SessionRuntime == nil || services.SessionEvents == nil ||
		services.Files == nil || services.Git == nil || services.Tunnel == nil || services.Updates == nil {
		return errors.New("all execution platform services and read ports are required")
	}
	groups := []struct {
		definitions []PlatformCapabilityDefinition
		executor    PlatformDomainExecutor
	}{
		{sessionPlatformDefinitions(), &sessionPlatformExecutor{service: services.Sessions, queries: services.SessionQueries, runtime: services.SessionRuntime}},
		{sessionWatcherPlatformDefinitions(), &sessionWatcherPlatformExecutor{service: services.SessionWatchers, statuses: services.SessionEvents}},
		{sessionSuggestionPlatformDefinitions(), &sessionSuggestionPlatformExecutor{service: services.SessionSuggestions}},
		{fileExecutionPlatformDefinitions(), &fileExecutionPlatformExecutor{service: services.FileMutations, reader: services.Files}},
		{gitExecutionPlatformDefinitions(), &gitExecutionPlatformExecutor{service: services.GitMutations, reader: services.Git}},
		{hookExecutionPlatformDefinitions(), &hookExecutionPlatformExecutor{service: services.HookResponses}},
		{voiceExecutionPlatformDefinitions(), &voiceExecutionPlatformExecutor{service: services.Voice}},
		{tunnelExecutionPlatformDefinitions(), &tunnelExecutionPlatformExecutor{service: services.TunnelMutations, reader: services.Tunnel}},
		{updateExecutionPlatformDefinitions(), &updateExecutionPlatformExecutor{service: services.UpdateMutations, reader: services.Updates}},
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

func executionCapability(name, service string, risk application.CapabilityRisk, approval application.ApprovalPolicy, scopes ...string) PlatformCapabilityDefinition {
	required := make([]application.CapabilityScope, len(scopes))
	for i, scope := range scopes {
		required[i] = application.CapabilityScope(scope)
	}
	return PlatformCapabilityDefinition{
		Name: application.CapabilityName(name), Scopes: required, Risk: risk, Approval: approval, Mutation: risk != application.CapabilityRiskRead,
		Handler: application.CapabilityHandler(name), Service: application.CapabilityServiceName(service),
	}
}

func executionReadCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return executionCapability(name, service, application.CapabilityRiskRead, application.ApprovalNone, scopes...)
}

func executionWriteCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return executionCapability(name, service, application.CapabilityRiskWrite, application.ApprovalByPolicy, scopes...)
}

func executionDestructiveCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return executionCapability(name, service, application.CapabilityRiskDestructive, application.ApprovalExplicit, scopes...)
}

func executionUnsafeCapability(name, service string, scopes ...string) PlatformCapabilityDefinition {
	return executionCapability(name, service, application.CapabilityRiskUnsafe, application.ApprovalExplicit, scopes...)
}

func executionPayloadLimit(definition PlatformCapabilityDefinition, maximum int) PlatformCapabilityDefinition {
	definition.Limits.MaxPayloadBytes = maximum
	return definition
}

type executionCommandTarget struct {
	Type      string          `json:"type,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	ID        json.RawMessage `json:"id,omitempty"`
	ProjectID int64           `json:"project_id,omitempty"`
}

func decodeExecutionTarget(raw json.RawMessage) (executionCommandTarget, error) {
	var target executionCommandTarget
	if err := decodeExecutionJSON(raw, &target); err != nil || target.ProjectID < 0 {
		return executionCommandTarget{}, platformFailure("platform_target_invalid", "the execution target is invalid", false)
	}
	target.Type = strings.TrimSpace(target.Type)
	target.Kind = strings.TrimSpace(target.Kind)
	if utf8.RuneCountInString(target.Type) > 100 || utf8.RuneCountInString(target.Kind) > 100 {
		return executionCommandTarget{}, platformFailure("platform_target_invalid", "the execution target type is too large", false)
	}
	return target, nil
}

func executionStringID(target executionCommandTarget, label string) (string, error) {
	if len(bytes.TrimSpace(target.ID)) == 0 {
		return "", platformFailure("platform_target_invalid", label+" is required", false)
	}
	var value string
	if err := json.Unmarshal(target.ID, &value); err != nil {
		return "", platformFailure("platform_target_invalid", label+" must be a string", false)
	}
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > maxExecutionIDRunes || strings.IndexByte(value, 0) >= 0 {
		return "", platformFailure("platform_target_invalid", label+" is invalid or too large", false)
	}
	return value, nil
}

func executionProjectID(target executionCommandTarget, fallback int64) (int64, error) {
	if target.ProjectID > 0 {
		return target.ProjectID, nil
	}
	if fallback > 0 {
		return fallback, nil
	}
	if len(bytes.TrimSpace(target.ID)) > 0 && (target.Type == "project" || target.Kind == "project") {
		var id int64
		if err := json.Unmarshal(target.ID, &id); err == nil && id > 0 {
			return id, nil
		}
	}
	return 0, platformFailure("platform_target_invalid", "project_id is required", false)
}

func decodeExecutionPayload[T any](raw json.RawMessage, output *T) error {
	if err := decodeExecutionJSON(raw, output); err != nil {
		return platformFailure("platform_payload_invalid", "the execution payload is invalid", false)
	}
	return nil
}

func decodeExecutionJSON(raw json.RawMessage, output any) error {
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

func requireEmptyExecutionPayload(raw json.RawMessage) error {
	var payload map[string]json.RawMessage
	if err := decodeExecutionPayload(raw, &payload); err != nil {
		return err
	}
	if len(payload) != 0 {
		return platformFailure("platform_payload_invalid", "this capability does not accept payload fields", false)
	}
	return nil
}

type executionValidatedCommand struct {
	preview any
	execute func(context.Context, application.ActionAuthorization) (any, error)
}

func (c *executionValidatedCommand) DryRunResult() any {
	if c == nil {
		return nil
	}
	return c.preview
}

func (c *executionValidatedCommand) Execute(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
	if c == nil || c.execute == nil {
		return nil, errors.New("execution command executor is unavailable")
	}
	return c.execute(ctx, authorization)
}

func executionPreview(handler application.CapabilityHandler, values map[string]any) map[string]any {
	result := map[string]any{"handler": handler, "valid": true}
	for key, value := range values {
		result[key] = value
	}
	return result
}

func normalizeExecutionRelativePath(value string, optional bool) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if optional && value == "" {
		return "", nil
	}
	if value == "" || utf8.RuneCountInString(value) > maxExecutionPathRunes || strings.IndexByte(value, 0) >= 0 || strings.HasPrefix(value, "/") {
		return "", platformFailure("platform_path_invalid", "path must be a bounded relative path", false)
	}
	if len(value) >= 2 && value[1] == ':' && unicode.IsLetter(rune(value[0])) {
		return "", platformFailure("platform_path_invalid", "path must not be drive-qualified", false)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", platformFailure("platform_path_invalid", "path must remain inside its project", false)
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", platformFailure("platform_path_invalid", "path must remain inside its project", false)
	}
	return cleaned, nil
}

func boundedExecutionText(value string, maximum int) (string, bool) {
	value = executionANSIPattern.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, value)
	value = executionNamedSecretPattern.ReplaceAllString(value, "[REDACTED]")
	value = executionOpaqueSecretPattern.ReplaceAllString(value, "[REDACTED]")
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

func sanitizeExecutionPublicURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
