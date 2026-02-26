package configsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"openpoet/internal/database"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SyncProgressReporter is an interface for reporting sync progress.
type SyncProgressReporter interface {
	BroadcastSyncProgress(projectID int64, step, status, detail string)
}

type ConfigSyncer struct {
	db          *database.DB
	decryptFunc func(string, string) (string, error)
	hub         SyncProgressReporter
	serverAddr  string
}

func NewConfigSyncer(db *database.DB, decryptFunc func(string, string) (string, error), serverAddr string) *ConfigSyncer {
	return &ConfigSyncer{
		db:          db,
		decryptFunc: decryptFunc,
		serverAddr:  serverAddr,
	}
}

// SetHub sets the WebSocket hub for broadcasting sync progress.
func (cs *ConfigSyncer) SetHub(hub SyncProgressReporter) {
	cs.hub = hub
}

func (cs *ConfigSyncer) reportProgress(projectID int64, step, status, detail string) {
	if cs.hub != nil {
		cs.hub.BroadcastSyncProgress(projectID, step, status, detail)
	}
}

// SyncToProject syncs global configuration to a project
func (cs *ConfigSyncer) SyncToProject(ctx context.Context, project *database.Project) error {
	if project.Type == "local" {
		return cs.syncToLocal(ctx, project)
	}
	return cs.syncToRemote(ctx, project)
}

func (cs *ConfigSyncer) syncToLocal(ctx context.Context, project *database.Project) error {
	// Copilot projects use a different sync path
	if project.Backend == "copilot" {
		return cs.syncToLocalCopilot(ctx, project)
	}

	projectPath := project.Path
	claudeDir := filepath.Join(projectPath, ".claude")
	skillsDir := filepath.Join(claudeDir, "skills")
	hooksDir := filepath.Join(claudeDir, "hooks")

	// Ensure directories exist
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude/skills directory: %w", err)
	}
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude/hooks directory: %w", err)
	}

	// Sync bridge.sh hook script
	cs.reportProgress(project.ID, "hooks", "running", "Syncing hook scripts...")
	if err := cs.syncBridgeScriptLocal(hooksDir); err != nil {
		cs.reportProgress(project.ID, "hooks", "error", err.Error())
		return fmt.Errorf("failed to sync bridge.sh: %w", err)
	}
	cs.reportProgress(project.ID, "hooks", "done", "Hook scripts synced")

	// Auto-detect: import skills from disk before pushing DB skills
	cs.importSkillsFromDisk(ctx, skillsDir, project)

	// Sync skills with smart tracking
	cs.reportProgress(project.ID, "skills", "running", "Syncing skills...")
	if err := cs.syncSkillsToLocal(ctx, skillsDir, project); err != nil {
		cs.reportProgress(project.ID, "skills", "error", err.Error())
		return fmt.Errorf("failed to sync skills: %w", err)
	}
	cs.reportProgress(project.ID, "skills", "done", "Skills synced")

	// Auto-detect: import MCP servers from .mcp.json
	cs.importMCPsFromDisk(ctx, projectPath, project)

	// MCP servers are passed via --mcp-config CLI flag at session start.
	cs.reportProgress(project.ID, "mcps", "done", "MCP servers configured via CLI flag at session start")

	// Sync hooks into settings.json (merge, preserving user's other settings)
	cs.reportProgress(project.ID, "settings", "running", "Syncing hooks to settings...")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	existing := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	existing["hooks"] = cs.buildHooksConfig()

	settingsJSON, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(settingsPath, settingsJSON, 0644); err != nil {
		cs.reportProgress(project.ID, "settings", "error", err.Error())
		return fmt.Errorf("failed to write settings.json: %w", err)
	}
	cs.reportProgress(project.ID, "settings", "done", "Hooks synced")

	// Sync CLAUDE.md → memory doc
	cs.reportProgress(project.ID, "memory_doc", "running", "Syncing CLAUDE.md to memory doc...")
	claudeMDPath := filepath.Join(projectPath, "CLAUDE.md")
	if data, err := os.ReadFile(claudeMDPath); err == nil {
		if _, err := cs.db.UpsertMemoryDoc(ctx, project.ID, string(data), "sync", "Synced from CLAUDE.md"); err != nil {
			cs.reportProgress(project.ID, "memory_doc", "error", err.Error())
		} else {
			cs.reportProgress(project.ID, "memory_doc", "done", "CLAUDE.md synced to memory doc")
		}
	} else {
		cs.reportProgress(project.ID, "memory_doc", "done", "No CLAUDE.md found")
	}

	return nil
}

