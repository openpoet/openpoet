package automation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/updater"
	"openpoet/internal/voice"
)

type executionPlatformFakePorts struct {
	sessions       []database.Session
	session        *database.Session
	sessionOutput  []byte
	eventStatus    SessionEventStatusView
	files          []FileMetadataAutomationView
	fileRead       OperationalFileReadResult
	preview        FileMetadataAutomationView
	gitStatus      GitStatusAutomationView
	gitBranches    GitBranchesAutomationView
	gitLog         []GitLogEntryAutomationView
	gitDiff        string
	gitShow        GitShowAutomationView
	tunnelStatus   TunnelStatusAutomationView
	devices        []database.PairedDevice
	updateStatus   *updater.UpdateStatus
	readCalls      int
	updateChecks   int
	voiceCalls     int
	voiceResult    *voice.TranscriptionResult
	activeSessions map[string]bool
	incidents      []database.CoordinatorIncident
	fileActivity   []database.SessionFileActivity
}

func (f *executionPlatformFakePorts) ListSessions(context.Context) ([]database.Session, error) {
	f.readCalls++
	return append([]database.Session(nil), f.sessions...), nil
}

func (f *executionPlatformFakePorts) GetSession(context.Context, string) (*database.Session, error) {
	f.readCalls++
	return f.session, nil
}

func (f *executionPlatformFakePorts) IsSessionRunning(id string) bool {
	return f.activeSessions[id]
}

func (f *executionPlatformFakePorts) GetSessionOutput(string) ([]byte, error) {
	f.readCalls++
	return append([]byte(nil), f.sessionOutput...), nil
}

func (f *executionPlatformFakePorts) SessionEventStatus(context.Context, string) (SessionEventStatusView, error) {
	f.readCalls++
	return f.eventStatus, nil
}

func (f *executionPlatformFakePorts) ListOperationalFiles(context.Context, OperationalFileScope, string, int) ([]FileMetadataAutomationView, error) {
	f.readCalls++
	return append([]FileMetadataAutomationView(nil), f.files...), nil
}

func (f *executionPlatformFakePorts) ReadOperationalFile(context.Context, OperationalFileScope, string, int) (OperationalFileReadResult, error) {
	f.readCalls++
	return f.fileRead, nil
}

func (f *executionPlatformFakePorts) OperationalFilePreviewMetadata(context.Context, OperationalFileScope, string) (FileMetadataAutomationView, error) {
	f.readCalls++
	return f.preview, nil
}

func (f *executionPlatformFakePorts) OperationalGitStatus(context.Context, int64, int) (GitStatusAutomationView, error) {
	f.readCalls++
	return f.gitStatus, nil
}

func (f *executionPlatformFakePorts) OperationalGitBranches(context.Context, int64, int) (GitBranchesAutomationView, error) {
	f.readCalls++
	return f.gitBranches, nil
}

func (f *executionPlatformFakePorts) OperationalGitLog(context.Context, int64, GitLogQuery) ([]GitLogEntryAutomationView, error) {
	f.readCalls++
	return append([]GitLogEntryAutomationView(nil), f.gitLog...), nil
}

func (f *executionPlatformFakePorts) OperationalGitDiff(context.Context, int64, GitDiffQuery) (string, error) {
	f.readCalls++
	return f.gitDiff, nil
}

func (f *executionPlatformFakePorts) OperationalGitShow(context.Context, int64, string, int) (GitShowAutomationView, error) {
	f.readCalls++
	return f.gitShow, nil
}

func (f *executionPlatformFakePorts) OperationalTunnelStatus(context.Context) (TunnelStatusAutomationView, error) {
	f.readCalls++
	return f.tunnelStatus, nil
}

func (f *executionPlatformFakePorts) ListPairedDevices(context.Context) ([]database.PairedDevice, error) {
	f.readCalls++
	return append([]database.PairedDevice(nil), f.devices...), nil
}

