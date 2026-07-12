package application

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"openpoet/internal/database"
)

const (
	maxProjectTextBytes   = 2 << 20
	maxProjectUploadBytes = 10 << 20
	maxSessionUploadBytes = 100 << 20
	maxSessionUploadFiles = 100
	maxPasteImageBytes    = 10 << 20
)

type FileWrite struct {
	Path string
	Data []byte
}

type FileMutationPort interface {
	WriteFiles(context.Context, *database.Project, []FileWrite) error
}

type FileMutationChange struct {
	Action    string
	ProjectID int64
	SessionID string
	Paths     []string
	Actor     Actor
}

type FileMutationEffects interface {
	PublishFileMutation(context.Context, FileMutationChange)
}

type FileMutationService struct {
	store   SessionStore
	writer  FileMutationPort
	effects FileMutationEffects
	now     func() time.Time
}

func NewFileMutationService(store SessionStore, writer FileMutationPort, effects FileMutationEffects) *FileMutationService {
	return &FileMutationService{store: store, writer: writer, effects: effects, now: time.Now}
}

type WriteProjectFileCommand struct {
	ProjectID     int64
	Path          string
	Content       string
	Authorization ActionAuthorization
}

func (s *FileMutationService) WriteProjectFile(ctx context.Context, command WriteProjectFileCommand) (FileWrite, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return FileWrite{}, err
	}
	if len([]byte(command.Content)) > maxProjectTextBytes {
		return FileWrite{}, validationError("project_file_too_large", "Project text file exceeds 2 MiB")
	}
	return s.writeProject(ctx, command.ProjectID, "project_file_written", "", []FileWrite{{Path: command.Path, Data: []byte(command.Content)}}, maxProjectTextBytes, command.Authorization)
}

type UploadProjectFileCommand struct {
	ProjectID     int64
	Path          string
	Data          []byte
	Authorization ActionAuthorization
}

func (s *FileMutationService) UploadProjectFile(ctx context.Context, command UploadProjectFileCommand) (FileWrite, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return FileWrite{}, err
	}
	return s.writeProject(ctx, command.ProjectID, "project_file_uploaded", "", []FileWrite{{Path: command.Path, Data: command.Data}}, maxProjectUploadBytes, command.Authorization)
}

type UploadSessionFilesCommand struct {
	SessionID     string
	Directory     string
	Files         []FileWrite
	Authorization ActionAuthorization
}

func (s *FileMutationService) UploadSessionFiles(ctx context.Context, command UploadSessionFilesCommand) ([]FileWrite, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return nil, err
	}
	if len(command.Files) == 0 || len(command.Files) > maxSessionUploadFiles {
		return nil, validationError("session_files_count_invalid", "Session upload must contain between 1 and 100 files")
	}
	session, project, err := s.resolveSessionProject(ctx, command.SessionID)
	if err != nil {
		return nil, err
	}
	directory, err := normalizeOptionalRelativePath(command.Directory)
	if err != nil {
		return nil, err
	}
	writes := make([]FileWrite, len(command.Files))
	total := 0
	for i, file := range command.Files {
		name, pathErr := normalizeRelativePath(file.Path)
		if pathErr != nil {
			return nil, pathErr
		}
		if directory != "" {
			name = path.Join(directory, name)
		}
		total += len(file.Data)
		if total > maxSessionUploadBytes {
			return nil, validationError("session_files_too_large", "Session upload exceeds 100 MiB")
		}
		writes[i] = FileWrite{Path: name, Data: append([]byte(nil), file.Data...)}
	}
	if err := s.write(ctx, project, writes); err != nil {
		return nil, err
	}
	s.publish(ctx, FileMutationChange{
		Action: "session_files_uploaded", ProjectID: project.ID, SessionID: session.ID,
		Paths: fileWritePaths(writes), Actor: command.Authorization.Actor,
	})
	return writes, nil
}

type PasteSessionImageCommand struct {
	SessionID     string
	Directory     string
	Filename      string
	DataURL       string
	Authorization ActionAuthorization
}