// syncToLocalCopilot syncs configuration for Copilot CLI projects.
// Uses .github/ directory instead of .claude/ for hooks and instructions.
func (cs *ConfigSyncer) syncToLocalCopilot(ctx context.Context, project *database.Project) error {
	projectPath := project.Path
	githubDir := filepath.Join(projectPath, ".github")
	hooksDir := filepath.Join(githubDir, "hooks")

	// Ensure directories exist
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("failed to create .github/hooks directory: %w", err)
	}

	// Sync Copilot bridge script
	cs.reportProgress(project.ID, "hooks", "running", "Syncing Copilot hook scripts...")
	if err := cs.syncCopilotBridgeScriptLocal(hooksDir); err != nil {
		cs.reportProgress(project.ID, "hooks", "error", err.Error())
		return fmt.Errorf("failed to sync copilot-bridge.sh: %w", err)
	}

	// Write hooks.json for Copilot
	hooksConfig := cs.buildCopilotHooksConfig()
	hooksJSON, _ := json.MarshalIndent(hooksConfig, "", "  ")
	hooksConfigPath := filepath.Join(hooksDir, "hooks.json")
	if err := os.WriteFile(hooksConfigPath, hooksJSON, 0644); err != nil {
		cs.reportProgress(project.ID, "hooks", "error", err.Error())
		return fmt.Errorf("failed to write hooks.json: %w", err)
	}
	cs.reportProgress(project.ID, "hooks", "done", "Copilot hook scripts synced")

	// Sync skills → .github/copilot-instructions.md
	cs.reportProgress(project.ID, "skills", "running", "Syncing skills to Copilot instructions...")
	if err := cs.syncSkillsToCopilotInstructions(ctx, githubDir, project); err != nil {
		cs.reportProgress(project.ID, "skills", "error", err.Error())
		return fmt.Errorf("failed to sync skills to copilot instructions: %w", err)
	}
	cs.reportProgress(project.ID, "skills", "done", "Skills synced to copilot-instructions.md")

	// Auto-detect MCP servers from .mcp.json
	cs.importMCPsFromDisk(ctx, projectPath, project)
	cs.reportProgress(project.ID, "mcps", "done", "MCP servers configured via CLI flag at session start")

	// Sync AGENTS.md → memory doc (Copilot's equivalent of CLAUDE.md)
	cs.reportProgress(project.ID, "memory_doc", "running", "Syncing AGENTS.md to memory doc...")
	agentsMDPath := filepath.Join(projectPath, "AGENTS.md")
	claudeMDPath := filepath.Join(projectPath, "CLAUDE.md")
	// Try AGENTS.md first, fallback to CLAUDE.md
	mdPath := agentsMDPath
	if _, err := os.Stat(agentsMDPath); os.IsNotExist(err) {
		mdPath = claudeMDPath
	}
	if data, err := os.ReadFile(mdPath); err == nil {
		if _, err := cs.db.UpsertMemoryDoc(ctx, project.ID, string(data), "sync", "Synced from "+filepath.Base(mdPath)); err != nil {
			cs.reportProgress(project.ID, "memory_doc", "error", err.Error())
		} else {
			cs.reportProgress(project.ID, "memory_doc", "done", filepath.Base(mdPath)+" synced to memory doc")
		}
	} else {
		cs.reportProgress(project.ID, "memory_doc", "done", "No AGENTS.md or CLAUDE.md found")
	}

	return nil
}

func (cs *ConfigSyncer) syncToRemote(ctx context.Context, project *database.Project) error {
	// Build SSH config
	config, err := cs.buildSSHConfig(project)
	if err != nil {
		return err
	}

	// Connect
	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()

	// Create SFTP client
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("SFTP connection failed: %w", err)
	}
	defer sftpClient.Close()

	claudeDir := filepath.Join(project.Path, ".claude")
	skillsDir := filepath.Join(claudeDir, "skills")
	hooksDir := filepath.Join(claudeDir, "hooks")

	// Ensure directories exist
	sftpClient.MkdirAll(skillsDir)
	sftpClient.MkdirAll(hooksDir)

	// Sync bridge.sh hook script to remote
	cs.reportProgress(project.ID, "hooks", "running", "Syncing hook scripts to remote...")
	if err := cs.syncBridgeScriptRemote(client, sftpClient, hooksDir); err != nil {
		cs.reportProgress(project.ID, "hooks", "error", err.Error())
		return fmt.Errorf("failed to sync bridge.sh to remote: %w", err)
	}
	cs.reportProgress(project.ID, "hooks", "done", "Hook scripts synced")

	// Auto-detect: import skills from remote before pushing DB skills
	cs.importSkillsFromRemote(ctx, sftpClient, skillsDir, project)

	// Sync skills with smart tracking
	cs.reportProgress(project.ID, "skills", "running", "Syncing skills to remote...")
	if err := cs.syncSkillsToRemote(ctx, sftpClient, skillsDir, project); err != nil {
		cs.reportProgress(project.ID, "skills", "error", err.Error())
		return fmt.Errorf("failed to sync skills to remote: %w", err)
	}
	cs.reportProgress(project.ID, "skills", "done", "Skills synced")

	// Auto-detect: import MCP servers from remote .mcp.json
	cs.importMCPsFromRemote(ctx, sftpClient, project.Path, project)

	// MCP servers are passed via --mcp-config CLI flag at session start.
	cs.reportProgress(project.ID, "mcps", "done", "MCP servers configured via CLI flag at session start")

	// Sync hooks into settings.json (merge, preserving user's other settings)
	cs.reportProgress(project.ID, "settings", "running", "Syncing hooks to remote settings...")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	existing := make(map[string]interface{})
	if f, err := sftpClient.Open(settingsPath); err == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		json.Unmarshal(data, &existing)
	}

	existing["hooks"] = cs.buildHooksConfig()

	settingsJSON, _ := json.MarshalIndent(existing, "", "  ")
	f, err := sftpClient.Create(settingsPath)
	if err != nil {
		cs.reportProgress(project.ID, "settings", "error", err.Error())
		return fmt.Errorf("failed to create settings.json: %w", err)
	}
	f.Write(settingsJSON)
	f.Close()
	cs.reportProgress(project.ID, "settings", "done", "Hooks synced")

	// Sync CLAUDE.md → memory doc (remote)
	cs.reportProgress(project.ID, "memory_doc", "running", "Syncing CLAUDE.md to memory doc...")
	claudeMDPath := filepath.Join(project.Path, "CLAUDE.md")
	if rf, err := sftpClient.Open(claudeMDPath); err == nil {
		data, readErr := io.ReadAll(rf)
		rf.Close()
		if readErr == nil {
			if _, err := cs.db.UpsertMemoryDoc(ctx, project.ID, string(data), "sync", "Synced from CLAUDE.md"); err != nil {
				cs.reportProgress(project.ID, "memory_doc", "error", err.Error())
			} else {
				cs.reportProgress(project.ID, "memory_doc", "done", "CLAUDE.md synced to memory doc")
			}
		} else {
			cs.reportProgress(project.ID, "memory_doc", "error", readErr.Error())
		}
	} else {
		cs.reportProgress(project.ID, "memory_doc", "done", "No CLAUDE.md found")
	}

	return nil
}

