package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestLatestSessionReportRef: the ref points at the most recently updated
// report of the session, "" when the session has none.
func TestLatestSessionReportRef(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "refs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	ref, err := db.LatestSessionReportRef(ctx, "sess-none")
	if err != nil || ref != "" {
		t.Fatalf("no-report session must yield empty ref (got %q, err %v)", ref, err)
	}

	upsert := func(reportID, turnID string) {
		t.Helper()
		err := db.WithReportTx(ctx, func(tx *ReportTx) error {
			return tx.UpsertSessionReport(ctx, &StructuredSessionReportRecord{
				ReportID: reportID, SessionID: "sess-r", TurnID: turnID,
				ReportDate: "2026-07-19", Timezone: "UTC", State: "updated",
				DecisionsJSON: "[]", VerificationJSON: "{}", EvidenceJSON: "[]",
				CompletedTaskIDsJSON: "[]", ChangedTaskIDsJSON: "[]", IncompleteReasonsJSON: "[]",
				NeedsFromCoordinator: "[]",
				SessionStartedAt:     time.Now(), TurnStartedAt: time.Now(),
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	upsert("rep-1", "m1")
	upsert("rep-2", "m2")
	// bump m2 as most recently updated deterministically
	if _, err := db.Exec("UPDATE structured_session_reports SET updated_at=datetime('now','+1 hour') WHERE report_id='rep-2'"); err != nil {
		t.Fatal(err)
	}

	ref, err = db.LatestSessionReportRef(ctx, "sess-r")
	if err != nil || ref != "rep-2" {
		t.Fatalf("expected rep-2, got %q (err %v)", ref, err)
	}
	record, err := db.LatestStructuredSessionReport(ctx, "sess-r")
	if err != nil || record == nil || record.ReportID != "rep-2" {
		t.Fatalf("LatestStructuredSessionReport mismatch: %+v err=%v", record, err)
	}
}
