package application

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/database"
)

const (
	maxRemoteBrowseCredentialBytes = 64 << 10
	maxRemoteBrowseEntries         = 1000
	maxRemotePathRunes             = 4096
)

var remoteHostnamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

type ProjectOperationStore interface {
	GetProject(context.Context, int64) (*database.Project, error)
}

type RemoteBrowseConnection struct {
	Host       string
	Port       int
	User       string
	AuthType   string
	Credential string
	Path       string
	// ProjectID lets the browser reuse a saved project's stored credential.
	// The edit form never re-displays a password or private key, so without
	// this a saved project could never be browsed.
	ProjectID int64
	// StoredCredential is set by the service (never by the caller) when the
	// connection must fall back to the saved project's credential.
	StoredCredential bool
}

type RemoteDirectoryEntry struct {
	Name  string
	Path  string
	IsDir bool
}

type RemoteDirectoryResult struct {
	Current string
	Entries []RemoteDirectoryEntry
}

type ProjectOperationPort interface {
	ValidateRemoteProject(context.Context, *database.Project) error
	BrowseRemoteDirectory(context.Context, RemoteBrowseConnection) (RemoteDirectoryResult, error)
}

type ProjectOperationChange struct {
	Action     string
	ProjectID  int64
	EntryCount int
	Actor      Actor
}

type ProjectOperationEffects interface {
	PublishProjectOperation(context.Context, ProjectOperationChange)
}

type ProjectOperationService struct {
	store   ProjectOperationStore
	port    ProjectOperationPort
	effects ProjectOperationEffects
}

func NewProjectOperationService(store ProjectOperationStore, port ProjectOperationPort, effects ProjectOperationEffects) *ProjectOperationService {
	return &ProjectOperationService{store: store, port: port, effects: effects}
}

type ValidateProjectCommand struct {
	ProjectID     int64
	Authorization ActionAuthorization
}

type ProjectValidationResult struct {
	ProjectID int64
	Status    string
	Type      string
}

func (s *ProjectOperationService) Validate(ctx context.Context, command ValidateProjectCommand) (ProjectValidationResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ProjectValidationResult{}, err
	}
	if command.ProjectID <= 0 {
		return ProjectValidationResult{}, validationError("invalid_project_id", "Project ID must be positive")
	}
	if s.store == nil {
		return ProjectValidationResult{}, validationError("project_store_unavailable", "Project store is unavailable")
	}
	project, err := s.store.GetProject(ctx, command.ProjectID)
	if err != nil || project == nil {
		if errors.Is(err, sql.ErrNoRows) || project == nil {
			return ProjectValidationResult{}, notFoundError("project_not_found", "Project not found", err)
		}
		return ProjectValidationResult{}, err
	}
	if project.Type != "local" && project.Type != "remote" {
		return ProjectValidationResult{}, validationError("project_type_invalid", "Project type must be local or remote")
	}
	if project.Type == "remote" {
		if s.port == nil {
			return ProjectValidationResult{}, validationError("project_validator_unavailable", "Remote project validator is unavailable")
		}
		if err := s.port.ValidateRemoteProject(ctx, project); err != nil {
			return ProjectValidationResult{}, err
		}
	}
	result := ProjectValidationResult{ProjectID: project.ID, Status: "ok", Type: project.Type}
	s.publish(ctx, ProjectOperationChange{
		Action: "validated", ProjectID: project.ID, Actor: command.Authorization.Actor,
	})
	return result, nil
}

type BrowseRemoteProjectCommand struct {
	Connection    RemoteBrowseConnection
	Authorization ActionAuthorization
}

func (s *ProjectOperationService) BrowseRemote(ctx context.Context, command BrowseRemoteProjectCommand) (RemoteDirectoryResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return RemoteDirectoryResult{}, err
	}
	request, err := s.resolveStoredBrowseCredential(ctx, command.Connection)
	if err != nil {
		return RemoteDirectoryResult{}, err
	}
	connection, err := normalizeRemoteBrowseConnection(request)
	if err != nil {
		return RemoteDirectoryResult{}, err
	}
	if s.port == nil {
		return RemoteDirectoryResult{}, validationError("remote_browser_unavailable", "Remote project browser is unavailable")
	}
	result, err := s.port.BrowseRemoteDirectory(ctx, connection)
	if err != nil {
		return RemoteDirectoryResult{}, err
	}
	result, err = normalizeRemoteDirectoryResult(result)
	if err != nil {
		return RemoteDirectoryResult{}, err
	}
	s.publish(ctx, ProjectOperationChange{
		Action: "remote_browsed", EntryCount: len(result.Entries), Actor: command.Authorization.Actor,
	})
	return result, nil
}