// syncableSkill is a unified representation for syncing both global and project-specific skills.
type syncableSkill struct {
	Name           string
	Content        string
	UpdatedAt      time.Time // DB updated_at for timestamp comparison
	GlobalSkillID  int64     // >0 for global skills, 0 for project skills
	ProjectSkillID int64     // >0 for project skills, 0 for global skills
}

// getSkillsForProject returns the effective list of skills to sync for a project,
// respecting the project's skill_policy and per-project overrides.
func (cs *ConfigSyncer) getSkillsForProject(ctx context.Context, project *database.Project) ([]syncableSkill, error) {
	var result []syncableSkill

	if project.SkillPolicy == "custom" {
		// Custom: use per-project global skill overrides
		globalSkills, err := cs.db.ListEnabledSkillsForProject(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to list enabled skills for project: %w", err)
		}
		for _, s := range globalSkills {
			result = append(result, syncableSkill{Name: s.Name, Content: s.Content, UpdatedAt: s.UpdatedAt, GlobalSkillID: s.ID})
		}
	} else {
		// Inherit: use all globally enabled skills
		globalSkills, err := cs.db.ListEnabledSkills(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list enabled skills: %w", err)
		}
		for _, s := range globalSkills {
			result = append(result, syncableSkill{Name: s.Name, Content: s.Content, UpdatedAt: s.UpdatedAt, GlobalSkillID: s.ID})
		}
	}

	// Always add enabled project-specific skills
	projectSkills, err := cs.db.ListEnabledProjectSkills(ctx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list project skills: %w", err)
	}
	// Project skills override global skills with the same name
	globalNames := make(map[string]bool)
	for _, s := range result {
		globalNames[s.Name] = true
	}
	for _, ps := range projectSkills {
		if globalNames[ps.Name] {
			// Project skill overrides global — remove the global one
			for i, s := range result {
				if s.Name == ps.Name {
					result = append(result[:i], result[i+1:]...)
					break
				}
			}
		}
		result = append(result, syncableSkill{Name: ps.Name, Content: ps.Content, UpdatedAt: ps.UpdatedAt, ProjectSkillID: ps.ID})
	}

	return result, nil
}

// syncSkillsToLocal syncs skills to a local project, tracking which files OpenPoet manages.
// Skills are written as .claude/skills/<name>/SKILL.md with YAML frontmatter.
func (cs *ConfigSyncer) syncSkillsToLocal(ctx context.Context, skillsDir string, project *database.Project) error {
	projectID := project.ID

	skills, err := cs.getSkillsForProject(ctx, project)
	if err != nil {
		return err
	}

	// Build set of expected directory names (new format: <name>/SKILL.md)
	expectedDirs := make(map[string]bool)
	for _, skill := range skills {
		expectedDirs[skill.Name] = true
	}

	// Get previously synced files for this project
	var syncedFiles []database.SyncedSkillFile
	if projectID > 0 {
		syncedFiles, _ = cs.db.ListSyncedSkillFiles(ctx, projectID)
		// Clean up stale ones
		for _, sf := range syncedFiles {
			// Extract dir name from tracked file (could be old "name.md" or new "name/SKILL.md")
			dirName := sf.FileName
			if strings.HasSuffix(dirName, ".md") && !strings.Contains(dirName, "/") {
				// Old flat format: "name.md" -> remove flat file
				dirName = strings.TrimSuffix(dirName, ".md")
				oldPath := filepath.Join(skillsDir, sf.FileName)
				os.Remove(oldPath)
			}
			if strings.HasSuffix(dirName, "/SKILL.md") {
				dirName = strings.TrimSuffix(dirName, "/SKILL.md")
			}

			if !expectedDirs[dirName] {
				// Skill deleted/disabled/renamed — remove directory
				dirPath := filepath.Join(skillsDir, dirName)
				os.RemoveAll(dirPath)
				cs.db.DeleteSyncedSkillFile(ctx, sf.ID)
			}
		}
	}

	// Build synced_at lookup for timestamp comparison
	syncedAtByFile := make(map[string]time.Time)
	for _, sf := range syncedFiles {
		syncedAtByFile[sf.FileName] = sf.SyncedAt
	}

	// Write skills as <name>/SKILL.md
	for _, skill := range skills {
		skillDirPath := filepath.Join(skillsDir, skill.Name)
		skillFilePath := filepath.Join(skillDirPath, "SKILL.md")
		trackedName := skill.Name + "/SKILL.md"

		// For project skills only: check if file was externally modified since last sync
		if skill.ProjectSkillID > 0 {
			if info, err := os.Stat(skillFilePath); err == nil {
				if syncedAt, ok := syncedAtByFile[trackedName]; ok {
					if info.ModTime().After(syncedAt.Add(time.Second)) {
						// File was edited after OpenPoet last wrote it — update DB from file
						if raw, readErr := os.ReadFile(skillFilePath); readErr == nil {
							name, content, category := parseSkillMD(string(raw), skill.Name)
							if dbSkill, getErr := cs.db.GetProjectSkill(ctx, skill.ProjectSkillID); getErr == nil {
								dbSkill.Content = content
								dbSkill.Name = name
								if category != "" {
									dbSkill.Category = category
								}
								cs.db.UpdateProjectSkill(ctx, dbSkill)
								skill.Content = content
								log.Printf("[configsync] project skill '%s' updated from disk (file newer than synced_at)", name)
							}
						}
					}
				}
			}
		}

		if err := os.MkdirAll(skillDirPath, 0755); err != nil {
			return fmt.Errorf("failed to create skill dir %s: %w", skill.Name, err)
		}

		content := buildSkillMD(skill.Name, skill.Content)
		if err := os.WriteFile(skillFilePath, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write skill %s: %w", skill.Name, err)
		}

		// Also clean up legacy flat file if it exists
		legacyPath := filepath.Join(skillsDir, skill.Name+".md")
		os.Remove(legacyPath)

		// Track with new format
		if projectID > 0 {
			if skill.GlobalSkillID > 0 {
				cs.db.UpsertSyncedSkillFile(ctx, projectID, skill.GlobalSkillID, trackedName)
				cs.db.IncrementSkillSyncCount(ctx, skill.GlobalSkillID)
			} else {
				// Project skill — track with skill_id 0 (NULL)
				cs.db.UpsertSyncedSkillFile(ctx, projectID, 0, trackedName)
				cs.db.IncrementProjectSkillSyncCount(ctx, skill.ProjectSkillID)
			}
		}
	}

	return nil
}

