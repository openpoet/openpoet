package automation

import (
	"context"
	"strings"
	"unicode/utf8"

	"openpoet/internal/application"
)

type GitFileStatusAutomationView struct {
	Path    string `json:"path"`
	Index   string `json:"index,omitempty"`
	Work    string `json:"work,omitempty"`
	OldPath string `json:"old_path,omitempty"`
}

type GitStatusAutomationView struct {
	Branch    string                        `json:"branch"`
	Staged    []GitFileStatusAutomationView `json:"staged"`
	Unstaged  []GitFileStatusAutomationView `json:"unstaged"`
	Untracked []GitFileStatusAutomationView `json:"untracked"`
	Truncated bool                          `json:"truncated,omitempty"`
}

type GitBranchAutomationView struct {
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	IsHead   bool   `json:"is_head,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Date     string `json:"date,omitempty"`
}

type GitBranchesAutomationView struct {
	Current   string                    `json:"current"`
	Local     []GitBranchAutomationView `json:"local"`
	Remote    []GitBranchAutomationView `json:"remote"`
	Tags      []GitBranchAutomationView `json:"tags"`
	Truncated bool                      `json:"truncated,omitempty"`
}

type GitLogEntryAutomationView struct {
	Hash        string   `json:"hash"`
	ShortHash   string   `json:"short_hash,omitempty"`
	Parents     []string `json:"parents,omitempty"`
	AuthorName  string   `json:"author_name,omitempty"`
	AuthorEmail string   `json:"author_email,omitempty"`
	Date        string   `json:"date,omitempty"`
	Message     string   `json:"message"`
	Refs        []string `json:"refs,omitempty"`
}

type GitLogQuery struct {
	Ref   string
	Limit int
}

type GitDiffQuery struct {
	Base     string
	Ref      string
	Path     string
	MaxBytes int
}

type GitTextAutomationView struct {
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Redacted  bool   `json:"redacted"`
}

type GitShowAutomationView struct {
	Commit GitLogEntryAutomationView `json:"commit"`
	Diff   GitTextAutomationView     `json:"diff"`
}

type GitOperationalReadPort interface {
	OperationalGitStatus(context.Context, int64, int) (GitStatusAutomationView, error)
	OperationalGitBranches(context.Context, int64, int) (GitBranchesAutomationView, error)
	OperationalGitLog(context.Context, int64, GitLogQuery) ([]GitLogEntryAutomationView, error)
	OperationalGitDiff(context.Context, int64, GitDiffQuery) (string, error)
	OperationalGitShow(context.Context, int64, string, int) (GitShowAutomationView, error)
}

func gitExecutionPlatformDefinitions() []PlatformCapabilityDefinition {
	return []PlatformCapabilityDefinition{
		executionReadCapability("git.status", "git_operations", "git:read"),
		executionReadCapability("git.branches", "git_operations", "git:read"),
		executionReadCapability("git.log", "git_operations", "git:read"),
		executionReadCapability("git.diff", "git_operations", "git:read"),
		executionReadCapability("git.show", "git_operations", "git:read"),
		executionWriteCapability("git.stage", "git_mutations", "git:write"),
		executionWriteCapability("git.unstage", "git_mutations", "git:write"),
		executionPayloadLimit(executionDestructiveCapability("git.commit", "git_mutations", "git:write"), 20<<10),
	}
}

type gitExecutionPlatformExecutor struct {
	service *application.GitMutationService
	reader  GitOperationalReadPort
}

type gitLimitPayload struct {
	Limit int `json:"limit,omitempty"`
}

type gitLogPayload struct {
	Ref   string `json:"ref,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type gitDiffPayload struct {
	Base     string `json:"base,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Path     string `json:"path,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type gitShowPayload struct {
	Ref      string `json:"ref"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type gitFilesPayload struct {
	ProjectID int64    `json:"project_id,omitempty"`
	Files     []string `json:"files"`
}

type gitCommitPayload struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Message   string `json:"message"`
}

func (e *gitExecutionPlatformExecutor) Validate(_ context.Context, input PlatformExecutionInput) (PlatformValidatedCommand, error) {
	target, err := decodeExecutionTarget(input.Target)
	if err != nil {
		return nil, err
	}
	switch input.Handler {
	case "git.status":
		projectID, payload, err := gitProjectAndLimit(target, input.Payload, maxExecutionResultEntries)
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			status, err := e.reader.OperationalGitStatus(ctx, projectID, payload.Limit)
			return sanitizeGitStatus(status, payload.Limit), err
		}}, nil
	case "git.branches":
		projectID, payload, err := gitProjectAndLimit(target, input.Payload, 500)
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "limit": payload.Limit}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			branches, err := e.reader.OperationalGitBranches(ctx, projectID, payload.Limit)
			return sanitizeGitBranches(branches, payload.Limit), err
		}}, nil
	case "git.log":
		projectID, err := executionProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		var payload gitLogPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Ref, err = normalizeGitRef(payload.Ref, true)
		if err != nil || payload.Limit < 0 || payload.Limit > 100 {
			return nil, platformFailure("platform_payload_invalid", "git log query is invalid", false)
		}
		if payload.Limit == 0 {
			payload.Limit = 30
		}
		query := GitLogQuery{Ref: payload.Ref, Limit: payload.Limit}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "limit": payload.Limit, "has_ref": payload.Ref != ""}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			entries, err := e.reader.OperationalGitLog(ctx, projectID, query)
			if err != nil {
				return nil, err
			}
			if len(entries) > payload.Limit {
				entries = entries[:payload.Limit]
			}
			for i := range entries {
				entries[i] = sanitizeGitLogEntry(entries[i])
			}
			return entries, nil
		}}, nil
	case "git.diff":
		projectID, err := executionProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		var payload gitDiffPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Base, err = normalizeGitRef(payload.Base, true)
		if err != nil {
			return nil, err
		}
		payload.Ref, err = normalizeGitRef(payload.Ref, true)
		if err != nil {
			return nil, err
		}
		payload.Path, err = normalizeExecutionRelativePath(payload.Path, true)
		if err != nil {
			return nil, err
		}
		if payload.MaxBytes < 0 || payload.MaxBytes > maxExecutionDiffBytes {
			return nil, platformFailure("platform_payload_invalid", "git diff max_bytes exceeds 256 KiB", false)
		}
		if payload.MaxBytes == 0 {
			payload.MaxBytes = 64 << 10
		}
		// Ask the typed port for the bounded maximum, then redact before
		// applying the caller's smaller window. This avoids cutting through a
		// credential immediately before the redaction boundary.
		query := GitDiffQuery{Base: payload.Base, Ref: payload.Ref, Path: payload.Path, MaxBytes: maxExecutionDiffBytes}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "path": payload.Path, "max_bytes": payload.MaxBytes}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			content, err := e.reader.OperationalGitDiff(ctx, projectID, query)
			return gitTextView(content, payload.MaxBytes), err
		}}, nil
	case "git.show":
		projectID, err := executionProjectID(target, 0)
		if err != nil {
			return nil, err
		}
		var payload gitShowPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Ref, err = normalizeGitRef(payload.Ref, false)
		if err != nil || payload.MaxBytes < 0 || payload.MaxBytes > maxExecutionDiffBytes {
			return nil, platformFailure("platform_payload_invalid", "git show query is invalid", false)
		}
		if payload.MaxBytes == 0 {
			payload.MaxBytes = 64 << 10
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "max_bytes": payload.MaxBytes}), execute: func(ctx context.Context, _ application.ActionAuthorization) (any, error) {
			show, err := e.reader.OperationalGitShow(ctx, projectID, payload.Ref, maxExecutionDiffBytes)
			if err != nil {
				return nil, err
			}
			show.Commit = sanitizeGitLogEntry(show.Commit)
			show.Diff = gitTextView(show.Diff.Content, payload.MaxBytes)
			return show, nil
		}}, nil
	case "git.stage", "git.unstage":
		var payload gitFilesPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		files, err := normalizeExecutionGitPaths(payload.Files)
		if err != nil {
			return nil, err
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "file_count": len(files)}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			var err error
			if input.Handler == "git.stage" {
				err = e.service.Stage(ctx, application.StageGitFilesCommand{ProjectID: projectID, Files: files, Authorization: authorization})
			} else {
				err = e.service.Unstage(ctx, application.UnstageGitFilesCommand{ProjectID: projectID, Files: files, Authorization: authorization})
			}
			return map[string]any{"updated": err == nil, "project_id": projectID, "file_count": len(files)}, err
		}}, nil
	case "git.commit":
		var payload gitCommitPayload
		if err := decodeExecutionPayload(input.Payload, &payload); err != nil {
			return nil, err
		}
		projectID, err := executionProjectID(target, payload.ProjectID)
		if err != nil {
			return nil, err
		}
		message := strings.TrimSpace(payload.Message)
		if message == "" || utf8.RuneCountInString(message) > 4000 || strings.IndexByte(message, 0) >= 0 {
			return nil, platformFailure("platform_payload_invalid", "git commit message is invalid or too large", false)
		}
		return &executionValidatedCommand{preview: executionPreview(input.Handler, map[string]any{"project_id": projectID, "message_runes": utf8.RuneCountInString(message)}), execute: func(ctx context.Context, authorization application.ActionAuthorization) (any, error) {
			result, err := e.service.Commit(ctx, application.CommitGitCommand{ProjectID: projectID, Message: message, Authorization: authorization})
			hash, _ := boundedExecutionText(result.Hash, 200)
			return map[string]any{"committed": err == nil, "project_id": projectID, "hash": hash}, err
		}}, nil
	default:
		return nil, platformFailure("platform_handler_unsupported", "the git capability handler is unsupported", false)
	}
}

func gitProjectAndLimit(target executionCommandTarget, raw []byte, maximum int) (int64, gitLimitPayload, error) {
	projectID, err := executionProjectID(target, 0)
	if err != nil {
		return 0, gitLimitPayload{}, err
	}
	var payload gitLimitPayload
	if err := decodeExecutionPayload(raw, &payload); err != nil {
		return 0, gitLimitPayload{}, err
	}
	if payload.Limit < 0 || payload.Limit > maximum {
		return 0, gitLimitPayload{}, platformFailure("platform_payload_invalid", "git result limit is invalid", false)
	}
	if payload.Limit == 0 {
		payload.Limit = min(200, maximum)
	}
	return projectID, payload, nil
}

func normalizeExecutionGitPaths(paths []string) ([]string, error) {
	if len(paths) == 0 || len(paths) > 200 {
		return nil, platformFailure("platform_payload_invalid", "git mutation requires 1 to 200 paths", false)
	}
	result := make([]string, len(paths))
	total := 0
	for i, value := range paths {
		if strings.TrimSpace(value) == "." {
			result[i] = "."
			total++
			continue
		}
		normalized, err := normalizeExecutionRelativePath(value, false)
		if err != nil {
			return nil, err
		}
		total += len(normalized)
		if total > 64<<10 {
			return nil, platformFailure("platform_payload_invalid", "git paths exceed 64 KiB", false)
		}
		result[i] = normalized
	}
	return result, nil
}

func normalizeGitRef(value string, optional bool) (string, error) {
	value = strings.TrimSpace(value)
	if optional && value == "" {
		return "", nil
	}
	if value == "" || len(value) > 500 || strings.IndexByte(value, 0) >= 0 || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\r\n") {
		return "", platformFailure("platform_payload_invalid", "git ref is invalid or too large", false)
	}
	return value, nil
}

func sanitizeGitStatus(status GitStatusAutomationView, limit int) GitStatusAutomationView {
	status.Branch, _ = boundedExecutionText(status.Branch, 500)
	remaining := limit
	status.Staged, remaining, status.Truncated = sanitizeGitFileStatuses(status.Staged, remaining, status.Truncated)
	status.Unstaged, remaining, status.Truncated = sanitizeGitFileStatuses(status.Unstaged, remaining, status.Truncated)
	status.Untracked, _, status.Truncated = sanitizeGitFileStatuses(status.Untracked, remaining, status.Truncated)
	return status
}

func sanitizeGitFileStatuses(items []GitFileStatusAutomationView, remaining int, truncated bool) ([]GitFileStatusAutomationView, int, bool) {
	if remaining < 0 {
		remaining = 0
	}
	if len(items) > remaining {
		items = items[:remaining]
		truncated = true
	}
	for i := range items {
		items[i].Path, _ = boundedExecutionText(items[i].Path, maxExecutionPathRunes)
		items[i].OldPath, _ = boundedExecutionText(items[i].OldPath, maxExecutionPathRunes)
		items[i].Index, _ = boundedExecutionText(items[i].Index, 10)
		items[i].Work, _ = boundedExecutionText(items[i].Work, 10)
	}
	return items, remaining - len(items), truncated
}

func sanitizeGitBranches(branches GitBranchesAutomationView, limit int) GitBranchesAutomationView {
	branches.Current, _ = boundedExecutionText(branches.Current, 500)
	remaining := limit
	branches.Local, remaining, branches.Truncated = sanitizeGitBranchList(branches.Local, remaining, branches.Truncated)
	branches.Remote, remaining, branches.Truncated = sanitizeGitBranchList(branches.Remote, remaining, branches.Truncated)
	branches.Tags, _, branches.Truncated = sanitizeGitBranchList(branches.Tags, remaining, branches.Truncated)
	return branches
}

func sanitizeGitBranchList(items []GitBranchAutomationView, remaining int, truncated bool) ([]GitBranchAutomationView, int, bool) {
	if remaining < 0 {
		remaining = 0
	}
	if len(items) > remaining {
		items = items[:remaining]
		truncated = true
	}
	for i := range items {
		items[i].Name, _ = boundedExecutionText(items[i].Name, 500)
		items[i].Hash, _ = boundedExecutionText(items[i].Hash, 200)
		items[i].Upstream, _ = boundedExecutionText(items[i].Upstream, 500)
		items[i].Date, _ = boundedExecutionText(items[i].Date, 100)
	}
	return items, remaining - len(items), truncated
}

func sanitizeGitLogEntry(entry GitLogEntryAutomationView) GitLogEntryAutomationView {
	entry.Hash, _ = boundedExecutionText(entry.Hash, 200)
	entry.ShortHash, _ = boundedExecutionText(entry.ShortHash, 100)
	entry.AuthorName, _ = boundedExecutionText(entry.AuthorName, 500)
	entry.AuthorEmail, _ = boundedExecutionText(entry.AuthorEmail, 500)
	entry.Date, _ = boundedExecutionText(entry.Date, 100)
	entry.Message, _ = boundedExecutionText(entry.Message, 4000)
	if len(entry.Parents) > 20 {
		entry.Parents = entry.Parents[:20]
	}
	if len(entry.Refs) > 100 {
		entry.Refs = entry.Refs[:100]
	}
	for i := range entry.Parents {
		entry.Parents[i], _ = boundedExecutionText(entry.Parents[i], 200)
	}
	for i := range entry.Refs {
		entry.Refs[i], _ = boundedExecutionText(entry.Refs[i], 500)
	}
	return entry
}

func gitTextView(content string, maximum int) GitTextAutomationView {
	content, truncated := boundedExecutionText(content, maximum)
	return GitTextAutomationView{Content: content, Bytes: len([]byte(content)), Truncated: truncated, Redacted: true}
}
