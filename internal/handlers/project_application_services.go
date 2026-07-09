package handlers

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"path/filepath"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/session"
)

type projectOperationAdapter struct {
	api *API
}

func NewProjectOperationAdapter(api *API) application.ProjectOperationPort {
	return projectOperationAdapter{api: api}
}

func (a projectOperationAdapter) ValidateRemoteProject(ctx context.Context, project *database.Project) error {
	if a.api == nil || a.api.encryptor == nil {
		return errors.New("project validator is unavailable")
	}
	if project == nil || project.Type != "remote" {
		return errors.New("remote project is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return session.ValidateConnection(project, a.api.encryptor.Decrypt)
}

func (a projectOperationAdapter) BrowseRemoteDirectory(ctx context.Context, connection application.RemoteBrowseConnection) (application.RemoteDirectoryResult, error) {
	if a.api == nil || a.api.encryptor == nil {
		return application.RemoteDirectoryResult{}, errors.New("remote browser is unavailable")
	}
	select {
	case <-ctx.Done():
		return application.RemoteDirectoryResult{}, ctx.Err()
	default:
	}

	project := &database.Project{
		Path:        "/",
		Type:        "remote",
		SSHHost:     sql.NullString{String: connection.Host, Valid: true},
		SSHPort:     sql.NullInt64{Int64: int64(connection.Port), Valid: true},
		SSHUser:     sql.NullString{String: connection.User, Valid: true},
		SSHAuthType: sql.NullString{String: connection.AuthType, Valid: connection.AuthType != "default_keys"},
	}
	if connection.Credential != "" {
		encrypted, iv, err := a.api.encryptor.Encrypt(connection.Credential)
		if err != nil {
			return application.RemoteDirectoryResult{}, errors.New("failed to protect ephemeral SSH credential")
		}
		project.SSHCredentialEncrypted = sql.NullString{String: encrypted, Valid: true}
		project.SSHCredentialIV = sql.NullString{String: iv, Valid: true}
	}

	manager := files.NewRemoteFileManager(project, a.api.DecryptFunc())
	home, _ := manager.HomeDir()
	hostIsWindows := looksLikeSFTPWindowsPath(home)
	browsePath := connection.Path
	if browsePath == "" {
		browsePath = home
		if browsePath == "" {
			browsePath = "/"
		}
	}
	sftpPath := browsePath
	if hostIsWindows {
		sftpPath = windowsPathToSFTP(browsePath)
	}

	fileList, err := manager.List(sftpPath)
	if err != nil {
		return application.RemoteDirectoryResult{}, err
	}
	current := browsePath
	if hostIsWindows {
		current = sftpPathToWindows(sftpPath)
	}
	entries := make([]application.RemoteDirectoryEntry, 0, len(fileList))
	for _, file := range fileList {
		if !file.IsDir {
			continue
		}
		entryPath := filepath.Join(browsePath, file.Name)
		if hostIsWindows {
			entryPath = sftpPathToWindows(path.Join(sftpPath, file.Name))
		}
		entries = append(entries, application.RemoteDirectoryEntry{
			Name: file.Name, Path: entryPath, IsDir: true,
		})
	}
	return application.RemoteDirectoryResult{Current: current, Entries: entries}, nil
}

func (a *API) PublishProjectOperation(_ context.Context, change application.ProjectOperationChange) {
	if a == nil || a.hub == nil {
		return
	}
	a.hub.BroadcastStateUpdate("project_operation", map[string]interface{}{
		"action": change.Action, "project_id": change.ProjectID, "entry_count": change.EntryCount,
	})
}

var (
	_ application.ProjectOperationStore   = (*database.DB)(nil)
	_ application.ProjectOperationPort    = projectOperationAdapter{}
	_ application.ProjectOperationEffects = (*API)(nil)
)