// syncSkillsToRemote syncs skills to a remote project via SFTP, tracking which files OpenPoet manages.
// Skills are written as .claude/skills/<name>/SKILL.md with YAML frontmatter.
func (cs *ConfigSyncer) syncSkillsToRemote(ctx context.Context, sftpClient *sftp.Client, skillsDir string, project *database.Project) error {
	skills, err := cs.getSkillsForProject(ctx, project)
	if err != nil {
		return err
	}

	// Build set of expected directory names
	expectedDirs := make(map[string]bool)
	for _, skill := range skills {
		expectedDirs[skill.Name] = true
	}

	// Get previously synced files for this project and remove stale ones
	syncedFiles, _ := cs.db.ListSyncedSkillFiles(ctx, project.ID)
	for _, sf := range syncedFiles {
		dirName := sf.FileName
		if strings.HasSuffix(dirName, ".md") && !strings.Contains(dirName, "/") {
			// Old flat format
			dirName = strings.TrimSuffix(dirName, ".md")
			sftpClient.Remove(filepath.Join(skillsDir, sf.FileName))
		}
		if strings.HasSuffix(dirName, "/SKILL.md") {
			dirName = strings.TrimSuffix(dirName, "/SKILL.md")
		}

		if !expectedDirs[dirName] {
			dirPath := filepath.Join(skillsDir, dirName)
			// Remove SKILL.md then directory
			sftpClient.Remove(filepath.Join(dirPath, "SKILL.md"))
			sftpClient.RemoveDirectory(dirPath)
			cs.db.DeleteSyncedSkillFile(ctx, sf.ID)
		}
	}

	// Build synced_at lookup for timestamp comparison
	syncedAtByFile := make(map[string]time.Time)
	for _, sf := range syncedFiles {
		syncedAtByFile[sf.FileName] = sf.SyncedAt
	}

	// Write skills as <name>/SKILL.md
	for _, skill := range skills {
		skillDirPath := filepath.Join(skillsDir, skill.Name)
		skillFilePath := filepath.Join(skillDirPath, "SKILL.md")
		trackedName := skill.Name + "/SKILL.md"

		// For project skills only: check if remote file was externally modified since last sync
		if skill.ProjectSkillID > 0 {
			if info, err := sftpClient.Stat(skillFilePath); err == nil {
				if syncedAt, ok := syncedAtByFile[trackedName]; ok {
					if info.ModTime().After(syncedAt.Add(time.Second)) {
						// File was edited after OpenPoet last wrote it — update DB from file
						if f, readErr := sftpClient.Open(skillFilePath); readErr == nil {
							raw, ioErr := io.ReadAll(f)
							f.Close()
							if ioErr == nil {
								name, content, category := parseSkillMD(string(raw), skill.Name)
								if dbSkill, getErr := cs.db.GetProjectSkill(ctx, skill.ProjectSkillID); getErr == nil {
									dbSkill.Content = content
									dbSkill.Name = name
									if category != "" {
										dbSkill.Category = category
									}
									cs.db.UpdateProjectSkill(ctx, dbSkill)
									skill.Content = content
									log.Printf("[configsync] project skill '%s' updated from remote disk (file newer than synced_at)", name)
								}
							}
						}
					}
				}
			}
		}

		sftpClient.MkdirAll(skillDirPath)

		content := buildSkillMD(skill.Name, skill.Content)
		f, err := sftpClient.Create(skillFilePath)
		if err != nil {
			return fmt.Errorf("failed to create skill file: %w", err)
		}
		f.Write([]byte(content))
		f.Close()

		// Clean up legacy flat file
		sftpClient.Remove(filepath.Join(skillsDir, skill.Name+".md"))

		if skill.GlobalSkillID > 0 {
			cs.db.UpsertSyncedSkillFile(ctx, project.ID, skill.GlobalSkillID, trackedName)
			cs.db.IncrementSkillSyncCount(ctx, skill.GlobalSkillID)
		} else {
			cs.db.UpsertSyncedSkillFile(ctx, project.ID, 0, trackedName)
			cs.db.IncrementProjectSkillSyncCount(ctx, skill.ProjectSkillID)
		}
	}

	return nil
}

