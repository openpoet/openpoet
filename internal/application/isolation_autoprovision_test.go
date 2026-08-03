package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openpoet/internal/database"
)

// Integration coverage for automatic worktree isolation, exercised against REAL
// git repositories and a real SQLite database — the two places the previous
// fake-port tests could not tell success from silent no-op.

func testActor() ActionAuthorization {
	return ActionAuthorization{Actor: Actor{Type: "test", ID: "t"}}
}

// newIsolationFixture builds a real one-commit git repo registered as a local
// claude_code project, plus a WorkspaceService running real git.
func newIsolationFixture(t *testing.T) (*database.DB, *database.Project, *WorkspaceService, context.Context) {
	t.Helper()
	db, err := database.New(filepath.Join(t.TempDir(), "iso.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	if err := writeTestFile(filepath.Join(repo, "a.go"), "package a\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-qm", "init")

	project := &database.Project{Name: "iso", Path: repo, Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	return db, project, NewWorkspaceService(db, realGitPort{}, noopSyncer{}), ctx
}

// startLiveMainSession inserts a live session pinned to the MAIN checkout, which
// is what makes isolation:"auto" decide the main path is busy.
func startLiveMainSession(t *testing.T, db *database.DB, projectID int64, id string) {
	t.Helper()
	if err := db.CreateSession(context.Background(), &database.Session{
		ID: id, ProjectID: projectID, Status: "running", StartTime: time.Now(),
		Backend: "claude_code",
	}); err != nil {
		t.Fatal(err)
	}
}

// worktreeCount counts the git worktrees registered for a repo (the main
// checkout always counts as one).
func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := realGitPort{}.RunGit(context.Background(),
		&database.Project{Path: repo, Type: "local"}, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	return strings.Count(out, "worktree ")
}

// TestIsolationMainCheckoutModes: the default must stay a zero-overhead main
// checkout. "" / "never" never provision, and "auto" does not provision while
// nothing else holds the tree.
func TestIsolationMainCheckoutModes(t *testing.T) {
	db, project, service, ctx := newIsolationFixture(t)
	_ = db
	for _, mode := range []string{"", IsolationNever, "NEVER", IsolationAuto, " auto "} {
		decision, err := service.ResolveIsolation(ctx, project.ID, mode, testActor())
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if decision.WorkspaceID != "" || decision.ReservationToken != "" {
			t.Fatalf("mode %q must use the main checkout, got %+v", mode, decision)
		}
	}
	if n := worktreeCount(t, project.Path); n != 1 {
		t.Fatalf("no lane should exist yet, found %d worktrees", n)
	}
}

// TestIsolationAutoProvisionsWhenMainBusy is the core fix: "auto" used to FAIL
// with no_workspace_ready when the main path was busy and no lane had been
// pre-provisioned. The whole point of automatic isolation is that the second
// session on a project gets its own tree without anyone planning for it.
func TestIsolationAutoProvisionsWhenMainBusy(t *testing.T) {
	db, project, service, ctx := newIsolationFixture(t)
	startLiveMainSession(t, db, project.ID, "live-main")

	decision, err := service.ResolveIsolation(ctx, project.ID, IsolationAuto, testActor())
	if err != nil {
		t.Fatalf("auto isolation must provision on demand, got error: %v", err)
	}
	if decision.WorkspaceID == "" {
		t.Fatal("auto isolation returned the busy main checkout")
	}
	if decision.ReservationToken == "" {
		t.Fatal("the decision must hand back the reservation that claims the lane")
	}

	ws, err := service.Get(ctx, decision.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	// Auto-opened lanes are marked so they stay distinguishable from lanes a
	// human named and expects to find again.
	if !strings.HasPrefix(ws.Name, "auto-") {
		t.Fatalf("platform-opened lane should be named auto-*, got %q", ws.Name)
	}
	// The lane is a REAL worktree on disk, registered with git, on its own branch.
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatalf("lane directory missing: %v", err)
	}
	if n := worktreeCount(t, project.Path); n != 2 {
		t.Fatalf("expected main + 1 lane worktree, found %d", n)
	}
	if ws.Branch != "openpoet/"+ws.Name {
		t.Fatalf("lane branch = %q", ws.Branch)
	}
	// It is claimed, not free: a peer must not be handed the same lane.
	if ws.Status != "leased" || ws.LeasedBySessionID.String != "pending:"+decision.ReservationToken {
		t.Fatalf("lane not reserved by the decision: status=%s lease=%q", ws.Status, ws.LeasedBySessionID.String)
	}
}

// TestIsolationAlwaysProvisionsEvenWhenMainFree: "always" is the deterministic
// mode an orchestrator uses when it already knows two workstreams overlap — it
// must not wait for the main path to look busy.
func TestIsolationAlwaysProvisionsEvenWhenMainFree(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)

	decision, err := service.ResolveIsolation(ctx, project.ID, IsolationAlways, testActor())
	if err != nil {
		t.Fatal(err)
	}
	if decision.WorkspaceID == "" {
		t.Fatal(`isolation:"always" used the main checkout`)
	}
	if n := worktreeCount(t, project.Path); n != 2 {
		t.Fatalf("expected main + 1 lane, found %d", n)
	}
}

// TestIsolationPrefersIdlePooledLane: provisioning runs git and possibly a whole
// environment manifest, so an already-idle lane must be reused rather than
// growing the worktree count on every request.
func TestIsolationPrefersIdlePooledLane(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	pooled, err := service.Create(ctx, CreateWorkspaceCommand{
		ProjectID: project.ID, Name: "pooled", Authorization: testActor(),
	})
	if err != nil {
		t.Fatal(err)
	}

	decision, err := service.ResolveIsolation(ctx, project.ID, IsolationAlways, testActor())
	if err != nil {
		t.Fatal(err)
	}
	if decision.WorkspaceID != pooled.ID {
		t.Fatalf("expected the idle pooled lane %s, got %s", pooled.ID, decision.WorkspaceID)
	}
	if n := worktreeCount(t, project.Path); n != 2 {
		t.Fatalf("a second worktree was provisioned instead of reusing the pool: %d", n)
	}
}

// TestIsolationConcurrentFanOutGivesDistinctLanes is the fan-out robustness
// case: an orchestrator starting N workers on one project at the same instant.
// Pick-and-reserve used to be two round-trips, so concurrent creates selected the
// same lane and all but one failed with a retryable error — precisely when
// isolation matters most. Every caller must now get its OWN lane, with no errors.
func TestIsolationConcurrentFanOutGivesDistinctLanes(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)

	const workers = 5
	var wg sync.WaitGroup
	ids := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			decision, err := service.ResolveIsolation(ctx, project.ID, IsolationAlways, testActor())
			ids[idx], errs[idx] = decision.WorkspaceID, err
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d failed to get a lane: %v", i, errs[i])
		}
		if ids[i] == "" {
			t.Fatalf("worker %d got the main checkout instead of a lane", i)
		}
		if seen[ids[i]] {
			t.Fatalf("lane %s handed to two workers — double booking", ids[i])
		}
		seen[ids[i]] = true
	}
	if n := worktreeCount(t, project.Path); n != workers+1 {
		t.Fatalf("expected main + %d lanes on disk, found %d", workers, n)
	}
}

