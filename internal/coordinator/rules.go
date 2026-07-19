package coordinator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"openpoet/internal/database"
)

// Rule and severity vocabulary — pinned to the V59 CHECK constraints.
const (
	RuleFileOverlap     = "file_overlap"
	RuleSameTask        = "same_task"
	RuleSharedClaudeDir = "shared_claude_dir"

	SeverityInfo     = "info"
	SeverityWarn     = "warn"
	SeverityCritical = "critical"
)

// Incident is one open conflict, keyed by (rule, scope_key) with hysteresis:
// re-evidence bumps counters but re-emits a conflict.detected event at most
// once per hysteresisWindow.
type Incident struct {
	ID              string
	Rule            string
	Severity        string
	ProjectID       int64
	ScopeKey        string
	Sessions        []string
	State           string
	FirstDetectedAt time.Time
	LastEvidenceAt  time.Time
	EvidenceCount   int64
	Details         map[string]interface{}
}

// incidentFromRow rebuilds the in-memory incident from its durable row at
// startup, preserving identity (id), counters and hysteresis across restarts.
func incidentFromRow(row database.CoordinatorIncident) (*Incident, error) {
	var sessions []string
	if err := json.Unmarshal([]byte(row.SessionsJSON), &sessions); err != nil {
		return nil, fmt.Errorf("sessions_json: %w", err)
	}
	details := map[string]interface{}{}
	if row.DetailsJSON != "" {
		if err := json.Unmarshal([]byte(row.DetailsJSON), &details); err != nil {
			return nil, fmt.Errorf("details_json: %w", err)
		}
	}
	return &Incident{
		ID:              row.ID,
		Rule:            row.Rule,
		Severity:        row.Severity,
		ProjectID:       row.ProjectID,
		ScopeKey:        row.ScopeKey,
		Sessions:        sessions,
		State:           row.State,
		FirstDetectedAt: row.FirstDetectedAt,
		LastEvidenceAt:  row.LastEvidenceAt,
		EvidenceCount:   row.EvidenceCount,
		Details:         details,
	}, nil
}

func (i Incident) toRow() database.CoordinatorIncident {
	sessionsJSON, err := json.Marshal(i.Sessions)
	if err != nil {
		sessionsJSON = []byte("[]")
	}
	details := i.Details
	if details == nil {
		details = map[string]interface{}{}
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		detailsJSON = []byte("{}")
	}
	return database.CoordinatorIncident{
		ID:              i.ID,
		Rule:            i.Rule,
		Severity:        i.Severity,
		ProjectID:       i.ProjectID,
		ScopeKey:        i.ScopeKey,
		SessionsJSON:    string(sessionsJSON),
		State:           i.State,
		FirstDetectedAt: i.FirstDetectedAt.UTC(),
		LastEvidenceAt:  i.LastEvidenceAt.UTC(),
		EvidenceCount:   i.EvidenceCount,
		DetailsJSON:     string(detailsJSON),
	}
}

// noteWriteClaim registers a write claim and evaluates R1/R5 against every
// other live writer of the same (projectKey, rel). Precedence: a path under
// .claude/ is the configsync shared-state hazard (R5, warn), never a code
// conflict (R1, critical) — shared config contention must not read as a
// critical code collision.
func (c *Coordinator) noteWriteClaim(si *sessionInfo, rel, tool string, ts time.Time) {
	key := claimKey{projectKey: si.projectKey, rel: rel}
	set := c.claims[key]
	if set == nil {
		set = make(map[string]TouchKind)
		c.claims[key] = set
	}
	for otherID, kind := range set {
		if otherID == si.id || kind != KindWrite {
			continue
		}
		other := c.sessions[otherID]
		if other == nil || !other.live {
			continue
		}
		rule, severity := RuleFileOverlap, SeverityCritical
		if IsClaudeDirPath(rel) {
			rule, severity = RuleSharedClaudeDir, SeverityWarn
		}
		scope := si.projectKey + "|" + rel + "|" + pairKey(si.id, otherID)
		c.recordIncident(rule, severity, si.projectID, scope, sortedPair(si.id, otherID),
			map[string]interface{}{"path": rel, "tool": tool}, ts)
	}
	set[si.id] = KindWrite
}

// evalSameTask fires R3 whenever two live sessions share a task link — the
// duplicated-work smell, regardless of which files each touches.
func (c *Coordinator) evalSameTask(si *sessionInfo, ts time.Time) {
	if si.taskID == 0 || !si.live {
		return
	}
	for _, other := range c.sessions {
		if other == nil || other.id == si.id || !other.live || other.taskID != si.taskID {
			continue
		}
		scope := fmt.Sprintf("task:%d|%s", si.taskID, pairKey(si.id, other.id))
		c.recordIncident(RuleSameTask, SeverityWarn, si.projectID, scope, sortedPair(si.id, other.id),
			map[string]interface{}{"task_id": si.taskID}, ts)
	}
}

// recordIncident upserts the in-memory incident, marks it dirty for the
// flusher, applies emission hysteresis, and escalates new critical incidents.
// Caller holds c.mu.
func (c *Coordinator) recordIncident(rule, severity string, projectID int64, scopeKey string, sessions []string, details map[string]interface{}, ts time.Time) {
	key := rule + "|" + scopeKey
	inc, ok := c.incidents[key]
	fresh := false
	if !ok {
		fresh = true
		inc = &Incident{
			ID:              newIncidentID(),
			Rule:            rule,
			Severity:        severity,
			ProjectID:       projectID,
			ScopeKey:        scopeKey,
			Sessions:        sessions,
			State:           "open",
			FirstDetectedAt: ts,
			LastEvidenceAt:  ts,
			EvidenceCount:   1,
			Details:         details,
		}
		c.incidents[key] = inc
	} else {
		inc.EvidenceCount++
		inc.LastEvidenceAt = ts
	}
	c.dirtyIncidents[key] = struct{}{}
	if last, seen := c.lastEmit[key]; !seen || ts.Sub(last) >= hysteresisWindow {
		c.lastEmit[key] = ts
		c.queueEventLocked(conflictDetectedEvent(*inc, ts))
	}
	if fresh && severity == SeverityCritical {
		if c.OnEscalate != nil {
			// Rate-limit human escalation per project: a parallel refactor touching
			// 30 shared files is ONE situation, not 30 webpush bursts. The incident
			// rows and conflict.detected events still record every conflict.
			if last, seen := c.lastEscalate[projectID]; !seen || ts.Sub(last) >= escalateCooldown {
				c.lastEscalate[projectID] = ts
				// Escalation does notification/DB work — never on the indexer goroutine.
				go c.OnEscalate(*inc)
			}
		}
		if c.OnBrainConsult != nil {
			// One-shot per incident: `fresh` is true exactly once per incident
			// key, so no extra guard is needed. NOT per-project rate-limited —
			// each distinct critical incident gets its own consult (the brain's
			// own daily budget bounds total spend).
			go c.OnBrainConsult(*inc)
		}
	}
}

// escalateCooldown is the per-project floor between human escalations.
const escalateCooldown = 60 * time.Second

func pairKey(a, b string) string {
	p := sortedPair(a, b)
	return p[0] + "+" + p[1]
}

func sortedPair(a, b string) []string {
	pair := []string{a, b}
	sort.Strings(pair)
	return pair
}

func newIncidentID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("C-%d", time.Now().UnixNano())
	}
	return "C-" + hex.EncodeToString(buf)
}