// ──── Auto-detection: import from disk ────

// parseSkillMD parses a SKILL.md file and extracts the name, content body, and category.
// It is the inverse of buildSkillMD. If the file has YAML frontmatter, name is extracted
// from it (fallback to dirName). The content returned is the raw file content as-is
// (including frontmatter if present), suitable for storing in the database.
func parseSkillMD(raw string, dirName string) (name, content, category string) {
	trimmed := strings.TrimSpace(raw)
	name = dirName // default

	if !strings.HasPrefix(trimmed, "---") {
		// No frontmatter — use content as-is
		return name, trimmed, ""
	}

	// Find closing ---
	rest := trimmed[3:] // skip opening ---
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return name, trimmed, ""
	}

	frontmatter := rest[:idx]

	// Parse frontmatter lines for name and description
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
			if v != "" {
				name = v
			}
		}
		if strings.HasPrefix(line, "description:") {
			category = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
			if len(category) > 64 {
				category = category[:64]
			}
		}
	}

	// Store the full raw content (with frontmatter) so buildSkillMD preserves it on re-sync
	return name, trimmed, category
}

// importSkillsFromDisk scans .claude/skills/ for SKILL.md files not already tracked
// by OpenPoet and imports them as project-scoped skills.
func (cs *ConfigSyncer) importSkillsFromDisk(ctx context.Context, skillsDir string, project *database.Project) error {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil // directory doesn't exist yet, nothing to import
	}

	// Build set of already-tracked file names
	syncedFiles, _ := cs.db.ListSyncedSkillFiles(ctx, project.ID)
	trackedDirs := make(map[string]bool)
	for _, sf := range syncedFiles {
		dirName := sf.FileName
		if strings.HasSuffix(dirName, "/SKILL.md") {
			dirName = strings.TrimSuffix(dirName, "/SKILL.md")
		}
		trackedDirs[dirName] = true
	}

	// Build set of existing project skill names
	existingSkills, _ := cs.db.ListProjectSkills(ctx, project.ID)
	existingNames := make(map[string]bool)
	for _, ps := range existingSkills {
		existingNames[ps.Name] = true
	}

	// Also build set of global skill names to avoid importing duplicates
	globalSkills, _ := cs.db.ListSkills(ctx)
	globalNames := make(map[string]bool)
	for _, s := range globalSkills {
		globalNames[s.Name] = true
	}

	imported := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()

		// Skip if already tracked by OpenPoet
		if trackedDirs[dirName] {
			continue
		}

		// Check if SKILL.md exists
		skillPath := filepath.Join(skillsDir, dirName, "SKILL.md")
		raw, err := os.ReadFile(skillPath)
		if err != nil {
			continue // no SKILL.md in this dir
		}

		name, content, category := parseSkillMD(string(raw), dirName)

		// Skip if already exists as project skill or global skill
		if existingNames[name] || globalNames[name] {
			continue
		}

		ps := &database.ProjectSkill{
			ProjectID: project.ID,
			Name:      name,
			Content:   content,
			Enabled:   true,
			Category:  category,
		}
		if err := cs.db.CreateProjectSkill(ctx, ps); err != nil {
			log.Printf("[configsync] failed to import skill '%s' for project %d: %v", name, project.ID, err)
			continue
		}
		existingNames[name] = true
		imported++
		log.Printf("[configsync] imported skill '%s' from disk for project %d (ID: %d)", name, project.ID, ps.ID)
	}

	if imported > 0 {
		cs.reportProgress(project.ID, "skills_import", "done", fmt.Sprintf("Imported %d skill(s) from disk", imported))
	}
	return nil
}

