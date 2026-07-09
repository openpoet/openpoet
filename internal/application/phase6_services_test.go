package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"openpoet/internal/database"
)

func phase6Actor() ActionAuthorization {
	return ActionAuthorization{Actor: Actor{Type: "agent", ID: "helena"}}
}

func phase6Approval() ActionAuthorization {
	return ActionAuthorization{
		Actor: Actor{Type: "agent", ID: "helena"}, Approved: true,
		ApprovedBy: "presidente", Reason: "approved remote project access",
	}
}

type phase6ProjectStore struct {
	project *database.Project
	err     error
	calls   int
}

func (s *phase6ProjectStore) GetProject(context.Context, int64) (*database.Project, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.project == nil {
		return nil, sql.ErrNoRows
	}
	copy := *s.project
	return &copy, nil
}

type phase6ProjectPort struct {
	validateCalls int
	browseCalls   int
	connection    RemoteBrowseConnection
	result        RemoteDirectoryResult
	validateErr   error
	browseErr     error
}

func (p *phase6ProjectPort) ValidateRemoteProject(context.Context, *database.Project) error {
	p.validateCalls++
	return p.validateErr
}

func (p *phase6ProjectPort) BrowseRemoteDirectory(_ context.Context, connection RemoteBrowseConnection) (RemoteDirectoryResult, error) {
	p.browseCalls++
	p.connection = connection
	return p.result, p.browseErr
}

type phase6ProjectEffects struct{ changes []ProjectOperationChange }

func (e *phase6ProjectEffects) PublishProjectOperation(_ context.Context, change ProjectOperationChange) {
	e.changes = append(e.changes, change)
}

func TestProjectValidationRequiresActorAndAuditsSuccess(t *testing.T) {
	store := &phase6ProjectStore{project: &database.Project{ID: 7, Type: "local"}}
	port := &phase6ProjectPort{}
	effects := &phase6ProjectEffects{}
	service := NewProjectOperationService(store, port, effects)

	if _, err := service.Validate(context.Background(), ValidateProjectCommand{ProjectID: 7}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("missing actor must be rejected, got %v", err)
	}
	if store.calls != 0 || port.validateCalls != 0 || len(effects.changes) != 0 {
		t.Fatalf("unauthorized validation crossed boundary: store=%d port=%d effects=%d", store.calls, port.validateCalls, len(effects.changes))
	}

	result, err := service.Validate(context.Background(), ValidateProjectCommand{ProjectID: 7, Authorization: phase6Actor()})
	if err != nil || result.Status != "ok" || result.ProjectID != 7 || result.Type != "local" {
		t.Fatalf("unexpected local validation: %#v err=%v", result, err)
	}
	if port.validateCalls != 0 {
		t.Fatalf("local validation must not open a remote connection: %d", port.validateCalls)
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "validated" || effects.changes[0].Actor.ID != "helena" {
		t.Fatalf("validation audit missing: %#v", effects.changes)
	}
}

func TestRemoteProjectValidationPublishesOnlyAfterConnectionSuccess(t *testing.T) {
	connectionErr := errors.New("SSH connection failed")
	store := &phase6ProjectStore{project: &database.Project{ID: 7, Type: "remote"}}
	port := &phase6ProjectPort{validateErr: connectionErr}
	effects := &phase6ProjectEffects{}
	service := NewProjectOperationService(store, port, effects)

	if _, err := service.Validate(context.Background(), ValidateProjectCommand{ProjectID: 7, Authorization: phase6Actor()}); !errors.Is(err, connectionErr) {
		t.Fatalf("expected connection failure, got %v", err)
	}
	if len(effects.changes) != 0 {
		t.Fatalf("failed validation published audit effect: %#v", effects.changes)
	}
	port.validateErr = nil
	if _, err := service.Validate(context.Background(), ValidateProjectCommand{ProjectID: 7, Authorization: phase6Actor()}); err != nil {
		t.Fatal(err)
	}
	if port.validateCalls != 2 || len(effects.changes) != 1 {
		t.Fatalf("successful connection was not audited once: calls=%d effects=%v", port.validateCalls, effects.changes)
	}
}

func TestRemoteBrowseRequiresExplicitApprovalBeforeCredentialUse(t *testing.T) {
	port := &phase6ProjectPort{}
	service := NewProjectOperationService(nil, port, nil)
	command := BrowseRemoteProjectCommand{
		Connection: RemoteBrowseConnection{
			Host: "server.example", User: "deploy", AuthType: "password", Credential: "secret",
		},
		Authorization: phase6Actor(),
	}
	if _, err := service.BrowseRemote(context.Background(), command); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("remote browse without explicit approval must fail, got %v", err)
	}
	if port.browseCalls != 0 {
		t.Fatalf("credential reached remote adapter without approval: %d", port.browseCalls)
	}
}

