package automation

import (
	"context"
	"errors"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
)

type fakeTranscripts struct{ events []*jsonlview.SessionEvent }

func (f fakeTranscripts) SessionTranscript(context.Context, string) ([]*jsonlview.SessionEvent, string, error) {
	return f.events, "", nil
}

func TestSessionsMessagesCapabilityUsesTheSharedService(t *testing.T) {
	session := &database.Session{ID: "s1", ProjectID: 12}
	queries := questionSessionQueries{session: session}
	executor := &sessionPlatformExecutor{queries: queries, messages: application.NewSessionMessageService(queries, fakeTranscripts{events: []*jsonlview.SessionEvent{{
		Type: "user", UUID: "0000000a-0000", Timestamp: time.Now(),
		Message: &jsonlview.EventMessage{Role: "user", ContentBlocks: []jsonlview.ContentBlock{{Type: "text", Text: "oi"}}},
	}}})}

	result, err := runSessionCommand(t, executor, "sessions.messages", `{"id":"s1"}`, `{"last_n":5}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	view := result.(*application.SessionMessagesResult)
	if view.Mode != "list" || len(view.Messages) != 1 || view.Messages[0].Text != "oi" || view.Messages[0].ID != "0000000a" {
		t.Fatalf("result = %+v", view)
	}

	// Payload errors are rejected before execution, as platform_payload_invalid.
	var failure *PlatformDispatchError
	if _, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: "sessions.messages", Target: []byte(`{"id":"s1"}`), Payload: []byte(`{"last_n":21}`),
	}); !errors.As(err, &failure) || failure.Code != "platform_payload_invalid" {
		t.Fatalf("invalid payload err = %v", err)
	}
	// The client's project filter applies.
	if _, err := runSessionCommand(t, executor, "sessions.messages", `{"id":"s1"}`, `{}`, &ProjectScopeSet{Allowed: map[int64]bool{47: true}}); application.ErrorCode(err) != "session_not_found" {
		t.Fatalf("out-of-scope err = %v", err)
	}
	executor.messages = nil
	if _, err := runSessionCommand(t, executor, "sessions.messages", `{"id":"s1"}`, `{}`, nil); !errors.As(err, &failure) || failure.Code != "platform_service_unavailable" {
		t.Fatalf("missing service err = %v", err)
	}
}
