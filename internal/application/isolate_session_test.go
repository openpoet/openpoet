package application

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
)

// Scenario coverage for Isolate — moving a LIVE session into its own worktree
// lane. The dangerous parts are the preconditions (a live PTY cannot chdir, so
// the move is a stop/reopen that loses the transcript and would strand
// uncommitted work), so most of this pins the refusals.

type isolateWorkspaces struct {
	lane          *database.Workspace
	dirty         []string
	resolveErr    error
	bindErr       error
	bound         string
	releasedFor   string
	dirtyProbedOn []string
}

func (w *isolateWorkspaces) ResolveForSession(context.Context, *database.Project, string, string) (*database.Workspace, error) {
	return w.lane, nil
}
func (w *isolateWorkspaces) Reserve(context.Context, string) (string, error) { return "tok", nil }
func (w *isolateWorkspaces) Bind(_ context.Context, workspaceID, _, sessionID string) error {
	if w.bindErr != nil {
		return w.bindErr
	}
	w.bound = workspaceID + ":" + sessionID
	return nil
}
func (w *isolateWorkspaces) ReleaseReservation(context.Context, string, string) error { return nil }
func (w *isolateWorkspaces) ReLease(context.Context, string, string) error            { return nil }
func (w *isolateWorkspaces) ReleaseForSession(_ context.Context, sessionID string) error {
	w.releasedFor = sessionID
	return nil
}
func (w *isolateWorkspaces) ResolveIsolation(context.Context, int64, string, ActionAuthorization) (IsolationDecision, error) {
	if w.resolveErr != nil {
		return IsolationDecision{}, w.resolveErr
	}
	return IsolationDecision{WorkspaceID: w.lane.ID, ReservationToken: "tok"}, nil
}
func (w *isolateWorkspaces) Get(context.Context, string) (*database.Workspace, error) {
	return w.lane, nil
}
func (w *isolateWorkspaces) DirtyPaths(_ context.Context, _ *database.Project, candidates []string) ([]string, error) {
	w.dirtyProbedOn = candidates
	return w.dirty, nil
}

func isolateApproval() ActionAuthorization {
	return ActionAuthorization{
		Actor: Actor{Type: "test", ID: "t"}, Approved: true, ApprovedBy: "t", Reason: "isolate",
	}
}