func (f *executionPlatformFakePorts) LastCheck() *updater.UpdateStatus {
	f.readCalls++
	return f.updateStatus
}

func (f *executionPlatformFakePorts) CheckForUpdate(context.Context) (*updater.UpdateStatus, error) {
	f.readCalls++
	f.updateChecks++
	return f.updateStatus, nil
}

func (f *executionPlatformFakePorts) TranscribeAudio(context.Context, []byte, string, string) (*voice.TranscriptionResult, error) {
	f.voiceCalls++
	return f.voiceResult, nil
}

func (f *executionPlatformFakePorts) ListCoordinatorIncidents(context.Context, int64, string, int) ([]database.CoordinatorIncident, error) {
	f.readCalls++
	return append([]database.CoordinatorIncident(nil), f.incidents...), nil
}

func (f *executionPlatformFakePorts) GetCoordinatorIncident(_ context.Context, id string) (*database.CoordinatorIncident, error) {
	f.readCalls++
	for i := range f.incidents {
		if f.incidents[i].ID == id {
			incident := f.incidents[i]
			return &incident, nil
		}
	}
	return nil, nil
}

func (f *executionPlatformFakePorts) ListSessionFileActivity(context.Context, string, int) ([]database.SessionFileActivity, error) {
	f.readCalls++
	return append([]database.SessionFileActivity(nil), f.fileActivity...), nil
}

func executionPlatformTestServices(ports *executionPlatformFakePorts) ExecutionPlatformServices {
	return ExecutionPlatformServices{
		Sessions:           application.NewSessionService(nil, nil, nil, nil, nil, nil, nil, nil),
		SessionWatchers:    application.NewSessionEventWatcherService(nil, nil, nil),
		SessionSuggestions: application.NewSessionTaskSuggestionService(nil, nil, nil, nil),
		FileMutations:      application.NewFileMutationService(nil, nil, nil),
		GitMutations:       application.NewGitMutationService(nil, nil, nil),
		HookResponses:      application.NewHookResponseService(nil, nil),
		Voice:              application.NewVoiceTranscriptionService(ports),
		TunnelMutations:    application.NewTunnelMutationService(nil, nil),
		UpdateMutations:    application.NewUpdateMutationService(nil, nil, nil),
		SessionQueries:     ports,
		SessionRuntime:     ports,
		SessionEvents:      ports,
		Files:              ports,
		Git:                ports,
		Tunnel:             ports,
		Updates:            ports,
		Conflicts:          ports,
		Workspaces:         application.NewWorkspaceService(nil, nil, nil),
		Blackboard:         ports,
		Environments:       ports,
	}
}

func (f *executionPlatformFakePorts) BlackboardGet(context.Context, string, int64, string) (*database.BlackboardEntry, error) {
	return nil, nil
}

func (f *executionPlatformFakePorts) BlackboardPut(context.Context, database.BlackboardPutInput) (int64, error) {
	return 1, nil
}

func (f *executionPlatformFakePorts) ApproveManifest(context.Context, int64, string, string) (map[string]any, error) {
	return map[string]any{"approved": true}, nil
}

func executionPlatformTestRegistry(t *testing.T, ports *executionPlatformFakePorts) (*application.CapabilityRegistry, *PlatformCapabilityRegistry) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err = RegisterExecutionPlatformCapabilities(registry, executionPlatformTestServices(ports)); err != nil {
		t.Fatal(err)
	}
	return capabilities, registry
}

func executionPlatformDefinitionsForTest() []PlatformCapabilityDefinition {
	groups := [][]PlatformCapabilityDefinition{
		sessionPlatformDefinitions(), sessionWatcherPlatformDefinitions(), sessionSuggestionPlatformDefinitions(),
		fileExecutionPlatformDefinitions(), gitExecutionPlatformDefinitions(), hookExecutionPlatformDefinitions(),
		voiceExecutionPlatformDefinitions(), tunnelExecutionPlatformDefinitions(), updateExecutionPlatformDefinitions(),
		conflictPlatformDefinitions(), workspacePlatformDefinitions(), blackboardPlatformDefinitions(),
		environmentPlatformDefinitions(),
	}
	var result []PlatformCapabilityDefinition
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}

