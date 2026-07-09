package application

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"openpoet/internal/database"
)

const (
	maxSessionSuggestionHeadBytes        = 5_000
	maxSessionSuggestionTailBytes        = 25_000
	maxSessionSuggestionTitleRunes       = 80
	maxSessionSuggestionDescriptionRunes = 4_000
	maxSessionSuggestionNameRunes        = 500
)

var (
	sessionSuggestionCursorPattern = regexp.MustCompile(`\x1b\[(\d*)C`)
	sessionSuggestionANSIPattern   = regexp.MustCompile("\\x1b\\[[?0-9;]*[a-zA-Z]|\\x1b\\][^\\x07]*(?:\\x07|\\x1b\\\\)|\\x1b[^[\\]].?")
	sessionSuggestionNamedSecret   = regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?(?:TOKEN|KEY)|REFRESH[_-]?TOKEN|AUTH[_-]?TOKEN|PRIVATE[_-]?KEY|SSH[_-]?KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?)[A-Z0-9_]*\b\s*[:=]\s*[^\s,;]+`)
	sessionSuggestionOpaqueSecret  = regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`)
)

type SessionTaskSuggestionStore interface {
	GetSession(context.Context, string) (*database.Session, error)
	GetProject(context.Context, int64) (*database.Project, error)
}

type SessionTaskSuggestionOutput interface {
	GetSessionOutput(string) ([]byte, error)
}

type SessionTaskDataSuggestionRequest struct {
	SessionID   string
	ProjectID   int64
	ProjectName string
	SessionName string
	Context     string
}

type SessionTaskDataSuggestionProviderResult struct {
	Title       string
	Description string
	Priority    string
	Model       string
}

type SessionTaskDataSuggestionProvider interface {
	SuggestTaskData(context.Context, SessionTaskDataSuggestionRequest) (SessionTaskDataSuggestionProviderResult, error)
}

type SessionTaskSuggestionAuditEntry struct {
	Event        string
	SessionID    string
	ProjectID    int64
	Actor        Actor
	ContextBytes int
	Model        string
	WasTruncated bool
	FailureCode  string
}

// SessionTaskSuggestionAuditor receives metadata only. Raw terminal context,
// prompts, provider output, credentials and backend error strings never cross
// this audit boundary.
type SessionTaskSuggestionAuditor interface {
	RecordSessionTaskSuggestion(context.Context, SessionTaskSuggestionAuditEntry)
}

type SessionTaskDataSuggestion struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	Priority     string `json:"priority"`
	WasTruncated bool   `json:"was_truncated,omitempty"`
}

type SuggestSessionTaskDataCommand struct {
	SessionID     string
	Authorization ActionAuthorization
}

type SessionTaskSuggestionService struct {
	store    SessionTaskSuggestionStore
	output   SessionTaskSuggestionOutput
	provider SessionTaskDataSuggestionProvider
	auditor  SessionTaskSuggestionAuditor
}

func NewSessionTaskSuggestionService(
	store SessionTaskSuggestionStore,
	output SessionTaskSuggestionOutput,
	provider SessionTaskDataSuggestionProvider,
	auditor SessionTaskSuggestionAuditor,
) *SessionTaskSuggestionService {
	return &SessionTaskSuggestionService{store: store, output: output, provider: provider, auditor: auditor}
}

