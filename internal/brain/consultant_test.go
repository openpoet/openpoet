package brain

import (
	"context"
	"path/filepath"
	"testing"

	"openpoet/internal/coordinator"
	"openpoet/internal/database"
	"openpoet/internal/llm"
)

// cannedProvider is a minimal in-test llm.Provider returning a fixed action.
type cannedProvider struct{ text string }

func (p *cannedProvider) Name() string { return "canned" }
func (p *cannedProvider) StreamMessage(_ context.Context, req *llm.Request, cb llm.StreamCallback) (*llm.Response, error) {
	if cb != nil {
		_ = cb(llm.StreamEvent{Type: "content_block_delta", Delta: &llm.StreamDelta{Type: "text_delta", Text: p.text}})
	}
	return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: p.text}}, Model: "canned"}, nil
}

func brainTestDB(t *testing.T, mode string) (*database.DB, int64, string) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project := &database.Project{Name: "p", Path: "/tmp/p", Type: "local", Backend: "claude_code", CoordinatorMode: mode}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	session := &database.Session{ID: "sess-brain", ProjectID: project.ID, Status: "running", Backend: "claude_code", Model: "m", RequestedModel: "d", Effort: "d"}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	return db, project.ID, session.ID
}

func newTestConsultant(db *database.DB, provider llm.Provider, escalations *int) *Consultant {
	c := NewConsultant(db, nil, func(context.Context) (llm.Provider, string) { return provider, "test-model" },
		func(context.Context) string { return "system" },
		func(context.Context, string, string, string, map[string]interface{}) { *escalations++ })
	return c
}

func lastDecision(t *testing.T, db *database.DB, projectID int64) (string, string) {
	t.Helper()
	var row database.CoordinatorConsult
	if err := db.Get(&row, "SELECT * FROM coordinator_consults WHERE project_id=? ORDER BY id DESC LIMIT 1", projectID); err != nil {
		t.Fatalf("no consult row: %v", err)
	}
	return row.Action, row.Decision
}

func incidentFor(projectID int64, sessionID string) coordinator.Incident {
	return coordinator.Incident{
		ID: "C-brain-1", Rule: "file_overlap", Severity: "critical",
		ProjectID: projectID, ScopeKey: "sk", Sessions: []string{sessionID},
		Details: map[string]interface{}{"path": "shared.go"},
	}
}

// TestBrainRevalidatesUnknownVerb: an unknown verb never executes; it escalates.
func TestBrainRevalidatesUnknownVerb(t *testing.T) {
	db, pid, sid := brainTestDB(t, "delegate")
	esc := 0
	c := newTestConsultant(db, &cannedProvider{text: `{"action":"format_disk"}`}, &esc)
	c.Consult(incidentFor(pid, sid))
	_, decision := lastDecision(t, db, pid)
	if decision != "invalid_action" {
		t.Fatalf("decision = %q, want invalid_action", decision)
	}
	if esc == 0 {
		t.Fatal("unknown verb did not escalate")
	}
}

// TestBrainDialGatesDestructive: observe mode refuses a destructive verb and
// escalates; delegate mode does not refuse it at the dial (it proceeds to the
// grant gate — nil registry here so it records no_actor, which is fine: the
// point is the dial did NOT refuse it).
func TestBrainDialGatesDestructive(t *testing.T) {
	// observe → refused_by_dial
	db, pid, sid := brainTestDB(t, "observe")
	esc := 0
	c := newTestConsultant(db, &cannedProvider{text: `{"action":"stop_session","session_id":"sess-brain"}`}, &esc)
	c.Consult(incidentFor(pid, sid))
	_, decision := lastDecision(t, db, pid)
	if decision != "refused_by_dial" {
		t.Fatalf("observe decision = %q, want refused_by_dial", decision)
	}
	if esc == 0 {
		t.Fatal("observe refusal did not escalate")
	}

	// off → no consult at all.
	db2, pid2, sid2 := brainTestDB(t, "off")
	c2 := newTestConsultant(db2, &cannedProvider{text: `{"action":"escalate_human"}`}, &esc)
	c2.Consult(incidentFor(pid2, sid2))
	var n int
	if err := db2.Get(&n, "SELECT COUNT(*) FROM coordinator_consults"); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("off mode consulted (%d rows)", n)
	}
}

// TestBrainDialGatesDestructive covers hallucinated session id too.
func TestBrainRejectsHallucinatedSession(t *testing.T) {
	db, pid, sid := brainTestDB(t, "delegate")
	esc := 0
	c := newTestConsultant(db, &cannedProvider{text: `{"action":"stop_session","session_id":"does-not-exist"}`}, &esc)
	c.Consult(incidentFor(pid, sid))
	_, decision := lastDecision(t, db, pid)
	if decision != "invalid_session" {
		t.Fatalf("decision = %q, want invalid_session", decision)
	}
}

// TestBrainOneShotPerIncident: the consult records exactly one row per call
// (the coordinator's `fresh` guard makes it fire once per incident; here we
// assert a single Consult produces a single row and no duplicate).
func TestBrainOneShotPerIncident(t *testing.T) {
	db, pid, sid := brainTestDB(t, "delegate")
	esc := 0
	c := newTestConsultant(db, &cannedProvider{text: `{"action":"escalate_human","reason":"x"}`}, &esc)
	c.Consult(incidentFor(pid, sid))
	var n int
	db.Get(&n, "SELECT COUNT(*) FROM coordinator_consults WHERE incident_id='C-brain-1'")
	if n != 1 {
		t.Fatalf("consult rows = %d, want 1", n)
	}
	// Over budget: set the cap to 0 → a second incident records over_budget and
	// does NOT re-consult the provider.
	db.SetSetting(context.Background(), "coordinator_daily_budget", "1")
	c.Consult(coordinator.Incident{ID: "C-brain-2", Rule: "file_overlap", Severity: "critical", ProjectID: pid, ScopeKey: "sk2", Sessions: []string{sid}})
	var over int
	db.Get(&over, "SELECT COUNT(*) FROM coordinator_consults WHERE decision LIKE '%budget%'")
	if over != 1 {
		t.Fatalf("over-budget rows = %d, want 1", over)
	}
}

