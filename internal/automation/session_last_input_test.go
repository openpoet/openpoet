package automation

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessioninput"
)

// attributingSignals is the hook handler's view of a session: it records the
// inputs senders announce and attributes the prompts the agent accepts.
type attributingSignals struct {
	ackingSignals
	inputs *sessioninput.Tracker
}

func (s *attributingSignals) ExpectSessionInput(id string, in sessioninput.Expected) {
	s.inputs.Expect(id, in)
}

func (s *attributingSignals) LastSessionInput(id string) (sessioninput.LastInput, bool) {
	return s.inputs.Last(id)
}

// promptingManager is a session whose agent turns each typed line into a
// prompt (UserPromptSubmit), unless swallow is set: then the line never
// becomes a message (a slash command, a dialog that ate it).
type promptingManager struct {
	closeCompletedManager
	signals *attributingSignals
	mu      sync.Mutex
	typed   string
	swallow bool
}

func (m *promptingManager) WriteToSession(id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if string(data) != "\n" {
		m.typed = string(data)
		return nil
	}
	if !m.swallow {
		m.signals.inputs.Observe(id, m.typed, true)
		m.signals.fire()
	}
	return nil
}

func lastInputHandler(t *testing.T, db *database.DB) (http.Handler, *promptingManager) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	platform, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	store := &closeCompletedStore{session: &database.Session{ID: "s1", ProjectID: 20, Status: "running"}}
	signals := &attributingSignals{inputs: sessioninput.NewTracker()}
	manager := &promptingManager{signals: signals}
	service := application.NewSessionService(store, manager, nil, nil, nil, nil, nil, nil,
		application.SessionCreationCollaborators{Signals: signals})
	sessions := &sessionPlatformExecutor{service: service, runtime: manager, queries: questionSessionQueries{session: store.session}}
	for _, definition := range sessionPlatformDefinitions() {
		if definition.Name == "sessions.send_input" || definition.Name == "sessions.get" {
			if err := platform.Register(definition, sessions); err != nil {
				t.Fatal(err)
			}
		}
	}
	return NewHandler(db, Dependencies{Capabilities: capabilities, PlatformCapabilities: platform}), manager
}

func sendInputAs(t *testing.T, handler http.Handler, token, commandID, text string) {
	t.Helper()
	response := automationRequest(t, handler, token, http.MethodPost, "/commands", commandID, map[string]any{
		"command_id": commandID, "capability": "sessions.send_input", "correlation_id": "task:500",
		"target":  map[string]any{"type": "session", "id": "s1"},
		"payload": map[string]any{"text": text, "await_ack": false},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("send_input status=%d body=%s", response.Code, response.Body.String())
	}
}

func getLastInput(t *testing.T, handler http.Handler, token, key string) *sessioninput.LastInput {
	t.Helper()
	response := automationRequest(t, handler, token, http.MethodPost, "/commands", key, map[string]any{
		"command_id": key, "capability": "sessions.get", "target": map[string]any{"type": "session", "id": "s1"}, "payload": map[string]any{},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("sessions.get status=%d body=%s", response.Code, response.Body.String())
	}
	_, view := decodeCommandResult[SessionAutomationView](t, response)
	return view.LastInput
}

func assertLastInput(t *testing.T, got *sessioninput.LastInput, source, actor, commandID string) {
	t.Helper()
	if got == nil {
		t.Fatalf("last_input missing, want source %s", source)
	}
	if got.Source != source || got.Actor != actor || got.CommandID != commandID || got.Basis != sessioninput.BasisPromptText {
		t.Fatalf("last_input = %+v, want source=%s actor=%q command_id=%q basis=prompt_text", *got, source, actor, commandID)
	}
}

// sessions.get reports who wrote the last prompt the agent accepted: nothing
// before any prompt, the automation command that sent it, a human who typed
// in the terminal, and a human again when an automation send never became a
// message (its text cannot claim the human's prompt).
func TestSessionsGetReportsLastInputOrigin(t *testing.T) {
	db := automationTestDB(t)
	handler, manager := lastInputHandler(t, db)
	client := provisionApprovalTestClient(t, db, "orq-attribution", ScopeSessionsWrite, ScopeSessionsRead)
	actor := "automation_client:" + client.Client.ID

	if got := getLastInput(t, handler, client.Token, "get-0"); got != nil {
		t.Fatalf("session without input reports last_input %+v", *got)
	}

	sendInputAs(t, handler, client.Token, "orq-cmd-1", "Revise o PR e responda no canal")
	assertLastInput(t, getLastInput(t, handler, client.Token, "get-1"), sessioninput.SourceAutomation, actor, "orq-cmd-1")

	manager.signals.inputs.Observe("s1", "agora faz o deploy em staging", true) // typed in the terminal
	assertLastInput(t, getLastInput(t, handler, client.Token, "get-2"), sessioninput.SourceHuman, "", "")

	manager.mu.Lock()
	manager.swallow = true
	manager.mu.Unlock()
	sendInputAs(t, handler, client.Token, "orq-cmd-2", "/compact")
	manager.signals.inputs.Observe("s1", "esquece isso, volta pro bug", true)
	assertLastInput(t, getLastInput(t, handler, client.Token, "get-3"), sessioninput.SourceHuman, "", "")
}

// The initial prompt of sessions.create (custom_prompt here) is credited to
// the creating client and its command.
func TestCreateCustomPromptIsAttributedToTheCreatingCommand(t *testing.T) {
	store := &automationStartSessionStore{
		project: &database.Project{ID: 7, Name: "OpenPoet", Type: "local", Backend: "claude_code"},
		task:    &database.ProjectTask{ID: 9, ProjectID: 7, Title: "Fix remote start", Status: "todo", Priority: "high"},
	}
	manager := &automationStartSessionManager{store: store}
	prompts := &automationInitialPromptCapture{}
	signals := &attributingSignals{inputs: sessioninput.NewTracker()}
	service := application.NewSessionService(
		store, manager, nil, automationStartSessionLinker{task: store.task}, nil, nil, nil, nil,
		application.SessionCreationCollaborators{Tasks: &automationTaskNotificationCapture{}, InitialInput: prompts, Signals: signals},
	)
	executor := &sessionPlatformExecutor{service: service, runtime: manager}
	command, err := executor.Validate(context.Background(), PlatformExecutionInput{
		Handler: "sessions.create", Target: json.RawMessage(`{"project_id":7}`),
		Payload: json.RawMessage(`{"task_id":9,"custom_prompt":"Inspect the failing flow first."}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := application.WithEventMetadata(context.Background(), application.EventMetadata{CommandID: "orq-create-1"})
	result, err := command.Execute(ctx, application.ActionAuthorization{Actor: application.Actor{Type: "automation_client", ID: "orq"}})
	if err != nil {
		t.Fatal(err)
	}
	if view := result.(SessionAutomationView); view.LastInput != nil {
		t.Fatalf("last_input before the agent accepted the prompt: %+v", *view.LastInput)
	}
	delivered := prompts.waitPrompts(t, 1)
	if len(delivered) != 1 {
		t.Fatalf("initial prompt not delivered: %v", delivered)
	}
	signals.inputs.Observe("automation-session", delivered[0], true)
	got, ok := service.LastInput("automation-session")
	if !ok || got.Source != sessioninput.SourceAutomation || got.Actor != "automation_client:orq" ||
		got.CommandID != "orq-create-1" || got.Via != sessioninput.ViaInitialPrompt {
		t.Fatalf("initial prompt last input = %+v (%v)", got, ok)
	}
}