func TestRemoteBrowseValidatesBoundedConnectionBeforeAdapter(t *testing.T) {
	port := &phase6ProjectPort{}
	service := NewProjectOperationService(nil, port, nil)
	base := RemoteBrowseConnection{Host: "server.example", User: "deploy", AuthType: "password", Credential: "secret"}
	cases := []RemoteBrowseConnection{
		{Host: "https://server.example", User: base.User, AuthType: base.AuthType, Credential: base.Credential},
		{Host: base.Host, Port: 70000, User: base.User, AuthType: base.AuthType, Credential: base.Credential},
		{Host: base.Host, User: "", AuthType: base.AuthType, Credential: base.Credential},
		{Host: base.Host, User: base.User, AuthType: "password"},
		{Host: base.Host, User: base.User, AuthType: "unknown", Credential: base.Credential},
		{Host: base.Host, User: base.User, AuthType: base.AuthType, Credential: strings.Repeat("x", maxRemoteBrowseCredentialBytes+1)},
		{Host: base.Host, User: base.User, AuthType: base.AuthType, Credential: base.Credential, Path: "bad\x00path"},
	}
	for i, connection := range cases {
		if _, err := service.BrowseRemote(context.Background(), BrowseRemoteProjectCommand{
			Connection: connection, Authorization: phase6Approval(),
		}); !ErrorIsKind(err, ErrorValidation) {
			t.Fatalf("case %d must fail validation, got %v", i, err)
		}
	}
	if port.browseCalls != 0 {
		t.Fatalf("invalid remote input reached adapter %d times", port.browseCalls)
	}
}

func TestRemoteBrowseRedactsCredentialAndBoundsDirectoryResult(t *testing.T) {
	entries := make([]RemoteDirectoryEntry, 0, maxRemoteBrowseEntries+2)
	entries = append(entries, RemoteDirectoryEntry{Name: "file.txt", Path: "/home/file.txt", IsDir: false})
	for i := 0; i < maxRemoteBrowseEntries+1; i++ {
		entries = append(entries, RemoteDirectoryEntry{
			Name: fmt.Sprintf("dir-%04d", i), Path: fmt.Sprintf("/home/dir-%04d", i), IsDir: true,
		})
	}
	port := &phase6ProjectPort{result: RemoteDirectoryResult{Current: "/home", Entries: entries}}
	effects := &phase6ProjectEffects{}
	service := NewProjectOperationService(nil, port, effects)
	credential := "ephemeral-password"

	result, err := service.BrowseRemote(context.Background(), BrowseRemoteProjectCommand{
		Connection: RemoteBrowseConnection{
			Host: "server.example", User: "deploy", AuthType: "password", Credential: credential, Path: "/home",
		},
		Authorization: phase6Approval(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if port.connection.Port != 22 || port.connection.Credential != credential {
		t.Fatalf("normalized adapter input is wrong: %#v", port.connection)
	}
	if result.Current != "/home" || len(result.Entries) != maxRemoteBrowseEntries {
		t.Fatalf("remote directory result was not bounded: current=%q entries=%d", result.Current, len(result.Entries))
	}
	if result.Entries[0].Name != "dir-0000" {
		t.Fatalf("non-directory entry was not removed: %#v", result.Entries[0])
	}
	if len(effects.changes) != 1 || effects.changes[0].Action != "remote_browsed" || effects.changes[0].EntryCount != maxRemoteBrowseEntries {
		t.Fatalf("browse audit is incomplete: %#v", effects.changes)
	}
	// Neither result nor audit types have a credential field; this assertion
	// additionally catches accidental plaintext inclusion in printable values.
	if strings.Contains(fmt.Sprintf("%#v %#v", result, effects.changes), credential) {
		t.Fatal("ephemeral credential leaked into result or audit effect")
	}
}

func TestRemoteBrowseFailureHasNoAuditEffect(t *testing.T) {
	browseErr := errors.New("SFTP permission denied")
	port := &phase6ProjectPort{browseErr: browseErr}
	effects := &phase6ProjectEffects{}
	service := NewProjectOperationService(nil, port, effects)
	_, err := service.BrowseRemote(context.Background(), BrowseRemoteProjectCommand{
		Connection: RemoteBrowseConnection{
			Host: "192.0.2.10", User: "deploy", AuthType: "key", Credential: "private-key", Path: "/srv",
		},
		Authorization: phase6Approval(),
	})
	if !errors.Is(err, browseErr) {
		t.Fatalf("expected browse failure, got %v", err)
	}
	if len(effects.changes) != 0 {
		t.Fatalf("failed browse published audit effect: %#v", effects.changes)
	}
}
