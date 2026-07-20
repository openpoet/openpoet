package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"openpoet/internal/database"
	"openpoet/internal/sessiontoken"
)

type coordinatorFixture struct {
	db     *database.DB
	server *httptest.Server
	tagID  int64
	// member project inside the group, and one outside it
	memberProjectID  int64
	outsideProjectID int64
}

func newCoordinatorFixture(t *testing.T) *coordinatorFixture {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "coord.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	member := &database.Project{Name: "member", Path: t.TempDir(), Type: "local"}
	if err := db.CreateProject(ctx, member); err != nil {
		t.Fatal(err)
	}
	outside := &database.Project{Name: "outside", Path: t.TempDir(), Type: "local"}
	if err := db.CreateProject(ctx, outside); err != nil {
		t.Fatal(err)
	}
	tag := &database.Tag{Name: "group", Color: "#fff"}
	if err := db.CreateTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE tags SET coordination=1 WHERE id=?", tag.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceProjectTagIDs(ctx, member.ID, []int64{tag.ID}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewCoordinatorHandler(db, Dependencies{}))
	t.Cleanup(server.Close)
	return &coordinatorFixture{
		db: db, server: server, tagID: tag.ID,
		memberProjectID: member.ID, outsideProjectID: outside.ID,
	}
}

// mintSession creates a synthetic session with valid credentials and returns
// (sessionID, bearer).
func (f *coordinatorFixture) mintSession(t *testing.T, projectID int64) (string, string) {
	t.Helper()
	ctx := context.Background()
	// Session ids must be dot-free: opst1_ tokens embed the id as
	// "<id>.<secret>" (real ids are UUIDs).
	id := fmt.Sprintf("sess-%d", time.Now().UnixNano())
	if err := f.db.CreateSession(ctx, &database.Session{
		ID: id, ProjectID: projectID, Status: "running", StartTime: time.Now(),
		Backend: "claude_code", Model: "unknown", RequestedModel: "default", Effort: "default",
	}); err != nil {
		t.Fatal(err)
	}
	token, hash, err := sessiontoken.NewMCPToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateSessionTokenHashes(ctx, id, hash, "unused"); err != nil {
		t.Fatal(err)
	}
	return id, token
}

