package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"openpoet/internal/coordinator"
	"openpoet/internal/database"
)

// ansiPattern + secret patterns mirror the automation package's redactor
// (boundedExecutionText) so the brief sent to the provider carries no raw ANSI
// and no leaked credentials — the brain talks to an external model, so the same
// hygiene that guards session output must guard the brief.
var (
	ansiPattern         = regexp.MustCompile("\\x1b\\[[?0-9;]*[a-zA-Z]|\\x1b\\][^\\x07]*(?:\\x07|\\x1b\\\\)|\\x1b[^[\\]].?")
	namedSecretPattern  = regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?(?:TOKEN|KEY)|REFRESH[_-]?TOKEN|AUTH[_-]?TOKEN|PRIVATE[_-]?KEY|SSH[_-]?KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?)[A-Z0-9_]*\b\s*[:=]\s*[^\s,;]+`)
	opaqueSecretPattern = regexp.MustCompile(`(?i)\b(?:opav1_[A-Za-z0-9._-]{6,}|op[sh]t1_[A-Za-z0-9._-]{6,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`)
)

func sanitize(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	s = namedSecretPattern.ReplaceAllString(s, "[REDACTED]")
	s = opaqueSecretPattern.ReplaceAllString(s, "[REDACTED]")
	return s
}

// buildBrief assembles the sanitized incident brief for the provider.
func (c *Consultant) buildBrief(ctx context.Context, inc coordinator.Incident, project *database.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CONFLICT INCIDENT %s\n", inc.ID)
	fmt.Fprintf(&b, "project: %s (mode=%s)\n", sanitize(project.Name), project.CoordinatorMode)
	fmt.Fprintf(&b, "rule: %s  severity: %s\n", inc.Rule, inc.Severity)
	fmt.Fprintf(&b, "sessions: %s\n", strings.Join(inc.Sessions, ", "))
	if inc.Details != nil {
		if path, ok := inc.Details["path"].(string); ok {
			fmt.Fprintf(&b, "contested_path: %s\n", sanitize(path))
		}
	}
	for _, sid := range inc.Sessions {
		rows, err := c.db.ListSessionFileActivity(ctx, sid, 20)
		if err != nil || len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "files touched by %s:\n", sid)
		for _, r := range rows {
			fmt.Fprintf(&b, "  - %s (%s)\n", sanitize(r.Path), r.Kind)
		}
	}
	brief := sanitize(b.String())
	if len(brief) > c.briefMaxLen {
		brief = brief[:c.briefMaxLen] + "\n...[TRUNCATED]"
	}
	return brief
}

var jsonFence = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// parseAction extracts a single JSON action object from the model text.
func parseAction(text string) (action, error) {
	trimmed := strings.TrimSpace(text)
	if m := jsonFence.FindStringSubmatch(trimmed); len(m) == 2 {
		trimmed = strings.TrimSpace(m[1])
	}
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return action{}, errors.New("no JSON object in model response")
	}
	var a action
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &a); err != nil {
		return action{}, err
	}
	if strings.TrimSpace(a.Action) == "" {
		return action{}, errors.New("action verb missing")
	}
	return a, nil
}

func (c *Consultant) dailyCap(ctx context.Context) int {
	raw, err := c.db.GetSetting(ctx, c.budgetKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return c.defaultCap
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return c.defaultCap
	}
	return n
}

// sessionExists returns true only when the session exists AND belongs to the
// incident's project. The project gate is a hard tenancy boundary: the brain
// reasons over one project's incident, so a proposed stop/message/set_model may
// only ever touch a session in THAT project — a hallucinated (or adversarial)
// cross-project session id is rejected, not steered.
func (c *Consultant) sessionExists(ctx context.Context, id string, projectID int64) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	s, err := c.db.GetSession(ctx, id)
	return err == nil && s != nil && s.ID == id && s.ProjectID == projectID
}

// taskExists likewise confirms the task is in the incident's project so a
// spawn can't be aimed at another tenant's task.
func (c *Consultant) taskExists(ctx context.Context, id int64, projectID int64) bool {
	t, err := c.db.GetTask(ctx, id)
	return err == nil && t != nil && t.ProjectID == projectID
}

// record persists the consult outcome AND emits the durable coordinator.consulted
// outbox event in one shot, then wakes long-poll awaiters.
func (c *Consultant) record(ctx context.Context, inc coordinator.Incident, actionVerb, decision string, cost float64) {
	if err := c.db.RecordCoordinatorConsult(ctx, database.CoordinatorConsult{
		IncidentID: inc.ID, ProjectID: inc.ProjectID, Action: actionVerb, Decision: decision, CostUSD: cost,
	}); err != nil {
		return
	}
	c.emitConsultedEvent(ctx, inc, actionVerb, decision)
}

func (c *Consultant) emitConsultedEvent(ctx context.Context, inc coordinator.Incident, actionVerb, decision string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"incident_id": inc.ID, "project_id": inc.ProjectID,
		"action": actionVerb, "decision": decision,
	})
	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := database.AppendEventOutbox(ctx, tx, database.EventOutboxAppend{
		EventID: uuid.NewString(), EventType: "coordinator.consulted",
		AggregateType: "conflict", AggregateID: inc.ID, Actor: "coordinator",
		SchemaVersion: 1, PayloadJSON: string(payload), OccurredAt: c.now().UTC(),
	}); err != nil {
		return
	}
	if err := tx.Commit(); err == nil {
		c.db.NotifyOutboxAppended()
	}
}

func (c *Consultant) escalateIncident(ctx context.Context, inc coordinator.Incident, title, body, _ string, _ float64) {
	if c.escalate == nil {
		return
	}
	// The body carries model-controlled text (proposed reasons, hallucinated
	// verbs/session-ids) into a rendered notification. Strip ANSI/secrets AND
	// HTML-escape it: it must be unable to leak a credential or inject markup.
	safeBody := html.EscapeString(sanitize(body))
	c.escalate(ctx, inc.ID, title, fmt.Sprintf("%s — incident %s (%s)", safeBody, inc.ID, inc.Rule), map[string]interface{}{
		"incident_id": inc.ID, "rule": inc.Rule, "project_id": inc.ProjectID,
	})
}

func strOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