func executionPlatformActor(definitions []PlatformCapabilityDefinition) Actor {
	scopes := ScopeSet{}
	for _, definition := range definitions {
		for _, scope := range definition.Scopes {
			scopes[Scope(scope)] = struct{}{}
		}
	}
	return Actor{Type: "automation_client", ID: "helena", ClientID: "helena", Name: "Helena", Scopes: scopes}
}

func TestExecutionPlatformRegistersCompleteUniqueSurface(t *testing.T) {
	definitions := executionPlatformDefinitionsForTest()
	if len(definitions) != 57 {
		t.Fatalf("execution surface has %d capabilities, want 57", len(definitions))
	}
	seen := make(map[application.CapabilityName]struct{}, len(definitions))
	for _, definition := range definitions {
		if _, duplicate := seen[definition.Name]; duplicate {
			t.Fatalf("duplicate execution capability %q", definition.Name)
		}
		seen[definition.Name] = struct{}{}
	}
	capabilities, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	if got := len(capabilities.List()); got != 57 {
		t.Fatalf("application registry has %d execution capabilities, want 57", got)
	}
	if got := len(registry.ListForActor(executionPlatformActor(definitions))); got != 57 {
		t.Fatalf("platform discovery has %d execution capabilities, want 57", got)
	}
}

func TestExecutionPlatformRegistrationRequiresAllPortsBeforeMutation(t *testing.T) {
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	services := executionPlatformTestServices(&executionPlatformFakePorts{})
	services.Git = nil
	if err = RegisterExecutionPlatformCapabilities(registry, services); err == nil {
		t.Fatal("registration accepted a missing operational read port")
	}
	if got := len(capabilities.List()); got != 0 {
		t.Fatalf("failed registration partially mutated registry with %d capabilities", got)
	}
}

type executionManifest struct {
	Routes []struct {
		Capability         application.CapabilityName `json:"capability"`
		Risk               string                     `json:"risk"`
		Scopes             []string                   `json:"scopes"`
		ApplicationService string                     `json:"application_service"`
	} `json:"routes"`
}

func TestExecutionPlatformMutationMetadataMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "automation", "ui-action-manifest.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest executionManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	definitions := make(map[application.CapabilityName]PlatformCapabilityDefinition)
	for _, definition := range executionPlatformDefinitionsForTest() {
		definitions[definition.Name] = definition
	}
	wantedServices := map[string]struct{}{
		"SessionService": {}, "SessionEventWatcherService": {}, "SessionTaskSuggestionService": {},
		"FileMutationService": {}, "GitMutationService": {}, "HookResponseService": {},
		"VoiceTranscriptionService": {}, "TunnelMutationService": {}, "UpdateMutationService": {},
	}
	riskMetadata := map[string]struct {
		risk     application.CapabilityRisk
		approval application.ApprovalPolicy
	}{
		"R1": {application.CapabilityRiskRead, application.ApprovalNone},
		"R2": {application.CapabilityRiskWrite, application.ApprovalByPolicy},
		"R3": {application.CapabilityRiskDestructive, application.ApprovalExplicit},
		"R4": {application.CapabilityRiskUnsafe, application.ApprovalExplicit},
	}
	checked := 0
	for _, route := range manifest.Routes {
		if _, wanted := wantedServices[route.ApplicationService]; !wanted {
			continue
		}
		definition, exists := definitions[route.Capability]
		if !exists {
			t.Errorf("manifest mutation %q has no execution adapter", route.Capability)
			continue
		}
		expected := riskMetadata[route.Risk]
		gotScopes := make([]string, len(definition.Scopes))
		for i, scope := range definition.Scopes {
			gotScopes[i] = string(scope)
		}
		if !reflect.DeepEqual(gotScopes, route.Scopes) || definition.Risk != expected.risk || definition.Approval != expected.approval {
			t.Errorf("metadata mismatch for %q: scopes=%v risk=%s approval=%s; want scopes=%v risk=%s approval=%s",
				route.Capability, gotScopes, definition.Risk, definition.Approval, route.Scopes, expected.risk, expected.approval)
		}
		if !definition.Mutation {
			t.Errorf("manifest mutation %q is not marked mutation", route.Capability)
		}
		checked++
	}
	if checked != 27 {
		t.Fatalf("checked %d execution mutations, want 27", checked)
	}
}