// importMCPsFromDisk reads .mcp.json from the project root and imports
// any MCP servers not already in the database as project-scoped servers.
// For existing project-scoped MCPs, updates the DB record if the file is newer.
func (cs *ConfigSyncer) importMCPsFromDisk(ctx context.Context, projectPath string, project *database.Project) error {
	mcpPath := filepath.Join(projectPath, ".mcp.json")
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		return nil // file doesn't exist, nothing to import
	}

	var mcpConfig struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &mcpConfig); err != nil {
		log.Printf("[configsync] failed to parse .mcp.json for project %d: %v", project.ID, err)
		return nil
	}

	if len(mcpConfig.MCPServers) == 0 {
		return nil
	}

	// Get .mcp.json file modification time for timestamp comparison
	var mcpFileMtime time.Time
	if info, err := os.Stat(mcpPath); err == nil {
		mcpFileMtime = info.ModTime()
	}

	// Build set of existing project MCP server names
	existing, _ := cs.db.ListProjectMCPServers(ctx, project.ID)
	existingNames := make(map[string]bool)
	for _, m := range existing {
		existingNames[m.Name] = true
	}

	// Also check global MCP servers
	globalServers, _ := cs.db.ListMCPServers(ctx)
	globalNames := make(map[string]bool)
	for _, m := range globalServers {
		globalNames[m.Name] = true
	}

	imported := 0
	updated := 0
	for name, rawServer := range mcpConfig.MCPServers {
		var server struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal(rawServer, &server); err != nil {
			log.Printf("[configsync] failed to parse MCP server '%s': %v", name, err)
			continue
		}
		if server.Command == "" {
			continue
		}

		argsJSON, _ := json.Marshal(server.Args)
		if server.Args == nil {
			argsJSON = []byte("[]")
		}
		envJSON, _ := json.Marshal(server.Env)
		if server.Env == nil {
			envJSON = []byte("{}")
		}

		if existingNames[name] {
			// Project MCP exists — update if file is newer
			if !mcpFileMtime.IsZero() {
				if dbMCP, err := cs.db.GetProjectMCPServerByName(ctx, project.ID, name); err == nil {
					if mcpFileMtime.After(dbMCP.UpdatedAt.Add(time.Second)) {
						dbMCP.Command = server.Command
						dbMCP.Args = string(argsJSON)
						dbMCP.Env = string(envJSON)
						if err := cs.db.UpdateProjectMCPServer(ctx, dbMCP); err != nil {
							log.Printf("[configsync] failed to update MCP server '%s' for project %d: %v", name, project.ID, err)
						} else {
							updated++
							log.Printf("[configsync] updated MCP server '%s' from .mcp.json for project %d (file newer)", name, project.ID)
						}
					}
				}
			}
			continue
		}
		if globalNames[name] {
			// Global MCP — never update from project file
			continue
		}

		// New MCP server — import as project-scoped
		m := &database.ProjectMCPServer{
			ProjectID: project.ID,
			Name:      name,
			Command:   server.Command,
			Args:      string(argsJSON),
			Env:       string(envJSON),
			Enabled:   true,
		}
		if err := cs.db.CreateProjectMCPServer(ctx, m); err != nil {
			log.Printf("[configsync] failed to import MCP server '%s' for project %d: %v", name, project.ID, err)
			continue
		}
		existingNames[name] = true
		imported++
		log.Printf("[configsync] imported MCP server '%s' from .mcp.json for project %d (ID: %d)", name, project.ID, m.ID)
	}

	if imported > 0 || updated > 0 {
		cs.reportProgress(project.ID, "mcps_import", "done", fmt.Sprintf("Imported %d, updated %d MCP server(s) from .mcp.json", imported, updated))
	}
	return nil
}

// importSkillsFromRemote scans .claude/skills/ on a remote project via SFTP
// and imports untracked SKILL.md files as project-scoped skills.
func (cs *ConfigSyncer) importSkillsFromRemote(ctx context.Context, sftpClient *sftp.Client, skillsDir string, project *database.Project) error {
	entries, err := sftpClient.ReadDir(skillsDir)
	if err != nil {
		return nil // directory doesn't exist yet
	}

	// Build set of already-tracked dirs
	syncedFiles, _ := cs.db.ListSyncedSkillFiles(ctx, project.ID)
	trackedDirs := make(map[string]bool)
	for _, sf := range syncedFiles {
		dirName := sf.FileName
		if strings.HasSuffix(dirName, "/SKILL.md") {
			dirName = strings.TrimSuffix(dirName, "/SKILL.md")
		}
		trackedDirs[dirName] = true
	}

	// Build existing name sets
	existingSkills, _ := cs.db.ListProjectSkills(ctx, project.ID)
	existingNames := make(map[string]bool)
	for _, ps := range existingSkills {
		existingNames[ps.Name] = true
	}
	globalSkills, _ := cs.db.ListSkills(ctx)
	globalNames := make(map[string]bool)
	for _, s := range globalSkills {
		globalNames[s.Name] = true
	}

	imported := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		if trackedDirs[dirName] {
			continue
		}

		skillPath := filepath.Join(skillsDir, dirName, "SKILL.md")
		f, err := sftpClient.Open(skillPath)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			continue
		}

		name, content, category := parseSkillMD(string(raw), dirName)
		if existingNames[name] || globalNames[name] {
			continue
		}

		ps := &database.ProjectSkill{
			ProjectID: project.ID,
			Name:      name,
			Content:   content,
			Enabled:   true,
			Category:  category,
		}
		if err := cs.db.CreateProjectSkill(ctx, ps); err != nil {
			continue
		}
		existingNames[name] = true
		imported++
		log.Printf("[configsync] imported skill '%s' from remote for project %d", name, project.ID)
	}

	if imported > 0 {
		cs.reportProgress(project.ID, "skills_import", "done", fmt.Sprintf("Imported %d skill(s) from remote", imported))
	}
	return nil
}

