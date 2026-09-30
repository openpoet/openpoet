package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/jsonlview"
)

// GET /api/sessions/{id}/messages reads the real transcript file through the
// structured view resolver and the shared SessionMessageService.
func TestGetSessionMessagesReadsTheStructuredTranscript(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	api, services := platformCompositionFixture(t)
	if err := api.ConfigurePlatformServices(services); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project := &database.Project{Name: "messages", Path: t.TempDir(), Type: "local", Backend: "claude_code", BackendConfig: "{}"}
	if err := services.DB.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sessionID := "5e551011-0000-4000-8000-000000000001"
	if err := services.DB.CreateSession(ctx, &database.Session{ID: sessionID, ProjectID: project.ID, Status: "stopped", StartTime: time.Now(), Backend: "claude_code"}); err != nil {
		t.Fatal(err)
	}
	transcript := jsonlview.ResolveJSONLPath(project.Path, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join([]string{
		`{"type":"user","uuid":"aaaaaaaa-1","timestamp":"2026-09-29T22:00:00Z","message":{"role":"user","content":"Qual é a configuração do deploy?"}}`,
		`{"type":"assistant","uuid":"bbbbbbbb-2","timestamp":"2026-09-29T22:00:05Z","message":{"id":"msg1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Veja o script."},{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user","uuid":"cccccccc-3","timestamp":"2026-09-29T22:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"deploy.sh"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcript, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(query string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sessionID+"/messages?"+query, nil)
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("id", sessionID)
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
		response := httptest.NewRecorder()
		api.GetSessionMessages(response, request)
		return response
	}

	response := get("last_n=5")
	var list application.SessionMessagesResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &list) != nil {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	if list.Mode != "list" || list.Total != 2 || len(list.Messages) != 2 || list.Messages[0].ID != "aaaaaaaa" || list.Messages[1].Text != "Veja o script." {
		t.Fatalf("list = %+v", list)
	}

	response = get("search=configuracao")
	var search application.SessionMessagesResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &search) != nil || len(search.Hits) != 1 || search.Hits[0].ID != "aaaaaaaa" {
		t.Fatalf("search = %d %s", response.Code, response.Body.String())
	}

	response = get("expand=bbbb")
	var expand application.SessionMessagesResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &expand) != nil || expand.Message == nil || expand.Message.Text != "Veja o script." {
		t.Fatalf("expand = %d %s", response.Code, response.Body.String())
	}

	if response = get("last_n=99"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "session_messages_invalid") {
		t.Fatalf("invalid last_n = %d %s", response.Code, response.Body.String())
	}
	if response = get("expand=deadbeef"); response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "session_message_not_found") {
		t.Fatalf("unknown message = %d %s", response.Code, response.Body.String())
	}
}
