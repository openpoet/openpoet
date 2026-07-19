// Package brain is OpenPoet's pluggable coordinator brain (Phase 4). When the
// conflict radar opens a critical incident it fires ONE bounded LLM consult
// that returns a closed action vocabulary; this package RE-VALIDATES the
// proposed action against the per-project autonomy dial, a daily budget, and
// hallucination checks BEFORE executing it as the scoped 'coordinator'
// automation client. The LLM proposes; the deterministic policy decides; every
// destructive verb still routes through a human grant. The brain is not
// resident — it costs nothing when the fleet is healthy.
package brain

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"openpoet/internal/automation"
	"openpoet/internal/coordinator"
	"openpoet/internal/database"
	"openpoet/internal/llm"
)

// closed action vocabulary — anything else escalates.
const (
	verbEscalateHuman = "escalate_human"
	verbStopSession   = "stop_session"
	verbSpawnSession  = "spawn_session"
	verbMessage       = "message_session"
	verbSetModel      = "set_model"
	verbDismiss       = "dismiss"
)

var knownVerbs = map[string]struct{}{
	verbEscalateHuman: {}, verbStopSession: {}, verbSpawnSession: {},
	verbMessage: {}, verbSetModel: {}, verbDismiss: {},
}

// destructiveVerbs may only be proposed under delegate mode; observe/assist
// refuse-and-escalate them.
var destructiveVerbs = map[string]struct{}{
	verbStopSession: {}, verbSpawnSession: {},
}

type action struct {
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	SessionID string `json:"session_id"`
	TaskID    *int64 `json:"task_id"`
	Brief     string `json:"brief"`
	Model     string `json:"model"`
	Text      string `json:"text"`
}

// Escalator raises a proactive human notification. Provided by main.go (wraps
// AIHandler.CreateProactiveNotification + the notification service).
type Escalator func(ctx context.Context, incidentID, title, body string, extra map[string]interface{})

// Consultant runs one bounded consult per critical incident and enforces policy.
type Consultant struct {
	db       *database.DB
	registry *automation.PlatformCapabilityRegistry
	provider func() llm.Provider // resolves the ai_coordinator slot (nil = unconfigured)
	// mu serializes consults so the daily-budget check and the consult record
	// are atomic across the concurrent per-incident goroutines — otherwise two
	// consults both read count<cap before either records and both proceed.
	// Consults are infrequent (one per critical incident), so serializing is fine.
	mu          sync.Mutex
	personaFn   func(ctx context.Context) string
	escalate    Escalator
	now         func() time.Time
	budgetKey   string
	defaultCap  int
	briefMaxLen int
}

func NewConsultant(db *database.DB, registry *automation.PlatformCapabilityRegistry, provider func() llm.Provider, persona func(ctx context.Context) string, escalate Escalator) *Consultant {
	return &Consultant{
		db: db, registry: registry, provider: provider, personaFn: persona, escalate: escalate,
		now: time.Now, budgetKey: "coordinator_daily_budget", defaultCap: 50, briefMaxLen: 4000,
	}
}

// Consult is the OnBrainConsult callback: fired once per critical incident, off
// the indexer goroutine. It never panics the process — every failure path
// escalates or records and returns.
func (c *Consultant) Consult(inc coordinator.Incident) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Serialize consults: the budget check + record must be atomic across the
	// concurrent per-incident goroutines.
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.consult(ctx, inc); err != nil {
		// Last-resort escalation so a brain failure never swallows an incident.
		c.escalateIncident(ctx, inc, "Brain consult failed", err.Error(), "consult_error", 0)
	}
}