// TestIsolationRejectsUnknownMode: an unrecognized mode must fail loudly. Silently
// treating it as "never" would put two sessions in one tree while the caller
// believed they were isolated.
func TestIsolationRejectsUnknownMode(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	_, err := service.ResolveIsolation(ctx, project.ID, "sometimes", testActor())
	if err == nil {
		t.Fatal("unknown isolation mode was silently accepted")
	}
	if code := applicationErrorCode(err); code != "invalid_isolation" {
		t.Fatalf("expected a typed invalid_isolation error, got %q (%v)", code, err)
	}
}

// TestResolveForSessionReservationHandshake: the lane comes back already claimed,
// so the session-create path must accept 'leased'-with-our-token and reject
// everything else — otherwise an isolated create would fail its own validation.
func TestResolveForSessionReservationHandshake(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	decision, err := service.ResolveIsolation(ctx, project.ID, IsolationAlways, testActor())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ResolveForSession(ctx, project, decision.WorkspaceID, decision.ReservationToken); err != nil {
		t.Fatalf("the reserving create must be able to use its own lane: %v", err)
	}
	_, err = service.ResolveForSession(ctx, project, decision.WorkspaceID, "someone-elses-token")
	if code := applicationErrorCode(err); code != "workspace_reservation_lost" {
		t.Fatalf("a foreign reservation token must be refused, got %q (%v)", code, err)
	}
	// A tokenless resolve of a leased lane is the old contract and must still refuse.
	_, err = service.ResolveForSession(ctx, project, decision.WorkspaceID, "")
	if code := applicationErrorCode(err); code != "workspace_not_ready" {
		t.Fatalf("a leased lane must not be handed to a create holding no reservation, got %q (%v)", code, err)
	}
}

// TestIsolatedLaneRoundTrip walks the whole point of the feature on real git:
// an isolated lane's edits are invisible to the main checkout, the merge is
// predicted clean, and merging folds the work back in.
func TestIsolatedLaneRoundTrip(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	decision, err := service.ResolveIsolation(ctx, project.ID, IsolationAlways, testActor())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := service.Get(ctx, decision.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}

	// Work happens in the lane.
	if err := writeTestFile(filepath.Join(ws.Path, "a.go"), "package a // isolated work\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, ws.Path, "commit", "-qam", "lane work")

	// The main checkout is untouched — that is the isolation.
	mainContent, err := os.ReadFile(filepath.Join(project.Path, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainContent), "isolated work") {
		t.Fatal("lane work leaked into the main checkout")
	}

	// The orchestrator can forecast the integration for free.
	prediction, err := service.PredictMerge(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !prediction.Clean {
		t.Fatalf("lane-only work should predict clean: %+v", prediction)
	}

	// Merging needs the lane free (the session ended) and explicit approval.
	if err := releaseLaneReservation(ctx, service, ws.ID); err != nil {
		t.Fatal(err)
	}
	result, err := service.Merge(ctx, MergeWorkspaceCommand{
		WorkspaceID: ws.ID,
		Authorization: ActionAuthorization{
			Actor: Actor{Type: "test", ID: "t"}, Approved: true, ApprovedBy: "test", Reason: "integration test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Merged {
		t.Fatalf("clean lane failed to merge: %+v", result)
	}
	merged, err := os.ReadFile(filepath.Join(project.Path, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "isolated work") {
		t.Fatal("merge reported success but the work is not in the main checkout")
	}
}

// releaseLaneReservation clears a lane's pending reservation so the destructive
// operations (which refuse a leased lane) can run.
func releaseLaneReservation(ctx context.Context, service *WorkspaceService, workspaceID string) error {
	ws, err := service.Get(ctx, workspaceID)
	if err != nil {
		return err
	}
	token := strings.TrimPrefix(ws.LeasedBySessionID.String, "pending:")
	return service.ReleaseReservation(ctx, workspaceID, token)
}
