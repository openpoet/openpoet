package configsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"openpoet/internal/database"
)

// Phase 7.3 (Maestro): remote workspace lanes. A lane on an SSH host gets the
// same materialize-only agent-config layer as a local lane — bridge, skills,
// hooks settings, CLAUDE.md, docs steering — with the same hard invariant:
// ZERO database writes (no skill import, no synced-file tracking, no memory
// write-back). N remote lanes must never ping-pong state through the DB.

// dialRemote opens the SSH+SFTP pair for a remote project/lane.
func (cs *ConfigSyncer) dialRemote(project *database.Project) (*ssh.Client, *sftp.Client, error) {
	config, err := cs.buildSSHConfig(project)
	if err != nil {
		return nil, nil, err
	}
	addr := fmt.Sprintf("%s:%d", project.SSHHost.String, project.SSHPort.Int64)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH connection failed: %w", err)
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("SFTP connection failed: %w", err)
	}
	return client, sftpClient, nil
}

// materializeToRemoteLane provisions a REMOTE lane's .claude layer over SFTP.
// laneProject is the shallow Project copy whose Path is the lane directory on
// the remote host.
func (cs *ConfigSyncer) materializeToRemoteLane(ctx context.Context, laneProject *database.Project) error {
	client, sftpClient, err := cs.dialRemote(laneProject)
	if err != nil {
		return err
	}
	defer client.Close()
	defer sftpClient.Close()

	claudeDir := filepath.Join(laneProject.Path, ".claude")
	skillsDir := filepath.Join(claudeDir, "skills")
	hooksDir := filepath.Join(claudeDir, "hooks")
	if err := sftpClient.MkdirAll(skillsDir); err != nil {
		return fmt.Errorf("create remote lane skills dir: %w", err)
	}
	if err := sftpClient.MkdirAll(hooksDir); err != nil {
		return fmt.Errorf("create remote lane hooks dir: %w", err)
	}

	if err := cs.syncBridgeScriptRemote(client, sftpClient, hooksDir); err != nil {
		return fmt.Errorf("materialize bridge.sh to remote lane: %w", err)
	}

	// Skills: DB → SFTP only, no synced_skill_files tracking (that ledger is
	// per-project state the lane must not touch).
	skills, err := cs.getSkillsForProject(ctx, laneProject)
	if err != nil {
		return err
	}
	for _, skill := range skills {
		dir := filepath.Join(skillsDir, skill.Name)
		if err := sftpClient.MkdirAll(dir); err != nil {
			return err
		}
		f, err := sftpClient.Create(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			return err
		}
		_, writeErr := f.Write([]byte(buildSkillMD(skill.Name, skill.Content)))
		f.Close()
		if writeErr != nil {
			return writeErr
		}
	}

	// Hooks block into the lane's settings.json (merge, keep other settings).
	settingsPath := filepath.Join(claudeDir, "settings.json")
	existing := make(map[string]interface{})
	if f, err := sftpClient.Open(settingsPath); err == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		_ = json.Unmarshal(data, &existing)
	}
	existing["hooks"] = cs.buildHooksConfig()
	settingsJSON, _ := json.MarshalIndent(existing, "", "  ")
	f, err := sftpClient.Create(settingsPath)
	if err != nil {
		return fmt.Errorf("create remote lane settings.json: %w", err)
	}
	if _, err := f.Write(settingsJSON); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// Memory doc: DB → file only (never import from the lane).
	if doc, docErr := cs.db.GetMemoryDoc(ctx, laneProject.ID); docErr == nil {
		if mf, createErr := sftpClient.Create(filepath.Join(laneProject.Path, "CLAUDE.md")); createErr == nil {
			_, _ = mf.Write([]byte(doc.Content))
			mf.Close()
		}
	}

	// Managed docs-steering file (same location as local lanes).
	steering := OpenPoetDocsInstructionBlock()
	steeringPath := filepath.Join(claudeDir, "CLAUDE.md")
	current := ""
	if sf, openErr := sftpClient.Open(steeringPath); openErr == nil {
		data, _ := io.ReadAll(sf)
		sf.Close()
		current = string(data)
	}
	updated := upsertDocsSteeringContent(current, steering)
	if updated != current {
		sf, createErr := sftpClient.Create(steeringPath)
		if createErr != nil {
			return fmt.Errorf("create remote lane steering file: %w", createErr)
		}
		_, writeErr := sf.Write([]byte(updated))
		sf.Close()
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}

// EnsureRemoteExcludeLine appends a pattern to the remote repo's
// .git/info/exclude once (the remote counterpart of ensureExcludeLine).
func (cs *ConfigSyncer) EnsureRemoteExcludeLine(ctx context.Context, project *database.Project, line string) error {
	client, sftpClient, err := cs.dialRemote(project)
	if err != nil {
		return err
	}
	defer client.Close()
	defer sftpClient.Close()

	gitDir := filepath.Join(project.Path, ".git")
	info, err := sftpClient.Stat(gitDir)
	if err != nil {
		return fmt.Errorf("remote project has no .git directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("remote project .git is not a directory (nested worktree?)")
	}
	infoDir := filepath.Join(gitDir, "info")
	if err := sftpClient.MkdirAll(infoDir); err != nil {
		return err
	}
	excludePath := filepath.Join(infoDir, "exclude")
	existing := ""
	if f, openErr := sftpClient.Open(excludePath); openErr == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		existing = string(data)
	}
	for _, existingLine := range strings.Split(existing, "\n") {
		if strings.TrimSpace(existingLine) == line {
			return nil
		}
	}
	f, err := sftpClient.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte("\n" + line + "\n"))
	return err
}

// ReadRemoteProjectFile reads a project-relative file over SFTP ("" flag when
// absent) — the remote counterpart of the local os.ReadFile seams.
func (cs *ConfigSyncer) ReadRemoteProjectFile(ctx context.Context, project *database.Project, relPath string) ([]byte, error) {
	client, sftpClient, err := cs.dialRemote(project)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	defer sftpClient.Close()
	f, err := sftpClient.Open(filepath.Join(project.Path, relPath))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
