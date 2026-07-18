package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/database"
	"openpoet/internal/files"
	"openpoet/internal/llm"
	"openpoet/internal/session"
)

var (
	platformPortNamedSecret  = regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?(?:TOKEN|KEY)|REFRESH[_-]?TOKEN|AUTH[_-]?TOKEN|PRIVATE[_-]?KEY|SSH[_-]?KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?)[A-Z0-9_]*\b\s*[:=]\s*[^\s,;]+`)
	platformPortOpaqueSecret = regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`)
)

type platformSessionEnvironmentProvider struct{ handler *AIHandler }

func (p platformSessionEnvironmentProvider) SessionEnvironment(_ context.Context, project *database.Project) (map[string]string, error) {
	result := map[string]string{}
	if project == nil || project.Backend != "claude_code" || p.handler == nil || p.handler.providerMgr == nil {
		return result, nil
	}
	var projectConfig struct {
		Provider         string `json:"provider"`
		ProviderConfigID int64  `json:"provider_config_id"`
		Model            string `json:"model"`
		SmallModel       string `json:"small_model"`
	}
	_ = json.Unmarshal([]byte(project.BackendConfig), &projectConfig)
	projectConfig.Provider = strings.ToLower(strings.TrimSpace(projectConfig.Provider))
	if projectConfig.Provider == "openai" || projectConfig.Provider == "openai_oauth" {
		if projectConfig.ProviderConfigID <= 0 {
			return nil, errors.New("OpenAI OAuth profile is required for this Claude Code project")
		}
		if p.handler.api == nil || p.handler.api.providerBridge == nil {
			return nil, errors.New("OpenAI provider bridge is unavailable")
		}
		profile, err := p.handler.api.db.GetAIConfig(context.Background(), projectConfig.ProviderConfigID)
		if err != nil || profile == nil || profile.ProviderType != "openai_oauth" {
			return nil, errors.New("configured OpenAI OAuth profile was not found")
		}
		model := strings.TrimSpace(projectConfig.Model)
		if model == "" {
			model = strings.TrimSpace(profile.Model)
		}
		if model == "" {
			return nil, errors.New("an OpenAI model is required for this Claude Code project")
		}
		baseURL, err := p.handler.api.providerBridge.EnsureProxy(context.Background(), projectConfig.ProviderConfigID)
		if err != nil {
			return nil, err
		}
		return openAIClaudeEnvironment(baseURL, model, projectConfig.SmallModel, project.Type == "remote"), nil
	}
	if projectConfig.Provider == "anthropic" {
		return result, nil
	}
	config := p.handler.providerMgr.GetSlotConfig(llm.SlotSession)
	if config == nil {
		return result, nil
	}
	switch config.ProviderType {
	case "openai_oauth":
		if config.ID <= 0 || p.handler.api == nil || p.handler.api.providerBridge == nil {
			return nil, errors.New("OpenAI OAuth session profile is unavailable")
		}
		baseURL, err := p.handler.api.providerBridge.EnsureProxy(context.Background(), config.ID)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Model) == "" {
			return nil, errors.New("an OpenAI model is required for the Claude Code session profile")
		}
		return openAIClaudeEnvironment(baseURL, config.Model, "", project.Type == "remote"), nil
	case "ollama", "ollama-sdk":
		baseURL := strings.TrimSpace(config.BaseURL)
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		model := strings.TrimSpace(config.Model)
		if model == "" {
			model = "qwen3-coder"
		}
		result["ANTHROPIC_BASE_URL"] = baseURL
		result["ANTHROPIC_API_KEY"] = ""
		result["ANTHROPIC_DEFAULT_OPUS_MODEL"] = model
		result["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
		result["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model
		result["CLAUDE_CODE_SUBAGENT_MODEL"] = model
		result["ANTHROPIC_AUTH_TOKEN"] = config.APIKey
		if result["ANTHROPIC_AUTH_TOKEN"] == "" {
			result["ANTHROPIC_AUTH_TOKEN"] = "ollama"
		}
	case "apikey":
		if config.APIKey != "" {
			result["ANTHROPIC_API_KEY"] = config.APIKey
		}
	}
	return result, nil
}

func openAIClaudeEnvironment(baseURL, model, smallModel string, remote bool) map[string]string {
	model = strings.TrimSpace(model)
	smallModel = strings.TrimSpace(smallModel)
	if smallModel == "" {
		smallModel = model
	}
	env := map[string]string{
		"ANTHROPIC_BASE_URL":                        strings.TrimRight(baseURL, "/"),
		"ANTHROPIC_API_KEY":                         "",
		"ANTHROPIC_AUTH_TOKEN":                      "unused",
		"CLAUDE_CODE_OAUTH_TOKEN":                   "",
		"OPENAI_API_KEY":                            "",
		"OPENAI_ACCESS_TOKEN":                       "",
		"CODEX_API_KEY":                             "",
		"ANTHROPIC_MODEL":                           model,
		"ANTHROPIC_SMALL_FAST_MODEL":                smallModel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":              model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL":            model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":             smallModel,
		"CLAUDE_CODE_SUBAGENT_MODEL":                smallModel,
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":           "272000",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":  "1",
		"CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1",
	}
	if remote {
		env[session.RemoteProviderTunnelEnv] = "1"
	}
	return env
}

type platformSessionNameStore struct{ db *database.DB }

func (s platformSessionNameStore) RenameSession(ctx context.Context, id, name string) error {
	if s.db == nil {
		return errors.New("session name store unavailable")
	}
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET name = ? WHERE id = ?", name, id)
	return err
}

type platformSessionTaskNotifier struct{ hook *HookHandler }

func (n platformSessionTaskNotifier) NotifyTaskLoaded(_ context.Context, sessionID string, task *database.ProjectTask) error {
	if n.hook == nil {
		return errors.New("session task notifier unavailable")
	}
	if task == nil {
		return errors.New("session task is unavailable")
	}
	n.hook.SetTaskNotification(sessionID, task.ID, task.Title, task.Description)
	return nil
}

type platformSessionInputSubmitter struct{ api *API }

func (s platformSessionInputSubmitter) SubmitSessionLine(ctx context.Context, sessionID, text string) error {
	if s.api == nil || s.api.sessionMgr == nil {
		return errors.New("session input submitter unavailable")
	}
	if strings.TrimSpace(text) == defaultTaskStartPrompt {
		if ctx == nil {
			ctx = context.Background()
		}
		readyCtx, cancel := context.WithTimeout(ctx, taskSessionReadyTimeout)
		err := s.api.waitForTaskSessionReady(readyCtx, sessionID)
		cancel()
		if err != nil {
			log.Printf("[Session] task prompt readiness wait ended for %s: %v", sessionID, err)
		}
	}
	delay := s.api.sessionLineSubmitDelay(ctx, sessionID)
	return s.api.sessionMgr.SubmitLineToSession(sessionID, text, delay)
}

type platformSessionInitialPromptSubmitter struct{ api *API }

func (s platformSessionInitialPromptSubmitter) SubmitInitialSessionPrompt(ctx context.Context, sessionID, text string) error {
	if s.api == nil || s.api.sessionMgr == nil {
		return errors.New("session input submitter unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readyCtx, cancel := context.WithTimeout(ctx, taskSessionReadyTimeout)
	err := s.api.waitForTaskSessionReady(readyCtx, sessionID)
	cancel()
	if err != nil {
		log.Printf("[Session] initial prompt readiness wait ended for %s: %v", sessionID, err)
	}
	delay := s.api.sessionLineSubmitDelay(ctx, sessionID)
	return s.api.sessionMgr.SubmitLineToSession(sessionID, text, delay)
}

type platformSessionRuntimeSettings struct{ api *API }

func (s platformSessionRuntimeSettings) SetSessionModel(ctx context.Context, sessionID, model string) (*database.Session, error) {
	if s.api == nil || s.api.sessionMgr == nil {
		return nil, errors.New("session runtime settings unavailable")
	}
	return s.api.sessionMgr.SetSessionModel(ctx, sessionID, model, s.api.sessionLineSubmitDelay(ctx, sessionID))
}

func (s platformSessionRuntimeSettings) SetSessionEffort(ctx context.Context, sessionID, effort string) (*database.Session, error) {
	if s.api == nil || s.api.sessionMgr == nil {
		return nil, errors.New("session runtime settings unavailable")
	}
	return s.api.sessionMgr.SetSessionEffort(ctx, sessionID, effort, s.api.sessionLineSubmitDelay(ctx, sessionID))
}

type platformSessionEventReader struct{ handler *StructuredViewHandler }

func (r platformSessionEventReader) SessionEventStatus(ctx context.Context, sessionID string) (automation.SessionEventStatusView, error) {
	if r.handler == nil {
		return automation.SessionEventStatusView{}, errors.New("session event reader unavailable")
	}
	r.handler.mu.Lock()
	_, watching := r.handler.watchers[sessionID]
	r.handler.mu.Unlock()
	if watching {
		return automation.SessionEventStatusView{SessionID: sessionID, Status: "watching"}, nil
	}
	_, reason := r.handler.resolveJSONLSourceContext(ctx, sessionID)
	if reason != "" {
		return automation.SessionEventStatusView{SessionID: sessionID, Status: "unavailable", Reason: reason}, nil
	}
	return automation.SessionEventStatusView{SessionID: sessionID, Status: "available"}, nil
}

type platformSessionSuggestionProvider struct{ handler *AIHandler }

func (p platformSessionSuggestionProvider) SuggestTaskData(ctx context.Context, input application.SessionTaskDataSuggestionRequest) (application.SessionTaskDataSuggestionProviderResult, error) {
	if p.handler == nil {
		return application.SessionTaskDataSuggestionProviderResult{}, errors.New("task suggestion provider unavailable")
	}
	provider := p.handler.getProviderForSlot(llm.SlotBackground)
	if provider == nil {
		return application.SessionTaskDataSuggestionProviderResult{}, errors.New("task suggestion provider unavailable")
	}
	model := p.handler.getSlotModel(llm.SlotBackground)
	prompt := "Infer one concise project task from this already-sanitized terminal excerpt. " +
		"Return only JSON with string fields title, description, and priority (low, medium, high).\n" +
		"Project: " + input.ProjectName + "\nSession: " + input.SessionName + "\nExcerpt:\n" + input.Context
	request := &llm.Request{
		System:    "You extract bounded project task metadata. Never repeat credentials or terminal secrets.",
		Messages:  []llm.Message{llm.NewTextMessage("user", prompt)},
		MaxTokens: 1200,
		Model:     model,
		SessionID: input.SessionID,
	}
	var text strings.Builder
	response, err := provider.StreamMessage(ctx, request, func(event llm.StreamEvent) error {
		if event.Delta != nil && event.Delta.Type == "text_delta" {
			text.WriteString(event.Delta.Text)
		}
		return nil
	})
	if err != nil {
		return application.SessionTaskDataSuggestionProviderResult{}, errors.New("task suggestion provider failed")
	}
	if response != nil && text.Len() == 0 {
		for _, block := range response.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
	}
	raw := strings.TrimSpace(text.String())
	if start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); start >= 0 && end >= start {
		raw = raw[start : end+1]
	}
	var parsed struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    string `json:"priority"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return application.SessionTaskDataSuggestionProviderResult{}, errors.New("task suggestion provider returned an invalid response")
	}
	if response != nil {
		if strings.TrimSpace(response.Model) != "" {
			model = response.Model
		}
		_ = p.handler.api.db.CreateTokenUsage(ctx, &database.TokenUsage{
			Source: "ai_assistant", Subcategory: "session_task_suggestion", Model: model,
			SessionID:   sql.NullString{String: input.SessionID, Valid: input.SessionID != ""},
			ProjectID:   sql.NullInt64{Int64: input.ProjectID, Valid: input.ProjectID > 0},
			InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens,
			CacheReadTokens: response.Usage.CacheReadTokens, CacheCreationTokens: response.Usage.CacheCreationTokens,
			CostUSD: response.CostUSD,
		})
	}
	return application.SessionTaskDataSuggestionProviderResult{
		Title: parsed.Title, Description: parsed.Description, Priority: parsed.Priority, Model: model,
	}, nil
}

type platformFileReader struct{ handler *FileHandler }

func (r platformFileReader) ListOperationalFiles(ctx context.Context, scope automation.OperationalFileScope, path string, limit int) ([]automation.FileMetadataAutomationView, error) {
	project, err := r.project(ctx, scope)
	if err != nil {
		return nil, err
	}
	items, err := r.list(project, path)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if len(items) > limit {
		items = items[:limit]
	}
	views := make([]automation.FileMetadataAutomationView, len(items))
	for i, item := range items {
		views[i] = fileMetadataView(project, item)
	}
	return views, nil
}

func (r platformFileReader) ReadOperationalFile(ctx context.Context, scope automation.OperationalFileScope, path string, maxBytes int) (automation.OperationalFileReadResult, error) {
	project, err := r.project(ctx, scope)
	if err != nil {
		return automation.OperationalFileReadResult{}, err
	}
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 1 << 20
	}
	data, info, err := r.readBounded(project, path, int64(maxBytes)+1)
	if err != nil {
		return automation.OperationalFileReadResult{}, err
	}
	metadata := fileMetadataView(project, *info)
	if previewableTextMIME(metadata.MIMEType) {
		data = []byte(platformPortOpaqueSecret.ReplaceAllString(platformPortNamedSecret.ReplaceAllString(string(data), "[REDACTED]"), "[REDACTED]"))
	}
	return automation.OperationalFileReadResult{Metadata: metadata, Data: data}, nil
}

func (r platformFileReader) OperationalFilePreviewMetadata(ctx context.Context, scope automation.OperationalFileScope, path string) (automation.FileMetadataAutomationView, error) {
	project, err := r.project(ctx, scope)
	if err != nil {
		return automation.FileMetadataAutomationView{}, err
	}
	_, info, err := r.readBounded(project, path, 0)
	if err != nil {
		return automation.FileMetadataAutomationView{}, err
	}
	return fileMetadataView(project, *info), nil
}

func (r platformFileReader) project(ctx context.Context, scope automation.OperationalFileScope) (*database.Project, error) {
	if r.handler == nil || r.handler.api == nil || r.handler.api.db == nil {
		return nil, errors.New("file reader unavailable")
	}
	projectID := scope.ProjectID
	if projectID <= 0 && strings.TrimSpace(scope.SessionID) != "" {
		session, err := r.handler.api.db.GetSession(ctx, scope.SessionID)
		if err != nil {
			return nil, err
		}
		projectID = session.ProjectID
	}
	if projectID <= 0 {
		return nil, errors.New("project is required")
	}
	return r.handler.api.db.GetProject(ctx, projectID)
}

func (r platformFileReader) list(project *database.Project, path string) ([]files.FileInfo, error) {
	if project.Type == "local" {
		return files.NewLocalFileManager(project.Path).List(path)
	}
	return files.NewRemoteFileManager(project, r.handler.api.DecryptFunc()).List(path)
}

func (r platformFileReader) readBounded(project *database.Project, path string, maximum int64) ([]byte, *files.FileInfo, error) {
	if project.Type == "local" {
		reader, info, err := files.NewLocalFileManager(project.Path).ReadStream(path)
		if err != nil {
			return nil, nil, err
		}
		defer reader.Close()
		if maximum == 0 {
			return nil, info, nil
		}
		data, err := io.ReadAll(io.LimitReader(reader, maximum))
		return data, info, err
	}
	reader, info, sshClient, sftpClient, err := files.NewRemoteFileManager(project, r.handler.api.DecryptFunc()).ReadStream(path)
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()
	defer sftpClient.Close()
	defer sshClient.Close()
	if maximum == 0 {
		return nil, info, nil
	}
	data, err := io.ReadAll(io.LimitReader(reader, maximum))
	return data, info, err
}

func fileMetadataView(project *database.Project, item files.FileInfo) automation.FileMetadataAutomationView {
	mimeType := files.NewLocalFileManager(project.Path).GetMimeType(item.Path)
	if project.Type != "local" {
		mimeType = files.NewRemoteFileManager(project, nil).GetMimeType(item.Path)
	}
	return automation.FileMetadataAutomationView{
		Path: item.Path, Name: item.Name, Size: item.Size, IsDir: item.IsDir, MIMEType: mimeType,
		ModifiedAt: item.ModTime, Previewable: !item.IsDir && previewableMIME(mimeType),
	}
}

func previewableMIME(value string) bool {
	return strings.HasPrefix(value, "text/") || strings.HasPrefix(value, "image/") ||
		value == "application/json" || value == "application/pdf" || value == "application/javascript" || value == "application/xml"
}

func previewableTextMIME(value string) bool {
	return strings.HasPrefix(value, "text/") || value == "application/json" || value == "application/javascript" || value == "application/xml"
}

type platformGitReader struct{ handler *GitHandler }

func (r platformGitReader) project(ctx context.Context, id int64) (*database.Project, error) {
	if r.handler == nil || r.handler.api == nil || r.handler.api.db == nil {
		return nil, errors.New("git reader unavailable")
	}
	return r.handler.api.db.GetProject(ctx, id)
}

func (r platformGitReader) OperationalGitStatus(ctx context.Context, projectID int64, limit int) (automation.GitStatusAutomationView, error) {
	project, err := r.project(ctx, projectID)
	if err != nil {
		return automation.GitStatusAutomationView{}, err
	}
	branch, _ := r.handler.runGitCmd(ctx, project, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := r.handler.runGitCmd(ctx, project, "status", "--porcelain=v1", "-uall")
	if err != nil {
		return automation.GitStatusAutomationView{}, err
	}
	view := automation.GitStatusAutomationView{Branch: strings.TrimSpace(branch), Staged: []automation.GitFileStatusAutomationView{}, Unstaged: []automation.GitFileStatusAutomationView{}, Untracked: []automation.GitFileStatusAutomationView{}}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		x, y, file := string(line[0]), string(line[1]), line[3:]
		oldPath := ""
		if (x == "R" || x == "C") && strings.Contains(file, " -> ") {
			parts := strings.SplitN(file, " -> ", 2)
			oldPath, file = parts[0], parts[1]
		}
		item := automation.GitFileStatusAutomationView{Path: file, Index: x, Work: y, OldPath: oldPath}
		if x == "?" && y == "?" {
			view.Untracked = append(view.Untracked, item)
		} else {
			if x != " " && x != "?" {
				view.Staged = append(view.Staged, item)
			}
			if y != " " && y != "?" {
				view.Unstaged = append(view.Unstaged, item)
			}
		}
	}
	if limit > 0 && len(view.Staged)+len(view.Unstaged)+len(view.Untracked) > limit {
		view.Truncated = true
	}
	return view, nil
}

func (r platformGitReader) OperationalGitBranches(ctx context.Context, projectID int64, limit int) (automation.GitBranchesAutomationView, error) {
	project, err := r.project(ctx, projectID)
	if err != nil {
		return automation.GitBranchesAutomationView{}, err
	}
	out, err := r.handler.runGitCmd(ctx, project, "branch", "-a", "--format=%(refname:short)%00%(objectname:short)%00%(HEAD)%00%(upstream:short)%00%(committerdate:iso-strict)")
	if err != nil {
		return automation.GitBranchesAutomationView{}, err
	}
	view := automation.GitBranchesAutomationView{Local: []automation.GitBranchAutomationView{}, Remote: []automation.GitBranchAutomationView{}, Tags: []automation.GitBranchAutomationView{}}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\x00", 5)
		if len(parts) != 5 || strings.HasSuffix(parts[0], "/HEAD") {
			continue
		}
		item := automation.GitBranchAutomationView{Name: parts[0], Hash: parts[1], IsHead: parts[2] == "*", Upstream: parts[3], Date: parts[4]}
		if item.IsHead {
			view.Current = item.Name
		}
		if strings.HasPrefix(item.Name, "remotes/") || strings.HasPrefix(item.Name, "origin/") {
			view.Remote = append(view.Remote, item)
		} else {
			view.Local = append(view.Local, item)
		}
	}
	tags, tagErr := r.handler.runGitCmd(ctx, project, "tag", "--sort=-creatordate", "--format=%(refname:short)%00%(objectname:short)%00%(creatordate:iso-strict)")
	if tagErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(tags), "\n") {
			parts := strings.SplitN(line, "\x00", 3)
			if len(parts) == 3 {
				view.Tags = append(view.Tags, automation.GitBranchAutomationView{Name: parts[0], Hash: parts[1], Date: parts[2]})
			}
		}
	}
	if limit > 0 && len(view.Local)+len(view.Remote)+len(view.Tags) > limit {
		view.Truncated = true
	}
	return view, nil
}

func (r platformGitReader) OperationalGitLog(ctx context.Context, projectID int64, query automation.GitLogQuery) ([]automation.GitLogEntryAutomationView, error) {
	project, err := r.project(ctx, projectID)
	if err != nil {
		return nil, err
	}
	args := []string{"log"}
	if query.Ref != "" {
		if err := validateRef(query.Ref); err != nil {
			return nil, err
		}
		args = append(args, query.Ref)
	}
	args = append(args, "--topo-order", "--format=%H%x00%h%x00%P%x00%an%x00%ae%x00%aI%x00%s%x00%D", "--max-count="+strconv.Itoa(query.Limit))
	out, err := r.handler.runGitCmd(ctx, project, args...)
	if err != nil {
		return nil, err
	}
	return parsePlatformGitLog(out), nil
}

func (r platformGitReader) OperationalGitDiff(ctx context.Context, projectID int64, query automation.GitDiffQuery) (string, error) {
	project, err := r.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	args := []string{"diff"}
	for _, ref := range []string{query.Base, query.Ref} {
		if ref != "" {
			if err := validateRef(ref); err != nil {
				return "", err
			}
			args = append(args, ref)
		}
	}
	if query.Path != "" {
		args = append(args, "--", filepath.ToSlash(query.Path))
	}
	out, err := r.handler.runGitCmd(ctx, project, args...)
	return boundPortText(out, query.MaxBytes), err
}

func (r platformGitReader) OperationalGitShow(ctx context.Context, projectID int64, ref string, maxBytes int) (automation.GitShowAutomationView, error) {
	project, err := r.project(ctx, projectID)
	if err != nil {
		return automation.GitShowAutomationView{}, err
	}
	if err := validateRef(ref); err != nil {
		return automation.GitShowAutomationView{}, err
	}
	metadata, err := r.handler.runGitCmd(ctx, project, "show", "-s", "--format=%H%x00%h%x00%P%x00%an%x00%ae%x00%aI%x00%s%x00%D", ref)
	if err != nil {
		return automation.GitShowAutomationView{}, err
	}
	entries := parsePlatformGitLog(metadata)
	if len(entries) == 0 {
		return automation.GitShowAutomationView{}, errors.New("git commit not found")
	}
	diff, err := r.handler.runGitCmd(ctx, project, "show", "--format=", "--patch", ref)
	if err != nil {
		return automation.GitShowAutomationView{}, err
	}
	diff = boundPortText(diff, maxBytes)
	return automation.GitShowAutomationView{Commit: entries[0], Diff: automation.GitTextAutomationView{Content: diff, Bytes: len(diff), Truncated: len(diff) >= maxBytes, Redacted: false}}, nil
}

func parsePlatformGitLog(output string) []automation.GitLogEntryAutomationView {
	result := []automation.GitLogEntryAutomationView{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.SplitN(line, "\x00", 8)
		if len(parts) != 8 {
			continue
		}
		entry := automation.GitLogEntryAutomationView{Hash: parts[0], ShortHash: parts[1], AuthorName: parts[3], AuthorEmail: parts[4], Date: parts[5], Message: parts[6]}
		if parts[2] != "" {
			entry.Parents = strings.Fields(parts[2])
		}
		if parts[7] != "" {
			for _, ref := range strings.Split(parts[7], ",") {
				entry.Refs = append(entry.Refs, strings.TrimSpace(ref))
			}
		}
		result = append(result, entry)
	}
	return result
}

func boundPortText(value string, maximum int) string {
	if maximum > 0 && len(value) > maximum {
		return value[:maximum]
	}
	return value
}

type platformTunnelReader struct{ api *API }

func (r platformTunnelReader) OperationalTunnelStatus(ctx context.Context) (automation.TunnelStatusAutomationView, error) {
	if r.api == nil || r.api.db == nil {
		return automation.TunnelStatusAutomationView{}, errors.New("tunnel reader unavailable")
	}
	r.api.tunnelMu.Lock()
	status, publicURL := "disabled", ""
	if r.api.tunnelClient != nil {
		status, publicURL = r.api.tunnelClient.Status(), r.api.tunnelClient.PublicURL()
	}
	r.api.tunnelMu.Unlock()
	devices, err := r.api.db.ListPairedDevices(ctx)
	if err != nil {
		return automation.TunnelStatusAutomationView{}, err
	}
	token, err := r.api.db.GetSetting(ctx, "tunnel_relay_token")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return automation.TunnelStatusAutomationView{}, err
	}
	return automation.TunnelStatusAutomationView{Status: status, PublicURL: publicURL, DeviceCount: len(devices), HasToken: strings.TrimSpace(token) != ""}, nil
}

func (r platformTunnelReader) ListPairedDevices(ctx context.Context) ([]database.PairedDevice, error) {
	if r.api == nil || r.api.db == nil {
		return nil, errors.New("tunnel reader unavailable")
	}
	return r.api.db.ListPairedDevices(ctx)
}