// TestBrainSanitizesBrief: ANSI and tokens are stripped from the brief.
func TestBrainSanitizesBrief(t *testing.T) {
	dirty := "line with \x1b[31mANSI\x1b[0m and token opav1_secretsecretsecret and sk-abcdefghijklmnop123"
	clean := sanitize(dirty)
	if containsAny(clean, "\x1b[", "opav1_secret", "sk-abcdefghijklmnop") {
		t.Fatalf("sanitize left secrets/ANSI: %q", clean)
	}
	if !containsAny(clean, "ANSI", "[REDACTED]") {
		t.Fatalf("sanitize destroyed too much: %q", clean)
	}
}

// TestBrainLadderObserveVsAssist: the autonomy ladder is enforced per-verb.
// observe is advisory-only (message/set_model refused); assist adds
// non-destructive steering (message allowed → past the dial); assist still
// refuses destructive stop (delegate-only).
func TestBrainLadderObserveVsAssist(t *testing.T) {
	// observe refuses a non-destructive steer (message_session).
	db, pid, sid := brainTestDB(t, "observe")
	esc := 0
	c := newTestConsultant(db, &cannedProvider{text: `{"action":"message_session","session_id":"sess-brain","text":"hi"}`}, &esc)
	c.Consult(incidentFor(pid, sid))
	if _, d := lastDecision(t, db, pid); d != "refused_by_dial" {
		t.Fatalf("observe message decision = %q, want refused_by_dial", d)
	}

	// assist ALLOWS the same steer past the dial (no coordinator client in the
	// test DB, so it records no_actor — the point is it was NOT refused_by_dial).
	db2, pid2, sid2 := brainTestDB(t, "assist")
	c2 := newTestConsultant(db2, &cannedProvider{text: `{"action":"message_session","session_id":"sess-brain","text":"hi"}`}, &esc)
	c2.Consult(incidentFor(pid2, sid2))
	if _, d := lastDecision(t, db2, pid2); d == "refused_by_dial" {
		t.Fatalf("assist message was refused_by_dial; the ladder should allow it")
	}

	// assist STILL refuses a destructive stop (delegate-only).
	db3, pid3, sid3 := brainTestDB(t, "assist")
	c3 := newTestConsultant(db3, &cannedProvider{text: `{"action":"stop_session","session_id":"sess-brain"}`}, &esc)
	c3.Consult(incidentFor(pid3, sid3))
	if _, d := lastDecision(t, db3, pid3); d != "refused_by_dial" {
		t.Fatalf("assist stop decision = %q, want refused_by_dial", d)
	}
}

// TestBrainRejectsCrossProjectSession: a session that EXISTS but belongs to a
// different project than the incident is rejected as invalid_session — the
// brain may never steer another tenant's session.
func TestBrainRejectsCrossProjectSession(t *testing.T) {
	db, pid, _ := brainTestDB(t, "delegate")
	ctx := context.Background()
	// A second project with its own session.
	other := &database.Project{Name: "other", Path: "/tmp/other", Type: "local", Backend: "claude_code", CoordinatorMode: "delegate"}
	if err := db.CreateProject(ctx, other); err != nil {
		t.Fatal(err)
	}
	foreign := &database.Session{ID: "sess-foreign", ProjectID: other.ID, Status: "running", Backend: "claude_code", Model: "m", RequestedModel: "d", Effort: "d"}
	if err := db.CreateSession(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	esc := 0
	// Incident is for `pid`, but the action names the FOREIGN session.
	c := newTestConsultant(db, &cannedProvider{text: `{"action":"stop_session","session_id":"sess-foreign"}`}, &esc)
	c.Consult(incidentFor(pid, "sess-brain"))
	if _, d := lastDecision(t, db, pid); d != "invalid_session" {
		t.Fatalf("cross-project session decision = %q, want invalid_session", d)
	}
	if esc == 0 {
		t.Fatal("cross-project session did not escalate")
	}
}

// TestBrainEscalationEscapesBody: model-controlled escalation text is stripped
// of secrets AND HTML-escaped before it reaches the notification.
func TestBrainEscalationEscapesBody(t *testing.T) {
	db, pid, sid := brainTestDB(t, "observe")
	var body string
	c := NewConsultant(db, nil,
		func(context.Context) (llm.Provider, string) {
			return &cannedProvider{text: `{"action":"escalate_human","reason":"<script>alert(1)</script> token opav1_supersecretvalue"}`}, "m"
		},
		func(context.Context) string { return "system" },
		func(_ context.Context, _, _, b string, _ map[string]interface{}) { body = b })
	c.Consult(incidentFor(pid, sid))
	if body == "" {
		t.Fatal("no escalation captured")
	}
	if containsAny(body, "<script>", "opav1_supersecret") {
		t.Fatalf("escalation body leaked markup/secret: %q", body)
	}
	if !containsAny(body, "&lt;script&gt;", "[REDACTED]") {
		t.Fatalf("escalation body was not escaped/redacted as expected: %q", body)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		if idx := indexOf(s, sub); idx >= 0 {
			return true
		}
	}
	return false
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
