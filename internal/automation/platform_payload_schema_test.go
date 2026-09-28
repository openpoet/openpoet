package automation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func dispatchExecutionDryRun(t *testing.T, registry *PlatformCapabilityRegistry, actor Actor, capability, target, payload string) (PlatformDispatchResult, *PlatformDispatchError) {
	t.Helper()
	result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
		Capability: application.CapabilityName(capability), Actor: actor, DryRun: true,
		Target: json.RawMessage(target), Payload: json.RawMessage(payload),
	})
	if err == nil {
		return result, nil
	}
	var dispatchErr *PlatformDispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("%s returned untyped error %T %v", capability, err, err)
	}
	return result, dispatchErr
}

func TestPlatformPayloadErrorsNameTheFailingField(t *testing.T) {
	_, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	actor := executionPlatformActor(executionPlatformDefinitionsForTest())
	session := `{"type":"session","id":"5ee5b896-82af-46b8-8ad3-c75c20bd694f"}`
	cases := []struct {
		name, capability, payload string
		want                      []string
	}{
		{"host path on paste", "files.paste_session_image", `{"path":"/workspace/mylifeos/channels/_attachments/a.jpeg"}`,
			[]string{`"path" is not accepted`, `source {"project_id"`, "accepted fields:", "data_url (string)", "source (object)"}},
		{"file_path on paste", "files.paste_session_image", `{"file_path":"channels/_attachments/a.jpeg"}`,
			[]string{`"file_path" is not accepted`, "data_url"}},
		{"paste without image", "files.paste_session_image", `{}`, []string{`"data_url" is required`, "source"}},
		{"paste with both sources", "files.paste_session_image", `{"data_url":"data:image/png;base64,aQ==","source":{"project_id":1,"path":"a.png"}}`,
			[]string{"exactly one of data_url or source"}},
		{"paste source host path", "files.paste_session_image", `{"source":{"project_id":47,"path":"/workspace/mylifeos/a.jpeg"}}`,
			[]string{`"source.path" must be a path relative to the project root`}},
		{"session_id in hint payload", "sessions.image_prompt_hint", `{"session_id":"5ee5b896"}`,
			[]string{`"session_id" is not accepted`, `identify the session in the target`, "image_count (integer, required)"}},
		{"hint without count", "sessions.image_prompt_hint", `{}`, []string{`"image_count" is required`, "1 to 20"}},
		{"hint wrong type", "sessions.image_prompt_hint", `{"image_count":"one"}`, []string{`"image_count" must be integer (got JSON string)`}},
		{"payload on payloadless capability", "sessions.get", `{"session_id":"x","verbose":true}`,
			[]string{"accepts no payload fields", `"session_id"`, `"verbose"`, "identify the session in the target"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := dispatchExecutionDryRun(t, registry, actor, testCase.capability, session, testCase.payload)
			if err == nil {
				t.Fatal("invalid payload was accepted")
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Message, want) {
					t.Errorf("error %q does not contain %q", err.Message, want)
				}
			}
		})
	}
}

type fakeSessionIDResolver struct{ ids []string }

func (f fakeSessionIDResolver) MatchSessionIDs(_ context.Context, value string, limit int) ([]string, error) {
	var matches []string
	for _, id := range f.ids {
		if id == value {
			return []string{id}, nil
		}
	}
	for _, id := range f.ids {
		if strings.HasPrefix(id, value) && len(matches) < limit {
			matches = append(matches, id)
		}
	}
	return matches, nil
}