// importMCPsFromRemote reads .mcp.json from a remote project via SFTP
// and imports untracked MCP servers as project-scoped servers.
// For existing project-scoped MCPs, updates the DB record if the file is newer.
func (cs *ConfigSyncer) importMCPsFromRemote(ctx context.Context, sftpClient *sftp.Client, projectPath string, project *database.Project) error {
	mcpPath := filepath.Join(projectPath, ".mcp.json")
	f, err := sftpClient.Open(mcpPath)
	if err != nil {
		return nil
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil
	}

	var mcpConfig struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &mcpConfig); err != nil {
		return nil
	}
	if len(mcpConfig.MCPServers) == 0 {
		return nil
	}

	// Get remote .mcp.json file modification time
	var mcpFileMtime time.Time
	if info, err := sftpClient.Stat(mcpPath); err == nil {
		mcpFileMtime = info.ModTime()
	}

	existing, _ := cs.db.ListProjectMCPServers(ctx, project.ID)
	existingNames := make(map[string]bool)
	for _, m := range existing {
		existingNames[m.Name] = true
	}
	globalServers, _ := cs.db.ListMCPServers(ctx)
	globalNames := make(map[string]bool)
	for _, m := range globalServers {
		globalNames[m.Name] = true
	}

	imported := 0
	updated := 0
	for name, rawServer := range mcpConfig.MCPServers {
		var server struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal(rawServer, &server); err != nil || server.Command == "" {
			continue
		}

		argsJSON, _ := json.Marshal(server.Args)
		if server.Args == nil {
			argsJSON = []byte("[]")
		}
		envJSON, _ := json.Marshal(server.Env)
		if server.Env == nil {
			envJSON = []byte("{}")
		}

		if existingNames[name] {
			// Project MCP exists — update if file is newer
			if !mcpFileMtime.IsZero() {
				if dbMCP, err := cs.db.GetProjectMCPServerByName(ctx, project.ID, name); err == nil {
					if mcpFileMtime.After(dbMCP.UpdatedAt.Add(time.Second)) {
						dbMCP.Command = server.Command
						dbMCP.Args = string(argsJSON)
						dbMCP.Env = string(envJSON)
						if err := cs.db.UpdateProjectMCPServer(ctx, dbMCP); err == nil {
							updated++
							log.Printf("[configsync] updated MCP server '%s' from remote .mcp.json for project %d (file newer)", name, project.ID)
						}
					}
				}
			}
			continue
		}
		if globalNames[name] {
			// Global MCP — never update from project file
			continue
		}

		// New MCP server — import as project-scoped
		m := &database.ProjectMCPServer{
			ProjectID: project.ID,
			Name:      name,
			Command:   server.Command,
			Args:      string(argsJSON),
			Env:       string(envJSON),
			Enabled:   true,
		}
		if err := cs.db.CreateProjectMCPServer(ctx, m); err != nil {
			continue
		}
		existingNames[name] = true
		imported++
	}

	if imported > 0 || updated > 0 {
		cs.reportProgress(project.ID, "mcps_import", "done", fmt.Sprintf("Imported %d, updated %d MCP server(s) from remote .mcp.json", imported, updated))
	}
	return nil
}

// buildSkillMD generates a SKILL.md with YAML frontmatter from DB fields.
// If the content already has frontmatter (starts with ---), it is used as-is.
func buildSkillMD(name string, content string) string {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "---") {
		// Content already has frontmatter — use as-is
		return trimmed + "\n"
	}

	// Extract description from first markdown heading or first line
	description := "OpenPoet managed skill: " + name
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			description = strings.TrimPrefix(line, "# ")
			break
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			description = line
			if len(description) > 120 {
				description = description[:120]
			}
			break
		}
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + name + "\n")
	sb.WriteString("description: " + description + "\n")
	sb.WriteString("user-invocable: false\n")
	sb.WriteString("---\n\n")
	sb.WriteString(trimmed)
	sb.WriteString("\n")
	return sb.String()
}

// buildHooksConfig returns the hooks configuration that OpenPoet manages.
// This is the only section OpenPoet writes to a project's settings.json.
func (cs *ConfigSyncer) buildHooksConfig() map[string]interface{} {
	hookCommand := map[string]interface{}{
		"type":    "command",
		"command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/bridge.sh",
	}
	hookEntry := []interface{}{
		map[string]interface{}{
			"hooks": []interface{}{hookCommand},
		},
	}

	return map[string]interface{}{
		"PermissionRequest":  hookEntry,
		"PreToolUse":         hookEntry,
		"PostToolUse":        hookEntry,
		"PostToolUseFailure": hookEntry,
		"Notification":       hookEntry,
		"Stop":               hookEntry,
		"UserPromptSubmit":   hookEntry,
	}
}

// buildCopilotHooksConfig generates the hooks.json configuration for Copilot CLI.
func (cs *ConfigSyncer) buildCopilotHooksConfig() map[string]interface{} {
	hookCommand := map[string]interface{}{
		"type":    "command",
		"command": ".github/hooks/openpoet-bridge.sh",
	}
	hookEntry := []interface{}{
		map[string]interface{}{
			"hooks": []interface{}{hookCommand},
		},
	}

	return map[string]interface{}{
		"version": 1,
		"hooks": map[string]interface{}{
			"preToolUse":          hookEntry,
			"postToolUse":         hookEntry,
			"userPromptSubmitted": hookEntry,
			"sessionStart":        hookEntry,
			"sessionEnd":          hookEntry,
		},
	}
}

