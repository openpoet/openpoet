package automation

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"openpoet/internal/application"
)

const (
	maxAutomationProjectUpload = 10 << 20
	maxAutomationSessionUpload = 32 << 20
	maxAutomationFileRead      = 1 << 20
)

type OperationalFileScope struct {
	ProjectID int64
	SessionID string
}

type FileMetadataAutomationView struct {
	Path        string    `json:"path"`
	Name        string    `json:"name,omitempty"`
	Size        int64     `json:"size"`
	IsDir       bool      `json:"is_dir"`
	MIMEType    string    `json:"mime_type,omitempty"`
	ModifiedAt  time.Time `json:"modified_at,omitempty"`
	Previewable bool      `json:"previewable"`
}

type OperationalFileReadResult struct {
	Metadata FileMetadataAutomationView
	Data     []byte
}

type FileOperationalReadPort interface {
	ListOperationalFiles(context.Context, OperationalFileScope, string, int) ([]FileMetadataAutomationView, error)
	ReadOperationalFile(context.Context, OperationalFileScope, string, int) (OperationalFileReadResult, error)
	OperationalFilePreviewMetadata(context.Context, OperationalFileScope, string) (FileMetadataAutomationView, error)
}

type fileContentAutomationView struct {
	Metadata  FileMetadataAutomationView `json:"metadata"`
	Content   string                     `json:"content"`
	Encoding  string                     `json:"encoding"`
	Truncated bool                       `json:"truncated"`
}

func fileExecutionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("files.list", "file_operations", "files:read"),
		executionReadCapability("files.read", "file_operations", "files:read"),
		executionReadCapability("files.preview_metadata", "file_operations", "files:read"),
		executionPayloadLimit(executionDestructiveCapability("files.write", "file_mutations", "files:write"), 3<<20),
		executionPayloadLimit(executionDestructiveCapability("files.upload_project", "file_mutations", "files:write"), 16<<20),
		executionPayloadLimit(executionDestructiveCapability("files.upload_session", "file_mutations", "files:write", "sessions:write"), 48<<20),
		executionPayloadLimit(executionWriteCapability("files.paste_session_image", "file_mutations", "files:write", "sessions:write"), 16<<20),
	}
}

type fileExecutionPlatformExecutor struct {
	service *application.FileMutationService
	reader  FileOperationalReadPort
}