func TestExecutionPlatformReadSurfaceIsExplicit(t *testing.T) {
	want := []string{
		"blackboard.get",
		"conflicts.get", "conflicts.list",
		"files.list", "files.preview_metadata", "files.read", "git.branches", "git.diff", "git.log", "git.show", "git.status",
		"sessions.active", "sessions.events_status", "sessions.file_activity", "sessions.get", "sessions.history", "sessions.list",
		"tunnel.devices", "tunnel.status", "update.check", "update.status",
		"workspaces.get", "workspaces.list",
	}
	var got []string
	for _, definition := range executionPlatformDefinitionsForTest() {
		if definition.Risk != application.CapabilityRiskRead || definition.Approval != application.ApprovalNone || definition.Mutation {
			continue
		}
		got = append(got, string(definition.Name))
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("execution read surface mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestExecutionPlatformDiscoveryRequiresEveryManifestScope(t *testing.T) {
	_, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	actor := Actor{Type: "automation_client", ID: "helena", Scopes: ScopeSet{"tunnel:admin": {}}}
	find := func() PlatformCapabilityDescriptor {
		for _, descriptor := range registry.ListForActor(actor) {
			if descriptor.Name == "tunnel.enable" {
				return descriptor
			}
		}
		t.Fatal("tunnel.enable not discovered")
		return PlatformCapabilityDescriptor{}
	}
	descriptor := find()
	if descriptor.Allowed || !reflect.DeepEqual(descriptor.Scopes, []application.CapabilityScope{"tunnel:admin", "credentials:use"}) {
		t.Fatalf("incomplete tunnel scopes were allowed: %#v", descriptor)
	}
	actor.Scopes["credentials:use"] = struct{}{}
	if descriptor = find(); !descriptor.Allowed {
		t.Fatalf("complete tunnel scopes were rejected: %#v", descriptor)
	}
}

type executionDryRunCase struct {
	name       string
	target     string
	payload    string
	secretText []string
}

func executionDryRunCases() []executionDryRunCase {
	secretAudio := base64.StdEncoding.EncodeToString([]byte("audio-secret"))
	secretFile := base64.StdEncoding.EncodeToString([]byte("file-secret"))
	return []executionDryRunCase{
		{name: "sessions.list", target: `{}`, payload: `{}`},
		{name: "sessions.get", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.history", target: `{"id":"s1"}`, payload: `{"max_bytes":1024}`},
		{name: "sessions.active", target: `{}`, payload: `{}`},
		{name: "sessions.create", target: `{"project_id":1}`, payload: `{"environment":{"API_KEY":"session-secret"}}`, secretText: []string{"session-secret"}},
		{name: "sessions.stop", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.isolate", target: `{"id":"s1"}`, payload: `{"reason":"contested write","briefing":"brief-secret"}`, secretText: []string{"brief-secret"}},
		{name: "sessions.reopen", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.send_input", target: `{"id":"s1"}`, payload: `{"text":"input-secret"}`, secretText: []string{"input-secret"}},
		{name: "sessions.set_model", target: `{"id":"s1"}`, payload: `{"model":"gpt-test"}`},
		{name: "sessions.set_effort", target: `{"id":"s1"}`, payload: `{"effort":"high"}`},
		{name: "sessions.evaluate", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.image_prompt_hint", target: `{"id":"s1"}`, payload: `{"user_prompt":"prompt-secret","image_count":1}`, secretText: []string{"prompt-secret"}},
		{name: "sessions.events_status", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.events_watch_start", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.events_watch_stop", target: `{"id":"s1"}`, payload: `{}`},
		{name: "sessions.suggest_task_data", target: `{"id":"s1"}`, payload: `{}`},
		{name: "files.list", target: `{"type":"project","id":1}`, payload: `{}`},
		{name: "files.read", target: `{"type":"project","id":1}`, payload: `{"path":"README.md","max_bytes":1024}`},
		{name: "files.preview_metadata", target: `{"type":"project","id":1}`, payload: `{"path":"index.html"}`},
		{name: "files.write", target: `{"project_id":1}`, payload: `{"path":"README.md","content":"file-secret"}`, secretText: []string{"file-secret"}},
		{name: "files.upload_project", target: `{"project_id":1}`, payload: `{"path":"data.bin","data_base64":"` + secretFile + `"}`, secretText: []string{"file-secret", secretFile}},
		{name: "files.upload_session", target: `{"id":"s1"}`, payload: `{"files":[{"path":"data.bin","data_base64":"` + secretFile + `"}]}`, secretText: []string{"file-secret", secretFile}},
		{name: "files.paste_session_image", target: `{"id":"s1"}`, payload: `{"data_url":"data:image/png;base64,aQ=="}`},
		{name: "git.status", target: `{"project_id":1}`, payload: `{}`},
		{name: "git.branches", target: `{"project_id":1}`, payload: `{}`},
		{name: "git.log", target: `{"project_id":1}`, payload: `{"limit":10}`},
		{name: "git.diff", target: `{"project_id":1}`, payload: `{"path":"main.go","max_bytes":1024}`},
		{name: "git.show", target: `{"project_id":1}`, payload: `{"ref":"HEAD","max_bytes":1024}`},
		{name: "git.stage", target: `{"project_id":1}`, payload: `{"files":["main.go"]}`},
		{name: "git.unstage", target: `{"project_id":1}`, payload: `{"files":["main.go"]}`},
		{name: "git.commit", target: `{"project_id":1}`, payload: `{"message":"commit-secret"}`, secretText: []string{"commit-secret"}},
		{name: "hooks.respond_permission", target: `{"id":"s1"}`, payload: `{"behavior":"allow","message":"hook-secret"}`, secretText: []string{"hook-secret"}},
		{name: "hooks.respond_task_notification", target: `{"id":"s1"}`, payload: `{}`},
		{name: "voice.transcribe", target: `{}`, payload: `{"audio_base64":"` + secretAudio + `","filename":"recording.webm"}`, secretText: []string{"audio-secret", secretAudio}},
		{name: "tunnel.status", target: `{}`, payload: `{}`},
		{name: "tunnel.devices", target: `{}`, payload: `{}`},
		{name: "tunnel.enable", target: `{}`, payload: `{}`},
		{name: "tunnel.disable", target: `{}`, payload: `{}`},
		{name: "tunnel.revoke_device", target: `{"id":"device-1"}`, payload: `{}`},
		{name: "tunnel.delete_device", target: `{"id":"device-1"}`, payload: `{}`},
		{name: "tunnel.confirm_pairing", target: `{}`, payload: `{"code":"123456"}`, secretText: []string{"123456"}},
		{name: "update.status", target: `{}`, payload: `{}`},
		{name: "update.check", target: `{}`, payload: `{}`},
		{name: "conflicts.list", target: `{"project_id":1}`, payload: `{}`},
		{name: "conflicts.get", target: `{"type":"conflict","id":"C-1"}`, payload: `{}`},
		{name: "sessions.file_activity", target: `{"type":"session","id":"s1"}`, payload: `{}`},
		{name: "workspaces.list", target: `{"project_id":1}`, payload: `{}`},
		{name: "workspaces.get", target: `{"type":"workspace","id":"ws-1"}`, payload: `{}`},
		{name: "workspaces.create", target: `{"project_id":1}`, payload: `{"name":"lane-a"}`},
		{name: "workspaces.remove", target: `{"type":"workspace","id":"ws-1"}`, payload: `{}`},
		{name: "workspaces.merge", target: `{"type":"workspace","id":"ws-1"}`, payload: `{}`},
		{name: "workspaces.discard", target: `{"type":"workspace","id":"ws-1"}`, payload: `{}`},
		{name: "update.apply", target: `{}`, payload: `{}`},
		{name: "blackboard.get", target: `{}`, payload: `{"scope":"global","key":"k"}`},
		{name: "blackboard.put", target: `{}`, payload: `{"scope":"global","key":"k","value":{"n":1},"expected_version":0}`},
		{name: "environments.approve_manifest", target: `{"type":"project","project_id":1}`, payload: `{"content_sha256":""}`},
	}
}

func TestExecutionPlatformEveryCapabilitySupportsSideEffectFreeDryRun(t *testing.T) {
	definitions := executionPlatformDefinitionsForTest()
	ports := &executionPlatformFakePorts{}
	_, registry := executionPlatformTestRegistry(t, ports)
	actor := executionPlatformActor(definitions)
	cases := executionDryRunCases()
	if len(cases) != len(definitions) {
		t.Fatalf("dry-run table has %d capabilities, want %d", len(cases), len(definitions))
	}
	seen := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, duplicate := seen[testCase.name]; duplicate {
				t.Fatalf("duplicate dry-run case %q", testCase.name)
			}
			seen[testCase.name] = struct{}{}
			result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
				Capability: application.CapabilityName(testCase.name), Actor: actor, DryRun: true,
				Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "dry_run" {
				t.Fatalf("status=%q, want dry_run", result.Status)
			}
			encoded, err := json.Marshal(result.Result)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range testCase.secretText {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("dry-run preview leaked %q: %s", secret, encoded)
				}
			}
		})
	}
	if ports.readCalls != 0 || ports.voiceCalls != 0 {
		t.Fatalf("dry-run crossed an operational port boundary: reads=%d voice=%d", ports.readCalls, ports.voiceCalls)
	}
}

func TestExecutionPlatformR3AndR4RequireApprovalBeforeValidation(t *testing.T) {
	definitions := executionPlatformDefinitionsForTest()
	_, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	actor := executionPlatformActor(definitions)
	cases := make(map[string]executionDryRunCase)
	for _, testCase := range executionDryRunCases() {
		cases[testCase.name] = testCase
	}
	checked := 0
	for _, definition := range definitions {
		if definition.Risk != application.CapabilityRiskDestructive && definition.Risk != application.CapabilityRiskUnsafe {
			continue
		}
		testCase := cases[string(definition.Name)]
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
				Capability: definition.Name, Actor: actor, Reason: "authorized operation",
				Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
			})
			var dispatchErr *PlatformDispatchError
			if !errors.As(err, &dispatchErr) || dispatchErr.Code != "platform_approval_required" {
				t.Fatalf("unapproved %q crossed approval boundary: %T %v", definition.Name, err, err)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no R3/R4 execution capabilities were checked")
	}
}

func TestExecutionPlatformReadResultsAreBoundedAndRedacted(t *testing.T) {
	secret := "read-secret-marker"
	ports := &executionPlatformFakePorts{
		sessionOutput: []byte("before\nAPI_KEY=" + secret + "\nafter"),
		gitDiff:       "diff --git a/x b/x\n+TOKEN=" + secret,
		devices: []database.PairedDevice{{
			ID: "device-1", DeviceName: "Phone", UserAgent: "agent", EncryptionKey: secret,
			EncryptionKeyIV: "iv-marker", CreatedAt: time.Now(), LastSeenAt: time.Now(),
		}},
		updateStatus: &updater.UpdateStatus{
			Available: true, CurrentVersion: "1.0.0", LatestVersion: "2.0.0",
			ReleaseURL:   "https://user:password@example.com/release?token=" + secret,
			DownloadURL:  "https://example.com/download?secret=" + secret,
			ChecksumURL:  "https://example.com/checksum?secret=" + secret,
			ReleaseNotes: "PASSWORD=" + secret, Error: "backend " + secret,
		},
		voiceResult: &voice.TranscriptionResult{Text: "AUTH_TOKEN=" + secret, Duration: 1.5},
	}
	definitions := executionPlatformDefinitionsForTest()
	_, registry := executionPlatformTestRegistry(t, ports)
	actor := executionPlatformActor(definitions)
	cases := []executionDryRunCase{
		{name: "sessions.history", target: `{"id":"s1"}`, payload: `{"max_bytes":1024}`},
		{name: "git.diff", target: `{"project_id":1}`, payload: `{"max_bytes":1024}`},
		{name: "tunnel.devices", target: `{}`, payload: `{}`},
		{name: "update.check", target: `{}`, payload: `{}`},
		{name: "voice.transcribe", target: `{}`, payload: `{"audio_base64":"YQ==","filename":"recording.webm"}`},
	}
	for _, testCase := range cases {
		result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
			Capability: application.CapabilityName(testCase.name), Actor: actor,
			Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
		})
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		encoded, err := json.Marshal(result.Result)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{secret, "iv-marker", "download_url", "checksum_url", "encryption_key"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("%s leaked %q: %s", testCase.name, forbidden, encoded)
			}
		}
	}
}

