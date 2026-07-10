package automation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func automationContractHandler(t *testing.T, db *database.DB) http.Handler {
	t.Helper()
	service := application.NewProjectTaskService(db, nil)
	registry, err := application.NewProjectTaskCapabilityRegistry(service)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.RegisterWorkRunCapabilities(registry, application.NewWorkRunService(db)); err != nil {
		t.Fatal(err)
	}
	return CapturePeerAddress(NewHandler(db, Dependencies{
		Capabilities: registry,
		Snapshot:     db,
		Now: func() time.Time {
			return time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)
		},
	}))
}

func automationContractProject(t *testing.T, db *database.DB, name string) *database.Project {
	t.Helper()
	project := &database.Project{
		Name: name, Path: "/tmp/" + name, Type: "local", Backend: "claude_code", BackendConfig: "{}",
	}
	if err := db.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	return project
}

func automationRequest(t *testing.T, handler http.Handler, token, method, path, idempotencyKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody *bytes.Reader
	if body == nil {
		requestBody = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, "http://openpoet"+path, requestBody)
	req.RemoteAddr = "127.0.0.1:3210"
	req.Header.Set("Authorization", "Bearer "+token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeCommandResult[T any](t *testing.T, response *httptest.ResponseRecorder) (commandResponse, T) {
	t.Helper()
	var envelope struct {
		APIVersion    string                     `json:"api_version"`
		CommandID     string                     `json:"command_id"`
		CorrelationID string                     `json:"correlation_id"`
		Capability    application.CapabilityName `json:"capability"`
		Status        string                     `json:"status"`
		Actor         Actor                      `json:"actor"`
		Result        json.RawMessage            `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode command response: %v; body=%s", err, response.Body.String())
	}
	var result T
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode command result: %v; body=%s", err, response.Body.String())
	}
	return commandResponse{
		APIVersion: envelope.APIVersion, CommandID: envelope.CommandID,
		CorrelationID: envelope.CorrelationID, Capability: envelope.Capability,
		Status: envelope.Status, Actor: envelope.Actor,
	}, result
}

func decodeAutomationErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode automation error: %v; body=%s", err, response.Body.String())
	}
	return envelope.Error.Code
}

func TestCommandCreateDerivesActorAndReplaysLedgerResponse(t *testing.T) {
	db := automationTestDB(t)
	project := automationContractProject(t, db, "command-replay")
	client := provisionTestClient(t, db, ScopeTasksRead, ScopeTasksWrite)
	handler := automationContractHandler(t, db)
	body := map[string]any{
		"command_id": "cmd-create-1", "idempotency_key": "body-key-create-1",
		"actor":          map[string]any{"type": "root", "id": "spoofed"},
		"capability":     application.CapabilityTasksCreate,
		"target":         map[string]any{"type": "project", "id": project.ID},
		"payload":        map[string]any{"title": "Created through automation", "priority": "high"},
		"correlation_id": "corr-create-1",
	}

	first := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "", body)
	if first.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", first.Code, first.Body.String())
	}
	response, task := decodeCommandResult[database.ProjectTask](t, first)
	if response.APIVersion != APIVersion || response.Status != "succeeded" || response.CommandID != "cmd-create-1" {
		t.Fatalf("unexpected response metadata: %+v", response)
	}
	if response.Actor.Type != "automation_client" || response.Actor.ID != client.Client.ID || response.Actor.ID == "spoofed" {
		t.Fatalf("actor was not derived from bearer identity: %+v", response.Actor)
	}
	history, err := db.ListTaskHistory(context.Background(), task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[0].Actor != "automation_client:"+client.Client.ID {
		t.Fatalf("history actor=%q, want bearer client identity", history[0].Actor)
	}
	events, err := db.ListEventOutboxAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Actor != "automation_client:"+client.Client.ID || events[0].CorrelationID != "corr-create-1" ||
		events[1].EventType != automationCommandSucceededEventType || events[1].Actor != "automation_client:"+client.Client.ID || events[1].CorrelationID != "corr-create-1" {
		t.Fatalf("outbox did not receive authenticated command metadata: %+v", events)
	}

	replay := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d replay=%q body=%s", replay.Code, replay.Header().Get("Idempotency-Replayed"), replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("ledger did not replay the exact response\nfirst=%s\nreplay=%s", first.Body.String(), replay.Body.String())
	}
	tasks, err := db.ListTasksByProject(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("idempotent replay created %d tasks", len(tasks))
	}
}

func TestWorkRunAndPlanAutomationContracts(t *testing.T) {
	db := automationTestDB(t)
	client := provisionTestClient(t, db,
		ScopeProjectsRead, ScopeTasksRead, ScopeSessionsRead, ScopeNotificationsRead,
		ScopeWorkRunsRead, ScopeWorkRunsWrite, ScopePlansRead, ScopePlansWrite)
	handler := automationContractHandler(t, db)
	startBody := map[string]any{
		"command_id": "work-start-1", "capability": application.CapabilityWorkRunsStart,
		"target": map[string]any{},
		"payload": map[string]any{
			"title": "Acompanhar implementação", "description": "Core WorkRun",
			"source": "helena", "expected_minutes": 50,
			"execution_target": map[string]any{"type": "repository", "id": "openpoet"},
		},
	}
	startedResponse := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "header-only-start", startBody)
	if startedResponse.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", startedResponse.Code, startedResponse.Body.String())
	}
	_, run := decodeCommandResult[database.WorkRun](t, startedResponse)
	if run.Status != database.WorkRunStatusRunning || run.Version != 1 || run.ExecutionTarget == nil || run.ExecutionTarget.ID != "openpoet" {
		t.Fatalf("started run=%+v", run)
	}
	pauseBody := map[string]any{
		"command_id": "work-pause-1", "capability": application.CapabilityWorkRunsPause,
		"target": map[string]any{"type": "work_run", "id": run.ID}, "payload": map[string]any{},
		"expected_version": run.Version,
	}
	pausedResponse := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "header-pause", pauseBody)
	if pausedResponse.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", pausedResponse.Code, pausedResponse.Body.String())
	}
	_, paused := decodeCommandResult[database.WorkRun](t, pausedResponse)
	if paused.Status != database.WorkRunStatusPaused || paused.Version != 2 {
		t.Fatalf("paused run=%+v", paused)
	}
	resumeBody := map[string]any{
		"command_id": "work-resume-1", "capability": application.CapabilityWorkRunsResume,
		"target": map[string]any{"type": "work_run", "id": run.ID}, "payload": map[string]any{},
		"expected_version": paused.Version,
	}
	resumedResponse := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "header-resume", resumeBody)
	if resumedResponse.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", resumedResponse.Code, resumedResponse.Body.String())
	}

	planBody := map[string]any{
		"command_id": "plan-upsert-1", "capability": application.CapabilityPlansUpsert,
		"target": map[string]any{},
		"payload": map[string]any{
			"external_ref": "helena:daily:2026-07-09", "kind": "daily", "title": "Plano Helena",
			"period_start": "2026-07-09", "period_end": "2026-07-09", "timezone": "America/Sao_Paulo",
			"items": []map[string]any{{"external_ref": "helena:item:1", "title": "Integrar WorkRun", "sort_order": 10}},
		},
	}
	planResponse := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "header-plan", planBody)
	if planResponse.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResponse.Code, planResponse.Body.String())
	}
	_, plan := decodeCommandResult[database.Plan](t, planResponse)
	if plan.Version != 1 || len(plan.Items) != 1 || plan.Items[0].ExternalRef != "helena:item:1" {
		t.Fatalf("plan=%+v", plan)
	}

	snapshot := automationRequest(t, handler, client.Token, http.MethodGet, "/snapshot", "", nil)
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	var state snapshotResponse
	if err := json.Unmarshal(snapshot.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.ActiveWorkRuns) != 1 || state.ActiveWorkRuns[0].ID != run.ID || len(state.Plans) != 1 || len(state.Plans[0].Items) != 1 {
		t.Fatalf("snapshot work_runs=%+v plans=%+v", state.ActiveWorkRuns, state.Plans)
	}
}

func TestTaskCommandLifecycleUsesTypedApplicationService(t *testing.T) {
	db := automationTestDB(t)
	project := automationContractProject(t, db, "command-lifecycle")
	client := provisionTestClient(t, db, ScopeTasksRead, ScopeTasksWrite)
	broker := provisionApprovalTestClient(t, db, "lifecycle-approval-broker", ScopeApprovalsGrant)
	handler := automationContractHandler(t, db)

	execute := func(commandID string, capability application.CapabilityName, target, payload map[string]any, extras map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{
			"command_id": commandID, "capability": capability, "target": target, "payload": payload,
		}
		for key, value := range extras {
			body[key] = value
		}
		return automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "idem-"+commandID, body)
	}

	createdResponse := execute("create", application.CapabilityTasksCreate,
		map[string]any{"project_id": project.ID}, map[string]any{"title": "Lifecycle one"}, nil)
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	_, first := decodeCommandResult[database.ProjectTask](t, createdResponse)

	newTitle := "Lifecycle updated"
	updatedResponse := execute("update", application.CapabilityTasksUpdate,
		map[string]any{"project_id": project.ID, "task_id": first.ID}, map[string]any{"title": newTitle}, nil)
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	_, updated := decodeCommandResult[database.ProjectTask](t, updatedResponse)
	if updated.Title != newTitle {
		t.Fatalf("updated title=%q", updated.Title)
	}

	statusResponse := execute("status", application.CapabilityTasksChangeStatus,
		map[string]any{"project_id": project.ID, "task_id": first.ID}, map[string]any{"status": "in_progress"}, nil)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status command=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	secondResponse := execute("create-second", application.CapabilityTasksCreate,
		map[string]any{"project_id": project.ID}, map[string]any{"title": "Lifecycle two"}, nil)
	_, second := decodeCommandResult[database.ProjectTask](t, secondResponse)
	reorderResponse := execute("reorder", application.CapabilityTasksReorderProject,
		map[string]any{"project_id": project.ID}, map[string]any{"items": []map[string]any{
			{"id": second.ID, "sort_order": 1}, {"id": first.ID, "sort_order": 2},
		}}, nil)
	if reorderResponse.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", reorderResponse.Code, reorderResponse.Body.String())
	}

	session := &database.Session{
		ID: "automation-lifecycle-session", ProjectID: project.ID, Status: "running",
		Name: "Lifecycle session", StartTime: time.Now(), Backend: "claude_code",
	}
	if err := db.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	linkResponse := execute("link", application.CapabilityTasksLinkSession,
		map[string]any{"type": "session", "id": session.ID}, map[string]any{"task_id": first.ID}, nil)
	if linkResponse.Code != http.StatusOK {
		t.Fatalf("link status=%d body=%s", linkResponse.Code, linkResponse.Body.String())
	}
	storedSession, err := db.GetSession(context.Background(), session.ID)
	if err != nil || !storedSession.TaskID.Valid || storedSession.TaskID.Int64 != first.ID {
		t.Fatalf("session link=%+v err=%v", storedSession, err)
	}
	unlinkResponse := execute("unlink", application.CapabilityTasksUnlinkSession,
		map[string]any{"session_id": session.ID}, map[string]any{}, nil)
	if unlinkResponse.Code != http.StatusOK {
		t.Fatalf("unlink status=%d body=%s", unlinkResponse.Code, unlinkResponse.Body.String())
	}

	dryDelete := execute("delete-dry", application.CapabilityTasksDelete,
		map[string]any{"project_id": project.ID, "task_id": second.ID}, map[string]any{}, map[string]any{"dry_run": true})
	if dryDelete.Code != http.StatusOK {
		t.Fatalf("dry delete status=%d body=%s", dryDelete.Code, dryDelete.Body.String())
	}
	dryMetadata, _ := decodeCommandResult[map[string]any](t, dryDelete)
	if dryMetadata.Status != "dry_run" {
		t.Fatalf("dry run status=%q", dryMetadata.Status)
	}
	if _, err := db.GetTask(context.Background(), second.ID); err != nil {
		t.Fatalf("dry run deleted task: %v", err)
	}

	missingApproval := execute("delete-no-approval", application.CapabilityTasksDelete,
		map[string]any{"project_id": project.ID, "task_id": second.ID}, map[string]any{},
		map[string]any{"reason": "cleanup", "correlation_id": "task:authorization:lifecycle-delete-no-approval"})
	if missingApproval.Code != http.StatusConflict || decodeAutomationErrorCode(t, missingApproval) != "approval_required" {
		t.Fatalf("missing approval status=%d body=%s", missingApproval.Code, missingApproval.Body.String())
	}
	grant := issueApprovalGrantForTest(t, handler, broker.Token, client.Client.ID,
		application.CapabilityTasksDelete, "delete", "task:authorization:lifecycle-delete", 0)
	deleted := execute("delete", application.CapabilityTasksDelete,
		map[string]any{"project_id": project.ID, "task_id": second.ID}, map[string]any{},
		map[string]any{"reason": "cleanup", "approval_token": grant.ApprovalToken, "correlation_id": grant.AuthorizationRef})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}

func TestCommandRejectsUnsupportedVersionAndInsufficientScope(t *testing.T) {
	t.Run("expected version", func(t *testing.T) {
		db := automationTestDB(t)
		project := automationContractProject(t, db, "expected-version")
		client := provisionTestClient(t, db, ScopeTasksWrite)
		handler := automationContractHandler(t, db)
		response := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "version-key", map[string]any{
			"command_id": "version", "capability": application.CapabilityTasksCreate,
			"target": map[string]any{"project_id": project.ID}, "payload": map[string]any{"title": "Must not exist"},
			"expected_version": 3,
		})
		if response.Code != http.StatusUnprocessableEntity || decodeAutomationErrorCode(t, response) != "expected_version_unsupported" {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		tasks, _ := db.ListTasksByProject(context.Background(), project.ID)
		if len(tasks) != 0 {
			t.Fatal("unsupported expected_version was ignored")
		}
	})

	t.Run("scope", func(t *testing.T) {
		db := automationTestDB(t)
		project := automationContractProject(t, db, "insufficient-scope")
		client := provisionTestClient(t, db, ScopeTasksRead)
		handler := automationContractHandler(t, db)
		response := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "scope-key", map[string]any{
			"command_id": "scope", "capability": application.CapabilityTasksCreate,
			"target": map[string]any{"project_id": project.ID}, "payload": map[string]any{"title": "Must not exist"},
		})
		if response.Code != http.StatusForbidden || decodeAutomationErrorCode(t, response) != "insufficient_scope" {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestCapabilitiesAndSnapshotContracts(t *testing.T) {
	db := automationTestDB(t)
	project := automationContractProject(t, db, "snapshot")
	client := provisionTestClient(t, db,
		ScopeProjectsRead, ScopeTasksRead, ScopeSessionsRead, ScopeNotificationsRead, ScopeWorkRunsRead, ScopePlansRead,
	)
	handler := automationContractHandler(t, db)
	task := &database.ProjectTask{ProjectID: project.ID, Title: "Snapshot task", Status: "todo", Priority: "medium"}
	if err := db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	session := &database.Session{
		ID: "snapshot-session", ProjectID: project.ID, Status: "running", Name: "Snapshot",
		TaskID: sql.NullInt64{Int64: task.ID, Valid: true}, StartTime: time.Now(), Backend: "claude_code",
	}
	if err := db.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNotification(context.Background(), &database.Notification{
		SessionID: session.ID, Type: "info", Title: "Snapshot notification", Body: "body",
	}); err != nil {
		t.Fatal(err)
	}
	workRunService := application.NewWorkRunService(db, application.WorkRunServiceOptions{
		Now: func() time.Time { return time.Date(2026, 7, 9, 14, 58, 0, 0, time.UTC) },
	})
	activeRun, err := workRunService.Start(context.Background(), application.StartWorkRunCommand{
		Title: "Snapshot active run", Source: "test", ExpectedMinutes: 10, IdempotencyKey: "snapshot-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	capabilities := automationRequest(t, handler, client.Token, http.MethodGet, "/capabilities", "", nil)
	if capabilities.Code != http.StatusOK {
		t.Fatalf("capabilities status=%d body=%s", capabilities.Code, capabilities.Body.String())
	}
	var listed capabilitiesResponse
	if err := json.Unmarshal(capabilities.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.APIVersion != APIVersion || len(listed.Capabilities) != 24 {
		t.Fatalf("capabilities response=%+v", listed)
	}
	foundCreate := false
	for _, capability := range listed.Capabilities {
		if len(capability.Scopes) != 1 || capability.Scopes[0] != capability.Scope {
			t.Fatalf("legacy capability did not emit compatible scopes: %+v", capability)
		}
		if capability.Name == application.CapabilityTasksCreate {
			foundCreate = true
			if capability.Allowed || !capability.Mutation || capability.Handler != application.CapabilityHandlerTasksCreate || capability.Service != application.CapabilityServiceProjectTasks {
				t.Fatalf("unexpected create descriptor: %+v", capability)
			}
		}
	}
	if !foundCreate {
		t.Fatal("tasks.create capability missing")
	}

	snapshot := automationRequest(t, handler, client.Token, http.MethodGet, "/snapshot?notification_limit=10", "", nil)
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	var state snapshotResponse
	if err := json.Unmarshal(snapshot.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.APIVersion != APIVersion || state.Cursor != "1" || !state.GeneratedAt.Equal(time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("snapshot metadata=%+v", state)
	}
	if len(state.Projects) != 1 || len(state.Tasks) != 1 || len(state.Sessions) != 1 || len(state.Notifications) != 1 {
		t.Fatalf("snapshot counts projects=%d tasks=%d sessions=%d notifications=%d", len(state.Projects), len(state.Tasks), len(state.Sessions), len(state.Notifications))
	}
	if len(state.ActiveWorkRuns) != 1 || state.ActiveWorkRuns[0].ID != activeRun.ID || state.ActiveWorkRuns[0].ActiveSeconds != 120 {
		t.Fatalf("snapshot active work runs=%+v", state.ActiveWorkRuns)
	}
}

func TestCommandRejectsHeaderAndBodyIdempotencyMismatch(t *testing.T) {
	db := automationTestDB(t)
	project := automationContractProject(t, db, "idempotency-mismatch")
	client := provisionTestClient(t, db, ScopeTasksWrite)
	handler := automationContractHandler(t, db)
	response := automationRequest(t, handler, client.Token, http.MethodPost, "/commands", "header-key", map[string]any{
		"command_id": "mismatch", "idempotency_key": "body-key",
		"capability": application.CapabilityTasksCreate,
		"target":     map[string]any{"project_id": project.ID}, "payload": map[string]any{"title": "Must not exist"},
	})
	if response.Code != http.StatusBadRequest || decodeAutomationErrorCode(t, response) != "idempotency_key_conflict" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestVersionedAutomationSchemasMatchRegistry(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "automation", "v1")
	openAPIBytes, err := os.ReadFile(filepath.Join(root, "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var openAPI struct {
		OpenAPI    string                     `json:"openapi"`
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas struct {
				Capability struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"Capability"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIBytes, &openAPI); err != nil {
		t.Fatalf("invalid OpenAPI JSON: %v", err)
	}
	if openAPI.OpenAPI != "3.1.0" {
		t.Fatalf("OpenAPI version=%q", openAPI.OpenAPI)
	}
	for _, path := range []string{"/health", "/capabilities", "/approvals", "/commands", "/events", "/events/ack", "/snapshot"} {
		if _, ok := openAPI.Paths[path]; !ok {
			t.Fatalf("OpenAPI path %s missing", path)
		}
	}
	if _, ok := openAPI.Components.Schemas.Capability.Properties["scopes"]; !ok {
		t.Fatal("OpenAPI capability contract is missing scopes")
	}
	hasRequiredScopes := false
	for _, field := range openAPI.Components.Schemas.Capability.Required {
		if field == "scopes" {
			hasRequiredScopes = true
			break
		}
	}
	if !hasRequiredScopes {
		t.Fatal("OpenAPI capability contract does not require scopes")
	}

	schemaBytes, err := os.ReadFile(filepath.Join(root, "command-envelope.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID         string   `json:"$id"`
		Required   []string `json:"required"`
		Properties struct {
			Capability struct {
				Type    string `json:"type"`
				Pattern string `json:"pattern"`
			} `json:"capability"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatalf("invalid command schema JSON: %v", err)
	}
	if schema.ID == "" {
		t.Fatal("command schema is not versioned with an $id")
	}
	if schema.Properties.Capability.Type != "string" || schema.Properties.Capability.Pattern != platformIdentifierPattern.String() {
		t.Fatalf("command schema capability contract=%+v", schema.Properties.Capability)
	}
}
