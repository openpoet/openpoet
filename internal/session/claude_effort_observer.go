package session

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// claudeEffortScreenPattern finds the effort Claude Code shows on its own
// screen: the banner ("Opus 5.5 with high effort") and the prompt footer
// ("● high · /effort"). The footer follows /effort changes.
var claudeEffortScreenPattern = regexp.MustCompile(`(?i)(?:\bwith (low|medium|high|xhigh|max) effort\b|\b(low|medium|high|xhigh|max) · /effort\b)`)

// claudeEffortScanInterval rate-limits screen renders per session.
const claudeEffortScanInterval = 3 * time.Second

var claudeEffortLastScan sync.Map // sessionID -> time.Time

// observeClaudeEffort records the effort a Claude Code session's TUI says it
// runs. Claude reports no effort through hooks or transcript; its screen is
// the runtime's own statement of it.
func (m *Manager) observeClaudeEffort(sessionID string) {
	m.mu.RLock()
	rs, ok := m.sessions[sessionID]
	isClaude := ok && rs != nil && rs.session != nil && BackendType(rs.session.Backend) == BackendClaudeCode
	current := ""
	if isClaude {
		current = rs.session.EffectiveEffort
	}
	m.mu.RUnlock()
	if !isClaude {
		return
	}
	now := time.Now()
	if last, ok := claudeEffortLastScan.Load(sessionID); ok && now.Sub(last.(time.Time)) < claudeEffortScanInterval {
		return
	}
	claudeEffortLastScan.Store(sessionID, now)
	if effort := claudeScreenEffort(m.ScreenText(sessionID)); effort != "" && effort != current {
		m.RecordEffectiveSettings(context.Background(), sessionID, "", effort)
	}
}

// claudeScreenEffort returns the last effort the screen states, preferring
// the footer (current) over the startup banner.
func claudeScreenEffort(screen string) string {
	matches := claudeEffortScreenPattern.FindAllStringSubmatch(screen, -1)
	effort := ""
	for _, m := range matches {
		if m[2] != "" {
			effort = m[2]
		} else if effort == "" {
			effort = m[1]
		}
	}
	return strings.ToLower(effort)
}