// resolveStoredBrowseCredential decides whether this browse may run on the
// credential already stored for the project being edited. The edit form shows
// only "has_credential", never the secret itself, so a saved password or
// private key would otherwise be un-browsable. The fallback is deliberately
// narrow: the saved project must be remote and must still point at the same
// host, port, user and auth type being browsed, so a stored key is never
// offered to a host the user just retyped.
func (s *ProjectOperationService) resolveStoredBrowseCredential(ctx context.Context, connection RemoteBrowseConnection) (RemoteBrowseConnection, error) {
	connection.StoredCredential = false
	if connection.Credential != "" || connection.ProjectID <= 0 {
		return connection, nil
	}
	switch strings.TrimSpace(connection.AuthType) {
	case "password", "key", "key_passphrase":
	default:
		return connection, nil
	}
	if s.store == nil {
		return connection, nil
	}
	project, err := s.store.GetProject(ctx, connection.ProjectID)
	if err != nil || project == nil {
		if errors.Is(err, sql.ErrNoRows) || project == nil {
			return RemoteBrowseConnection{}, notFoundError("project_not_found", "Project not found", err)
		}
		return RemoteBrowseConnection{}, err
	}
	if project.Type != "remote" || !project.SSHCredentialEncrypted.Valid {
		return connection, nil
	}
	port := int(project.SSHPort.Int64)
	if port == 0 {
		port = 22
	}
	requestedPort := connection.Port
	if requestedPort == 0 {
		requestedPort = 22
	}
	if strings.TrimSpace(project.SSHHost.String) != strings.TrimSpace(connection.Host) ||
		strings.TrimSpace(project.SSHUser.String) != strings.TrimSpace(connection.User) ||
		port != requestedPort ||
		strings.TrimSpace(project.SSHAuthType.String) != strings.TrimSpace(connection.AuthType) {
		return RemoteBrowseConnection{}, validationError("remote_credential_required",
			"Retype the credential to browse a host other than the one saved for this project")
	}
	connection.StoredCredential = true
	return connection, nil
}

func normalizeRemoteBrowseConnection(connection RemoteBrowseConnection) (RemoteBrowseConnection, error) {
	connection.Host = strings.TrimSpace(connection.Host)
	hostForIP := strings.Trim(connection.Host, "[]")
	parsedIP := net.ParseIP(hostForIP)
	if connection.Host == "" || len(connection.Host) > 253 || strings.Contains(hostForIP, ":") ||
		(parsedIP == nil && !remoteHostnamePattern.MatchString(connection.Host)) {
		return RemoteBrowseConnection{}, validationError("remote_host_invalid", "Remote host must be a bounded hostname or IPv4 address")
	}
	if connection.Port == 0 {
		connection.Port = 22
	}
	if connection.Port < 1 || connection.Port > 65535 {
		return RemoteBrowseConnection{}, validationError("remote_port_invalid", "Remote SSH port must be between 1 and 65535")
	}
	connection.User = strings.TrimSpace(connection.User)
	if connection.User == "" || utf8.RuneCountInString(connection.User) > 256 || hasControlRune(connection.User) {
		return RemoteBrowseConnection{}, validationError("remote_user_invalid", "Remote SSH user is required and must be bounded")
	}
	connection.AuthType = strings.TrimSpace(connection.AuthType)
	if connection.AuthType == "" {
		connection.AuthType = "default_keys"
	}
	switch connection.AuthType {
	case "default_keys":
		if connection.Credential != "" {
			return RemoteBrowseConnection{}, validationError("remote_credential_unexpected", "Default-key authentication must not include a credential")
		}
	case "password", "key", "key_passphrase":
		if connection.Credential == "" && !connection.StoredCredential {
			return RemoteBrowseConnection{}, validationError("remote_credential_required", "Selected authentication requires an ephemeral credential")
		}
	default:
		return RemoteBrowseConnection{}, validationError("remote_auth_type_invalid", "Remote authentication type is invalid")
	}
	if len([]byte(connection.Credential)) > maxRemoteBrowseCredentialBytes || strings.IndexByte(connection.Credential, 0) >= 0 {
		return RemoteBrowseConnection{}, validationError("remote_credential_invalid", "Remote credential exceeds 64 KiB or contains NUL")
	}
	connection.Path = strings.TrimSpace(connection.Path)
	if utf8.RuneCountInString(connection.Path) > maxRemotePathRunes || strings.IndexByte(connection.Path, 0) >= 0 || hasControlRune(connection.Path) {
		return RemoteBrowseConnection{}, validationError("remote_path_invalid", "Remote path is invalid or exceeds 4096 characters")
	}
	return connection, nil
}

func normalizeRemoteDirectoryResult(result RemoteDirectoryResult) (RemoteDirectoryResult, error) {
	result.Current = strings.TrimSpace(result.Current)
	if result.Current == "" || utf8.RuneCountInString(result.Current) > maxRemotePathRunes || hasControlRune(result.Current) {
		return RemoteDirectoryResult{}, validationError("remote_browse_result_invalid", "Remote browser returned an invalid current path")
	}
	entries := make([]RemoteDirectoryEntry, 0, min(len(result.Entries), maxRemoteBrowseEntries))
	for _, entry := range result.Entries {
		if !entry.IsDir {
			continue
		}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Path = strings.TrimSpace(entry.Path)
		if entry.Name == "" || utf8.RuneCountInString(entry.Name) > 255 || hasControlRune(entry.Name) ||
			entry.Path == "" || utf8.RuneCountInString(entry.Path) > maxRemotePathRunes || hasControlRune(entry.Path) {
			return RemoteDirectoryResult{}, validationError("remote_browse_entry_invalid", "Remote browser returned an invalid directory entry")
		}
		entries = append(entries, entry)
		if len(entries) == maxRemoteBrowseEntries {
			break
		}
	}
	result.Entries = entries
	return result, nil
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (s *ProjectOperationService) publish(ctx context.Context, change ProjectOperationChange) {
	if s.effects != nil {
		s.effects.PublishProjectOperation(ctx, change)
	}
}