// syncCopilotBridgeScriptLocal writes the Copilot bridge script to .github/hooks/
func (cs *ConfigSyncer) syncCopilotBridgeScriptLocal(hooksDir string) error {
	bridgeScript := `#!/bin/bash
# OpenPoet Hook Bridge Script for GitHub Copilot CLI

HOOK_URL="${OPENPOET_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${OPENPOET_SESSION_ID}"
INPUT=$(cat)

EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

translate_response() {
    local RESPONSE="$1"
    if [ -z "$RESPONSE" ]; then return; fi
    if command -v jq &>/dev/null; then
        echo "$RESPONSE" | jq -c '.hookSpecificOutput.hookEventName = "preToolUse" |
            if .hookSpecificOutput.decision.behavior == "allow" then .hookSpecificOutput.decision.behavior = "approve" else . end' 2>/dev/null
    else
        echo "$RESPONSE" | sed 's/"behavior":"allow"/"behavior":"approve"/g' | sed 's/"hookEventName":"PermissionRequest"/"hookEventName":"preToolUse"/g'
    fi
}

case "$EVENT" in
    preToolUse)
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            translate_response "$RESPONSE"
        fi
        ;;
    postToolUse|userPromptSubmitted|sessionStart|sessionEnd)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
`
	bridgePath := filepath.Join(hooksDir, "openpoet-bridge.sh")
	if err := os.WriteFile(bridgePath, []byte(bridgeScript), 0755); err != nil {
		return err
	}
	return nil
}

// syncSkillsToCopilotInstructions concatenates all enabled skills into .github/copilot-instructions.md
func (cs *ConfigSyncer) syncSkillsToCopilotInstructions(ctx context.Context, githubDir string, project *database.Project) error {
	skills, err := cs.getSkillsForProject(ctx, project)
	if err != nil {
		return err
	}

	var sb strings.Builder
	sb.WriteString("# OpenPoet Custom Instructions\n\n")
	for _, skill := range skills {
		sb.WriteString("## " + skill.Name + "\n\n")
		sb.WriteString(skill.Content + "\n\n")
	}

	instructionsPath := filepath.Join(githubDir, "copilot-instructions.md")
	return os.WriteFile(instructionsPath, []byte(sb.String()), 0644)
}

// syncBridgeScriptLocal copies bridge.sh to the project's .claude/hooks/ directory
func (cs *ConfigSyncer) syncBridgeScriptLocal(hooksDir string) error {
	bridgeScript := `#!/bin/bash
# OpenPoet Hook Bridge Script
# Receives hook event JSON on stdin, POSTs to OpenPoet API, returns response on stdout.

HOOK_URL="${OPENPOET_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${OPENPOET_SESSION_ID}"
INPUT=$(cat)

EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

case "$EVENT" in
    PermissionRequest)
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            echo "$RESPONSE"
        fi
        ;;
    PreToolUse|PostToolUse|PostToolUseFailure|Notification|Stop|UserPromptSubmit)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
`
	bridgePath := filepath.Join(hooksDir, "bridge.sh")
	if err := os.WriteFile(bridgePath, []byte(bridgeScript), 0755); err != nil {
		return err
	}
	return nil
}

// syncBridgeScriptRemote copies bridge.sh to the remote project's .claude/hooks/ directory
func (cs *ConfigSyncer) syncBridgeScriptRemote(sshClient *ssh.Client, sftpClient *sftp.Client, hooksDir string) error {
	bridgeScript := `#!/bin/bash
HOOK_URL="${OPENPOET_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${OPENPOET_SESSION_ID}"
INPUT=$(cat)

EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

case "$EVENT" in
    PermissionRequest)
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            echo "$RESPONSE"
        fi
        ;;
    PreToolUse|PostToolUse|PostToolUseFailure|Notification|Stop|UserPromptSubmit)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
`
	bridgePath := filepath.Join(hooksDir, "bridge.sh")
	f, err := sftpClient.Create(bridgePath)
	if err != nil {
		return fmt.Errorf("failed to create bridge.sh on remote: %w", err)
	}
	f.Write([]byte(bridgeScript))
	f.Close()

	// Make executable
	session, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session for chmod: %w", err)
	}
	defer session.Close()
	session.Run("chmod +x " + bridgePath)

	return nil
}

func (cs *ConfigSyncer) buildSSHConfig(project *database.Project) (*ssh.ClientConfig, error) {
	authType := project.SSHAuthType.String
	user := project.SSHUser.String

	var authMethods []ssh.AuthMethod

	if authType == "password" {
		password, err := cs.decryptFunc(
			project.SSHCredentialEncrypted.String,
			project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.Password(password))
	} else if authType == "key" || authType == "key_passphrase" {
		keyData, err := cs.decryptFunc(
			project.SSHCredentialEncrypted.String,
			project.SSHCredentialIV.String,
		)
		if err != nil {
			return nil, err
		}

		signer, err := ssh.ParsePrivateKey([]byte(keyData))
		if err != nil {
			return nil, err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	} else {
		// Try default keys
		homeDir, _ := os.UserHomeDir()
		keyPaths := []string{
			homeDir + "/.ssh/id_rsa",
			homeDir + "/.ssh/id_ed25519",
			homeDir + "/.ssh/id_ecdsa",
		}
		for _, keyPath := range keyPaths {
			if keyData, err := os.ReadFile(keyPath); err == nil {
				if signer, err := ssh.ParsePrivateKey(keyData); err == nil {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
					break
				}
			}
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}, nil
}

// SyncAllProjects syncs configuration to all projects
func (cs *ConfigSyncer) SyncAllProjects(ctx context.Context) error {
	projects, err := cs.db.ListProjects(ctx)
	if err != nil {
		return err
	}

	var lastErr error
	for _, project := range projects {
		if err := cs.SyncToProject(ctx, &project); err != nil {
			lastErr = err
			// Continue with other projects
		} else {
			cs.db.UpdateProjectConfigSyncedAt(ctx, project.ID)
		}
	}

	return lastErr
}
