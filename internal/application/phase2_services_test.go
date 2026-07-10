package application

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
	"openpoet/internal/security"
)

type recordingApplicationEffects struct{ changes []ApplicationChange }

func (r *recordingApplicationEffects) PublishApplicationChange(_ context.Context, change ApplicationChange) {
	r.changes = append(r.changes, change)
}

type recordingAIReinitializer struct{ calls int }

func (r *recordingAIReinitializer) ReinitializeAI(context.Context) { r.calls++ }

type recordingConfigSynchronizer struct {
	projects []int64
	all      int
}

func (r *recordingConfigSynchronizer) SyncToProject(_ context.Context, project *database.Project) error {
	r.projects = append(r.projects, project.ID)
	return nil
}

func (r *recordingConfigSynchronizer) SyncAllProjects(context.Context) error {
	r.all++
	return nil
}

func approvedR4() R4Boundary {
	return R4Boundary{Actor: Actor{Type: "user", ID: "presidente"}, Approved: true, ApprovedBy: "presidente", Reason: "phase 2 service test"}
}

func applicationEncryptor(t *testing.T) *security.Encryptor {
	t.Helper()
	codec, err := security.NewEncryptor("phase-2-test-key")
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func TestPhase2SecretsAreEncryptedRedactedAndSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "phase2-restart.db")
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	project := createApplicationProject(t, db, "phase2-secrets")
	codec := applicationEncryptor(t)
	effects := &recordingApplicationEffects{}

	mcpService := NewMCPService(db, codec, effects)
	mcpView, err := mcpService.CreateGlobal(ctx, approvedR4(), MCPServerInput{
		Name: "private-mcp", Command: "npx private-mcp --token command-secret",
		Args: `["--token","args-secret"]`, Env: `{"API_KEY":"env-secret"}`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	storedMCP, err := db.GetMCPServer(ctx, mcpView.ID)
	if err != nil {
		t.Fatal(err)
	}
	for label, value := range map[string]string{"command": storedMCP.Command, "args": storedMCP.Args, "env": storedMCP.Env} {
		if !isEncryptedEnvelope(value) || strings.Contains(value, "secret") || strings.Contains(value, "private-mcp") {
			t.Fatalf("%s was not stored as an opaque encrypted envelope: %q", label, value)
		}
	}
	commandBefore, argsBefore, envBefore := storedMCP.Command, storedMCP.Args, storedMCP.Env
	disabled := false
	if _, err = mcpService.UpdateGlobal(ctx, approvedR4(), UpdateMCPServerCommand{ID: mcpView.ID, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	storedMCP, _ = db.GetMCPServer(ctx, mcpView.ID)
	if storedMCP.Command != commandBefore || storedMCP.Args != argsBefore || storedMCP.Env != envBefore {
		t.Fatal("partial MCP update replaced an omitted secret")
	}
	legacyMCP := &database.MCPServer{Name: "legacy-mcp", Command: "node legacy-command", Args: `[]`, Env: `{"TOKEN":"legacy-env-secret"}`, Enabled: true}
	if err = db.CreateMCPServer(ctx, legacyMCP); err != nil {
		t.Fatal(err)
	}
	if _, err = mcpService.UpdateGlobal(ctx, approvedR4(), UpdateMCPServerCommand{ID: legacyMCP.ID, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	legacyMCP, _ = db.GetMCPServer(ctx, legacyMCP.ID)
	if !isEncryptedEnvelope(legacyMCP.Command) || !isEncryptedEnvelope(legacyMCP.Args) || !isEncryptedEnvelope(legacyMCP.Env) || strings.Contains(legacyMCP.Env, "legacy-env-secret") {
		t.Fatalf("legacy MCP plaintext was not migrated on update: %+v", legacyMCP)
	}

	toolService := NewCustomToolService(db, codec, effects)
	toolView, err := toolService.Create(ctx, approvedR4(), project.ID, CustomToolInput{
		Name: "private-tool", Command: "deploy --token tool-secret", Parameters: `{}`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	storedTool, _ := db.GetProjectTool(ctx, toolView.ID)
	if !isEncryptedEnvelope(storedTool.Command) || strings.Contains(storedTool.Command, "tool-secret") {
		t.Fatalf("custom tool command leaked at rest: %q", storedTool.Command)
	}
	toolCommandBefore := storedTool.Command
	toolDisabled := false
	if _, err = toolService.Update(ctx, approvedR4(), project.ID, UpdateCustomToolCommand{ID: toolView.ID, Enabled: &toolDisabled}); err != nil {
		t.Fatal(err)
	}
	storedTool, _ = db.GetProjectTool(ctx, toolView.ID)
	if storedTool.Command != toolCommandBefore {
		t.Fatal("partial custom-tool update replaced an omitted command")
	}

	aiService := NewAIConfigService(db, codec, effects, nil)
	aiConfig, err := aiService.Create(ctx, approvedR4(), AIConfigInput{Name: "private-ai", ProviderType: "anthropic", APIKey: "ai-secret-key", ExtraJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	storedAI, _ := db.GetAIConfig(ctx, aiConfig.ID)
	if storedAI.APIKeyEncrypted == "" || strings.Contains(storedAI.APIKeyEncrypted, "ai-secret-key") {
		t.Fatal("AI API key was not encrypted")
	}
	model := "updated-model"
	if _, err = aiService.Update(ctx, approvedR4(), UpdateAIConfigCommand{ID: aiConfig.ID, Model: &model}); err != nil {
		t.Fatal(err)
	}
	updatedAI, _ := db.GetAIConfig(ctx, aiConfig.ID)
	if updatedAI.APIKeyEncrypted != storedAI.APIKeyEncrypted || updatedAI.APIKeyIV != storedAI.APIKeyIV {
		t.Fatal("partial AI config update replaced an omitted API key")
	}

	configuration := NewConfigurationService(db, codec, effects, nil, nil)
	if err = configuration.UpdateSettings(ctx, approvedR4(), map[string]string{"anthropic_api_key": "setting-secret", "ai_model": "model"}); err != nil {
		t.Fatal(err)
	}
	settings, err := configuration.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settingsJSON, _ := json.Marshal(settings)
	if strings.Contains(string(settingsJSON), "setting-secret") || strings.Contains(string(settingsJSON), "anthropic_api_key\"") || strings.Contains(string(settingsJSON), "anthropic_api_key_iv") {
		t.Fatalf("settings view leaked secret material: %s", settingsJSON)
	}

	for name, value := range map[string]any{"mcp": *mcpView, "tool": *toolView, "ai": *aiConfig} {
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), `"command":`) || strings.Contains(string(encoded), "api_key_encrypted") {
			t.Fatalf("%s public result leaked a secret field: %s", name, encoded)
		}
	}

	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restartedMCP := NewMCPService(reopened, codec, nil)
	views, err := restartedMCP.ListGlobal(ctx)
	if err != nil || len(views) != 2 || !views[0].HasCommand || views[0].Enabled {
		t.Fatalf("redacted MCP state did not survive restart: views=%+v err=%v", views, err)
	}
	restartedSettings := NewConfigurationService(reopened, codec, nil, nil, nil)
	settings, err = restartedSettings.Settings(ctx)
	if err != nil || settings["anthropic_api_key_preview"] == "" || settings["ai_model"] != "model" {
		t.Fatalf("redacted settings did not survive restart: settings=%v err=%v", settings, err)
	}
}

func TestPhase2TransactionsPublishEffectsOnlyAfterCommit(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	effects := &recordingApplicationEffects{}
	skills := NewSkillService(db, effects)
	if _, err := db.Exec(`CREATE TRIGGER reject_skill_snapshot BEFORE INSERT ON skill_versions BEGIN SELECT RAISE(ABORT, 'snapshot unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := skills.Create(ctx, SkillInput{Name: "atomic", Content: "body", Enabled: true}); err == nil {
		t.Fatal("expected transactional skill creation to fail")
	}
	var skillCount int
	if err := db.GetContext(ctx, &skillCount, `SELECT COUNT(*) FROM skills`); err != nil {
		t.Fatal(err)
	}
	if skillCount != 0 || len(effects.changes) != 0 {
		t.Fatalf("failed transaction leaked state/effect: skills=%d effects=%+v", skillCount, effects.changes)
	}

	if _, err := db.Exec(`CREATE TRIGGER reject_setting BEFORE INSERT ON settings WHEN NEW.key = 'ollama_api_key_iv' BEGIN SELECT RAISE(ABORT, 'setting unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	reinitializer := &recordingAIReinitializer{}
	configuration := NewConfigurationService(db, applicationEncryptor(t), effects, reinitializer, nil)
	if err := configuration.UpdateSettings(ctx, approvedR4(), map[string]string{"ollama_api_key": "atomic-secret"}); err == nil {
		t.Fatal("expected transactional settings update to fail")
	}
	values, err := db.GetAllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if values["ollama_api_key"] != "" || values["ollama_api_key_preview"] != "" || len(effects.changes) != 0 || reinitializer.calls != 0 {
		t.Fatalf("failed settings transaction leaked state/effect: values=%v effects=%+v reinit=%d", values, effects.changes, reinitializer.calls)
	}

	config := &database.AIConfig{Name: "atomic-assignment", ProviderType: "local", ExtraJSON: "{}"}
	if err := db.CreateAIConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_ai_assignment BEFORE UPDATE ON ai_config_assignments WHEN NEW.slot = 'ai_background' BEGIN SELECT RAISE(ABORT, 'assignment unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	aiService := NewAIConfigService(db, applicationEncryptor(t), effects, reinitializer)
	if err := aiService.Assign(ctx, approvedR4(), map[string]*int64{"ai_chat": &config.ID, "ai_background": &config.ID}); err == nil {
		t.Fatal("expected transactional assignment update to fail")
	}
	assignments, err := db.GetAIConfigAssignments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assignments {
		if assignment.ConfigID.Valid {
			t.Fatalf("failed assignment transaction leaked state: %+v", assignments)
		}
	}
	if len(effects.changes) != 0 || reinitializer.calls != 0 {
		t.Fatalf("failed assignment transaction emitted effects: effects=%+v reinit=%d", effects.changes, reinitializer.calls)
	}
}

func TestPhase2ServicesValidateR4PoliciesSharesAndCRUD(t *testing.T) {
	ctx := context.Background()
	db := applicationTestDB(t)
	first := createApplicationProject(t, db, "phase2-first")
	second := createApplicationProject(t, db, "phase2-second")
	codec := applicationEncryptor(t)
	effects := &recordingApplicationEffects{}

	mcpService := NewMCPService(db, codec, effects)
	if _, err := mcpService.CreateGlobal(ctx, R4Boundary{}, MCPServerInput{Name: "blocked", Command: "cmd"}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("expected explicit R4 boundary error, got %v", err)
	}
	toolService := NewCustomToolService(db, codec, effects)
	if _, err := toolService.Create(ctx, R4Boundary{}, first.ID, CustomToolInput{Name: "blocked", Command: "cmd", Parameters: `{}`}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("expected custom-tool R4 boundary error, got %v", err)
	}

	skillService := NewSkillService(db, effects)
	skill, err := skillService.Create(ctx, SkillInput{Name: "planner", Content: "version one", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	content := "version two"
	if _, err = skillService.Update(ctx, UpdateSkillCommand{ID: skill.ID, Content: &content}); err != nil {
		t.Fatal(err)
	}
	versions, err := skillService.ListVersions(ctx, skill.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("skill versioning failed: versions=%+v err=%v", versions, err)
	}
	if _, err = skillService.Restore(ctx, skill.ID, versions[1].ID); err != nil {
		t.Fatal(err)
	}
	if err = skillService.ReplaceProjectConfig(ctx, first.ID, map[int64]bool{skill.ID: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = skillService.CreateProjectSkill(ctx, first.ID, SkillInput{Name: "project-only", Content: "body", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	agentService := NewAIAgentService(db, effects)
	agent, err := agentService.Create(ctx, AIAgentInput{Name: "reviewer", SystemPrompt: "Review carefully", ToolPolicy: `{}`, ProjectFilter: `{}`, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err = agentService.Update(ctx, UpdateAIAgentCommand{ID: agent.ID, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err = agentService.Delete(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}

	syncer := &recordingConfigSynchronizer{}
	configuration := NewConfigurationService(db, codec, effects, nil, syncer)
	policy := `{"mode":"custom","allowed":["tasks.list"],"denied":["shell"]}`
	if err = configuration.UpdateToolPolicies(ctx, approvedR4(), map[string]string{"session": policy}); err != nil {
		t.Fatal(err)
	}
	if err = configuration.UpdateProjectToolPolicy(ctx, approvedR4(), first.ID, policy); err != nil {
		t.Fatal(err)
	}
	if err = configuration.ReplaceProjectShares(ctx, first.ID, []int64{second.ID}); err != nil {
		t.Fatal(err)
	}
	shares, err := configuration.ProjectShares(ctx, first.ID)
	if err != nil || len(shares) != 1 || shares[0].ProjectID != second.ID {
		t.Fatalf("project shares failed: shares=%+v err=%v", shares, err)
	}
	if err = configuration.ReplaceProjectShares(ctx, first.ID, []int64{first.ID}); !ErrorIsKind(err, ErrorValidation) {
		t.Fatalf("expected self-share validation error, got %v", err)
	}
	if err = configuration.SyncProject(ctx, approvedR4(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err = configuration.SyncAll(ctx, approvedR4()); err != nil {
		t.Fatal(err)
	}
	if len(syncer.projects) != 1 || syncer.projects[0] != first.ID || syncer.all != 1 {
		t.Fatalf("sync orchestration failed: %+v", syncer)
	}
}