func TestPlatformSessionTargetAcceptsPrefixAndSessionIDAlias(t *testing.T) {
	_, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	const full = "5ee5b896-82af-46b8-8ad3-c75c20bd694f"
	registry.SetSessionIDResolver(fakeSessionIDResolver{ids: []string{full, "abcdef12-0001", "abcdef13-0002"}})
	actor := executionPlatformActor(executionPlatformDefinitionsForTest())
	payload := `{"image_count":1}`
	for _, target := range []string{
		`{"type":"session","id":"5ee5b896"}`,
		`{"type":"session","session_id":"5ee5b896"}`,
		`{"session_id":"5ee5b896"}`,
		`{"type":"session","id":"` + full + `"}`,
	} {
		result, err := dispatchExecutionDryRun(t, registry, actor, "sessions.image_prompt_hint", target, payload)
		if err != nil {
			t.Fatalf("target %s rejected: %v", target, err)
		}
		if got := result.Result.(map[string]any)["session_id"]; got != full {
			t.Errorf("target %s resolved to %v, want %s", target, got, full)
		}
	}
	failures := map[string]string{
		`{"type":"session","id":"abcdef1"}`:                          "platform_target_ambiguous",
		`{"type":"session","id":"5ee5"}`:                             "platform_target_invalid",
		`{"type":"session","id":"5ee5b896","session_id":"abcdef12"}`: "platform_target_invalid",
		`{"type":"session"}`:                                         "platform_target_invalid",
	}
	for target, code := range failures {
		if _, err := dispatchExecutionDryRun(t, registry, actor, "sessions.image_prompt_hint", target, payload); err == nil || err.Code != code {
			t.Errorf("target %s: err=%v, want code %s", target, err, code)
		}
	}
	// Unknown ids pass through so the owning service reports not-found.
	result, err := dispatchExecutionDryRun(t, registry, actor, "sessions.image_prompt_hint", `{"type":"session","id":"ffffffff"}`, payload)
	if err != nil || result.Result.(map[string]any)["session_id"] != "ffffffff" {
		t.Fatalf("unknown id: result=%v err=%v", result.Result, err)
	}
}

type pasteSessionStore struct {
	application.SessionStore
	session *database.Session
	project *database.Project
}

func (s pasteSessionStore) GetSession(context.Context, string) (*database.Session, error) {
	return s.session, nil
}

func (s pasteSessionStore) GetProject(context.Context, int64) (*database.Project, error) {
	return s.project, nil
}

type pasteFileWriter struct {
	project *database.Project
	writes  []application.FileWrite
}

func (w *pasteFileWriter) WriteFiles(_ context.Context, project *database.Project, writes []application.FileWrite) error {
	w.project, w.writes = project, append(w.writes, writes...)
	return nil
}

type pasteSourceReader struct {
	*executionPlatformFakePorts
	scope    OperationalFileScope
	path     string
	maxBytes int
}

func (r *pasteSourceReader) ReadOperationalFile(_ context.Context, scope OperationalFileScope, path string, maxBytes int) (OperationalFileReadResult, error) {
	r.scope, r.path, r.maxBytes = scope, path, maxBytes
	return r.fileRead, nil
}

