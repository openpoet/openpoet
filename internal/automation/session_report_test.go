package automation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"openpoet/internal/application"
)

// TestEmitSessionReportSelf: the milestone report lands on the TOKEN's session
// (there is no session_id parameter to spoof), re-emitting the same turn_id
// updates instead of duplicating, finalize flips the state, and a tokenless
// call is rejected.
func TestEmitSessionReportSelf(t *testing.T) {
	f := newCoordinatorFixture(t)
	reports, err := application.NewReportService(f.db)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewSessionReportHandler(f.db, reports))
	t.Cleanup(server.Close)
	emitter := &coordinatorFixture{db: f.db, server: server}

	sid, bearer := f.mintSession(t, f.memberProjectID)

	status, body := emitter.call(t, bearer, "POST", "/report", map[string]any{
		"turn_id": "m1", "objective": "port the parser", "summary": "parser ported",
		"needs_from_coordinator": []string{"review contract"}, "next": "port the lexer",
	})
	if status != http.StatusOK || body["session_id"] != sid || body["turn_id"] != "m1" {
		t.Fatalf("emit: status=%d body=%v", status, body)
	}
	if body["state"] != "updated" || body["next_step"] != "port the lexer" {
		t.Fatalf("emit fields: %v", body)
	}

	// same turn_id updates in place and finalizes
	status, body = emitter.call(t, bearer, "POST", "/report", map[string]any{
		"turn_id": "m1", "objective": "port the parser", "summary": "parser + tests", "finalize": true,
	})
	if status != http.StatusOK || body["state"] != "finalized" {
		t.Fatalf("finalize: status=%d body=%v", status, body)
	}
	var count int
	if err := f.db.Get(&count, "SELECT COUNT(*) FROM structured_session_reports WHERE session_id=? AND turn_id='m1'", sid); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row for the milestone, got %d", count)
	}

	// tokenless → 401
	status, _ = emitter.call(t, "", "POST", "/report", map[string]any{"objective": "x", "summary": "y"})
	if status != http.StatusUnauthorized {
		t.Fatalf("tokenless emit: status=%d", status)
	}
}
