package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/database"
)

// TestStartHydratesOpenIncidents pins the restart-identity fix: after a
// restart, re-evidence of a persisted open incident must reuse the stored id
// (no phantom ids in events, no duplicate escalation, no evidence regression).
func TestStartHydratesOpenIncidents(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "hydrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)

	row := database.CoordinatorIncident{
		ID: "C-persisted01", Rule: RuleFileOverlap, Severity: SeverityCritical, ProjectID: 1,
		ScopeKey: "local|/proj|shared.go|sa+sb", SessionsJSON: `["sa","sb"]`, State: "open",
		FirstDetectedAt: base, LastEvidenceAt: base, EvidenceCount: 5, DetailsJSON: `{"path":"shared.go"}`,
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertCoordinatorIncidentTx(tx, row); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	c := New(db)
	clock := base.Add(10 * time.Minute) // past the hysteresis window
	c.now = func() time.Time { return clock }
	escalations := 0
	c.OnEscalate = func(Incident) { escalations++ }
	c.hydrateIncidents()

	sessions := map[string]*sessionInfo{
		"sa": localSession("sa", 1, "/proj", 0),
		"sb": localSession("sb", 1, "/proj", 0),
	}
	for id, si := range sessions {
		si.fetchedAt = clock
		c.sessions[id] = si
	}

	// Same conflict re-evidences after the "restart".
	c.process(touchMsg("sa", "Edit", "/proj/shared.go", KindWrite, clock))
	c.process(touchMsg("sb", "Edit", "/proj/shared.go", KindWrite, clock))

	inc, ok := c.incidents[RuleFileOverlap+"|local|/proj|shared.go|sa+sb"]
	if !ok {
		t.Fatal("hydrated incident not found under its (rule, scope_key)")
	}
	if inc.ID != "C-persisted01" {
		t.Fatalf("incident id = %s, want the persisted C-persisted01 (phantom id minted)", inc.ID)
	}
	if inc.EvidenceCount != 6 {
		t.Fatalf("evidence_count = %d, want 6 (5 persisted + 1 new)", inc.EvidenceCount)
	}
	if escalations != 0 {
		t.Fatalf("hydrated incident escalated again (%d) — duplicate notification", escalations)
	}
	for _, ev := range c.pendingEvents {
		if ev.EventType == "conflict.detected" {
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["incident_id"] != "C-persisted01" {
				t.Fatalf("event carries phantom incident_id %v", payload["incident_id"])
			}
		}
	}
}
