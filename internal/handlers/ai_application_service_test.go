package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/automation"

	"github.com/go-chi/chi/v5"
)

type aiUIConversationBackend struct {
	deleteID       int64
	deleteAllCalls int
	stopID         int64
	markReadID     int64
	lastAuth       application.ActionAuthorization
	lastMetadata   application.EventMetadata
}

func (b *aiUIConversationBackend) capture(ctx context.Context) {
	b.lastMetadata = application.EventMetadataFromContext(ctx)
}

func (b *aiUIConversationBackend) PrepareChatAtomic(context.Context, application.AIChatPreparationRequest) (*application.AIChatPreparation, error) {
	return nil, errors.New("not implemented")
}

func (b *aiUIConversationBackend) BeginAssistantMessage(context.Context, int64) (int64, error) {
	return 0, errors.New("not implemented")
}

func (b *aiUIConversationBackend) UpdateAssistantMessageProgress(context.Context, application.AIChatProgress) error {
	return errors.New("not implemented")
}

func (b *aiUIConversationBackend) CompleteAssistantMessageAtomic(context.Context, application.AIChatCompletion) error {
	return errors.New("not implemented")
}

func (b *aiUIConversationBackend) DeleteConversationAtomic(ctx context.Context, id int64, authorization application.ActionAuthorization) error {
	b.capture(ctx)
	b.deleteID = id
	b.lastAuth = authorization
	return nil
}

func (b *aiUIConversationBackend) DeleteAllConversationsAtomic(ctx context.Context, authorization application.ActionAuthorization) (int64, error) {
	b.capture(ctx)
	b.deleteAllCalls++
	b.lastAuth = authorization
	return 3, nil
}

func (b *aiUIConversationBackend) StopConversation(ctx context.Context, id int64) error {
	b.capture(ctx)
	b.stopID = id
	return nil
}

func (b *aiUIConversationBackend) InitiateConversationAtomic(context.Context, application.AIInitiationRequest) (*application.AIConversationReference, error) {
	return nil, errors.New("not implemented")
}

func (b *aiUIConversationBackend) MarkConversationRead(ctx context.Context, id int64) error {
	b.capture(ctx)
	b.markReadID = id
	return nil
}

func (b *aiUIConversationBackend) GetSuggestion(context.Context, int64) (*application.AISuggestionRecord, error) {
	return nil, errors.New("not implemented")
}

func (b *aiUIConversationBackend) AcceptSuggestionAtomic(context.Context, int64, application.ActionAuthorization) (application.AISuggestionAcceptance, error) {
	return application.AISuggestionAcceptance{}, errors.New("not implemented")
}

func (b *aiUIConversationBackend) DismissSuggestionAtomic(context.Context, int64, application.ActionAuthorization) error {
	return errors.New("not implemented")
}

func (b *aiUIConversationBackend) DiscussSuggestionAtomic(context.Context, int64, application.ActionAuthorization) (*application.AIConversationReference, error) {
	return nil, errors.New("not implemented")
}

func aiUIHandlerWithSharedService(t *testing.T, backend application.AIConversationBackend) *AIHandler {
	t.Helper()
	registry, err := automation.NewPlatformCapabilityRegistry(application.NewCapabilityRegistry())
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewAIAssistantService(nil, backend, nil, nil)
	api := &API{
		platformCapabilities: registry,
		platformServices: &PlatformApplicationServices{
			Collaboration: automation.CollaborationPlatformServices{AI: service},
		},
	}
	return NewAIHandler(api, nil)
}

func aiUIRouteRequest(method, path, id string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", id)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func assertAIUIBoundary(t *testing.T, backend *aiUIConversationBackend, method, path string) {
	t.Helper()
	if backend.lastMetadata.Actor != platformUIActor() {
		t.Fatalf("event actor = %+v, want %+v", backend.lastMetadata.Actor, platformUIActor())
	}
	if backend.lastMetadata.CorrelationID == "" {
		t.Fatal("event correlation ID was not propagated")
	}
	if backend.lastAuth.Actor.Type != "" {
		if backend.lastAuth.Actor != platformUIActor() || !backend.lastAuth.Approved || backend.lastAuth.ApprovedBy != "user:"+platformUIActorID {
			t.Fatalf("authorization = %+v", backend.lastAuth)
		}
		if backend.lastAuth.Reason != method+" "+path {
			t.Fatalf("authorization reason = %q", backend.lastAuth.Reason)
		}
	}
}

