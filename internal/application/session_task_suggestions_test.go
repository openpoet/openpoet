package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"openpoet/internal/database"
)

type fakeSessionTaskSuggestionProvider struct {
	request SessionTaskDataSuggestionRequest
	result  SessionTaskDataSuggestionProviderResult
	err     error
	calls   int
}

func (p *fakeSessionTaskSuggestionProvider) SuggestTaskData(_ context.Context, request SessionTaskDataSuggestionRequest) (SessionTaskDataSuggestionProviderResult, error) {
	p.calls++
	p.request = request
	return p.result, p.err
}

type recordingSessionTaskSuggestionAuditor struct {
	entries []SessionTaskSuggestionAuditEntry
}

func (a *recordingSessionTaskSuggestionAuditor) RecordSessionTaskSuggestion(_ context.Context, entry SessionTaskSuggestionAuditEntry) {
	a.entries = append(a.entries, entry)
}

func TestSessionTaskSuggestionServiceAuthorizesRedactsBoundsAndAudits(t *testing.T) {
	raw := []byte("\x1b[31m❯ implement integration\x1b[0m\nAPI_KEY=head-secret\n" +
		strings.Repeat("session activity\n", 3_000) +
		"Authorization: Bearer bearer-secret\nGITHUB_TOKEN=tail-secret\n")
	original := append([]byte(nil), raw...)
	store := &phase3Store{
		project: &database.Project{ID: 7, Name: "OpenPoet api_key=project-secret"},
		session: &database.Session{ID: "s1", ProjectID: 7, Name: "Session password=session-secret", Status: "running"},
	}
	manager := &phase3SessionManager{output: raw, running: true}
	provider := &fakeSessionTaskSuggestionProvider{result: SessionTaskDataSuggestionProviderResult{
		Title:       strings.Repeat("T", maxSessionSuggestionTitleRunes+20) + " api_key=title-secret",
		Description: "Deliver the integration. password=description-secret",
		Priority:    "unexpected",
		Model:       "model access_token=model-secret",
	}}
	auditor := &recordingSessionTaskSuggestionAuditor{}
	service := NewSessionTaskSuggestionService(store, manager, provider, auditor)

	if _, err := service.SuggestTaskData(context.Background(), SuggestSessionTaskDataCommand{SessionID: "s1"}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("unauthenticated suggestion must fail: %v", err)
	}
	if provider.calls != 0 || len(auditor.entries) != 0 {
		t.Fatalf("unauthorized call crossed provider/audit boundary: calls=%d audit=%+v", provider.calls, auditor.entries)
	}

	result, err := service.SuggestTaskData(context.Background(), SuggestSessionTaskDataCommand{
		SessionID: "s1", Authorization: phase3Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("service mutated the runtime output buffer")
	}
	if provider.calls != 1 || provider.request.SessionID != "s1" || provider.request.ProjectID != 7 {
		t.Fatalf("provider request=%+v calls=%d", provider.request, provider.calls)
	}
	providerJSON, _ := json.Marshal(provider.request)
	for _, secret := range []string{"head-secret", "bearer-secret", "tail-secret", "project-secret", "session-secret"} {
		if strings.Contains(string(providerJSON), secret) {
			t.Fatalf("provider request leaked %q: %s", secret, providerJSON)
		}
	}
	if !strings.Contains(provider.request.Context, "[REDACTED]") || strings.Contains(provider.request.Context, "\x1b") {
		t.Fatalf("provider context was not cleaned/redacted: %q", provider.request.Context)
	}
	if utf8.RuneCountInString(result.Title) != maxSessionSuggestionTitleRunes || !result.WasTruncated || result.Priority != "medium" {
		t.Fatalf("bounded result=%+v", result)
	}
	if strings.Contains(result.Description, "description-secret") || !strings.Contains(result.Description, "[REDACTED]") {
		t.Fatalf("result description leaked provider output: %q", result.Description)
	}
	if len(auditor.entries) != 2 || auditor.entries[0].Event != "started" || auditor.entries[1].Event != "succeeded" {
		t.Fatalf("audit=%+v", auditor.entries)
	}
	auditJSON, _ := json.Marshal(auditor.entries)
	for _, secret := range []string{"head-secret", "tail-secret", "model-secret", "description-secret", "title-secret"} {
		if strings.Contains(string(auditJSON), secret) {
			t.Fatalf("audit leaked %q: %s", secret, auditJSON)
		}
	}
	if auditor.entries[1].ContextBytes <= 0 || auditor.entries[1].FailureCode != "" {
		t.Fatalf("audit metadata=%+v", auditor.entries[1])
	}
}

func TestSessionTaskSuggestionServiceRedactsProviderFailuresAndAuditsOutcome(t *testing.T) {
	store := &phase3Store{
		project: &database.Project{ID: 7, Name: "OpenPoet"},
		session: &database.Session{ID: "s1", ProjectID: 7, Name: "Session", Status: "running"},
	}
	manager := &phase3SessionManager{output: []byte("❯ fix the task"), running: true}
	provider := &fakeSessionTaskSuggestionProvider{err: errors.New("upstream failed api_key=backend-secret")}
	auditor := &recordingSessionTaskSuggestionAuditor{}
	service := NewSessionTaskSuggestionService(store, manager, provider, auditor)

	_, err := service.SuggestTaskData(context.Background(), SuggestSessionTaskDataCommand{
		SessionID: "s1", Authorization: phase3Actor,
	})
	if err == nil || strings.Contains(err.Error(), "backend-secret") {
		t.Fatalf("provider error was not safely projected: %v", err)
	}
	if len(auditor.entries) != 2 || auditor.entries[1].Event != "failed" || auditor.entries[1].FailureCode != "provider_failed" {
		t.Fatalf("failure audit=%+v", auditor.entries)
	}
	encoded, _ := json.Marshal(auditor.entries)
	if strings.Contains(string(encoded), "backend-secret") {
		t.Fatalf("failure audit leaked backend error: %s", encoded)
	}
}

func TestSessionTaskSuggestionServiceRejectsMissingOutputAndInvalidProviderResult(t *testing.T) {
	store := &phase3Store{
		project: &database.Project{ID: 7, Name: "OpenPoet"},
		session: &database.Session{ID: "s1", ProjectID: 7, Name: "Session", Status: "running"},
	}
	auditor := &recordingSessionTaskSuggestionAuditor{}
	provider := &fakeSessionTaskSuggestionProvider{}
	service := NewSessionTaskSuggestionService(store, &phase3SessionManager{}, provider, auditor)
	_, err := service.SuggestTaskData(context.Background(), SuggestSessionTaskDataCommand{SessionID: "s1", Authorization: phase3Actor})
	if !ErrorIsKind(err, ErrorNotFound) || len(auditor.entries) != 1 || auditor.entries[0].FailureCode != "session_output_unavailable" {
		t.Fatalf("missing output err=%v audit=%+v", err, auditor.entries)
	}

	auditor.entries = nil
	service = NewSessionTaskSuggestionService(store, &phase3SessionManager{output: []byte("readable")}, provider, auditor)
	_, err = service.SuggestTaskData(context.Background(), SuggestSessionTaskDataCommand{SessionID: "s1", Authorization: phase3Actor})
	if !ErrorIsKind(err, ErrorValidation) || len(auditor.entries) != 2 || auditor.entries[1].FailureCode != "provider_response_invalid" {
		t.Fatalf("invalid provider result err=%v audit=%+v", err, auditor.entries)
	}
}
