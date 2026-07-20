package automation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// TestMissionWorkerAttachBackfill: attaching an existing group session to a
// mission backfills its rolling last_report_ref from the latest dense report;
// a mission of another group is refused; the single-active rule surfaces typed.
func TestMissionWorkerAttachBackfill(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()

	sid, bearer := f.mintSession(t, f.memberProjectID)
	status, body := f.call(t, bearer, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusOK {
		t.Fatalf("elect: %d %v", status, body)
	}
	fence := int64(body["fence_version"].(float64))

	// worker session with a dense report
	workerID, workerBearer := f.mintSession(t, f.memberProjectID)
	reports, err := application.NewReportService(f.db)
	if err != nil {
		t.Fatal(err)
	}
	reportServer := httptest.NewServer(NewSessionReportHandler(f.db, reports))
	t.Cleanup(reportServer.Close)
	emitter := &coordinatorFixture{db: f.db, server: reportServer}
	if status, body = emitter.call(t, workerBearer, "POST", "/report", map[string]any{
		"turn_id": "m1", "objective": "obj", "summary": "sum",
	}); status != http.StatusOK {
		t.Fatalf("emit: %d %v", status, body)
	}
	wantRef, _ := f.db.LatestSessionReportRef(ctx, workerID)
	if wantRef == "" {
		t.Fatal("report ref missing")
	}

	// mission + attach
	status, body = f.call(t, bearer, "POST", "/missions", map[string]any{"goal": "ship it", "fence_version": fence})
	if status != http.StatusCreated {
		t.Fatalf("start mission: %d %v", status, body)
	}
	missionID := int64(body["mission_id"].(float64))

	status, body = f.call(t, bearer, "POST", "/missions", map[string]any{"goal": "again", "fence_version": fence})
	if status != http.StatusConflict || errCode(body) != "mission_already_active" {
		t.Fatalf("second active mission must be typed: %d %v", status, body)
	}

	status, body = f.call(t, bearer, "POST", "/missions/workers/attach", map[string]any{
		"mission_id": missionID, "session_id": workerID, "role": "qa", "fence_version": fence,
	})
	if status != http.StatusOK || body["last_report_ref"] != wantRef {
		t.Fatalf("attach backfill: %d %v (want ref %s)", status, body, wantRef)
	}
	workers, err := f.db.ListMissionWorkers(ctx, missionID)
	if err != nil || len(workers) != 1 || workers[0].SessionID != workerID || workers[0].LastReportRef != wantRef {
		t.Fatalf("roster row wrong: %+v err=%v", workers, err)
	}

	// a fresh report refreshes the rolling ref via the emit path
	if status, body = emitter.call(t, workerBearer, "POST", "/report", map[string]any{
		"turn_id": "m2", "objective": "obj2", "summary": "sum2",
	}); status != http.StatusOK {
		t.Fatalf("emit m2: %d %v", status, body)
	}
	newRef, _ := f.db.LatestSessionReportRef(ctx, workerID)
	workers, _ = f.db.ListMissionWorkers(ctx, missionID)
	if newRef == wantRef || workers[0].LastReportRef != newRef {
		t.Fatalf("rolling ref not refreshed on emit: %s vs %s", workers[0].LastReportRef, newRef)
	}

	// mission of another group → mismatch for this coordinator
	otherMission, err := f.db.CreateMission(ctx, "other", 9999, "sess-else")
	if err != nil {
		t.Fatal(err)
	}
	status, body = f.call(t, bearer, "POST", "/missions/workers/attach", map[string]any{
		"mission_id": otherMission.ID, "session_id": workerID, "role": "qa", "fence_version": fence,
	})
	if status != http.StatusForbidden || errCode(body) != "mission_group_mismatch" {
		t.Fatalf("cross-group mission must be refused: %d %v", status, body)
	}
	_ = sid
}

// TestMergeRequiresMissionGrant: the merge gate refuses with the typed
// "conversational" code before any dispatch when the mission has no grant.
func TestMergeRequiresMissionGrant(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	_, bearer := f.mintSession(t, f.memberProjectID)
	status, body := f.call(t, bearer, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusOK {
		t.Fatalf("elect: %d %v", status, body)
	}
	fence := int64(body["fence_version"].(float64))
	status, body = f.call(t, bearer, "POST", "/missions", map[string]any{"goal": "integrate", "fence_version": fence})
	if status != http.StatusCreated {
		t.Fatalf("mission: %d %v", status, body)
	}
	missionID := int64(body["mission_id"].(float64))

	// A workspace row in the member project (no real git needed — the grant
	// check fires BEFORE any dispatch).
	if _, err := f.db.Exec(`INSERT INTO workspaces (id, project_id, name, branch, base_ref, path, status)
		VALUES ('ws-m', ?, 'm', 'openpoet/m', 'main', '/tmp/ws-m', 'ready')`, f.memberProjectID); err != nil {
		t.Fatal(err)
	}
	status, body = f.call(t, bearer, "POST", "/workspaces/ws-m/merge", map[string]any{
		"mission_id": missionID, "fence_version": fence,
	})
	if status != http.StatusForbidden || errCode(body) != "mission_grant_required" {
		t.Fatalf("grant-less merge must be typed: %d %v", status, body)
	}

	// Exhausted grant → the other half of the conversation.
	if err := f.db.CreateMissionGrant(ctx, &database.MissionGrant{
		MissionID: missionID, Capability: "workspaces.merge", UsesRemaining: 0,
		ExpiresAt: time.Now().Add(time.Hour), GrantedBy: "user",
	}); err != nil {
		t.Fatal(err)
	}
	status, body = f.call(t, bearer, "POST", "/workspaces/ws-m/merge", map[string]any{
		"mission_id": missionID, "fence_version": fence,
	})
	if status != http.StatusForbidden || errCode(body) != "mission_grant_exhausted" {
		t.Fatalf("exhausted merge must be typed: %d %v", status, body)
	}
}