func (s *SessionTaskSuggestionService) SuggestTaskData(ctx context.Context, command SuggestSessionTaskDataCommand) (SessionTaskDataSuggestion, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return SessionTaskDataSuggestion{}, err
	}
	if utf8.RuneCountInString(strings.TrimSpace(command.Authorization.Actor.Type)) > 200 ||
		utf8.RuneCountInString(strings.TrimSpace(command.Authorization.Actor.ID)) > 200 {
		return SessionTaskDataSuggestion{}, validationError("action_actor_invalid", "Actor type and ID must not exceed 200 characters")
	}
	if s == nil || s.store == nil || s.output == nil || s.provider == nil {
		return SessionTaskDataSuggestion{}, validationError("session_task_suggestion_unavailable", "Session task suggestion service is unavailable")
	}
	sessionID := strings.TrimSpace(command.SessionID)
	if sessionID == "" || utf8.RuneCountInString(sessionID) > 200 {
		return SessionTaskDataSuggestion{}, validationError("invalid_session_id", "Session ID is required and must not exceed 200 characters")
	}
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return SessionTaskDataSuggestion{}, notFoundError("session_not_found", "Session not found", err)
	}
	project, err := s.store.GetProject(ctx, session.ProjectID)
	if err != nil || project == nil {
		return SessionTaskDataSuggestion{}, notFoundError("project_not_found", "Project not found", err)
	}

	rawOutput, err := s.output.GetSessionOutput(session.ID)
	if err != nil || len(rawOutput) == 0 {
		s.audit(ctx, SessionTaskSuggestionAuditEntry{
			Event: "failed", SessionID: session.ID, ProjectID: project.ID,
			Actor: command.Authorization.Actor, FailureCode: "session_output_unavailable",
		})
		return SessionTaskDataSuggestion{}, notFoundError("session_output_unavailable", "Session is not running or has no output", nil)
	}
	contextText, contextTruncated := sanitizeSessionSuggestionContext(rawOutput)
	if contextText == "" {
		s.audit(ctx, SessionTaskSuggestionAuditEntry{
			Event: "failed", SessionID: session.ID, ProjectID: project.ID,
			Actor: command.Authorization.Actor, FailureCode: "session_output_unreadable",
		})
		return SessionTaskDataSuggestion{}, notFoundError("session_output_unreadable", "Session has no readable output", nil)
	}
	request := SessionTaskDataSuggestionRequest{
		SessionID: session.ID, ProjectID: project.ID,
		ProjectName: sanitizeSessionSuggestionText(project.Name, maxSessionSuggestionNameRunes),
		SessionName: sanitizeSessionSuggestionText(session.Name, maxSessionSuggestionNameRunes),
		Context:     contextText,
	}
	auditBase := SessionTaskSuggestionAuditEntry{
		SessionID: session.ID, ProjectID: project.ID, Actor: command.Authorization.Actor,
		ContextBytes: len([]byte(contextText)), WasTruncated: contextTruncated,
	}
	started := auditBase
	started.Event = "started"
	s.audit(ctx, started)

	providerResult, err := s.provider.SuggestTaskData(ctx, request)
	if err != nil {
		failed := auditBase
		failed.Event = "failed"
		failed.FailureCode = "provider_failed"
		s.audit(ctx, failed)
		return SessionTaskDataSuggestion{}, errors.New("session task suggestion provider failed")
	}
	result, outputTruncated, err := normalizeSessionTaskSuggestion(providerResult)
	if err != nil {
		failed := auditBase
		failed.Event = "failed"
		failed.FailureCode = "provider_response_invalid"
		s.audit(ctx, failed)
		return SessionTaskDataSuggestion{}, err
	}
	result.WasTruncated = contextTruncated || outputTruncated
	completed := auditBase
	completed.Event = "succeeded"
	completed.Model = sanitizeSessionSuggestionText(providerResult.Model, 200)
	completed.WasTruncated = result.WasTruncated
	s.audit(ctx, completed)
	return result, nil
}

func (s *SessionTaskSuggestionService) audit(ctx context.Context, entry SessionTaskSuggestionAuditEntry) {
	if s.auditor != nil {
		s.auditor.RecordSessionTaskSuggestion(ctx, entry)
	}
}

func sanitizeSessionSuggestionContext(output []byte) (string, bool) {
	// Redact before sampling. This prevents a head/tail boundary from splitting a
	// private-key or token pattern and exposing only the unmatched secret half.
	output = []byte(redactSessionSuggestionValue(string(output)))
	truncated := false
	if len(output) > maxSessionSuggestionHeadBytes+maxSessionSuggestionTailBytes {
		head := append([]byte(nil), output[:maxSessionSuggestionHeadBytes]...)
		tail := append([]byte(nil), output[len(output)-maxSessionSuggestionTailBytes:]...)
		output = append(head, append([]byte("\n...[TRUNCATED]...\n"), tail...)...)
		truncated = true
	} else {
		output = append([]byte(nil), output...)
	}
	text := string(output)
	text = sessionSuggestionCursorPattern.ReplaceAllString(text, " ")
	text = sessionSuggestionANSIPattern.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, text)
	text = sanitizeSessionSuggestionText(text, maxSessionSuggestionHeadBytes+maxSessionSuggestionTailBytes)
	return text, truncated
}

func sanitizeSessionSuggestionText(value string, limit int) string {
	value = redactSessionSuggestionValue(value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "\n...[TRUNCATED]"
	}
	return value
}

func redactSessionSuggestionValue(value string) string {
	value = redactReportSecrets(value)
	value = sessionSuggestionNamedSecret.ReplaceAllString(value, "[REDACTED]")
	value = sessionSuggestionOpaqueSecret.ReplaceAllString(value, "[REDACTED]")
	return strings.TrimSpace(value)
}

func normalizeSessionTaskSuggestion(providerResult SessionTaskDataSuggestionProviderResult) (SessionTaskDataSuggestion, bool, error) {
	title, titleTruncated := truncateSessionSuggestionText(providerResult.Title, maxSessionSuggestionTitleRunes)
	if title == "" {
		return SessionTaskDataSuggestion{}, false, validationError("session_task_suggestion_invalid", "Task suggestion provider returned an empty title")
	}
	description, descriptionTruncated := truncateSessionSuggestionText(providerResult.Description, maxSessionSuggestionDescriptionRunes)
	priority := strings.ToLower(strings.TrimSpace(providerResult.Priority))
	switch priority {
	case "low", "medium", "high", "urgent":
	default:
		priority = "medium"
	}
	return SessionTaskDataSuggestion{
		Title: title, Description: description, Priority: priority,
	}, titleTruncated || descriptionTruncated, nil
}

func truncateSessionSuggestionText(value string, limit int) (string, bool) {
	value = redactSessionSuggestionValue(value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]), true
	}
	return value, false
}

var _ SessionTaskSuggestionStore = (*database.DB)(nil)