func (s *FileMutationService) PasteSessionImage(ctx context.Context, command PasteSessionImageCommand) (FileWrite, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return FileWrite{}, err
	}
	session, project, err := s.resolveSessionProject(ctx, command.SessionID)
	if err != nil {
		return FileWrite{}, err
	}
	mimeType, encoded, ok := strings.Cut(strings.TrimSpace(command.DataURL), ",")
	if !ok || !strings.HasPrefix(mimeType, "data:image/") || !strings.Contains(mimeType, ";base64") {
		return FileWrite{}, validationError("image_data_invalid", "Image must be a base64 data URL")
	}
	extensions := map[string]string{
		"data:image/png;base64": ".png", "data:image/jpeg;base64": ".jpg",
		"data:image/gif;base64": ".gif", "data:image/webp;base64": ".webp",
	}
	extension, allowed := extensions[strings.ToLower(mimeType)]
	if !allowed {
		return FileWrite{}, validationError("image_type_invalid", "Image type must be PNG, JPEG, GIF, or WebP")
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxPasteImageBytes {
		return FileWrite{}, validationError("image_too_large", "Image exceeds 10 MiB")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > maxPasteImageBytes {
		return FileWrite{}, validationError("image_data_invalid", "Image base64 is invalid or empty")
	}
	filename := strings.TrimSpace(command.Filename)
	if filename == "" {
		filename = fmt.Sprintf("paste_%d%s", s.now().UnixNano(), extension)
	}
	filename, err = normalizeRelativePath(filename)
	if err != nil {
		return FileWrite{}, err
	}
	directory, err := normalizeOptionalRelativePath(command.Directory)
	if err != nil {
		return FileWrite{}, err
	}
	if directory != "" {
		filename = path.Join(directory, filename)
	}
	write := FileWrite{Path: filename, Data: data}
	if err := s.write(ctx, project, []FileWrite{write}); err != nil {
		return FileWrite{}, err
	}
	s.publish(ctx, FileMutationChange{
		Action: "session_image_pasted", ProjectID: project.ID, SessionID: session.ID,
		Paths: []string{filename}, Actor: command.Authorization.Actor,
	})
	return write, nil
}

func (s *FileMutationService) writeProject(ctx context.Context, projectID int64, action, sessionID string, writes []FileWrite, maxBytes int, authorization ActionAuthorization) (FileWrite, error) {
	if projectID <= 0 {
		return FileWrite{}, validationError("invalid_project_id", "Project ID must be positive")
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		if errors.Is(err, sql.ErrNoRows) || project == nil {
			return FileWrite{}, notFoundError("project_not_found", "Project not found", err)
		}
		return FileWrite{}, err
	}
	writes, err = normalizeFileWrites(writes, maxBytes)
	if err != nil {
		return FileWrite{}, err
	}
	if err := s.write(ctx, project, writes); err != nil {
		return FileWrite{}, err
	}
	s.publish(ctx, FileMutationChange{
		Action: action, ProjectID: project.ID, SessionID: sessionID,
		Paths: fileWritePaths(writes), Actor: authorization.Actor,
	})
	return writes[0], nil
}

func (s *FileMutationService) resolveSessionProject(ctx context.Context, sessionID string) (*database.Session, *database.Project, error) {
	sessionID, err := validateActionSessionID(sessionID)
	if err != nil {
		return nil, nil, err
	}
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		if errors.Is(err, sql.ErrNoRows) || session == nil {
			return nil, nil, notFoundError("session_not_found", "Session not found", err)
		}
		return nil, nil, err
	}
	project, err := s.store.GetProject(ctx, session.ProjectID)
	if err != nil || project == nil {
		if errors.Is(err, sql.ErrNoRows) || project == nil {
			return nil, nil, notFoundError("project_not_found", "Project not found", err)
		}
		return nil, nil, err
	}
	return session, project, nil
}

func (s *FileMutationService) write(ctx context.Context, project *database.Project, writes []FileWrite) error {
	if s.writer == nil {
		return validationError("file_writer_unavailable", "File writer is unavailable")
	}
	return s.writer.WriteFiles(ctx, project, writes)
}

func normalizeFileWrites(writes []FileWrite, maxBytes int) ([]FileWrite, error) {
	if len(writes) == 0 {
		return nil, validationError("file_required", "At least one file is required")
	}
	result := make([]FileWrite, len(writes))
	total := 0
	for i, write := range writes {
		normalized, err := normalizeRelativePath(write.Path)
		if err != nil {
			return nil, err
		}
		total += len(write.Data)
		if total > maxBytes {
			return nil, validationError("file_too_large", "File payload exceeds its bounded limit")
		}
		result[i] = FileWrite{Path: normalized, Data: append([]byte(nil), write.Data...)}
	}
	return result, nil
}

func normalizeRelativePath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || utf8.RuneCountInString(value) > 4096 || strings.IndexByte(value, 0) >= 0 || strings.HasPrefix(value, "/") {
		return "", validationError("file_path_invalid", "File path must be a bounded relative path")
	}
	if len(value) >= 2 && value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) {
		return "", validationError("file_path_invalid", "File path must not contain a drive-qualified path")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", validationError("file_path_outside_project", "File path must remain inside the project")
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", validationError("file_path_outside_project", "File path must remain inside the project")
	}
	return cleaned, nil
}

func normalizeOptionalRelativePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return normalizeRelativePath(value)
}

func fileWritePaths(writes []FileWrite) []string {
	paths := make([]string, len(writes))
	for i := range writes {
		paths[i] = writes[i].Path
	}
	return paths
}

func (s *FileMutationService) publish(ctx context.Context, change FileMutationChange) {
	if s.effects != nil {
		s.effects.PublishFileMutation(ctx, change)
	}
}
