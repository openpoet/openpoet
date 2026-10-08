package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// codexRolloutTailBytes bounds how much of a rollout is read to find its last
// turn_context: turns are short records, the bulk of a rollout is output.
const codexRolloutTailBytes = 512 << 10

// readCodexRolloutTurnContext returns the model and reasoning effort of the
// last turn_context recorded in a Codex rollout: what the runtime really ran.
func readCodexRolloutTurnContext(path string) (model, effort string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > codexRolloutTailBytes {
		if _, err := f.Seek(-codexRolloutTailBytes, io.SeekEnd); err != nil {
			return "", "", false
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"turn_context"`)) {
			continue
		}
		if m, e, found := parseCodexTurnContext(line); found {
			model, effort, ok = m, e, true
		}
	}
	return model, effort, ok
}

func parseCodexTurnContext(line []byte) (model, effort string, ok bool) {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Model             string  `json:"model"`
			Effort            *string `json:"effort"`
			ReasoningEffort   *string `json:"reasoning_effort"`
			CollaborationMode struct {
				Settings struct {
					Model           string  `json:"model"`
					ReasoningEffort *string `json:"reasoning_effort"`
				} `json:"settings"`
			} `json:"collaboration_mode"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil || record.Type != "turn_context" {
		return "", "", false
	}
	p := record.Payload
	model = strings.TrimSpace(p.Model)
	if model == "" {
		model = strings.TrimSpace(p.CollaborationMode.Settings.Model)
	}
	for _, candidate := range []*string{p.Effort, p.ReasoningEffort, p.CollaborationMode.Settings.ReasoningEffort} {
		if candidate != nil && strings.TrimSpace(*candidate) != "" {
			effort = strings.TrimSpace(*candidate)
			break
		}
	}
	return model, effort, model != "" || effort != ""
}

// RefreshCodexRolloutSettings records the model and effort of a Codex TUI
// session from the turn_context of its rollout, when the rollout is a file on
// this host (local projects). The app-server runtime reports them itself.
func (m *Manager) RefreshCodexRolloutSettings(ctx context.Context, sessionID, transcriptPath string) bool {
	transcriptPath = strings.TrimSpace(transcriptPath)
	if transcriptPath == "" || !filepath.IsAbs(transcriptPath) {
		return false
	}
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok || rs == nil || rs.session == nil || rs.remote || BackendType(rs.session.Backend) != BackendCodex {
		return false
	}
	if _, appServer := rs.runner.(*CodexRunner); appServer {
		return false
	}
	model, effort, found := readCodexRolloutTurnContext(transcriptPath)
	if !found {
		return false
	}
	m.RecordEffectiveSettings(ctx, sessionID, model, effort)
	return true
}