func newIsolateFixture(t *testing.T) (*SessionService, *phase3Store, *isolateWorkspaces, *phase3SessionEffects) {
	t.Helper()
	// Reopen stats the lane directory before restarting a runner into it, so the
	// fixture uses real paths rather than pretending.
	root := t.TempDir()
	lanePath := filepath.Join(root, ".openpoet", "worktrees", "auto-1")
	if err := os.MkdirAll(lanePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := &phase3Store{
		project: &database.Project{ID: 7, Path: root, Type: "local", Backend: "claude_code"},
		session: &database.Session{ID: "s1", ProjectID: 7, Status: "running"},
	}
	lanes := &isolateWorkspaces{lane: &database.Workspace{
		ID: "ws-1", ProjectID: 7, Name: "auto-1", Branch: "openpoet/auto-1",
		BaseRef: "main", Path: lanePath, Status: "leased",
	}}
	effects := &phase3SessionEffects{}
	service := NewSessionService(store, &phase3SessionManager{}, nil, nil, nil, nil, nil, effects,
		SessionCreationCollaborators{Workspaces: lanes})
	return service, store, lanes, effects
}

// TestIsolateMovesLiveSessionIntoItsOwnLane is the happy path: the session ends
// up repointed at the lane, the lane is leased to it, and it is told where it
// landed — a reopened session has no transcript, so the briefing is the only
// thing carrying the work forward.
func TestIsolateMovesLiveSessionIntoItsOwnLane(t *testing.T) {
	service, store, lanes, effects := newIsolateFixture(t)

	result, err := service.Isolate(context.Background(), IsolateSessionCommand{
		SessionID:     "s1",
		Reason:        "conflicts with session s2 on internal/foo.go",
		Briefing:      "Finish the retry backoff in internal/foo.go.",
		Authorization: isolateApproval(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Workspace.ID != "ws-1" {
		t.Fatalf("landed in workspace %q", result.Workspace.ID)
	}
	if store.session.WorkDir != result.Workspace.Path {
		t.Fatalf("session was not repointed at the lane: work_dir=%q", store.session.WorkDir)
	}
	if store.session.WorkspaceID.String != "ws-1" {
		t.Fatalf("session workspace_id = %q", store.session.WorkspaceID.String)
	}
	// The lease must move to the session BEFORE the restart, or the lane looks
	// free to a peer while the runner is down.
	if lanes.bound != "ws-1:s1" {
		t.Fatalf("lane was not bound to the session: %q", lanes.bound)
	}
	if len(effects.changes) == 0 || effects.changes[len(effects.changes)-1].Action != "isolated" {
		t.Fatalf("no 'isolated' change published: %+v", effects.changes)
	}
}

// TestIsolateBriefingIsSelfContained: the reopened session starts with an empty
// transcript, so the briefing has to state where it is, why, and what to do.
func TestIsolateBriefingIsSelfContained(t *testing.T) {
	lane := &database.Workspace{
		ID: "ws-1", Name: "auto-1", Branch: "openpoet/auto-1", BaseRef: "main",
		Path: "/proj/.openpoet/worktrees/auto-1",
	}
	brief := isolationBriefing(lane, IsolateSessionCommand{
		Reason:   "conflicts with session s2 on internal/foo.go",
		Briefing: "Finish the retry backoff.",
	})
	for _, want := range []string{
		"/proj/.openpoet/worktrees/auto-1", // where it is now
		"openpoet/auto-1",                  // the branch its work lands on
		"main",                             // what it branched from
		"conflicts with session s2",        // why it was moved
		"Finish the retry backoff.",        // the objective, since history is gone
		"Commit your work on this branch",  // how the work gets back
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("briefing is missing %q:\n%s", want, brief)
		}
	}
}

// TestIsolateRefusesUncommittedWork is the data-loss guard. A lane branches from
// HEAD, so work written but not committed would silently stay in the old tree.
// Refusing (and naming the files) is the only honest option — moving them is
// destructive surgery that needs its own approval.
func TestIsolateRefusesUncommittedWork(t *testing.T) {
	service, store, lanes, _ := newIsolateFixture(t)
	store.footprint = []string{"internal/foo.go", "internal/bar.go"}
	lanes.dirty = []string{"internal/foo.go"}

	_, err := service.Isolate(context.Background(), IsolateSessionCommand{
		SessionID: "s1", Authorization: isolateApproval(),
	})
	if code := applicationErrorCode(err); code != "session_has_uncommitted_work" {
		t.Fatalf("expected session_has_uncommitted_work, got %q (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "internal/foo.go") {
		t.Errorf("the refusal must name the files at risk: %v", err)
	}
	// Only the session's OWN writes are probed — a file somebody else dirtied is
	// not this session's problem and must not block its move.
	if len(lanes.dirtyProbedOn) != 2 {
		t.Errorf("dirty probe should cover the session footprint, got %v", lanes.dirtyProbedOn)
	}
	if store.session.WorkDir != "" {
		t.Error("a refused isolation must not repoint the session")
	}
}

// TestIsolateAllowsCommittedWork: a session whose writes are already committed
// moves cleanly — the lane branches from HEAD, which contains them.
func TestIsolateAllowsCommittedWork(t *testing.T) {
	service, store, lanes, _ := newIsolateFixture(t)
	store.footprint = []string{"internal/foo.go"}
	lanes.dirty = nil // committed → nothing dirty

	if _, err := service.Isolate(context.Background(), IsolateSessionCommand{
		SessionID: "s1", Authorization: isolateApproval(),
	}); err != nil {
		t.Fatalf("committed work should not block isolation: %v", err)
	}
}

// TestIsolateRefusals pins the remaining preconditions.
func TestIsolateRefusals(t *testing.T) {
	t.Run("requires explicit approval", func(t *testing.T) {
		service, _, _, _ := newIsolateFixture(t)
		_, err := service.Isolate(context.Background(), IsolateSessionCommand{
			SessionID:     "s1",
			Authorization: ActionAuthorization{Actor: Actor{Type: "test", ID: "t"}},
		})
		if err == nil {
			t.Fatal("stopping and restarting a live session must require approval")
		}
	})

	t.Run("only a live session", func(t *testing.T) {
		service, store, _, _ := newIsolateFixture(t)
		store.session.Status = "stopped"
		_, err := service.Isolate(context.Background(), IsolateSessionCommand{
			SessionID: "s1", Authorization: isolateApproval(),
		})
		if code := applicationErrorCode(err); code != "session_not_live" {
			t.Fatalf("expected session_not_live, got %q (%v)", code, err)
		}
	})

	t.Run("already isolated", func(t *testing.T) {
		service, store, _, _ := newIsolateFixture(t)
		store.session.WorkspaceID = sql.NullString{String: "ws-other", Valid: true}
		_, err := service.Isolate(context.Background(), IsolateSessionCommand{
			SessionID: "s1", Authorization: isolateApproval(),
		})
		if code := applicationErrorCode(err); code != "session_already_isolated" {
			t.Fatalf("expected session_already_isolated, got %q (%v)", code, err)
		}
	})
}

// TestIsolateReleasesLaneWhenTheStopFails: a lane claimed for a move that never
// happened must not stay leased to a session still living in the main tree.
func TestIsolateReleasesLaneWhenTheStopFails(t *testing.T) {
	root := t.TempDir()
	store := &phase3Store{
		project: &database.Project{ID: 7, Path: root, Type: "local", Backend: "claude_code"},
		session: &database.Session{ID: "s1", ProjectID: 7, Status: "running"},
	}
	lanes := &isolateWorkspaces{lane: &database.Workspace{
		ID: "ws-1", ProjectID: 7, Name: "auto-1", Branch: "openpoet/auto-1",
		Path: filepath.Join(root, ".openpoet", "worktrees", "auto-1"),
	}}
	manager := &phase3SessionManager{stopErr: errStopFailed}
	service := NewSessionService(store, manager, nil, nil, nil, nil, nil, &phase3SessionEffects{},
		SessionCreationCollaborators{Workspaces: lanes})

	if _, err := service.Isolate(context.Background(), IsolateSessionCommand{
		SessionID: "s1", Authorization: isolateApproval(),
	}); err == nil {
		t.Fatal("a failed stop must fail the isolation")
	}
	if lanes.releasedFor != "s1" {
		t.Fatalf("the claimed lane was not released after the failed move (released=%q)", lanes.releasedFor)
	}
	if store.session.WorkDir != "" {
		t.Error("a failed isolation must leave the session in its original tree")
	}
}

var errStopFailed = errStop{}

type errStop struct{}

func (errStop) Error() string { return "stop failed" }