func (c *Consultant) consult(ctx context.Context, inc coordinator.Incident) error {
	project, err := c.db.GetProject(ctx, inc.ProjectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	mode := project.CoordinatorMode
	if mode == "" || mode == "off" {
		return nil // dial off: the brain never consults for this project.
	}
	// Daily budget: an over-budget incident is recorded (so the count is
	// visible) and escalated, but NOT consulted.
	cap := c.dailyCap(ctx)
	if used, _ := c.db.CountCoordinatorConsultsToday(ctx); used >= cap {
		c.record(ctx, inc, "", "over_budget", 0)
		c.escalateIncident(ctx, inc, "Coordinator over daily budget", fmt.Sprintf("Incident %s not consulted (daily budget %d reached)", inc.ID, cap), "", 0)
		return nil
	}

	provider := c.provider()
	if provider == nil {
		// Unconfigured slot: no spend, no consult — just the existing escalation.
		return nil
	}

	brief := c.buildBrief(ctx, inc, project)
	req := &llm.Request{
		System:    c.personaFn(ctx),
		Messages:  []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: brief}}}},
		MaxTokens: 512,
	}
	var text strings.Builder
	resp, err := provider.StreamMessage(ctx, req, func(ev llm.StreamEvent) error {
		if ev.Type == "content_block_delta" && ev.Delta != nil && ev.Delta.Type == "text_delta" {
			text.WriteString(ev.Delta.Text)
		}
		return nil
	})
	cost := 0.0
	if resp != nil {
		cost = resp.CostUSD
		if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
			usageCost := cost
			if usageCost == 0 {
				usageCost = llm.CalculateCost(resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)
			}
			_ = c.db.CreateTokenUsage(ctx, &database.TokenUsage{
				Source: "ai_assistant", Subcategory: "coordinator_consult", Model: resp.Model,
				InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
				CacheReadTokens: resp.Usage.CacheReadTokens, CacheCreationTokens: resp.Usage.CacheCreationTokens,
				CostUSD: usageCost,
			})
			cost = usageCost
		}
	}
	if err != nil {
		c.record(ctx, inc, "", "consult_error", cost)
		c.escalateIncident(ctx, inc, "Brain consult error", err.Error(), "", cost)
		return nil
	}

	act, parseErr := parseAction(text.String())
	if parseErr != nil {
		c.decideAndRecord(ctx, inc, mode, act, "invalid_json", cost)
		c.escalateIncident(ctx, inc, "Coordinator: unparseable action", "The brain returned no valid JSON action; escalating to a human.", "", cost)
		return nil
	}
	c.decideAndRecord(ctx, inc, mode, act, "", cost)
	return nil
}

// decideAndRecord is the deterministic policy layer: it re-validates the
// proposed verb (closed vocabulary, hallucination checks, dial gating) and
// either executes it as the coordinator client or escalates. `forced` overrides
// the decision label (used for the unparseable path).
func (c *Consultant) decideAndRecord(ctx context.Context, inc coordinator.Incident, mode string, act action, forced string, cost float64) {
	if forced != "" {
		c.record(ctx, inc, act.Action, forced, cost)
		return
	}
	verb := strings.TrimSpace(act.Action)
	if _, known := knownVerbs[verb]; !known {
		c.record(ctx, inc, verb, "invalid_action", cost)
		c.escalateIncident(ctx, inc, "Coordinator: unknown action", fmt.Sprintf("The brain proposed an unrecognized action %q; escalating.", verb), "", cost)
		return
	}
	// Dial gating: observe/assist may never DIRECTLY drive destructive verbs.
	if _, destructive := destructiveVerbs[verb]; destructive && mode != "delegate" {
		c.record(ctx, inc, verb, "refused_by_dial", cost)
		c.escalateIncident(ctx, inc, "Coordinator action refused by policy", fmt.Sprintf("Action %q is not permitted in %q mode; escalating for a human.", verb, mode), "", cost)
		return
	}

	switch verb {
	case verbEscalateHuman, verbDismiss:
		decision := "escalated"
		if verb == verbDismiss {
			decision = "dismissed"
		}
		c.record(ctx, inc, verb, decision, cost)
		if verb == verbEscalateHuman {
			c.escalateIncident(ctx, inc, "Coordinator escalation", strOr(act.Reason, "The coordinator escalated this conflict to a human."), "", cost)
		}
	case verbStopSession, verbMessage, verbSetModel:
		if !c.sessionExists(ctx, act.SessionID) {
			c.record(ctx, inc, verb, "invalid_session", cost)
			c.escalateIncident(ctx, inc, "Coordinator: hallucinated session", fmt.Sprintf("Action %q named a non-existent session %q; escalating.", verb, act.SessionID), "", cost)
			return
		}
		c.dispatchSessionVerb(ctx, inc, verb, act, cost)
	case verbSpawnSession:
		if act.TaskID != nil && *act.TaskID > 0 && !c.taskExists(ctx, *act.TaskID) {
			c.record(ctx, inc, verb, "invalid_task", cost)
			c.escalateIncident(ctx, inc, "Coordinator: hallucinated task", fmt.Sprintf("spawn_session named a non-existent task %d; escalating.", *act.TaskID), "", cost)
			return
		}
		c.dispatchSpawn(ctx, inc, act, cost)
	}
}