func (f *coordinatorFixture) call(t *testing.T, bearer, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// errCode digs the typed code out of the {"error":{"code":...}} envelope.
func errCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

// TestCoordinatorElectAndRenew: the initiating session wins the lease at fence
// v1; renewing as the holder bumps the fence; a rival session cannot take a
// live lease and the conflict names the current holder.
func TestCoordinatorElectAndRenew(t *testing.T) {
	f := newCoordinatorFixture(t)
	sid, bearer := f.mintSession(t, f.memberProjectID)

	status, body := f.call(t, bearer, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusOK || body["elected"] != true {
		t.Fatalf("first elect: status=%d body=%v", status, body)
	}
	if body["fence_version"].(float64) != 1 {
		t.Fatalf("expected fence v1, got %v", body["fence_version"])
	}
	// renewal by the holder bumps the fence
	status, body = f.call(t, bearer, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusOK || body["elected"] != true || body["fence_version"].(float64) != 2 {
		t.Fatalf("renew: status=%d body=%v", status, body)
	}
	// a rival member session loses and learns who holds the lease
	_, rivalBearer := f.mintSession(t, f.memberProjectID)
	status, body = f.call(t, rivalBearer, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusConflict || body["elected"] != false || body["holder_session_id"] != sid {
		t.Fatalf("rival elect should conflict naming holder: status=%d body=%v", status, body)
	}
}

// TestCoordinatorElectRequiresMembership: a session whose project is outside
// the group cannot elect itself; a non-coordination tag is refused.
func TestCoordinatorElectRequiresMembership(t *testing.T) {
	f := newCoordinatorFixture(t)
	_, outsiderBearer := f.mintSession(t, f.outsideProjectID)
	status, body := f.call(t, outsiderBearer, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusForbidden || errCode(body) != "coordinator_not_member" {
		t.Fatalf("outsider elect: status=%d body=%v", status, body)
	}

	plain := &database.Tag{Name: "plain", Color: "#000"}
	if err := f.db.CreateTag(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	_, memberBearer := f.mintSession(t, f.memberProjectID)
	status, body = f.call(t, memberBearer, "POST", "/elect", map[string]any{"group": plain.ID})
	if status != http.StatusBadRequest || errCode(body) != "coordinator_group_invalid" {
		t.Fatalf("non-coordination tag: status=%d body=%v", status, body)
	}

	// no bearer at all → 401
	status, _ = f.call(t, "", "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusUnauthorized {
		t.Fatalf("tokenless elect: status=%d", status)
	}

	// one coordinator lease per session: holding group A forbids electing into
	// group B (keeps lease→group resolution deterministic).
	second := &database.Tag{Name: "second-group", Color: "#0f0"}
	if err := f.db.CreateTag(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec("UPDATE tags SET coordination=1 WHERE id=?", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceProjectTagIDs(context.Background(), f.memberProjectID, []int64{f.tagID, second.ID}); err != nil {
		t.Fatal(err)
	}
	_, holderBearer := f.mintSession(t, f.memberProjectID)
	if status, body = f.call(t, holderBearer, "POST", "/elect", map[string]any{"group": f.tagID}); status != http.StatusOK {
		t.Fatalf("elect into first group: %d %v", status, body)
	}
	status, body = f.call(t, holderBearer, "POST", "/elect", map[string]any{"group": second.ID})
	if status != http.StatusConflict || errCode(body) != "coordinator_lease_held_elsewhere" {
		t.Fatalf("second-group elect must be refused: status=%d body=%v", status, body)
	}
}

// TestCoordinatorFenceStaleFailsClosed: after the lease changes hands, the
// ex-holder's fence is rejected with coordinator_fence_stale BEFORE any side
// effect (no dispatch happens — the fixture has no platform registry, so a
// dispatch attempt would 500, not 409).
func TestCoordinatorFenceStaleFailsClosed(t *testing.T) {
	f := newCoordinatorFixture(t)
	_, bearerA := f.mintSession(t, f.memberProjectID)
	status, body := f.call(t, bearerA, "POST", "/elect", map[string]any{"group": f.tagID})
	if status != http.StatusOK {
		t.Fatalf("elect A: %d %v", status, body)
	}
	oldFence := int64(body["fence_version"].(float64))

	// B takes over (simulating expiry-takeover by CAS on the live version).
	sidB, _ := f.mintSession(t, f.memberProjectID)
	value, _ := json.Marshal(map[string]string{"session_id": sidB})
	expected := oldFence
	if _, err := f.db.BlackboardPut(context.Background(), database.BlackboardPutInput{
		ScopeType: "group", ScopeID: f.tagID, Key: CoordinatorLeaseKey,
		ValueJSON: string(value), ExpectedVersion: &expected, TTLSeconds: 60, Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	status, body = f.call(t, bearerA, "POST", "/sessions", map[string]any{
		"project_id": f.memberProjectID, "fence_version": oldFence,
	})
	if status != http.StatusConflict || errCode(body) != "coordinator_fence_stale" {
		t.Fatalf("stale fence must fail closed: status=%d body=%v", status, body)
	}
	// A mutation without any lease at all is typed not_coordinator on reads.
	_, bearerC := f.mintSession(t, f.memberProjectID)
	status, body = f.call(t, bearerC, "GET", "/sessions", nil)
	if status != http.StatusForbidden || errCode(body) != "coordinator_not_coordinator" {
		t.Fatalf("lease-less read must be typed: status=%d body=%v", status, body)
	}
}

// TestCoordinatorTierScopeFromGroup: the session actor's ProjectFilter names
// the group tag, so the resolved scope allows exactly the member projects (and
// the tag itself) — the entire cross-project reach rests on the V65 enforcement.
func TestCoordinatorTierScopeFromGroup(t *testing.T) {
	f := newCoordinatorFixture(t)
	actor := coordinatorSessionActor("sess-x", f.tagID)
	if actor.Type != "session" || actor.ID != "sess-x" {
		t.Fatalf("actor identity wrong: %+v", actor)
	}
	if actor.Scopes.Has(ScopeApprovalsGrant) || actor.Scopes.Has(ScopeApprovalsSelf) {
		t.Fatal("coordinator session actor must never hold approvals scopes")
	}
	scope := resolveActorProjectScope(context.Background(), f.db, actor)
	if !scope.Restricted() {
		t.Fatal("scope must be restricted to the group")
	}
	if !scope.Allows(f.memberProjectID) {
		t.Fatal("member project must be in scope")
	}
	if scope.Allows(f.outsideProjectID) {
		t.Fatal("outside project must be out of scope")
	}
	if !scope.AllowsTag(f.tagID) {
		t.Fatal("the group tag itself must be allowed (blackboard group scope)")
	}
}
