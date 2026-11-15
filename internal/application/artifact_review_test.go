package application

import (
	"context"
	"testing"

	"openpoet/internal/database"
)

// An artifact created with a review waits for the user's decision: it says so
// in its views and is left out of a listing of documents.
func TestArtifactWithAReviewIsNotListedAsADocument(t *testing.T) {
	ctx := context.Background()
	service, db, _ := artifactTestService(t)
	auth := phase4Authorization()
	plain, err := service.Create(ctx, CreateArtifactCommand{Title: "Notes", Content: "# Notes\n", Format: "markdown", Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := service.Create(ctx, CreateArtifactCommand{Title: "Skill: deploy", Content: "# Skill Proposal\n", Format: "markdown",
		Review: &ArtifactReviewRequest{Kind: database.ArtifactReviewSkill, MessageID: 9}, Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Artifact.Review != nil || proposal.Artifact.Review == nil || proposal.Artifact.Review.Kind != "skill" || proposal.Artifact.Review.Status != "pending" {
		t.Fatalf("reviews in the publish results: plain %+v, proposal %+v", plain.Artifact.Review, proposal.Artifact.Review)
	}
	if stored, err := db.GetArtifactReview(ctx, proposal.Artifact.ID); err != nil || stored == nil || stored.MessageID != 9 {
		t.Fatalf("stored review = %+v err=%v", stored, err)
	}
	if view, err := service.Get(ctx, proposal.Artifact.ID); err != nil || view.Review == nil || view.Review.Kind != "skill" {
		t.Fatalf("detail of the proposal = %+v err=%v", view, err)
	}
	if view, err := service.Get(ctx, plain.Artifact.ID); err != nil || view.Review != nil {
		t.Fatalf("detail of the plain document = %+v err=%v", view, err)
	}

	documents, err := service.List(ctx, ArtifactListQuery{})
	if err != nil || len(documents) != 1 || documents[0].ID != plain.Artifact.ID {
		t.Fatalf("listing of documents = %+v err=%v", documents, err)
	}
	all, err := service.List(ctx, ArtifactListQuery{IncludeReviewed: true})
	if err != nil || len(all) != 2 {
		t.Fatalf("listing with reviewed artifacts = %+v err=%v", all, err)
	}
	for _, view := range all {
		if view.ID == proposal.Artifact.ID && (view.Review == nil || view.Review.Status != "pending") {
			t.Fatalf("listed proposal = %+v", view)
		}
	}

	for name, review := range map[string]*ArtifactReviewRequest{
		"unknown kind":     {Kind: "wish"},
		"negative message": {Kind: database.ArtifactReviewTask, MessageID: -1},
	} {
		if _, err := service.Create(ctx, CreateArtifactCommand{Title: "X", Content: "x", Format: "markdown", Review: review, Authorization: auth}); !ErrorIsKind(err, ErrorValidation) {
			t.Fatalf("%s: err = %v, want a validation error", name, err)
		}
	}
}