func TestPasteSessionImageReadsSourceFromAnotherProject(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	ports := &executionPlatformFakePorts{fileRead: OperationalFileReadResult{
		Metadata: FileMetadataAutomationView{Path: "channels/_attachments/photo.png", Size: int64(len(png))}, Data: png,
	}}
	reader := &pasteSourceReader{executionPlatformFakePorts: ports}
	writer := &pasteFileWriter{}
	sessionProject := &database.Project{ID: 12, Name: "home server", Path: "/home/dev", Type: "local"}
	services := executionPlatformTestServices(ports)
	services.Files = reader
	services.FileMutations = application.NewFileMutationService(pasteSessionStore{
		session: &database.Session{ID: "sess-full-id", ProjectID: 12}, project: sessionProject,
	}, writer, nil)
	registry, err := NewPlatformCapabilityRegistry(application.NewCapabilityRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterExecutionPlatformCapabilities(registry, services); err != nil {
		t.Fatal(err)
	}
	approval, err := NewValidatedPlatformApproval("presidente")
	if err != nil {
		t.Fatal(err)
	}
	actor := executionPlatformActor(executionPlatformDefinitionsForTest())
	actor.Scopes[ScopeFilesRead] = struct{}{}
	request := PlatformDispatchRequest{
		Capability: "files.paste_session_image", Actor: actor, Approval: approval, Reason: "paste attachment",
		Target:  json.RawMessage(`{"type":"session","id":"sess-full-id"}`),
		Payload: json.RawMessage(`{"directory":"inbox","source":{"project_id":47,"path":"channels/_attachments/photo.png"}}`),
	}
	result, err := DispatchPlatformCapability(context.Background(), registry, request)
	if err != nil {
		t.Fatal(err)
	}
	if reader.scope.ProjectID != 47 || reader.path != "channels/_attachments/photo.png" || reader.maxBytes != maxAutomationPasteImage {
		t.Fatalf("source read scope=%+v path=%q max=%d", reader.scope, reader.path, reader.maxBytes)
	}
	if writer.project != sessionProject || len(writer.writes) != 1 || writer.writes[0].Path != "inbox/photo.png" || string(writer.writes[0].Data) != string(png) {
		t.Fatalf("unexpected write to %+v: %+v", writer.project, writer.writes)
	}
	if got := result.Result.(map[string]any)["path"]; got != "inbox/photo.png" {
		t.Fatalf("result path=%v", got)
	}

	// The inline data_url form keeps working.
	request.Payload = json.RawMessage(`{"filename":"inline.png","data_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(png) + `"}`)
	if _, err := DispatchPlatformCapability(context.Background(), registry, request); err != nil || writer.writes[1].Path != "inline.png" {
		t.Fatalf("inline paste: err=%v writes=%+v", err, writer.writes)
	}

	// Non-images are refused after the read.
	ports.fileRead.Data = []byte("just text, not an image")
	request.Payload = json.RawMessage(`{"source":{"project_id":47,"path":"notes.txt"}}`)
	if _, err := DispatchPlatformCapability(context.Background(), registry, request); err == nil || !strings.Contains(err.Error(), "not a PNG, JPEG, GIF, or WebP") {
		t.Fatalf("non-image source: %v", err)
	}

	// Reading a source needs files:read and a source project inside the client's filter.
	delete(actor.Scopes, ScopeFilesRead)
	request.Actor = actor
	if _, err := DispatchPlatformCapability(context.Background(), registry, request); err == nil || !strings.Contains(err.Error(), "files:read") {
		t.Fatalf("source without files:read: %v", err)
	}
	if _, err := validateSessionImageSource(sessionImageSource{ProjectID: 47, Path: "a.png"}, PlatformExecutionInput{
		ActorScopes: ScopeSet{ScopeFilesRead: {}}, ProjectScope: &ProjectScopeSet{Allowed: map[int64]bool{12: true}},
	}); err == nil || !strings.Contains(err.Error(), "source project") {
		t.Fatalf("out-of-scope source project: %v", err)
	}
}

func TestDiscoveryPublishesPayloadSchemas(t *testing.T) {
	definitions := executionPlatformDefinitionsForTest()
	_, registry := executionPlatformTestRegistry(t, &executionPlatformFakePorts{})
	descriptors := map[application.CapabilityName]PlatformCapabilityDescriptor{}
	for _, descriptor := range registry.ListForActor(executionPlatformActor(definitions)) {
		descriptors[descriptor.Name] = descriptor
	}
	fields := func(name application.CapabilityName) map[string]PlatformPayloadField {
		descriptor := descriptors[name]
		if descriptor.Payload == nil {
			t.Fatalf("%s publishes no payload schema", name)
		}
		byName := map[string]PlatformPayloadField{}
		for _, field := range descriptor.Payload.Fields {
			byName[field.Name] = field
		}
		return byName
	}
	paste := fields("files.paste_session_image")
	if paste["data_url"].Type != "string" || paste["source"].Type != "object" || len(paste["source"].Fields) != 2 ||
		!paste["source"].Fields[0].Required || paste["source"].Fields[1].Name != "path" || paste["data_url"].Description == "" {
		t.Fatalf("paste schema = %+v", paste)
	}
	if descriptors["files.paste_session_image"].Payload.Target != sessionTargetDescription ||
		len(descriptors["files.paste_session_image"].Payload.Example) == 0 || descriptors["files.paste_session_image"].MaxPayloadBytes != 16<<20 {
		t.Fatalf("paste descriptor = %+v", descriptors["files.paste_session_image"])
	}
	hint := fields("sessions.image_prompt_hint")
	if !hint["image_count"].Required || hint["user_prompt"].Required {
		t.Fatalf("hint schema = %+v", hint)
	}
	if got := fields("sessions.get"); len(got) != 0 {
		t.Fatalf("sessions.get should declare an empty payload, got %+v", got)
	}
	// Every session and file capability documents its contract.
	for _, group := range [][]PlatformCapabilityDefinition{
		sessionPlatformDefinitions(), sessionWatcherPlatformDefinitions(), sessionSuggestionPlatformDefinitions(), fileExecutionPlatformDefinitions(),
	} {
		for _, definition := range group {
			if definition.Payload == nil || definition.Payload.Target == "" {
				t.Errorf("%s has no payload schema", definition.Name)
			}
		}
	}
}