func TestAIConversationUIHandlersUseSharedApplicationService(t *testing.T) {
	backend := &aiUIConversationBackend{}
	handler := aiUIHandlerWithSharedService(t, backend)

	t.Run("delete one", func(t *testing.T) {
		const path = "/api/ai/conversations/41"
		response := httptest.NewRecorder()
		handler.HandleDeleteConversation(response, aiUIRouteRequest(http.MethodDelete, path, "41"))
		if response.Code != http.StatusNoContent || backend.deleteID != 41 {
			t.Fatalf("status=%d deleteID=%d body=%s", response.Code, backend.deleteID, response.Body.String())
		}
		assertAIUIBoundary(t, backend, http.MethodDelete, path)
	})

	t.Run("delete all", func(t *testing.T) {
		const path = "/api/ai/conversations"
		response := httptest.NewRecorder()
		handler.HandleDeleteAllConversations(response, aiUIRouteRequest(http.MethodDelete, path, ""))
		if response.Code != http.StatusNoContent || backend.deleteAllCalls != 1 {
			t.Fatalf("status=%d deleteAllCalls=%d body=%s", response.Code, backend.deleteAllCalls, response.Body.String())
		}
		assertAIUIBoundary(t, backend, http.MethodDelete, path)
	})

	t.Run("mark read", func(t *testing.T) {
		const path = "/api/ai/conversations/42/read"
		backend.lastAuth = application.ActionAuthorization{}
		response := httptest.NewRecorder()
		handler.HandleMarkRead(response, aiUIRouteRequest(http.MethodPost, path, "42"))
		if response.Code != http.StatusOK || backend.markReadID != 42 {
			t.Fatalf("status=%d markReadID=%d body=%s", response.Code, backend.markReadID, response.Body.String())
		}
		assertAIUIBoundary(t, backend, http.MethodPost, path)
	})

	t.Run("stop preserves SSE cleanup at the HTTP edge", func(t *testing.T) {
		const id int64 = 43
		const path = "/api/ai/conversations/43/stop"
		cancelled := false
		handler.activeStreams[id] = &activeStreamInfo{cancel: func() { cancelled = true }}
		response := httptest.NewRecorder()
		handler.HandleStopChat(response, aiUIRouteRequest(http.MethodPost, path, strconv.FormatInt(id, 10)))
		if response.Code != http.StatusOK || backend.stopID != id || !cancelled {
			t.Fatalf("status=%d stopID=%d cancelled=%v body=%s", response.Code, backend.stopID, cancelled, response.Body.String())
		}
		if _, active := handler.activeStreams[id]; active {
			t.Fatal("stopped stream remained registered")
		}
		assertAIUIBoundary(t, backend, http.MethodPost, path)
	})
}

func TestAIConversationUIHandlerDoesNotFallbackWhenPlatformUnavailable(t *testing.T) {
	handler := NewAIHandler(&API{}, nil)
	response := httptest.NewRecorder()
	handler.HandleDeleteConversation(response, aiUIRouteRequest(http.MethodDelete, "/api/ai/conversations/7", "7"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAICustomToolPathsNeverPersistOrCarryStoredCommands(t *testing.T) {
	source, err := os.ReadFile("ai.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, forbidden := range []string{
		`"command":      tool.Command`, `"command": tool.Command`, `Extra["command"]`,
		"CreateProjectTool(ctx", "UpdateProjectTool(ctx", "DeleteProjectTool(ctx",
		"CreateMCPServer(ctx", "UpdateMCPServer(ctx", "DeleteMCPServer(ctx",
		"CreateProjectMCPServer(ctx", "UpdateProjectMCPServer(ctx", "DeleteProjectMCPServer(ctx",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("AI tool path contains forbidden direct command/write pattern %q", forbidden)
		}
	}
	if calls := strings.Count(text, "resolveCustomToolForExecution("); calls != 2 {
		t.Fatalf("stored custom tool resolver calls = %d, want local and SSH execution boundaries", calls)
	}
}