type fileListPayload struct {
	Path  string `json:"path,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type fileReadPayload struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type fileWritePayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type fileUploadPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Path      string `json:"path"`
	Data      string `json:"data_base64"`
}

type sessionFileUploadPayload struct {
	Directory string `json:"directory,omitempty"`
	Files     []struct {
		Path string `json:"path"`
		Data string `json:"data_base64"`
	} `json:"files"`
}

type sessionImagePayload struct {
	Directory string `json:"directory,omitempty"`
	Filename  string `json:"filename,omitempty"`
	DataURL   string `json:"data_url"`
}

func (e *fileExecutionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "files.list":
		scope, err := executionFileScope(target)
		if err != nil {
			return nil, err
		}
		var payload fileListPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Path, err = normalizeExecutionRelativePath(payload.Path, true)
		if err != nil {
			return nil, err
		}
		if payload.Limit < 0 || payload.Limit > maxExecutionResultEntries {
			return nil, platformFailure("platform_payload_invalid", "file list limit is invalid", false)
		}
		if payload.Limit == 0 {
			payload.Limit = 200
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"path": payload.Path, "limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			items, err := e.reader.ListOperationalFiles(ctx, scope, payload.Path, payload.Limit)
			if err != nil {
				return nil, err
			}
			if len(items) > payload.Limit {
				items = items[:payload.Limit]
			}
			for i := range items {
				items[i] = sanitizeFileMetadata(items[i])
			}
			return items, nil
		}}, nil
	case "files.read":
		scope, err := executionFileScope(target)
		if err != nil {
			return nil, err
		}
		var payload fileReadPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Path, err = normalizeExecutionRelativePath(payload.Path, false)
		if err != nil {
			return nil, err
		}
		if payload.MaxBytes < 0 || payload.MaxBytes > maxAutomationFileRead {
			return nil, platformFailure("platform_payload_invalid", "file read max_bytes exceeds 1 MiB", false)
		}
		if payload.MaxBytes == 0 {
			payload.MaxBytes = 128 << 10
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"path": payload.Path, "max_bytes": payload.MaxBytes}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			result, err := e.reader.ReadOperationalFile(ctx, scope, payload.Path, payload.MaxBytes)
			if err != nil {
				return nil, err
			}
			truncated := len(result.Data) > payload.MaxBytes
			if truncated {
				result.Data = result.Data[:payload.MaxBytes]
			}
			return fileContentAutomationView{
				Metadata: sanitizeFileMetadata(result.Metadata), Content: base64.StdEncoding.EncodeToString(result.Data),
				Encoding: "base64", Truncated: truncated,
			}, nil
		}}, nil
	case "files.preview_metadata":
		scope, err := executionFileScope(target)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Path string `json:"path"`
		}
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Path, err = normalizeExecutionRelativePath(payload.Path, false)
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"path": payload.Path}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			metadata, err := e.reader.OperationalFilePreviewMetadata(ctx, scope, payload.Path)
			return sanitizeFileMetadata(metadata), err
		}}, nil
	case "files.write":
		var payload fileWritePayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		payload.Path, err = normalizeExecutionRelativePath(payload.Path, false)
		if err != nil {
			return nil, err
		}
		if len([]byte(payload.Content)) > 2<<20 {
			return nil, platformFailure("platform_payload_invalid", "project text file exceeds 2 MiB", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "path": payload.Path, "size": len([]byte(payload.Content))}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			write, err := e.service.WriteProjectFile(ctx, application.WriteProjectFileCommand{ProjectID: projectID, Path: payload.Path, Content: payload.Content, Authorization: authorization})
			return fileWriteResult(write), err
		}}, nil
	case "files.upload_project":
		var payload fileUploadPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		payload.Path, err = normalizeExecutionRelativePath(payload.Path, false)
		if err != nil {
			return nil, err
		}
		data, err := decodeExecutionBase64(payload.Data, maxAutomationProjectUpload, "project upload")
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "path": payload.Path, "size": len(data)}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			write, err := e.service.UploadProjectFile(ctx, application.UploadProjectFileCommand{ProjectID: projectID, Path: payload.Path, Data: data, Authorization: authorization})
			return fileWriteResult(write), err
		}}, nil
	case "files.upload_session":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionFileUploadPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Directory, err = normalizeExecutionRelativePath(payload.Directory, true)
		if err != nil || len(payload.Files) == 0 || len(payload.Files) > 100 {
			return nil, platformFailure("platform_payload_invalid", "session upload must contain 1 to 100 bounded files", false)
		}
		writes := make([]application.FileWrite, len(payload.Files))
		total := 0
		for i, file := range payload.Files {
			file.Path, err = normalizeExecutionRelativePath(file.Path, false)
			if err != nil {
				return nil, err
			}
			data, decodeErr := decodeExecutionBase64(file.Data, maxAutomationSessionUpload-total, "session upload")
			if decodeErr != nil {
				return nil, decodeErr
			}
			total += len(data)
			writes[i] = application.FileWrite{Path: file.Path, Data: data}
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "directory": payload.Directory, "file_count": len(writes), "total_size": total}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			result, err := e.service.UploadSessionFiles(ctx, application.UploadSessionFilesCommand{SessionID: sessionID, Directory: payload.Directory, Files: writes, Authorization: authorization})
			if err != nil {
				return nil, err
			}
			views := make([]map[string]any, len(result))
			for i, write := range result {
				views[i] = fileWriteResult(write)
			}
			return views, nil
		}}, nil
	case "files.paste_session_image":
		sessionID, err := executionStringID(target, "session id")
		if err != nil {
			return nil, err
		}
		var payload sessionImagePayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Directory, err = normalizeExecutionRelativePath(payload.Directory, true)
		if err != nil {
			return nil, err
		}
		if payload.Filename != "" {
			payload.Filename, err = normalizeExecutionRelativePath(payload.Filename, false)
			if err != nil {
				return nil, err
			}
		}
		if len(payload.DataURL) == 0 || len(payload.DataURL) > 14<<20 || !strings.HasPrefix(strings.ToLower(payload.DataURL), "data:image/") {
			return nil, platformFailure("platform_payload_invalid", "image data URL is invalid or too large", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"session_id": sessionID, "directory": payload.Directory, "has_filename": payload.Filename != "", "encoded_bytes": len(payload.DataURL)}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			write, err := e.service.PasteSessionImage(ctx, application.PasteSessionImageCommand{SessionID: sessionID, Directory: payload.Directory, Filename: payload.Filename, DataURL: payload.DataURL, Authorization: authorization})
			return fileWriteResult(write), err
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the file capability handler is unsupported", false)
	}
}

func executionFileScope(target executionCommandTarget) (OperationalFileScope, error) {
	if target.ProjectID > 0 {
		return OperationalFileScope{ProjectID: target.ProjectID}, nil
	}
	if target.Type == "project" || target.Kind == "project" {
		projectID, err := executionProjectID(target, 0)
		return OperationalFileScope{ProjectID: projectID}, err
	}
	if target.Type == "session" || target.Kind == "session" {
		sessionID, err := executionStringID(target, "session id")
		return OperationalFileScope{SessionID: sessionID}, err
	}
	return OperationalFileScope{}, platformFailure("platform_target_invalid", "file target must identify a project or session", false)
}

func decodeExecutionBase64(value string, maximum int, label string) ([]byte, error) {
	if value == "" || maximum <= 0 || base64.StdEncoding.DecodedLen(len(value)) > maximum {
		return nil, platformFailure("platform_payload_invalid", label+" data is empty or too large", false)
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(data) == 0 || len(data) > maximum {
		return nil, platformFailure("platform_payload_invalid", label+" data must be valid bounded base64", false)
	}
	return data, nil
}

func fileWriteResult(write application.FileWrite) map[string]any {
	return map[string]any{"path": write.Path, "size": len(write.Data)}
}

func sanitizeFileMetadata(value FileMetadataAutomationView) FileMetadataAutomationView {
	value.Path, _ = boundedExecutionText(value.Path, maxExecutionPathRunes)
	value.Name, _ = boundedExecutionText(value.Name, 512)
	value.MIMEType, _ = boundedExecutionText(value.MIMEType, 200)
	if value.Size < 0 {
		value.Size = 0
	}
	return value
}
