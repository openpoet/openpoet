package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/configsync"
	"openpoet/internal/database"
	"openpoet/internal/files"
	runtime "openpoet/internal/session"
	"openpoet/internal/voice"
	"openpoet/internal/websocket"
)

var ErrNoPendingPermission = errors.New("no pending permission request")

// RespondPermission is the direct application adapter for a pending hook.
// Hook ingest remains an internal-only HTTP concern and is intentionally not
// exposed through this adapter.
func (h *HookHandler) RespondPermission(ctx context.Context, sessionID string, response application.HookPermissionResponse) error {
	h.mu.Lock()
	pending, ok := h.pending[sessionID]
	h.mu.Unlock()
	if !ok {
		return ErrNoPendingPermission
	}

	converted := PermissionResponse{
		Behavior: response.Behavior, Message: response.Message, ToolName: response.ToolName,
		Answers: response.Answers, PermissionSuggestions: response.PermissionSuggestions,
	}
	select {
	case pending.responseCh <- converted:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errors.New("permission response channel is full")
	}

	if h.hub != nil {
		h.hub.BroadcastHookEvent(sessionID, &websocket.Message{
			Type: websocket.MsgTypeHookPermissionResolved,
			Data: map[string]interface{}{"session_id": sessionID, "behavior": response.Behavior},
		})
	}
	return nil
}

func (h *HookHandler) RespondTaskNotification(ctx context.Context, sessionID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	h.ClearTaskNotification(sessionID)
	return nil
}

func (h *HookHandler) StoreImagePromptHint(ctx context.Context, sessionID, userPrompt string, imageCount int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	h.mu.Lock()
	h.imagePromptMeta[sessionID] = userPrompt
	h.mu.Unlock()
	return nil
}

func (h *HookHandler) EvaluateSession(ctx context.Context, sessionID, trigger string, outputSnapshot []byte) bool {
	if h.OnEvaluateSession == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	default:
	}
	return h.OnEvaluateSession(sessionID, trigger, append([]byte(nil), outputSnapshot...))
}

func (h *StructuredViewHandler) StartSessionWatcher(ctx context.Context, sessionID string) (string, string, error) {
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	default:
	}
	h.mu.Lock()
	_, exists := h.watchers[sessionID]
	h.mu.Unlock()
	if exists {
		return "already_watching", "", nil
	}

	source, reason := h.resolveJSONLSourceContext(ctx, sessionID)
	if reason != "" {
		return "unavailable", reason, nil
	}
	if source.isRemote {
		h.startRemoteWatching(sessionID, source)
	} else {
		h.startLocalWatching(sessionID, source.localPath)
	}

	h.mu.Lock()
	_, started := h.watchers[sessionID]
	h.mu.Unlock()
	if !started {
		return "unavailable", "watcher_start_failed", nil
	}
	return "watching", "", nil
}

func (h *StructuredViewHandler) StopSessionWatcherDirect(ctx context.Context, sessionID string) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, exists := h.watchers[sessionID]
	if !exists {
		return false, nil
	}
	entry.stopFunc()
	delete(h.watchers, sessionID)
	return true, nil
}

// sessionEventWatcherAdapter avoids changing the existing shutdown-oriented
// StopSessionWatcher method while satisfying the application port directly.
type sessionEventWatcherAdapter struct {
	handler *StructuredViewHandler
}

func NewSessionEventWatcherAdapter(handler *StructuredViewHandler) application.SessionEventWatcherPort {
	return sessionEventWatcherAdapter{handler: handler}
}

func (a sessionEventWatcherAdapter) StartSessionWatcher(ctx context.Context, sessionID string) (string, string, error) {
	return a.handler.StartSessionWatcher(ctx, sessionID)
}

func (a sessionEventWatcherAdapter) StopSessionWatcher(ctx context.Context, sessionID string) (bool, error) {
	return a.handler.StopSessionWatcherDirect(ctx, sessionID)
}

func (h *FileHandler) WriteFiles(ctx context.Context, project *database.Project, writes []application.FileWrite) error {
	if project == nil {
		return errors.New("project is required")
	}
	for _, write := range writes {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if project.Type == "local" {
			if err := files.NewLocalFileManager(project.Path).Write(write.Path, write.Data); err != nil {
				return err
			}
			continue
		}
		if err := files.NewRemoteFileManager(project, h.api.DecryptFunc()).Write(write.Path, write.Data); err != nil {
			return err
		}
	}
	return nil
}