func TestExecutionSessionHistoryRedactsBeforeTailSampling(t *testing.T) {
	ports := &executionPlatformFakePorts{sessionOutput: []byte(strings.Repeat("x", 100) + "\nAPI_KEY=read-secret-marker")}
	definitions := executionPlatformDefinitionsForTest()
	_, registry := executionPlatformTestRegistry(t, ports)
	result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: "sessions.history", Actor: executionPlatformActor(definitions),
		Target: json.RawMessage(`{"id":"s1"}`), Payload: json.RawMessage(`{"max_bytes":8}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.Result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "marker") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("tail sampling exposed a credential suffix: %s", encoded)
	}
}

func TestExecutionPlatformRejectsUnboundedInputsDuringDryRun(t *testing.T) {
	definitions := executionPlatformDefinitionsForTest()
	_, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	actor := executionPlatformActor(definitions)
	cases := []executionDryRunCase{
		{name: "sessions.send_input", target: `{"id":"s1"}`, payload: `{"text":"` + strings.Repeat("x", (16<<10)+1) + `"}`},
		{name: "sessions.history", target: `{"id":"s1"}`, payload: `{"max_bytes":65537}`},
		{name: "files.read", target: `{"type":"project","id":1}`, payload: `{"path":"../secret","max_bytes":1}`},
		{name: "git.diff", target: `{"project_id":1}`, payload: `{"max_bytes":262145}`},
		{name: "voice.transcribe", target: `{}`, payload: `{"audio_base64":"not-base64","filename":"recording.webm"}`},
		{name: "update.apply", target: `{}`, payload: `{"force":true}`},
	}
	for _, testCase := range cases {
		if _, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
			Capability: application.CapabilityName(testCase.name), Actor: actor, DryRun: true,
			Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
		}); err == nil {
			t.Errorf("%s accepted unbounded or incomplete input", testCase.name)
		}
	}
}

type executionUpdateManager struct {
	status      *updater.UpdateStatus
	checkCalls  int
	applyCalls  int
	packageName string
}

func (m *executionUpdateManager) DetectPackageManager() string { return m.packageName }
func (m *executionUpdateManager) CheckForUpdate(context.Context) (*updater.UpdateStatus, error) {
	m.checkCalls++
	return m.status, nil
}
func (m *executionUpdateManager) DownloadAndApply(context.Context, *updater.UpdateStatus) error {
	m.applyCalls++
	return nil
}

type executionActiveSessions struct{ count int }

func (s executionActiveSessions) ActiveSessionCount() int { return s.count }

func executionUpdateRegistry(t *testing.T, manager *executionUpdateManager, active int) *PlatformCapabilityRegistry {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	definition := updateExecutionPlatformDefinitions()[2]
	executor := &updateExecutionPlatformExecutor{
		service: application.NewUpdateMutationService(manager, executionActiveSessions{count: active}, nil),
		reader:  &executionPlatformFakePorts{},
	}
	if err := registry.Register(definition, executor); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestExecutionPlatformUpdateProtectsActiveSessionsAndSupportsAcknowledgedForce(t *testing.T) {
	approval, err := NewValidatedPlatformApproval("presidente")
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{Type: "automation_client", ID: "helena", Scopes: ScopeSet{"update:execute": {}}}
	manager := &executionUpdateManager{status: &updater.UpdateStatus{Available: true, LatestVersion: "2.0.0"}}
	registry := executionUpdateRegistry(t, manager, 2)
	request := PlatformDispatchRequest{
		Capability: "update.apply", Actor: actor, Reason: "apply approved update", Approval: approval,
		Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{}`),
	}
	request.Payload = json.RawMessage(`{"force":true}`)
	if _, err = DispatchPlatformCapability(context.Background(), registry, request); err == nil {
		t.Fatal("force update was allowed without literal active-session acknowledgement")
	}
	if manager.checkCalls != 0 || manager.applyCalls != 0 {
		t.Fatalf("unacknowledged force update reached manager: check=%d apply=%d", manager.checkCalls, manager.applyCalls)
	}

	request.Payload = json.RawMessage(`{}`)
	if _, err = DispatchPlatformCapability(context.Background(), registry, request); err == nil {
		t.Fatal("normal update was allowed while sessions were active")
	}
	if manager.checkCalls != 0 || manager.applyCalls != 0 {
		t.Fatalf("blocked update reached manager: check=%d apply=%d", manager.checkCalls, manager.applyCalls)
	}

	request.Payload = json.RawMessage(`{"force":true,"acknowledge_active_sessions":true}`)
	result, err := DispatchPlatformCapability(context.Background(), registry, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" || manager.checkCalls != 1 || manager.applyCalls != 1 {
		t.Fatalf("acknowledged force update did not execute once: result=%#v check=%d apply=%d", result, manager.checkCalls, manager.applyCalls)
	}
}

