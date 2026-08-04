package application

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"openpoet/internal/database"
)

// Integration coverage for PlanMerges against REAL git repositories. The whole
// point is the lane-vs-lane signal, which no amount of faking would exercise
// honestly: two lanes that each rewrote the same file both predict "clean"
// against main, and only comparing them to EACH OTHER reveals the fight.

// laneWithEdit provisions a lane and commits one edit in it.
func laneWithEdit(t *testing.T, service *WorkspaceService, projectID int64, name string, files map[string]string) *database.Workspace {
	t.Helper()
	ws, err := service.Create(context.Background(), CreateWorkspaceCommand{
		ProjectID: projectID, Name: name, Authorization: testActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		if err := writeTestFile(filepath.Join(ws.Path, path), content); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, ws.Path, "add", ".")
	gitRun(t, ws.Path, "commit", "-qm", "work in "+name)
	return ws
}

func planEntry(t *testing.T, plan *MergePlan, name string) MergePlanEntry {
	t.Helper()
	for _, e := range plan.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("lane %q missing from the plan (%d entries)", name, len(plan.Entries))
	return MergePlanEntry{}
}

// TestPlanMergesOrdersLeastEntangledFirst is the motivating case: three lanes,
// two of which rewrote the same file. The independent lane must be scheduled
// first, and the two entangled ones must be told about each other BY NAME and BY
// FILE — merging blind would take one of them into an avoidable conflict.
func TestPlanMergesOrdersLeastEntangledFirst(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	// The fixture repo ships a.go; add the files the lanes will fight over.
	if err := writeTestFile(filepath.Join(project.Path, "util.go"), "package util\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(filepath.Join(project.Path, "solo.go"), "package solo\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, project.Path, "add", ".")
	gitRun(t, project.Path, "commit", "-qm", "seed")

	laneWithEdit(t, service, project.ID, "alpha", map[string]string{"util.go": "package util // alpha\n"})
	laneWithEdit(t, service, project.ID, "beta", map[string]string{"util.go": "package util // beta\n"})
	laneWithEdit(t, service, project.ID, "solo", map[string]string{"solo.go": "package solo // solo\n"})

	plan, err := service.PlanMerges(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 3 {
		t.Fatalf("expected 3 mergeable lanes, got %d", len(plan.Entries))
	}
	if plan.Entries[0].Name != "solo" {
		t.Fatalf("the independent lane must be merged first, got %q", plan.Entries[0].Name)
	}
	if plan.Independent != 1 {
		t.Fatalf("independent count = %d, want 1", plan.Independent)
	}

	// Every lane is clean against MAIN — which is exactly why the lane-vs-lane
	// comparison has to exist.
	for _, entry := range plan.Entries {
		if !entry.Clean {
			t.Fatalf("lane %q should predict clean against main: %+v", entry.Name, entry.ConflictFiles)
		}
	}

	alpha := planEntry(t, plan, "alpha")
	if len(alpha.CollidesWith) != 1 || alpha.CollidesWith[0].Name != "beta" {
		t.Fatalf("alpha should collide with beta, got %+v", alpha.CollidesWith)
	}
	if len(alpha.CollidesWith[0].Files) != 1 || alpha.CollidesWith[0].Files[0] != "util.go" {
		t.Fatalf("the collision must name the contested file, got %+v", alpha.CollidesWith[0].Files)
	}
	// Symmetric: beta must hear about alpha too.
	beta := planEntry(t, plan, "beta")
	if len(beta.CollidesWith) != 1 || beta.CollidesWith[0].Name != "alpha" {
		t.Fatalf("beta should collide with alpha, got %+v", beta.CollidesWith)
	}
	if len(planEntry(t, plan, "solo").CollidesWith) != 0 {
		t.Fatal("the independent lane must report no collisions")
	}

	// The plan is an ORDER, not a set of verdicts: each merge moves HEAD and
	// invalidates the `clean` of everything still queued.
	if !plan.RevalidateBeforeEachMerge {
		t.Fatal("the plan must tell the caller to re-predict before each merge")
	}
}

// TestPlanMergesRealityCheck proves the ordering is not cosmetic: following it,
// the first merge lands clean; the collision the plan predicted is exactly the
// one the second merge then hits.
func TestPlanMergesRealityCheck(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	if err := writeTestFile(filepath.Join(project.Path, "util.go"), "package util\n"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, project.Path, "add", ".")
	gitRun(t, project.Path, "commit", "-qm", "seed")

	alpha := laneWithEdit(t, service, project.ID, "alpha", map[string]string{"util.go": "package util // alpha\n"})
	beta := laneWithEdit(t, service, project.ID, "beta", map[string]string{"util.go": "package util // beta\n"})

	plan, err := service.PlanMerges(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Entries[0]
	if len(first.CollidesWith) == 0 {
		t.Fatal("both lanes touch util.go; the plan should say so")
	}

	// Merge the first in the planned order.
	firstID := first.WorkspaceID
	if err := releaseLaneReservation(ctx, service, firstID); err != nil {
		t.Fatal(err)
	}
	result, err := service.Merge(ctx, MergeWorkspaceCommand{
		WorkspaceID: firstID,
		Authorization: ActionAuthorization{
			Actor: Actor{Type: "test", ID: "t"}, Approved: true, ApprovedBy: "t", Reason: "plan step 1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Merged {
		t.Fatalf("the first lane in the plan should merge cleanly: %+v", result.ConflictFiles)
	}

	// The other lane now predicts the conflict the plan warned about.
	secondID := alpha.ID
	if firstID == alpha.ID {
		secondID = beta.ID
	}
	prediction, err := service.PredictMerge(ctx, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if prediction.Clean {
		t.Fatal("after the first merge the second lane must no longer predict clean — that is why the plan says to re-predict")
	}
	if len(prediction.ConflictFiles) == 0 || prediction.ConflictFiles[0] != "util.go" {
		t.Fatalf("the conflict should be the file the plan named, got %+v", prediction.ConflictFiles)
	}
}

// TestPlanMergesSkipsNothingToMerge: lanes with no commits of their own, and
// lanes already merged or removed, are not integration work.
func TestPlanMergesSkipsNothingToMerge(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)

	// A freshly provisioned lane has no commits ahead of its base.
	if _, err := service.Create(ctx, CreateWorkspaceCommand{
		ProjectID: project.ID, Name: "empty", Authorization: testActor(),
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanMerges(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("a lane with no commits is not merge work: %+v", plan.Entries)
	}

	// One real lane shows up.
	laneWithEdit(t, service, project.ID, "real", map[string]string{"a.go": "package a // real\n"})
	plan, err = service.PlanMerges(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Name != "real" {
		t.Fatalf("expected only the lane with commits, got %+v", plan.Entries)
	}
	if !strings.EqualFold(plan.Entries[0].Branch, "openpoet/real") {
		t.Fatalf("entry branch = %q", plan.Entries[0].Branch)
	}
	if len(plan.Entries[0].ChangedFiles) != 1 || plan.Entries[0].ChangedFiles[0] != "a.go" {
		t.Fatalf("changed files = %+v", plan.Entries[0].ChangedFiles)
	}
}

// TestPlanMergesEmptyProject: a project with no lanes plans nothing, and says so
// without erroring.
func TestPlanMergesEmptyProject(t *testing.T) {
	_, project, service, ctx := newIsolationFixture(t)
	plan, err := service.PlanMerges(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 0 || plan.Independent != 0 {
		t.Fatalf("empty project should plan nothing, got %+v", plan)
	}
	if plan.ProjectID != project.ID {
		t.Fatalf("plan project = %d", plan.ProjectID)
	}
}