func (h *GitHandler) EnsureRepository(ctx context.Context, project *database.Project) error {
	output, err := h.runGitCmd(ctx, project, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != "true" {
		return errors.New("project directory is not a git repository")
	}
	return nil
}

func (h *GitHandler) Stage(ctx context.Context, project *database.Project, paths []string) error {
	args := append([]string{"add", "--"}, paths...)
	_, err := h.runGitCmd(ctx, project, args...)
	return err
}

func (h *GitHandler) Unstage(ctx context.Context, project *database.Project, paths []string) error {
	args := append([]string{"reset", "HEAD", "--"}, paths...)
	_, err := h.runGitCmd(ctx, project, args...)
	return err
}

func (h *GitHandler) CommitDirect(ctx context.Context, project *database.Project, message string) (application.GitCommitResult, error) {
	output, err := h.runGitCmd(ctx, project, "commit", "-m", message)
	if err != nil {
		return application.GitCommitResult{}, err
	}
	hashOutput, hashErr := h.runGitCmd(ctx, project, "rev-parse", "HEAD")
	if hashErr != nil {
		return application.GitCommitResult{}, fmt.Errorf("commit created but hash resolution failed: %w", hashErr)
	}
	return application.GitCommitResult{Message: strings.TrimSpace(output), Hash: strings.TrimSpace(hashOutput)}, nil
}

type gitMutationAdapter struct {
	handler *GitHandler
}

func NewGitMutationAdapter(handler *GitHandler) application.GitMutationPort {
	return gitMutationAdapter{handler: handler}
}

func (a gitMutationAdapter) EnsureRepository(ctx context.Context, project *database.Project) error {
	return a.handler.EnsureRepository(ctx, project)
}

func (a gitMutationAdapter) Stage(ctx context.Context, project *database.Project, paths []string) error {
	return a.handler.Stage(ctx, project, paths)
}

func (a gitMutationAdapter) Unstage(ctx context.Context, project *database.Project, paths []string) error {
	return a.handler.Unstage(ctx, project, paths)
}

func (a gitMutationAdapter) Commit(ctx context.Context, project *database.Project, message string) (application.GitCommitResult, error) {
	return a.handler.CommitDirect(ctx, project, message)
}

// gitCommandAdapter exposes the generic local-or-SSH git executor to the
// WorkspaceService (worktree add/list/remove, rev-parse) without widening the
// mutation port.
type gitCommandAdapter struct {
	handler *GitHandler
}

func NewGitCommandAdapter(handler *GitHandler) application.WorkspaceGitPort {
	return gitCommandAdapter{handler: handler}
}

func (a gitCommandAdapter) RunGit(ctx context.Context, project *database.Project, args ...string) (string, error) {
	return a.handler.runGitCmd(ctx, project, args...)
}

func (h *VoiceHandler) TranscribeAudio(ctx context.Context, data []byte, filename, language string) (*voice.TranscriptionResult, error) {
	if h.getProviderConfig == nil {
		return nil, errors.New("voice provider configuration is unavailable")
	}
	providerType, apiKey, model := h.getProviderConfig()
	if apiKey == "" {
		return nil, fmt.Errorf("%s API key not configured", providerType.String())
	}
	provider, err := voice.NewTranscriptionProvider(providerType, apiKey, model)
	if err != nil {
		return nil, err
	}
	if language != "" {
		provider.SetLanguage(language)
	}
	return provider.TranscribeBytes(ctx, data, filename)
}

var (
	_ application.HookResponsePort           = (*HookHandler)(nil)
	_ application.SessionEvaluator           = (*HookHandler)(nil)
	_ application.SessionPromptHintStore     = (*HookHandler)(nil)
	_ application.FileMutationPort           = (*FileHandler)(nil)
	_ application.VoiceTranscriptionPort     = (*VoiceHandler)(nil)
	_ application.SessionStore               = (*database.DB)(nil)
	_ application.SessionManager             = (*runtime.Manager)(nil)
	_ application.SessionConfigurationSyncer = (*configsync.ConfigSyncer)(nil)
	_ application.SessionTaskLinker          = (*application.ProjectTaskService)(nil)
)