func TestExecutionPlatformForceUpdateDryRunIsRestartSafe(t *testing.T) {
	manager := &executionUpdateManager{status: &updater.UpdateStatus{Available: true, LatestVersion: "2.0.0"}}
	registry := executionUpdateRegistry(t, manager, 3)
	actor := Actor{Type: "automation_client", ID: "helena", Scopes: ScopeSet{"update:execute": {}}}
	result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: "update.apply", Actor: actor, DryRun: true, Target: json.RawMessage(`{}`),
		Payload: json.RawMessage(`{"force":true,"acknowledge_active_sessions":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "dry_run" || manager.checkCalls != 0 || manager.applyCalls != 0 {
		t.Fatalf("force dry-run crossed restart boundary: result=%#v check=%d apply=%d", result, manager.checkCalls, manager.applyCalls)
	}
}

func TestExecutionSessionViewOmitsPlanAndProviderIdentifiers(t *testing.T) {
	view := sessionAutomationView(database.Session{
		ID: "s1", ProjectID: 1, Status: "running", Name: "work",
		TaskID: sql.NullInt64{Int64: 7, Valid: true}, PlanContent: "plan-secret",
		ProviderSessionID: "provider-secret", StartTime: time.Now(),
	}, &executionPlatformFakePorts{activeSessions: map[string]bool{"s1": true}})
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"plan-secret", "provider-secret", "plan_content", "provider_session_id"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("session view leaked %q: %s", forbidden, encoded)
		}
	}
}
